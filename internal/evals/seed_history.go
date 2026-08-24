package evals

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/store"
)

// seedContextCapBytes bounds a synthesized seed-history request's total
// marshaled size, per DESIGN.md.
const seedContextCapBytes = 20 * 1024

// seedSystemPrompt is the minimal coding-agent system prompt every
// synthesized seed-history request carries.
const seedSystemPrompt = `You are a coding agent working in this repository. Use the Edit tool to replace an exact old_string with a new_string in an existing file, the MultiEdit tool to apply several such replacements to one file in order, or the Write tool to create a new file with full content. Make the change described in the task using the current file contents shown below.`

// seedToolDefs are the Edit/MultiEdit/Write tool definitions every
// synthesized seed-history request carries.
var seedToolDefs = []anthropic.Tool{
	{
		Name:        "Edit",
		Description: "Replace an exact occurrence of old_string with new_string in the named file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["file_path","old_string","new_string"]}`),
	},
	{
		Name:        "MultiEdit",
		Description: "Apply multiple old_string to new_string replacements to the named file, in order.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["old_string","new_string"]}}},"required":["file_path","edits"]}`),
	},
	{
		Name:        "Write",
		Description: "Create or overwrite the named file with the given content.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`),
	},
}

// seedTextExtensions bounds seed-history's "code" file filter: files with
// one of these lowercase extensions are eligible; anything else (binary
// assets, lockfiles, images, fonts, compiled artifacts) is excluded from a
// commit's touched-file set. A commit left with zero eligible files after
// this filter is skipped entirely (nothing to build a task from).
var seedTextExtensions = map[string]bool{
	".go": true, ".php": true, ".vue": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".ts": true, ".tsx": true, ".sql": true, ".sh": true, ".bash": true, ".py": true, ".rb": true,
	".md": true, ".yml": true, ".yaml": true, ".json": true, ".css": true, ".scss": true, ".html": true,
	".toml": true,
}

// SeedHistoryOptions controls one `eval seed-history` run.
type SeedHistoryOptions struct {
	RepoPath     string
	Since        string // "YYYY-MM-DD", passed to `git log --since`
	Max          int    // stop once this many NEW tasks are inserted
	MaxFiles     int
	MaxDiffLines int
	Grep         string
}

// SeedHistorySummary reports what one seed-history run did.
type SeedHistorySummary struct {
	Considered         int
	Inserted           int
	Deduped            int
	SkippedMergeOrRoot int
	SkippedNoCodeFiles int
	SkippedOversize    int
	SkippedContextCap  int
}

// SeedHistory seeds the eval library from opts.RepoPath's git history, per
// DESIGN.md "eval seed-history": each eligible commit becomes an
// origin='history' task with repo_head set to the commit's PARENT sha (the
// starting state), a mechanical commit-subject brief marked
// brief_source='commit_subject' (see eval reverse-briefs), a synthesized
// Anthropic request handing over the touched files' parent-state content,
// and a synthesized reference response reconstructing the commit's actual
// diff as Edit/MultiEdit/Write tool_use blocks.
func SeedHistory(db *sql.DB, cfg *config.Config, opts SeedHistoryOptions) (*SeedHistorySummary, error) {
	summary := &SeedHistorySummary{}

	existing, err := loadExistingHistoryShas(db)
	if err != nil {
		return nil, fmt.Errorf("loading existing history tasks for dedup: %w", err)
	}

	shas, err := listCandidateCommits(opts)
	if err != nil {
		return nil, fmt.Errorf("listing candidate commits: %w", err)
	}

	for _, sha := range shas {
		if opts.Max > 0 && summary.Inserted >= opts.Max {
			break
		}
		if existing[sha] {
			summary.Deduped++
			continue
		}
		summary.Considered++

		inserted, skipReason, err := seedOneCommit(db, cfg, opts, sha)
		if err != nil {
			return nil, fmt.Errorf("seeding commit %s: %w", sha, err)
		}
		switch {
		case inserted:
			summary.Inserted++
		case skipReason == skipMergeOrRoot:
			summary.SkippedMergeOrRoot++
		case skipReason == skipNoCodeFiles:
			summary.SkippedNoCodeFiles++
		case skipReason == skipOversize:
			summary.SkippedOversize++
		case skipReason == skipContextCap:
			summary.SkippedContextCap++
		}
	}

	return summary, nil
}

