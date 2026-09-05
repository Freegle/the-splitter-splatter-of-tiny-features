package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempFile creates a file with the given content and returns its absolute path.
func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestDifftSimilarity_BinaryMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	got, ok := difftSimilarity(context.Background(), "/fake/a.txt", "/fake/b.txt")
	if got != 0 || ok {
		t.Errorf("difftSimilarity = %v, %v; want 0, false when binary is missing", got, ok)
	}
}

func TestDifftSimilarity_NonZeroExit(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
exit 1
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := writeTempFile(t, dir, "b.txt", "content b")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	if got != 0 || ok {
		t.Errorf("difftSimilarity = %v, %v; want 0, false on non-zero exit", got, ok)
	}
}

func TestDifftSimilarity_UnparseableOutput(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
echo 'not json at all'
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := writeTempFile(t, dir, "b.txt", "content b")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	if got != 0 || ok {
		t.Errorf("difftSimilarity = %v, %v; want 0, false on unparseable output", got, ok)
	}
}

func TestDifftSimilarity_OutputOnStderrOnly(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
echo '{"status":"unchanged"}' >&2
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := writeTempFile(t, dir, "b.txt", "content b")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	if got != 0 || ok {
		t.Errorf("difftSimilarity = %v, %v; want 0, false when JSON is only on stderr", got, ok)
	}
}

func TestDifftSimilarity_StatusUnchanged(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
echo '{"status":"unchanged","aligned_lines":[],"chunks":[]}'
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := writeTempFile(t, dir, "b.txt", "content b")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	if got != 1 || !ok {
		t.Errorf("difftSimilarity = %v, %v; want 1, true for unchanged status", got, ok)
	}
}

func TestDifftSimilarity_MainArithmetic(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
echo '{"status":"changed","aligned_lines":[[1,1],[2,2],[3,3],[4,4]],"chunks":[["a","b"],["c"]]}'
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := writeTempFile(t, dir, "b.txt", "content b")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	want := 0.25
	if !ok {
		t.Errorf("difftSimilarity ok = false; want true")
	}
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("difftSimilarity = %v, want %v", got, want)
	}
}

func TestDifftSimilarity_Clamping(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
echo '{"status":"changed","aligned_lines":[[1,1],[2,2]],"chunks":[["a"],["b"],["c"],["d"],["e"]]}'
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := writeTempFile(t, dir, "b.txt", "content b")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	if !ok {
		t.Errorf("difftSimilarity ok = false; want true")
	}
	if got != 0 {
		t.Errorf("difftSimilarity = %v, want 0 (clamped from negative)", got)
	}
}

func TestDifftSimilarity_FallbackToLineCount(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
echo '{"status":"changed","aligned_lines":[],"chunks":[["a"]]}'
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "different content")
	pathB := writeTempFile(t, dir, "b.txt", "line1\nline2\nline3\n")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	want := 0.75 // 1 changed out of 4 lines
	if !ok {
		t.Errorf("difftSimilarity ok = false; want true")
	}
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("difftSimilarity = %v, want %v", got, want)
	}
}

func TestDifftSimilarity_FallbackMissingPathB(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
echo '{"status":"changed","aligned_lines":[],"chunks":[]}'
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := filepath.Join(dir, "missing_b.txt")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	if got != 0 || ok {
		t.Errorf("difftSimilarity = %v, %v; want 0, false when pathB is unreadable", got, ok)
	}
}

func TestDifftSimilarity_MalformedChunkSkipped(t *testing.T) {
	withStubBinary(t, "difft", `#!/bin/sh
echo '{"status":"changed","aligned_lines":[[1,1],[2,2],[3,3],[4,4]],"chunks":[{"not":"an array"}]}'
`)
	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := writeTempFile(t, dir, "b.txt", "content b")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	if !ok {
		t.Errorf("difftSimilarity ok = false; want true")
	}
	if got != 1.0 {
		t.Errorf("difftSimilarity = %v, want 1.0 (malformed chunk skipped)", got)
	}
}

func TestDifftSimilarity_ArgumentsAndEnvironment(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	envFile := filepath.Join(t.TempDir(), "env.txt")
	withStubBinary(t, "difft", `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_FILE"
echo "$DFT_UNSTABLE" > "$ENV_FILE"
echo '{"status":"unchanged","aligned_lines":[],"chunks":[]}'
`)
	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("ENV_FILE", envFile)

	dir := t.TempDir()
	pathA := writeTempFile(t, dir, "a.txt", "content a")
	pathB := writeTempFile(t, dir, "b.txt", "content b")

	got, ok := difftSimilarity(context.Background(), pathA, pathB)
	if !ok {
		t.Errorf("difftSimilarity ok = false; want true")
	}
	if got != 1.0 {
		t.Errorf("difftSimilarity = %v, want 1.0", got)
	}

	content, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines in args file, got %d: %q", len(lines), string(content))
	}

	if lines[0] != "--display" {
		t.Errorf("first arg = %q, want --display", lines[0])
	}
	if lines[1] != "json" {
		t.Errorf("second arg = %q, want json", lines[1])
	}
	if lines[2] != pathA {
		t.Errorf("third arg = %q, want %q", lines[2], pathA)
	}
	if lines[3] != pathB {
		t.Errorf("fourth arg = %q, want %q", lines[3], pathB)
	}

	envContent, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading env file: %v", err)
	}
	if strings.TrimSpace(string(envContent)) != "yes" {
		t.Errorf("DFT_UNSTABLE = %q, want yes", strings.TrimSpace(string(envContent)))
	}
}

// The second `if totalLines == 0` block in difftSimilarity is unreachable:
// totalLines is either len(res.AlignedLines) (non-negative) or strings.Count(...) + 1 (at least 1).
// Therefore, no test can reach the case where totalLines == 0 after the fallback logic.
