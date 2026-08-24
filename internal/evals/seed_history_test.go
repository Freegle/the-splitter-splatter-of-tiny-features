package evals

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

// seedTestRepo builds a real throwaway git repository with a small,
// deliberately dated commit history covering every seed-history selection
// case: a root commit, a feature commit, a fix-up commit for it (within 14
// days, same file), an unrelated clean commit never revisited, an oversize
// commit, and a real merge commit. It returns the repo path and the shas
// of the feature/fix-up commits (for evidence assertions) plus the merge
// commit's sha (for the "merges are skipped" assertion).
func seedTestRepo(t *testing.T) (repoPath string, featureSHA, fixupSHA, mergeSHA string) {
	t.Helper()
	repoPath = t.TempDir()

	runGit(t, repoPath, "init", "-q", "-b", "main")
	writeSeedFile(t, repoPath, ".gitattributes", "* text=auto eol=lf\n")
	commitGit(t, repoPath, "root", "2024-01-01T00:00:00Z")

	writeSeedFile(t, repoPath, "internal/greet.go", "package greet\n\nfunc Greet(name string) string {\n\treturn \"Hello, World!\"\n}\n")
	featureSHA = commitGit(t, repoPath, "add greet function", "2024-01-02T00:00:00Z")

	writeSeedFile(t, repoPath, "internal/greet.go", "package greet\n\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name + \"!\"\n}\n")
	fixupSHA = commitGit(t, repoPath, "fix: greet ignores the name parameter", "2024-01-05T00:00:00Z")

	writeSeedFile(t, repoPath, "internal/util.go", "package util\n\nfunc Double(n int) int {\n\treturn n * 2\n}\n")
	commitGit(t, repoPath, "add double helper", "2024-01-10T00:00:00Z")

	var big strings.Builder
	for i := 0; i < 150; i++ {
		big.WriteString("// padding line for the oversize commit test\n")
	}
	writeSeedFile(t, repoPath, "internal/biglist.go", big.String())
	commitGit(t, repoPath, "add generated padding file", "2024-01-11T00:00:00Z")

	runGit(t, repoPath, "checkout", "-q", "-b", "side-branch")
	writeSeedFile(t, repoPath, "internal/side.go", "package side\n\nfunc Side() int { return 1 }\n")
	commitGit(t, repoPath, "add side branch file", "2024-01-12T00:00:00Z")
	runGit(t, repoPath, "checkout", "-q", "main")
	writeSeedFile(t, repoPath, "internal/other.go", "package other\n\nfunc Other() int { return 2 }\n")
	commitGit(t, repoPath, "add other file on main", "2024-01-13T00:00:00Z")
	runGitWithEnv(t, repoPath, []string{"GIT_AUTHOR_DATE=2024-01-14T00:00:00Z", "GIT_COMMITTER_DATE=2024-01-14T00:00:00Z"},
		"merge", "-q", "--no-ff", "-m", "Merge branch side-branch", "side-branch")
	mergeSHA = strings.TrimSpace(runGit(t, repoPath, "rev-parse", "HEAD"))

	return repoPath, featureSHA, fixupSHA, mergeSHA
}