type skipReason int

const (
	skipNone skipReason = iota
	skipMergeOrRoot
	skipNoCodeFiles
	skipOversize
	skipContextCap
)

// loadExistingHistoryShas returns the set of commit shas already seeded
// (origin='history'), read from each task's characteristics.commit_sha.
func loadExistingHistoryShas(db *sql.DB) (map[string]bool, error) {
	tasks, err := store.EvalTasksByOrigin(db, OriginHistory)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		c := ParseCharacteristics(t.Characteristics.String)
		if c.CommitSHA != "" {
			out[c.CommitSHA] = true
		}
	}
	return out, nil
}

// listCandidateCommits runs `git log` against opts.RepoPath with
// --no-merges and the caller's filters, returning candidate commit shas
// newest first (git log's default order): seed-history prefers recent
// commits.
func listCandidateCommits(opts SeedHistoryOptions) ([]string, error) {
	args := []string{"log", "--no-merges", "--format=%H"}
	if opts.Since != "" {
		args = append(args, "--since="+opts.Since)
	}
	if opts.Grep != "" {
		args = append(args, "--grep="+opts.Grep)
	}
	out, err := gitOutput(opts.RepoPath, args...)
	if err != nil {
		return nil, err
	}
	var shas []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			shas = append(shas, line)
		}
	}
	return shas, nil
}

