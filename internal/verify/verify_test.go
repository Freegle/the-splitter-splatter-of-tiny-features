package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/config"
)

func TestVerify_ExactMatch(t *testing.T) {
	frontier := textMessage("  Hello   world  ")
	local := textMessage("Hello world")

	v := New(Config{})
	res, err := v.Verify(context.Background(), "", "", frontier, local, "question_answer")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Stage != StageExact {
		t.Errorf("Stage = %q, want %q", res.Stage, StageExact)
	}
	if res.Similarity != 1 {
		t.Errorf("Similarity = %v, want 1", res.Similarity)
	}
	if res.Agree == nil || !*res.Agree {
		t.Errorf("Agree = %v, want true", res.Agree)
	}
}

func TestVerify_MiddleBand_NonEditTurn(t *testing.T) {
	frontier := textMessage("the quick brown fox jumps over the lazy dog today")
	local := textMessage("the quick brown fox jumps over the lazy cat today")

	cfg := Config{Thresholds: config.ThresholdsConfig{DefaultHigh: 0.95, DefaultLow: 0.5}}
	v := New(cfg)
	res, err := v.Verify(context.Background(), "", "", frontier, local, "question_answer")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Stage != StageAST {
		t.Errorf("Stage = %q, want %q", res.Stage, StageAST)
	}
	if res.Agree != nil {
		t.Errorf("Agree = %v, want nil (middle band)", *res.Agree)
	}
	// One substitution among 10 tokens: similarity = 1 - 1/10 = 0.9.
	if res.Similarity < 0.89 || res.Similarity > 0.91 {
		t.Errorf("Similarity = %v, want ~0.9", res.Similarity)
	}
}

func TestVerify_NonEditTurn_AgreesAboveHighThreshold(t *testing.T) {
	frontier := textMessage("the answer is forty two")
	local := textMessage("the answer is forty three")

	cfg := Config{Thresholds: config.ThresholdsConfig{DefaultHigh: 0.7, DefaultLow: 0.3}}
	v := New(cfg)
	res, err := v.Verify(context.Background(), "", "", frontier, local, "question_answer")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Agree == nil || !*res.Agree {
		t.Errorf("Agree = %v, want true (similarity %v >= high 0.7)", res.Agree, res.Similarity)
	}
}

func TestVerify_NonEditTurn_DisagreesBelowLowThreshold(t *testing.T) {
	frontier := textMessage("completely unrelated response text here")
	local := textMessage("nothing whatsoever in common at all")

	cfg := Config{Thresholds: config.ThresholdsConfig{DefaultHigh: 0.9, DefaultLow: 0.5}}
	v := New(cfg)
	res, err := v.Verify(context.Background(), "", "", frontier, local, "question_answer")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Agree == nil || *res.Agree {
		t.Errorf("Agree = %v, want false (similarity %v <= low 0.5)", res.Agree, res.Similarity)
	}
}

