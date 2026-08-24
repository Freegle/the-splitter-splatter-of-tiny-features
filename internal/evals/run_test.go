package evals

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/backend"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

func openRunTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "splitter.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

// runTestRepo builds a real throwaway git repo with one commit containing
// every file named in files (path -> initial content), returning the repo
// path and that commit's sha, so eval run's edit-turn tasks have a real
// worktree checkout target. Reuses seed_history_test.go's runGit/
// writeSeedFile helpers (same package).
func runTestRepo(t *testing.T, files map[string]string) (repoPath, commit string) {
	t.Helper()
	repoPath = t.TempDir()
	runGit(t, repoPath, "init", "-q", "-b", "main")
	writeSeedFile(t, repoPath, ".gitattributes", "* text=auto eol=lf\n")
	for path, content := range files {
		writeSeedFile(t, repoPath, path, content)
	}
	commit = commitGit(t, repoPath, "initial", "2024-01-01T00:00:00Z")
	return repoPath, commit
}

// insertEditTask inserts one active eval task whose request asks (in
// plain text naming filePath) for oldStr to become newStr in filePath, and
// whose reference response performs exactly that edit.
func insertEditTask(t *testing.T, db *sql.DB, repoHead, filePath, oldStr, newStr, difficulty, language string, extraContextBytes int) int64 {
	t.Helper()

	userText := "Edit " + filePath + ": replace '" + oldStr + "' with '" + newStr + "'."
	if extraContextBytes > 0 {
		userText += "\n" + strings.Repeat("x", extraContextBytes)
	}
	req := anthropic.MessagesRequest{
		Messages: []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: userText}}}},
		Tools:    seedToolDefs,
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	reqCompressed, err := store.Compress(reqJSON)
	if err != nil {
		t.Fatalf("compress request: %v", err)
	}

	refJSON, err := buildSeedReferenceMessage([]anthropic.ContentBlock{editToolUseBlock("toolu_ref", filePath, []diffHunk{{Old: oldStr, New: newStr}})})
	if err != nil {
		t.Fatalf("buildSeedReferenceMessage: %v", err)
	}
	refCompressed, err := store.Compress(refJSON)
	if err != nil {
		t.Fatalf("compress reference: %v", err)
	}

	c := Characteristics{Size: Size{Files: 1, ContextBytes: len(reqJSON)}, BriefSource: BriefSourceManual}
	id, _, err := store.InsertEvalTask(db, store.EvalTaskRow{
		CreatedTS:             "2026-08-24T00:00:00Z",
		RepoHead:              sql.NullString{String: repoHead, Valid: true},
		Brief:                 userText,
		TurnType:              sql.NullString{String: "single_file_edit", Valid: true},
		Language:              sql.NullString{String: language, Valid: language != ""},
		Difficulty:            sql.NullString{String: difficulty, Valid: difficulty != ""},
		RequestZstd:           reqCompressed,
		ReferenceResponseZstd: refCompressed,
		Origin:                OriginManual,
		Characteristics:       sql.NullString{String: c.JSON(), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}
	return id
}

// chatRequestUserText concatenates every "user" role message's content
// from a decoded backend.ChatRequest, the text a fake backend inspects to
// decide which task it is answering.
func chatRequestUserText(req backend.ChatRequest) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		if m.Role == "user" {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func writeChatResponse(t *testing.T, w http.ResponseWriter, filePath, oldStr, newStr string) {
	t.Helper()
	args, err := json.Marshal(map[string]string{"file_path": filePath, "old_string": oldStr, "new_string": newStr})
	if err != nil {
		t.Fatalf("marshaling tool call arguments: %v", err)
	}
	resp := backend.ChatResponse{
		ID: "chatcmpl-1",
		Choices: []backend.ChatChoice{{
			Message: backend.ChatMessage{
				Role: "assistant",
				ToolCalls: []backend.ChatToolCall{{
					ID: "call_1", Type: "function",
					Function: backend.ChatToolCallFunc{Name: "Edit", Arguments: string(args)},
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: backend.ChatUsage{PromptTokens: 100, CompletionTokens: 20},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Fatalf("encoding fake chat response: %v", err)
	}
}

// TestRun_EndToEndPassFailScorecardAndRegression runs two eval runs of
// different models against the same two tasks: the first model answers
// both correctly, the second answers one correctly and one wrong. It
// asserts per-task pass/fail, the scorecard's per-dimension grouping, and
// that the regression listing (second run vs the first) names exactly the
// task that regressed.
func TestRun_EndToEndPassFailScorecardAndRegression(t *testing.T) {
	repoPath, commit := runTestRepo(t, map[string]string{
		"x.go": "package x\n\nfunc F() string { return \"old1\" }\n",
		"y.go": "package y\n\nfunc G() string { return \"old2\" }\n",
	})
	db := openRunTestDB(t)

	taskX := insertEditTask(t, db, commit, "x.go", "old1", "new1", DifficultySimple, "go", 0)
	taskY := insertEditTask(t, db, commit, "y.go", "old2", "new2", DifficultySimple, "go", 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backend.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding chat request: %v", err)
		}
		text := chatRequestUserText(req)
		switch {
		case strings.Contains(text, "x.go"):
			if req.Model == "model-bad-on-y" || req.Model == "model-good" {
				writeChatResponse(t, w, "x.go", "old1", "new1")
			}
		case strings.Contains(text, "y.go"):
			if req.Model == "model-good" {
				writeChatResponse(t, w, "y.go", "old2", "new2")
			} else {
				// model-bad-on-y gets this one wrong.
				writeChatResponse(t, w, "y.go", "old2", "completely different and wrong text")
			}
		default:
			t.Fatalf("unrecognised task text: %q", text)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.RepoPath = repoPath
	cfg.Backends = map[string]config.BackendConfig{"fake": {BaseURL: server.URL}}
	cfg.Evals = config.EvalsConfig{LadderTrack: "none", StopWilsonUpper: 0.01, StopMinN: 1000, FutilityConsecutiveFails: 1000}

	firstSummary, err := Run(context.Background(), db, cfg, RunOptions{Backend: "fake", Model: "model-good"})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if firstSummary.TasksPassed != 2 {
		t.Fatalf("first run TasksPassed = %d, want 2", firstSummary.TasksPassed)
	}

	secondSummary, err := Run(context.Background(), db, cfg, RunOptions{Backend: "fake", Model: "model-bad-on-y"})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if secondSummary.TasksPassed != 1 {
		t.Fatalf("second run TasksPassed = %d, want 1", secondSummary.TasksPassed)
	}

	langEntry := secondSummary.Scorecard.ByDimension["language"]["go"]
	if langEntry.N != 2 || langEntry.Passed != 1 {
		t.Errorf("scorecard language[go] = %+v, want N=2 Passed=1", langEntry)
	}
	turnTypeEntry := secondSummary.Scorecard.ByDimension["turn_type"]["single_file_edit"]
	if turnTypeEntry.N != 2 {
		t.Errorf("scorecard turn_type[single_file_edit].N = %d, want 2", turnTypeEntry.N)
	}

	if secondSummary.PriorModel != "model-good" {
		t.Fatalf("PriorModel = %q, want model-good", secondSummary.PriorModel)
	}
	if len(secondSummary.Regressions) != 1 {
		t.Fatalf("Regressions = %+v, want exactly 1", secondSummary.Regressions)
	}
	if secondSummary.Regressions[0].TaskID != taskY {
		t.Errorf("regressed task id = %d, want %d (taskY)", secondSummary.Regressions[0].TaskID, taskY)
	}
	_ = taskX
}

// TestRun_LadderStopsFailingTrackAndSkipsHigherRungs builds a task set
// spanning rungs 1, 2, 3 and 4 on a "go" track plus one rung-1 task on a
// "vue" track. The fake backend answers every go-track rung 3 task wrong
// (3 consecutive fails, at the configured futility threshold) and every
// other task correctly. It asserts: the go track's rung 3 is abandoned,
// the go-track rung 4 task is recorded ladder_skipped (never even sent to
// the backend), the vue track's rung 1 task still runs and passes, and
// token totals only reflect the tasks actually attempted.
func TestRun_LadderStopsFailingTrackAndSkipsHigherRungs(t *testing.T) {
	files := map[string]string{
		"go1.go":  "package p\n\nfunc F1() string { return \"g1old\" }\n",
		"go2.go":  "package p\n\nfunc F2() string { return \"g2old\" }\n",
		"go3a.go": "package p\n\nfunc F3a() string { return \"g3aold\" }\n",
		"go3b.go": "package p\n\nfunc F3b() string { return \"g3bold\" }\n",
		"go3c.go": "package p\n\nfunc F3c() string { return \"g3cold\" }\n",
		"go4.go":  "package p\n\nfunc F4() string { return \"g4old\" }\n",
		"vue1.go": "package p\n\nfunc V1() string { return \"v1old\" }\n",
	}
	repoPath, commit := runTestRepo(t, files)
	db := openRunTestDB(t)

	// rung 1: single_file_edit, simple, small context.
	insertEditTask(t, db, commit, "go1.go", "g1old", "g1new", DifficultySimple, "go", 0)
	// rung 2: single_file_edit, simple, LARGE context (>=8KB).
	insertEditTask(t, db, commit, "go2.go", "g2old", "g2new", DifficultySimple, "go", 8500)
	// rung 3 x3: single_file_edit, unknown difficulty, small context. All fail.
	insertEditTask(t, db, commit, "go3a.go", "g3aold", "g3anew", "", "go", 0)
	insertEditTask(t, db, commit, "go3b.go", "g3bold", "g3bnew", "", "go", 0)
	insertEditTask(t, db, commit, "go3c.go", "g3cold", "g3cnew", "", "go", 0)
	// rung 4: single_file_edit, unknown difficulty, large context. Must be
	// skipped once rung 3 is abandoned.
	go4ID := insertEditTask(t, db, commit, "go4.go", "g4old", "g4new", "", "go", 8500)
	// vue track rung 1: independent of the go track's abandonment.
	vue1ID := insertEditTask(t, db, commit, "vue1.go", "v1old", "v1new", DifficultySimple, "vue", 0)

	attempted := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backend.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding chat request: %v", err)
		}
		text := chatRequestUserText(req)
		for file := range files {
			if strings.Contains(text, file) {
				attempted[file] = true
			}
		}
		switch {
		case strings.Contains(text, "go3a.go"):
			writeChatResponse(t, w, "go3a.go", "g3aold", "totally wrong")
		case strings.Contains(text, "go3b.go"):
			writeChatResponse(t, w, "go3b.go", "g3bold", "totally wrong")
		case strings.Contains(text, "go3c.go"):
			writeChatResponse(t, w, "go3c.go", "g3cold", "totally wrong")
		case strings.Contains(text, "go1.go"):
			writeChatResponse(t, w, "go1.go", "g1old", "g1new")
		case strings.Contains(text, "go2.go"):
			writeChatResponse(t, w, "go2.go", "g2old", "g2new")
		case strings.Contains(text, "go4.go"):
			t.Error("go4.go should never be sent to the backend: its rung was abandoned")
			writeChatResponse(t, w, "go4.go", "g4old", "g4new")
		case strings.Contains(text, "vue1.go"):
			writeChatResponse(t, w, "vue1.go", "v1old", "v1new")
		default:
			t.Fatalf("unrecognised task text: %q", text)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.RepoPath = repoPath
	cfg.Backends = map[string]config.BackendConfig{"fake": {BaseURL: server.URL}}
	cfg.Evals = config.EvalsConfig{
		LadderTrack: "language", StopWilsonUpper: 0.9, StopMinN: 1000, FutilityConsecutiveFails: 3,
	}

	summary, err := Run(context.Background(), db, cfg, RunOptions{Backend: "fake", Model: "ladder-model"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if attempted["go4.go"] {
		t.Error("go4.go must never be attempted once its rung was abandoned")
	}
	if summary.TasksSkipped != 1 {
		t.Errorf("TasksSkipped = %d, want 1 (go4)", summary.TasksSkipped)
	}
	if summary.TasksScored != 6 {
		t.Errorf("TasksScored = %d, want 6", summary.TasksScored)
	}
	// go1, go2 and vue1 pass; go3a/b/c fail: 3 passed.
	if summary.TasksPassed != 3 {
		t.Errorf("TasksPassed = %d, want 3", summary.TasksPassed)
	}

	goTrack, ok := summary.Ladder["go"]
	if !ok {
		t.Fatal("expected a ladder summary for track \"go\"")
	}
	if goTrack.StopRung != 3 || goTrack.Reason != "futility" {
		t.Errorf("go track summary = %+v, want StopRung=3 Reason=futility", goTrack)
	}
	vueTrack, ok := summary.Ladder["vue"]
	if !ok || vueTrack.StopRung != 0 {
		t.Errorf("vue track summary = %+v, want StopRung=0 (never abandoned)", vueTrack)
	}

	// Token totals only reflect the 6 tasks actually sent to the backend
	// (go4 never called doReplay at all).
	if summary.TokensIn != 600 {
		t.Errorf("TokensIn = %d, want 600 (6 attempted tasks x 100)", summary.TokensIn)
	}
	if summary.TokensOut != 120 {
		t.Errorf("TokensOut = %d, want 120 (6 attempted tasks x 20)", summary.TokensOut)
	}

	// The skipped task's own eval_results row must be ladder_skipped with
	// passed NULL, and the vue task must be recorded as passed.
	results, err := store.EvalResultsForRun(db, summary.RunID)
	if err != nil {
		t.Fatalf("EvalResultsForRun: %v", err)
	}
	byTask := map[int64]store.EvalResultRow{}
	for _, r := range results {
		byTask[r.EvalTaskID] = r
	}
	go4Result, ok := byTask[go4ID]
	if !ok {
		t.Fatal("expected an eval_results row for the skipped go4 task")
	}
	if go4Result.Passed.Valid {
		t.Errorf("go4 Passed = %+v, want NULL", go4Result.Passed)
	}
	if go4Result.Error.String != "ladder_skipped" {
		t.Errorf("go4 Error = %q, want ladder_skipped", go4Result.Error.String)
	}
	vue1Result, ok := byTask[vue1ID]
	if !ok || !vue1Result.Passed.Valid || vue1Result.Passed.Int64 != 1 {
		t.Errorf("vue1 result = %+v, want Passed=1", vue1Result)
	}
}