// seedOneCommit attempts to build and insert one history-origin eval task
// from sha. inserted is false with a skipReason when the commit is
// ineligible; a false inserted with skipNone means the (call_id, origin)
// (here always NULL call_id vs the pre-check above) dedup path is not
// reachable here since dedup is handled by the caller before this is
// called, so skipNone never actually occurs in practice for a false
// return, but is returned for completeness.
func seedOneCommit(db *sql.DB, cfg *config.Config, opts SeedHistoryOptions, sha string) (inserted bool, reason skipReason, err error) {
	meta, ok, err := loadCommitMeta(opts.RepoPath, sha)
	if err != nil {
		return false, skipNone, err
	}
	if !ok {
		return false, skipMergeOrRoot, nil
	}

	files, err := changedFiles(opts.RepoPath, meta.Parent, sha)
	if err != nil {
		return false, skipNone, err
	}

	touched, err := buildSeedTouchedFiles(opts.RepoPath, meta.Parent, sha, files)
	if err != nil {
		return false, skipNone, err
	}
	if len(touched) == 0 {
		return false, skipNoCodeFiles, nil
	}

	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 3
	}
	maxDiffLines := opts.MaxDiffLines
	if maxDiffLines <= 0 {
		maxDiffLines = 120
	}

	totalDiffLines := 0
	var paths []string
	var blocks []anthropic.ContentBlock
	for i, tf := range touched {
		paths = append(paths, tf.Path)
		id := fmt.Sprintf("toolu_seed_%d", i)
		if tf.IsNew {
			blocks = append(blocks, writeToolUseBlock(id, tf.Path, tf.NewContent))
			totalDiffLines += countLines(tf.NewContent)
			continue
		}
		if len(tf.Hunks) == 0 {
			continue
		}
		blocks = append(blocks, editToolUseBlock(id, tf.Path, tf.Hunks))
		totalDiffLines += tf.ChangedLines
	}
	if len(paths) == 0 {
		return false, skipNoCodeFiles, nil
	}
	if len(paths) > maxFiles || totalDiffLines > maxDiffLines {
		return false, skipOversize, nil
	}

	brief := seedBrief(meta.Subject, meta.Body)

	req, err := buildSeedRequest(brief, touched)
	if err != nil {
		return false, skipNone, err
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return false, skipNone, fmt.Errorf("marshaling synthesized request: %w", err)
	}
	if len(reqJSON) > seedContextCapBytes {
		return false, skipContextCap, nil
	}

	refJSON, err := buildSeedReferenceMessage(blocks)
	if err != nil {
		return false, skipNone, err
	}

	language := Language(paths)
	layer := Layer(paths, cfg.Layers)
	nature, natureEvidence := Nature(meta.Subject, paths, cfg.Layers)

	followups, err := followupCommits(opts.RepoPath, sha, paths)
	if err != nil {
		return false, skipNone, err
	}
	difficulty, difficultyEvidence := GitArchaeologyDifficulty(meta.Subject, meta.AuthorDate, followups)

	specClarity, specEvidence := SpecClarity(brief, paths)
	framework := Framework(paths, language, seedContentSample(touched))

	turnType := feature.TurnSingleFileEdit
	if len(paths) >= 2 {
		turnType = feature.TurnMultiFileEdit
	}
	subsystem := feature.Subsystem(paths)

	characteristics := Characteristics{
		Framework:    framework,
		SpecClarity:  specClarity,
		Size:         Size{Files: len(paths), DiffLines: totalDiffLines, ContextBytes: len(reqJSON)},
		TaskDate:     meta.AuthorDate.Format(time.RFC3339),
		Localization: LocalizationGiven,
		BriefSource:  BriefSourceCommitSubject,
		CommitSHA:    sha,
		Evidence: map[string]string{
			"spec_clarity": specEvidence,
			"difficulty":   difficultyEvidence,
		},
	}
	if natureEvidence != "" {
		characteristics.Evidence["nature"] = natureEvidence
	}

	reqCompressed, err := store.Compress(reqJSON)
	if err != nil {
		return false, skipNone, fmt.Errorf("compressing synthesized request: %w", err)
	}
	refCompressed, err := store.Compress(refJSON)
	if err != nil {
		return false, skipNone, fmt.Errorf("compressing synthesized reference: %w", err)
	}

	row := store.EvalTaskRow{
		CreatedTS:             time.Now().UTC().Format(time.RFC3339),
		RepoHead:              sql.NullString{String: meta.Parent, Valid: true},
		Brief:                 brief,
		TurnType:              sql.NullString{String: turnType, Valid: true},
		Subsystem:             sql.NullString{String: subsystem, Valid: subsystem != ""},
		RequestZstd:           reqCompressed,
		ReferenceResponseZstd: refCompressed,
		Origin:                OriginHistory,
		Language:              sql.NullString{String: language, Valid: language != ""},
		Layer:                 sql.NullString{String: layer, Valid: layer != ""},
		Nature:                sql.NullString{String: nature, Valid: nature != ""},
		Difficulty:            sql.NullString{String: difficulty, Valid: difficulty != ""},
		Characteristics:       sql.NullString{String: characteristics.JSON(), Valid: true},
	}
	_, didInsert, err := store.InsertEvalTask(db, row)
	if err != nil {
		return false, skipNone, fmt.Errorf("inserting history eval task for commit %s: %w", sha, err)
	}
	return didInsert, skipNone, nil
}

// seedBrief builds the mechanical commit-subject brief: the subject line
// plus the first non-empty paragraph of the body, capped to a sane length.
const seedBriefCap = 1000

func seedBrief(subject, body string) string {
	brief := subject
	if para := firstParagraph(body); para != "" {
		brief = brief + "\n\n" + para
	}
	if r := []rune(brief); len(r) > seedBriefCap {
		brief = string(r[:seedBriefCap])
	}
	return brief
}

