package agentic

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

// newAgenticTestRepo builds a real git repository in t.TempDir(): a go.mod
// and the given files (path -> content), committed. It returns the
// repository path and that commit's sha.
func newAgenticTestRepo(t *testing.T, files map[string]string) (repoPath, commit string) {
	t.Helper()
	repoPath = t.TempDir()

	runGit(t, repoPath, "init", "-q", "-b", "main")
	writeRepoFile(t, repoPath, ".gitattributes", "* text=auto eol=lf\n")
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module example.com/agentictest\n\ngo 1.21\n"
	}
	for path, content := range files {
		writeRepoFile(t, repoPath, path, content)
	}
	runGit(t, repoPath, "add", "-A")
	runGit(t, repoPath, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "initial")

	commit = strings.TrimSpace(runGit(t, repoPath, "rev-parse", "HEAD"))
	return repoPath, commit
}

func writeRepoFile(t *testing.T, repoPath, rel, content string) {
	t.Helper()
	full := filepath.Join(repoPath, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating parent dir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// toolCallSpec describes one tool_use block a fake backend response emits.
type toolCallSpec struct {
	Name  string
	ID    string
	Input map[string]any
}

// buildAssistantResponse marshals a complete Anthropic assistant message
// JSON (the shape RunLoop expects from a ReplayFunc) with the given text
// and tool calls, in that order.
func buildAssistantResponse(t *testing.T, text string, calls ...toolCallSpec) []byte {
	t.Helper()
	var blocks []anthropic.ContentBlock
	if text != "" {
		blocks = append(blocks, anthropic.ContentBlock{Type: anthropic.BlockText, Text: text})
	}
	for i, c := range calls {
		inputJSON, err := json.Marshal(c.Input)
		if err != nil {
			t.Fatalf("marshaling tool call input: %v", err)
		}
		id := c.ID
		if id == "" {
			id = "call_" + string(rune('a'+i))
		}
		blocks = append(blocks, anthropic.ContentBlock{Type: anthropic.BlockToolUse, ID: id, Name: c.Name, Input: inputJSON})
	}
	msg := struct {
		Content []anthropic.ContentBlock `json:"content"`
		Usage   anthropic.Usage          `json:"usage"`
	}{Content: blocks, Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 10}}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshaling assistant response: %v", err)
	}
	return b
}

// scriptedReplay returns a ReplayFunc that returns responses[i] on its i-th
// call. Calling it more times than len(responses) fails the test.
func scriptedReplay(t *testing.T, responses [][]byte) func(ctx context.Context, req anthropic.MessagesRequest) ([]byte, int64, int64, error) {
	t.Helper()
	idx := 0
	return func(ctx context.Context, req anthropic.MessagesRequest) ([]byte, int64, int64, error) {
		if idx >= len(responses) {
			t.Fatalf("fake backend called more times than scripted (%d calls scripted)", len(responses))
		}
		resp := responses[idx]
		idx++
		return resp, 10, 10, nil
	}
}