func writeSeedFile(t *testing.T, repoPath, rel, content string) {
	t.Helper()
	full := filepath.Join(repoPath, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating parent dir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
}

func commitGit(t *testing.T, repoPath, message, date string) string {
	t.Helper()
	runGit(t, repoPath, "add", "-A")
	runGitWithEnv(t, repoPath, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "commit", "-q", "-m", message)
	return strings.TrimSpace(runGit(t, repoPath, "rev-parse", "HEAD"))
}

func runGit(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	return runGitWithEnv(t, repoPath, nil, args...)
}

func runGitWithEnv(t *testing.T, repoPath string, extraEnv []string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", repoPath, "-c", "user.email=test@example.com", "-c", "user.name=test"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func openSeedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "splitter.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

func TestSeedHistory(t *testing.T) {
	repoPath, featureSHA, fixupSHA, mergeSHA := seedTestRepo(t)
	db := openSeedTestDB(t)
	cfg := config.Default()
	cfg.RepoPath = repoPath

	summary, err := SeedHistory(db, cfg, SeedHistoryOptions{
		RepoPath: repoPath, Since: "2020-01-01", MaxFiles: 3, MaxDiffLines: 120,
	})
	if err != nil {
		t.Fatalf("SeedHistory: %v", err)
	}

	if summary.Inserted == 0 {
		t.Fatal("expected at least one task to be inserted")
	}
	if summary.SkippedOversize == 0 {
		t.Error("expected the 150-line padding commit to be skipped as oversize")
	}
	if summary.SkippedMergeOrRoot == 0 {
		t.Error("expected at least the root commit to be skipped as merge/root")
	}

	tasks, err := store.EvalTasksByOrigin(db, OriginHistory)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}

	byCommit := map[string]store.EvalTaskRow{}
	for _, task := range tasks {
		c := ParseCharacteristics(task.Characteristics.String)
		if c.CommitSHA == "" {
			t.Errorf("task %d has no commit_sha recorded", task.ID)
			continue
		}
		byCommit[c.CommitSHA] = task
	}

	// The merge commit must never become a task.
	if _, ok := byCommit[mergeSHA]; ok {
		t.Error("merge commit should never be seeded as a task")
	}

	featureTask, ok := byCommit[featureSHA]
	if !ok {
		t.Fatal("expected the feature commit to be seeded")
	}
	featureC := ParseCharacteristics(featureTask.Characteristics.String)
	if featureC.BriefSource != BriefSourceCommitSubject {
		t.Errorf("feature task brief_source = %q, want %q", featureC.BriefSource, BriefSourceCommitSubject)
	}
	if featureTask.Difficulty.String != DifficultyChallenging {
		t.Errorf("feature task difficulty = %q, want %q (it has a fix-up follow-up)", featureTask.Difficulty.String, DifficultyChallenging)
	}
	if !strings.Contains(featureC.Evidence["difficulty"], fixupSHA) {
		t.Errorf("feature task difficulty evidence = %q, want it to name the fix-up commit %s", featureC.Evidence["difficulty"], fixupSHA)
	}
	if featureTask.RepoHead.String == "" {
		t.Error("feature task should have repo_head set to its parent commit")
	}

	fixupTask, ok := byCommit[fixupSHA]
	if !ok {
		t.Fatal("expected the fix-up commit to be seeded")
	}
	if fixupTask.Difficulty.String != DifficultyChallenging {
		t.Errorf("fix-up task difficulty = %q, want %q (its own subject matches the fix pattern)", fixupTask.Difficulty.String, DifficultyChallenging)
	}
	if fixupTask.RepoHead.String != featureSHA {
		t.Errorf("fix-up task repo_head = %q, want the feature commit sha %q (its parent)", fixupTask.RepoHead.String, featureSHA)
	}

	// The unrelated "add double helper" commit is never revisited by a
	// fix-pattern commit, so it should be simple.
	var cleanTask *store.EvalTaskRow
	for sha, task := range byCommit {
		if sha != featureSHA && sha != fixupSHA && strings.Contains(task.Brief, "double helper") {
			taskCopy := task
			cleanTask = &taskCopy
		}
	}
	if cleanTask == nil {
		t.Fatal("expected the 'add double helper' commit to be seeded")
	}
	if cleanTask.Difficulty.String != DifficultySimple {
		t.Errorf("clean task difficulty = %q, want %q", cleanTask.Difficulty.String, DifficultySimple)
	}
}

func TestSeedHistory_DedupOnRerun(t *testing.T) {
	repoPath, _, _, _ := seedTestRepo(t)
	db := openSeedTestDB(t)
	cfg := config.Default()
	cfg.RepoPath = repoPath

	opts := SeedHistoryOptions{RepoPath: repoPath, Since: "2020-01-01", MaxFiles: 3, MaxDiffLines: 120}

	first, err := SeedHistory(db, cfg, opts)
	if err != nil {
		t.Fatalf("first SeedHistory: %v", err)
	}
	if first.Inserted == 0 {
		t.Fatal("expected the first run to insert tasks")
	}

	second, err := SeedHistory(db, cfg, opts)
	if err != nil {
		t.Fatalf("second SeedHistory: %v", err)
	}
	if second.Inserted != 0 {
		t.Errorf("second run Inserted = %d, want 0 (every commit already seeded)", second.Inserted)
	}
	if second.Deduped != first.Inserted {
		t.Errorf("second run Deduped = %d, want %d (equal to what the first run inserted)", second.Deduped, first.Inserted)
	}
}

func TestSeedHistory_MaxLimitsInsertedCount(t *testing.T) {
	repoPath, _, _, _ := seedTestRepo(t)
	db := openSeedTestDB(t)
	cfg := config.Default()
	cfg.RepoPath = repoPath

	summary, err := SeedHistory(db, cfg, SeedHistoryOptions{
		RepoPath: repoPath, Since: "2020-01-01", Max: 1, MaxFiles: 3, MaxDiffLines: 120,
	})
	if err != nil {
		t.Fatalf("SeedHistory: %v", err)
	}
	if summary.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1 (bounded by -max)", summary.Inserted)
	}
}