func firstParagraph(body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// seedTouchedFile is one file this commit changed, with enough content to
// both build the synthesized request's context and the reference
// response's tool_use blocks.
type seedTouchedFile struct {
	Path          string
	IsNew         bool
	ParentContent string
	NewContent    string
	Hunks         []diffHunk
	ChangedLines  int
}

// buildSeedTouchedFiles resolves each changed file to a seedTouchedFile,
// skipping binary and non-code files, and modified files whose diff
// yields no parseable hunks (e.g. a pure mode change).
func buildSeedTouchedFiles(repoPath, parent, sha string, files []changedFile) ([]seedTouchedFile, error) {
	var out []seedTouchedFile
	for _, f := range files {
		if !seedTextExtensions[strings.ToLower(filepath.Ext(f.Path))] {
			continue
		}
		binary, err := isBinaryFile(repoPath, parent, sha, f.Path)
		if err != nil {
			return nil, err
		}
		if binary {
			continue
		}

		if f.Status == "A" {
			content, err := showFile(repoPath, sha, f.Path)
			if err != nil {
				return nil, err
			}
			out = append(out, seedTouchedFile{Path: f.Path, IsNew: true, NewContent: content})
			continue
		}

		parentContent, err := showFile(repoPath, parent, f.Path)
		if err != nil {
			return nil, err
		}
		diffText, err := gitOutput(repoPath, "diff", "-U1", "--no-renames", parent, sha, "--", f.Path)
		if err != nil {
			return nil, err
		}
		parsed := parseUnifiedDiff(diffText)
		if len(parsed.Hunks) == 0 {
			continue
		}
		out = append(out, seedTouchedFile{
			Path: f.Path, ParentContent: parentContent, Hunks: parsed.Hunks, ChangedLines: parsed.ChangedLines,
		})
	}
	return out, nil
}

// buildSeedRequest assembles the synthesized Anthropic request: the
// minimal coding-agent system prompt, the brief plus each touched file's
// parent-state content (or a "new file" marker), and the Edit/MultiEdit/
// Write tool definitions.
func buildSeedRequest(brief string, touched []seedTouchedFile) (anthropic.MessagesRequest, error) {
	var sb strings.Builder
	sb.WriteString(brief)
	sb.WriteString("\n\nCurrent contents of the files you may need to change:\n")
	for _, f := range touched {
		sb.WriteString("\n--- ")
		sb.WriteString(f.Path)
		sb.WriteString(" ---\n")
		if f.IsNew {
			sb.WriteString("(new file, does not exist yet)\n")
		} else {
			sb.WriteString(f.ParentContent)
			if !strings.HasSuffix(f.ParentContent, "\n") {
				sb.WriteString("\n")
			}
		}
	}

	systemJSON, err := json.Marshal(seedSystemPrompt)
	if err != nil {
		return anthropic.MessagesRequest{}, fmt.Errorf("marshaling seed system prompt: %w", err)
	}

	return anthropic.MessagesRequest{
		Model:  "synthesized",
		System: json.RawMessage(systemJSON),
		Messages: []anthropic.Message{
			{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: sb.String()}}},
		},
		Tools:     seedToolDefs,
		MaxTokens: 4096,
	}, nil
}