func TestVerify_EditTurn_IdenticalEdits_Agree(t *testing.T) {
	repoPath, commit := newTestRepo(t, "main.go", "package main\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	filePath := filepath.Join(repoPath, "main.go")

	frontier := mustMarshalMessage([]any{
		map[string]any{"type": "text", "text": "Fixing the subtraction bug."},
		toolUseBlock("Edit", map[string]any{"file_path": filePath, "old_string": "return a - b", "new_string": "return a + b"}),
	})
	local := mustMarshalMessage([]any{
		map[string]any{"type": "text", "text": "I will correct the operator used here."},
		toolUseBlock("Edit", map[string]any{"file_path": filePath, "old_string": "return a - b", "new_string": "return a + b"}),
	})

	v := New(Config{
		Thresholds:             config.ThresholdsConfig{DefaultHigh: 0.9, DefaultLow: 0.5},
		MaxConcurrentWorktrees: 2,
	})
	res, err := v.Verify(context.Background(), repoPath, commit, frontier, local, "single_file_edit")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Stage != StageAST {
		t.Errorf("Stage = %q, want %q", res.Stage, StageAST)
	}
	if res.Similarity < 0.99 {
		t.Errorf("Similarity = %v, want ~1 (identical resulting files)", res.Similarity)
	}
	if res.Agree == nil || !*res.Agree {
		t.Errorf("Agree = %v, want true", res.Agree)
	}
	if res.FrontierLint == "" || res.LocalLint == "" {
		t.Errorf("expected lint results per side (gofmt+govet fallback), got frontier=%q local=%q", res.FrontierLint, res.LocalLint)
	}
	if !strings.Contains(res.FrontierLint, `"ok":true`) {
		t.Errorf("expected frontier lint to report ok:true for gofmt-clean output, got %s", res.FrontierLint)
	}
}

func TestVerify_EditTurn_DivergentEdits_Disagree(t *testing.T) {
	repoPath, commit := newTestRepo(t, "main.go", "package main\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	filePath := filepath.Join(repoPath, "main.go")

	frontier := mustMarshalMessage([]any{
		toolUseBlock("Edit", map[string]any{"file_path": filePath, "old_string": "return a - b", "new_string": "return a + b"}),
	})
	local := mustMarshalMessage([]any{
		toolUseBlock("Write", map[string]any{
			"file_path": filePath,
			"content":   "package other\n\nvar Unrelated = \"nothing to do with the original file at all\"\n\nfunc DoesNotCompile() {\n\tpanic(\"completely different\")\n}\n",
		}),
	})

	v := New(Config{
		Thresholds:             config.ThresholdsConfig{DefaultHigh: 0.9, DefaultLow: 0.5},
		MaxConcurrentWorktrees: 2,
	})
	res, err := v.Verify(context.Background(), repoPath, commit, frontier, local, "single_file_edit")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Agree == nil || *res.Agree {
		t.Errorf("Agree = %v (similarity %v), want false", res.Agree, res.Similarity)
	}
	if res.Similarity > 0.5 {
		t.Errorf("Similarity = %v, want <= 0.5 for a total rewrite vs a one-line edit", res.Similarity)
	}
}

func TestVerify_EditTurn_ThresholdOverridePerLanguage(t *testing.T) {
	repoPath, commit := newTestRepo(t, "main.go", "package main\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	filePath := filepath.Join(repoPath, "main.go")

	edit := func(text string) []byte {
		return mustMarshalMessage([]any{
			map[string]any{"type": "text", "text": text},
			toolUseBlock("Edit", map[string]any{"file_path": filePath, "old_string": "return a - b", "new_string": "return a + b"}),
		})
	}
	frontier := edit("apply fix a")
	local := edit("apply fix b")

	// Without the override, DefaultHigh is unreachable (> 1) and DefaultLow
	// is just under a perfect score, so an identical-edit similarity of 1
	// lands in the middle band. The go/single_file_edit override lowers
	// the bar enough to agree outright, proving the override is honoured.
	cfg := config.ThresholdsConfig{
		DefaultHigh: 1.5,
		DefaultLow:  0.99,
		Overrides: map[string]config.ThresholdPair{
			"go/single_file_edit": {High: 0.9, Low: 0.5},
		},
	}

	v := New(Config{Thresholds: cfg, MaxConcurrentWorktrees: 2})
	res, err := v.Verify(context.Background(), repoPath, commit, frontier, local, "single_file_edit")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Similarity < 0.999 {
		t.Fatalf("Similarity = %v, want ~1 for identical edits", res.Similarity)
	}
	if res.Agree == nil || !*res.Agree {
		t.Errorf("Agree = %v, want true: the go/single_file_edit override (high=0.9) should have decided this, not the unreachable default (high=1.5)", res.Agree)
	}
}

func TestThresholdsFor(t *testing.T) {
	cfg := config.ThresholdsConfig{
		DefaultHigh: 0.9,
		DefaultLow:  0.5,
		Overrides: map[string]config.ThresholdPair{
			"go/single_file_edit": {High: 0.95, Low: 0.6},
		},
	}

	if h, l := thresholdsFor(cfg, "", "single_file_edit"); h != 0.9 || l != 0.5 {
		t.Errorf("no language: got (%v,%v), want (0.9,0.5)", h, l)
	}
	if h, l := thresholdsFor(cfg, "go", "single_file_edit"); h != 0.95 || l != 0.6 {
		t.Errorf("go/single_file_edit override: got (%v,%v), want (0.95,0.6)", h, l)
	}
	if h, l := thresholdsFor(cfg, "go", "multi_file_edit"); h != 0.9 || l != 0.5 {
		t.Errorf("go/multi_file_edit (no override): got (%v,%v), want defaults (0.9,0.5)", h, l)
	}

	if h, l := thresholdsFor(config.ThresholdsConfig{}, "", "x"); h != 0.9 || l != 0.5 {
		t.Errorf("zero-value config: got (%v,%v), want brief defaults (0.9,0.5)", h, l)
	}
}

func TestVerify_ApplyFailure_RecordedAndCounted(t *testing.T) {
	repoPath, commit := newTestRepo(t, "main.go", "package main\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	filePath := filepath.Join(repoPath, "main.go")

	// old_string is not present in the file: an apply failure.
	frontier := mustMarshalMessage([]any{
		toolUseBlock("Edit", map[string]any{"file_path": filePath, "old_string": "this text is not in the file", "new_string": "x"}),
	})
	local := mustMarshalMessage([]any{
		toolUseBlock("Edit", map[string]any{"file_path": filePath, "old_string": "return a - b", "new_string": "return a + b"}),
	})

	v := New(Config{Thresholds: config.ThresholdsConfig{DefaultHigh: 0.9, DefaultLow: 0.5}, MaxConcurrentWorktrees: 2})
	res, err := v.Verify(context.Background(), repoPath, commit, frontier, local, "single_file_edit")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !strings.Contains(res.FrontierLint, `"tool":"apply"`) {
		t.Errorf("expected an apply-failure entry in FrontierLint, got %s", res.FrontierLint)
	}
	if !strings.Contains(res.FrontierLint, `"ok":false`) {
		t.Errorf("expected the apply-failure entry to be ok:false, got %s", res.FrontierLint)
	}
}

func TestVerify_EditTurn_NoRepoHead_FallsBackToTokenSimilarity(t *testing.T) {
	frontier := editMessage("/some/repo/main.go", "old", "new")
	local := editMessage("/some/repo/main.go", "old", "different")

	v := New(Config{Thresholds: config.ThresholdsConfig{DefaultHigh: 0.9, DefaultLow: 0.1}})
	res, err := v.Verify(context.Background(), "/some/repo", "", frontier, local, "single_file_edit")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Stage != StageAST {
		t.Errorf("Stage = %q, want %q", res.Stage, StageAST)
	}
	if res.FrontierLint != "" || res.LocalLint != "" {
		t.Errorf("expected no lint output on the no-repo-HEAD fallback path, got frontier=%q local=%q", res.FrontierLint, res.LocalLint)
	}
}

func TestVerifyEditTurn_TeardownLeavesNoWorktrees(t *testing.T) {
	repoPath, commit := newTestRepo(t, "main.go", "package main\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	filePath := filepath.Join(repoPath, "main.go")

	frontier := editMessage(filePath, "return a - b", "return a + b")
	local := editMessage(filePath, "return a - b", "return a * b")

	v := New(Config{Thresholds: config.ThresholdsConfig{DefaultHigh: 0.9, DefaultLow: 0.5}, MaxConcurrentWorktrees: 2})
	if _, err := v.Verify(context.Background(), repoPath, commit, frontier, local, "single_file_edit"); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	leftover := findVerifyTempDirs(t)
	if len(leftover) != 0 {
		t.Errorf("leftover verify worktree dirs after run: %v", leftover)
	}
}

func findVerifyTempDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("reading temp dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), verifyTempPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}