// TestSeedHistory_ContextCapIsConfigurable builds two commits touching the
// same file: one that ADDS it (a new file's content is never shown as
// context, per buildSeedRequest, so this commit's synthesized request
// stays small) and one that later MODIFIES it (whose request context
// includes the file's full ~28KB parent-state content: too large for the
// old, hardcoded 20KB cap, but under the new default of 64KB). Confirms
// both that the raised default admits the modify commit and that a
// caller-configured lower cap still excludes it specifically.
func TestSeedHistory_ContextCapIsConfigurable(t *testing.T) {
	repoPath := t.TempDir()
	runGit(t, repoPath, "init", "-q", "-b", "main")
	writeSeedFile(t, repoPath, ".gitattributes", "* text=auto eol=lf\n")
	commitGit(t, repoPath, "root", "2024-01-01T00:00:00Z")

	var big strings.Builder
	for i := 0; i < 400; i++ {
		big.WriteString("// a real source line long enough to add up quickly across many lines\n")
	}
	writeSeedFile(t, repoPath, "internal/verylarge.go", big.String())
	commitGit(t, repoPath, "add a large file", "2024-01-02T00:00:00Z")

	writeSeedFile(t, repoPath, "internal/verylarge.go", strings.Replace(big.String(), "// a real", "// a fixed", 1))
	commitGit(t, repoPath, "touch the large file, exceeding the old 20KB cap alone", "2024-01-03T00:00:00Z")

	opts := SeedHistoryOptions{RepoPath: repoPath, Since: "2020-01-01", MaxFiles: 3, MaxDiffLines: 500}

	t.Run("default 64KB cap admits both", func(t *testing.T) {
		db := openSeedTestDB(t)
		cfg := config.Default()
		cfg.RepoPath = repoPath

		summary, err := SeedHistory(db, cfg, opts)
		if err != nil {
			t.Fatalf("SeedHistory: %v", err)
		}
		if summary.Inserted != 2 {
			t.Errorf("Inserted = %d, want 2 (the 64KB default cap should admit both commits)", summary.Inserted)
		}
		if summary.SkippedContextCap != 0 {
			t.Errorf("SkippedContextCap = %d, want 0", summary.SkippedContextCap)
		}
	})

	t.Run("a configured lower cap still skips the large modify commit", func(t *testing.T) {
		db := openSeedTestDB(t)
		cfg := config.Default()
		cfg.RepoPath = repoPath
		cfg.Evals.SeedContextBytes = 10 * 1024

		summary, err := SeedHistory(db, cfg, opts)
		if err != nil {
			t.Fatalf("SeedHistory: %v", err)
		}
		if summary.Inserted != 1 {
			t.Errorf("Inserted = %d, want 1 (only the small 'add' commit fits under a 10KB cap)", summary.Inserted)
		}
		if summary.SkippedContextCap != 1 {
			t.Errorf("SkippedContextCap = %d, want 1 (the large 'modify' commit)", summary.SkippedContextCap)
		}
	})
}

// TestSeedHistory_SynthesizedRequestMaxTokens confirms the synthesized
// request's max_tokens comes from [evals].max_answer_tokens, and that an
// unconfigured EvalsConfig still floors it to the package default rather
// than sending 0 (which internal/backend.ToOpenAI would otherwise quietly
// default to 4096, the bug this floor exists to prevent).
func TestSeedHistory_SynthesizedRequestMaxTokens(t *testing.T) {
	repoPath, _, _, _ := seedTestRepo(t)
	opts := SeedHistoryOptions{RepoPath: repoPath, Since: "2020-01-01", MaxFiles: 3, MaxDiffLines: 120}

	t.Run("configured value", func(t *testing.T) {
		db := openSeedTestDB(t)
		cfg := config.Default()
		cfg.RepoPath = repoPath
		cfg.Evals.MaxAnswerTokens = 9000

		if _, err := SeedHistory(db, cfg, opts); err != nil {
			t.Fatalf("SeedHistory: %v", err)
		}
		assertAllSeededRequestsHaveMaxTokens(t, db, 9000)
	})

	t.Run("unset falls back to the package default", func(t *testing.T) {
		db := openSeedTestDB(t)
		cfg := config.Default()
		cfg.RepoPath = repoPath
		cfg.Evals.MaxAnswerTokens = 0

		if _, err := SeedHistory(db, cfg, opts); err != nil {
			t.Fatalf("SeedHistory: %v", err)
		}
		assertAllSeededRequestsHaveMaxTokens(t, db, defaultMaxAnswerTokens)
	})
}