// seedContentSample concatenates a little of each touched file's content
// (parent-state, or the new content for an added file), the "when cheap"
// content Framework's gorm/fiber import sniff uses: this content is
// already in hand from buildSeedTouchedFiles, no extra I/O.
func seedContentSample(touched []seedTouchedFile) string {
	var sb strings.Builder
	for _, f := range touched {
		if f.IsNew {
			sb.WriteString(f.NewContent)
		} else {
			sb.WriteString(f.ParentContent)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// commitMeta is one commit's metadata, enough to seed a task from it.
type commitMeta struct {
	SHA        string
	Parent     string
	AuthorDate time.Time
	Subject    string
	Body       string
}

// loadCommitMeta reads sha's parent, author date, subject and body. ok is
// false (with a nil error) when sha is a merge or root commit (not exactly
// one parent): not an error, just ineligible.
func loadCommitMeta(repoPath, sha string) (*commitMeta, bool, error) {
	parentsOut, err := gitOutput(repoPath, "rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return nil, false, err
	}
	fields := strings.Fields(strings.TrimSpace(parentsOut))
	if len(fields) != 2 {
		return nil, false, nil
	}
	parent := fields[1]

	metaOut, err := gitOutput(repoPath, "log", "-1", "--format=%aI\x1f%s\x1f%b", sha)
	if err != nil {
		return nil, false, err
	}
	parts := strings.SplitN(metaOut, "\x1f", 3)
	if len(parts) < 2 {
		return nil, false, fmt.Errorf("unexpected git log metadata output for %s", sha)
	}
	authorDate, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, false, fmt.Errorf("parsing author date for %s: %w", sha, err)
	}
	body := ""
	if len(parts) > 2 {
		body = strings.TrimRight(parts[2], "\n")
	}

	return &commitMeta{SHA: sha, Parent: parent, AuthorDate: authorDate, Subject: parts[1], Body: body}, true, nil
}

// changedFile is one file a commit touched.
type changedFile struct {
	Status string // "A" or "M" (deletions and, with --no-renames, renames-as-D+A are excluded upstream)
	Path   string
}

// changedFiles lists parent..sha's added and modified files (deleted files
// carry nothing forward to build a task from and are excluded; renames are
// disabled so they surface as a plain delete plus add instead of a rename
// status this package would otherwise need to special-case).
func changedFiles(repoPath, parent, sha string) ([]changedFile, error) {
	out, err := gitOutput(repoPath, "diff", "--no-renames", "--name-status", parent, sha)
	if err != nil {
		return nil, err
	}
	var files []changedFile
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[0] != "A" && parts[0] != "M" {
			continue
		}
		files = append(files, changedFile{Status: parts[0], Path: parts[1]})
	}
	return files, nil
}

// isBinaryFile reports whether path's parent..sha diff is binary, via
// `git diff --numstat` (a binary file's added/removed counts are both "-").
func isBinaryFile(repoPath, parent, sha, path string) (bool, error) {
	out, err := gitOutput(repoPath, "diff", "--numstat", parent, sha, "--", path)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(out)
	return len(fields) >= 2 && fields[0] == "-" && fields[1] == "-", nil
}

// showFile returns path's exact content at commit sha, preserving any
// trailing newline the real file has: unlike gitOutput, this never trims
// trailing whitespace, since file content fidelity is what makes the
// reference response's Write blocks and the round-trip check exact.
func showFile(repoPath, sha, path string) (string, error) {
	cmd := exec.Command("git", "-C", repoPath, "show", sha+":"+path)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git show %s:%s: %w: %s", sha, path, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git show %s:%s: %w", sha, path, err)
	}
	return string(out), nil
}

// followupCommits returns every commit reachable from HEAD but not from
// sha (i.e. after it in history) that touches any of paths: the candidate
// pool GitArchaeologyDifficulty inspects for a fix-pattern follow-up
// within its own 14 day window (this function does not pre-filter by
// date, since GitArchaeologyDifficulty applies that window itself).
func followupCommits(repoPath, sha string, paths []string) ([]FollowupCommit, error) {
	args := []string{"log", sha + "..HEAD", "--format=%H\x1f%aI\x1f%s", "--"}
	args = append(args, paths...)
	out, err := gitOutput(repoPath, args...)
	if err != nil {
		// A repository whose HEAD does not descend from sha (uncommon,
		// e.g. sha is on an abandoned branch) is not a hard error: no
		// follow-up evidence is available, so treat it as none found.
		return nil, nil
	}

	var followups []FollowupCommit
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 3)
		if len(parts) != 3 {
			continue
		}
		date, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		followups = append(followups, FollowupCommit{SHA: parts[0], Subject: parts[2], Date: date})
	}
	sort.Slice(followups, func(i, j int) bool { return followups[i].Date.Before(followups[j].Date) })
	return followups, nil
}

// gitOutput runs `git -C repoPath <args...>` and returns its trimmed
// stdout. Plumbing formats only, per DESIGN.md's "no external git
// libraries" instruction for seed-history.
func gitOutput(repoPath string, args ...string) (string, error) {
	full := append([]string{"-C", repoPath}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}
