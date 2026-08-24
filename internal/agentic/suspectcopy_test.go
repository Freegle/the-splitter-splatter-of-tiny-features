package agentic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

// buildReferenceMessageJSON marshals a minimal complete Anthropic message
// carrying one Edit tool_use block for path, replacing oldStr with newStr:
// the shape a task's reference_response_zstd decompresses to.
func buildReferenceMessageJSON(t *testing.T, path, oldStr, newStr string) []byte {
	t.Helper()
	input, err := json.Marshal(map[string]string{"file_path": path, "old_string": oldStr, "new_string": newStr})
	if err != nil {
		t.Fatalf("marshaling edit input: %v", err)
	}
	msg := struct {
		Content []anthropic.ContentBlock `json:"content"`
	}{Content: []anthropic.ContentBlock{{Type: anthropic.BlockToolUse, ID: "ref1", Name: "Edit", Input: input}}}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshaling reference message: %v", err)
	}
	return b
}

func TestEvaluateSuspectCopy_VerbatimMatchFlags(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{
		"fix.go": "package main\n\nfunc Broken() int { return 1 }\n",
	})
	sandbox, err := NewSandbox(context.Background(), repoPath, commit)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	defer sandbox.Teardown()

	refJSON := buildReferenceMessageJSON(t, "fix.go", "return 1", "return 2 // the correct fix, verbatim, comments and all")

	parentSnapshot := snapshotReferenceFiles(sandbox.Dir, []string{"fix.go"})

	// The model reproduces the reference fix exactly, comment included.
	modelFinal := "package main\n\nfunc Broken() int { return 2 // the correct fix, verbatim, comments and all }\n"
	if err := os.WriteFile(filepath.Join(sandbox.Dir, "fix.go"), []byte(modelFinal), 0o644); err != nil {
		t.Fatalf("writing model final content: %v", err)
	}

	flag := EvaluateSuspectCopy(sandbox.Dir, refJSON, map[string]bool{}, parentSnapshot, 50)
	if flag == nil {
		t.Fatal("expected a suspect_copy flag for a verbatim match, got nil")
	}
	if flag.Type != CheatFlagSuspectCopy {
		t.Errorf("flag.Type = %q, want %q", flag.Type, CheatFlagSuspectCopy)
	}
}

func TestEvaluateSuspectCopy_DifferentFixDoesNotFlag(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{
		"fix.go": "package main\n\nfunc Broken() int { return 1 }\n",
	})
	sandbox, err := NewSandbox(context.Background(), repoPath, commit)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	defer sandbox.Teardown()

	refJSON := buildReferenceMessageJSON(t, "fix.go", "return 1", "return 2 // the correct fix, verbatim, comments and all")
	parentSnapshot := snapshotReferenceFiles(sandbox.Dir, []string{"fix.go"})

	// The model reaches a working but differently-written fix.
	modelFinal := "package main\n\nfunc Broken() int {\n\tvalue := 2\n\treturn value\n}\n"
	if err := os.WriteFile(filepath.Join(sandbox.Dir, "fix.go"), []byte(modelFinal), 0o644); err != nil {
		t.Fatalf("writing model final content: %v", err)
	}

	flag := EvaluateSuspectCopy(sandbox.Dir, refJSON, map[string]bool{}, parentSnapshot, 50)
	if flag != nil {
		t.Errorf("expected no suspect_copy flag for a differently-written fix, got %+v", flag)
	}
}

func TestEvaluateSuspectCopy_ExcludesHeldOutTestPaths(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{
		"fix.go":      "package main\n\nfunc Broken() int { return 1 }\n",
		"fix_test.go": "package main\n",
	})
	sandbox, err := NewSandbox(context.Background(), repoPath, commit)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	defer sandbox.Teardown()

	// A reference response whose only edit targets the held-out test file:
	// with that path excluded, there is nothing left to compare.
	refJSON := buildReferenceMessageJSON(t, "fix_test.go", "package main", "package main // test only")

	flag := EvaluateSuspectCopy(sandbox.Dir, refJSON, map[string]bool{"fix_test.go": true}, nil, 50)
	if flag != nil {
		t.Errorf("expected no flag when the only reference edit is an excluded held-out test path, got %+v", flag)
	}
}

func TestEvaluateSuspectCopy_MalformedReferenceReturnsNil(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"fix.go": "package main\n"})
	sandbox, err := NewSandbox(context.Background(), repoPath, commit)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	defer sandbox.Teardown()

	flag := EvaluateSuspectCopy(sandbox.Dir, []byte("not json"), map[string]bool{}, nil, 50)
	if flag != nil {
		t.Errorf("expected nil for a malformed reference message, got %+v", flag)
	}
}