func assertAllSeededRequestsHaveMaxTokens(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	tasks, err := store.EvalTasksByOrigin(db, OriginHistory)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected at least one seeded task")
	}
	for _, task := range tasks {
		reqJSON, err := store.Decompress(task.RequestZstd)
		if err != nil {
			t.Fatalf("decompressing request for task %d: %v", task.ID, err)
		}
		var req anthropicMessagesRequestMaxTokensOnly
		if err := json.Unmarshal(reqJSON, &req); err != nil {
			t.Fatalf("decoding request for task %d: %v", task.ID, err)
		}
		if req.MaxTokens != want {
			t.Errorf("task %d request max_tokens = %d, want %d", task.ID, req.MaxTokens, want)
		}
	}
}

// anthropicMessagesRequestMaxTokensOnly decodes just the max_tokens field
// of a stored request, enough for assertAllSeededRequestsHaveMaxTokens.
type anthropicMessagesRequestMaxTokensOnly struct {
	MaxTokens int `json:"max_tokens"`
}

// TestSeedHistory_ReferenceReconstructionRoundTrip confirms that the
// fix-up task's synthesized reference response, reapplied to the
// feature commit's actual parent-state file content, reproduces the
// fix-up commit's actual post-state content exactly.
func TestSeedHistory_ReferenceReconstructionRoundTrip(t *testing.T) {
	repoPath, featureSHA, fixupSHA, _ := seedTestRepo(t)
	db := openSeedTestDB(t)
	cfg := config.Default()
	cfg.RepoPath = repoPath

	if _, err := SeedHistory(db, cfg, SeedHistoryOptions{RepoPath: repoPath, Since: "2020-01-01", MaxFiles: 3, MaxDiffLines: 120}); err != nil {
		t.Fatalf("SeedHistory: %v", err)
	}

	tasks, err := store.EvalTasksByOrigin(db, OriginHistory)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	var fixupTask *store.EvalTaskRow
	for _, task := range tasks {
		c := ParseCharacteristics(task.Characteristics.String)
		if c.CommitSHA == fixupSHA {
			taskCopy := task
			fixupTask = &taskCopy
		}
	}
	if fixupTask == nil {
		t.Fatal("expected the fix-up commit to be seeded")
	}

	refJSON, err := store.Decompress(fixupTask.ReferenceResponseZstd)
	if err != nil {
		t.Fatalf("decompressing reference response: %v", err)
	}
	var msg seedReferenceMessage
	if err := json.Unmarshal(refJSON, &msg); err != nil {
		t.Fatalf("decoding reference message: %v", err)
	}

	parentContent, err := showFile(repoPath, featureSHA, "internal/greet.go")
	if err != nil {
		t.Fatalf("showFile parent: %v", err)
	}
	wantContent, err := showFile(repoPath, fixupSHA, "internal/greet.go")
	if err != nil {
		t.Fatalf("showFile fixup: %v", err)
	}

	got, err := ApplyReconstructedEdits(parentContent, msg.Content)
	if err != nil {
		t.Fatalf("ApplyReconstructedEdits: %v", err)
	}
	if got != wantContent {
		t.Errorf("round trip mismatch:\ngot:  %q\nwant: %q", got, wantContent)
	}
}

func TestParseUnifiedDiff_MultipleHunks(t *testing.T) {
	diff := `diff --git a/f.txt b/f.txt
index 1111111..2222222 100644
--- a/f.txt
+++ b/f.txt
@@ -1,3 +1,3 @@
 context1
-old1
+new1
 context2
@@ -10,3 +10,3 @@
 context3
-old2
+new2
 context4
`
	parsed := parseUnifiedDiff(diff)
	if len(parsed.Hunks) != 2 {
		t.Fatalf("len(Hunks) = %d, want 2", len(parsed.Hunks))
	}
	if !strings.Contains(parsed.Hunks[0].Old, "old1") || !strings.Contains(parsed.Hunks[0].New, "new1") {
		t.Errorf("hunk 0 = %+v, want to contain old1/new1", parsed.Hunks[0])
	}
	if !strings.Contains(parsed.Hunks[1].Old, "old2") || !strings.Contains(parsed.Hunks[1].New, "new2") {
		t.Errorf("hunk 1 = %+v, want to contain old2/new2", parsed.Hunks[1])
	}
	// Each hunk contributes one removed and one added line: 4 changed
	// lines total (ChangedLines counts both "-" and "+" lines).
	if parsed.ChangedLines != 4 {
		t.Errorf("ChangedLines = %d, want 4", parsed.ChangedLines)
	}
}
