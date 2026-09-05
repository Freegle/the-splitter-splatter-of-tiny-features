package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStubBinary installs an executable shell script named `name` in a fresh
// directory at the front of PATH, so exec.LookPath finds it. script must
// start with a #!/bin/sh line.
func withStubBinary(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestLintPHP_BinaryMissing(t *testing.T) {
	// Discard the real PATH so we don't accidentally find php on the host.
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	entry := lintPHP(context.Background(), t.TempDir(), "test.php")
	if entry.Tool != "" {
		t.Errorf("lintPHP Tool = %q, want empty string when php is missing", entry.Tool)
	}
	if entry.OK {
		t.Error("lintPHP OK = true, want false when php is missing")
	}
}

func TestLintESLint_BinaryMissing(t *testing.T) {
	// Discard the real PATH so we don't accidentally find eslint on the host.
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	entry := lintESLint(context.Background(), t.TempDir(), "test.js")
	if entry.Tool != "" {
		t.Errorf("lintESLint Tool = %q, want empty string when eslint is missing", entry.Tool)
	}
	if entry.OK {
		t.Error("lintESLint OK = true, want false when eslint is missing")
	}
}

func TestLintPHP_PassingFile(t *testing.T) {
	withStubBinary(t, "php", `#!/bin/sh
exit 0
`)
	worktreeDir := t.TempDir()
	entry := lintPHP(context.Background(), worktreeDir, "test.php")

	if entry.Tool != "php -l" {
		t.Errorf("lintPHP Tool = %q, want %q", entry.Tool, "php -l")
	}
	if !entry.OK {
		t.Error("lintPHP OK = false, want true for passing file")
	}
	if entry.Output != "" {
		t.Errorf("lintPHP Output = %q, want empty string", entry.Output)
	}
}

func TestLintPHP_FailingFileCapturesOutput(t *testing.T) {
	withStubBinary(t, "php", `#!/bin/sh
echo ""
echo "PHP Parse error: syntax error, unexpected ';' in bad.php on line 3"
exit 1
`)
	worktreeDir := t.TempDir()
	entry := lintPHP(context.Background(), worktreeDir, "bad.php")

	if entry.OK {
		t.Error("lintPHP OK = true, want false for failing file")
	}
	expectedOutput := "PHP Parse error: syntax error, unexpected ';' in bad.php on line 3"
	if entry.Output != expectedOutput {
		t.Errorf("lintPHP Output = %q, want %q", entry.Output, expectedOutput)
	}
}

func TestLintPHP_CapturesStderr(t *testing.T) {
	withStubBinary(t, "php", `#!/bin/sh
echo "went to stderr" >&2
exit 1
`)
	worktreeDir := t.TempDir()
	entry := lintPHP(context.Background(), worktreeDir, "test.php")

	if entry.OK {
		t.Error("lintPHP OK = true, want false for failing file")
	}
	expectedOutput := "went to stderr"
	if entry.Output != expectedOutput {
		t.Errorf("lintPHP Output = %q, want %q", entry.Output, expectedOutput)
	}
}

func TestLintPHP_PassesJoinedPathToLinter(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	withStubBinary(t, "php", `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_FILE"
exit 0
`)
	t.Setenv("ARGS_FILE", argsFile)

	worktreeDir := t.TempDir()
	relPath := "src/app/bad.php"
	entry := lintPHP(context.Background(), worktreeDir, relPath)

	if !entry.OK {
		t.Fatalf("lintPHP failed unexpectedly: %v", entry.Output)
	}

	content, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines in args file, got %d: %q", len(lines), string(content))
	}

	if lines[0] != "-l" {
		t.Errorf("first arg = %q, want -l", lines[0])
	}

	fullPath := filepath.Join(worktreeDir, relPath)
	if lines[1] != fullPath {
		t.Errorf("second arg = %q, want %q", lines[1], fullPath)
	}

	if lines[1] == relPath {
		t.Error("second arg equals relPath, expected joined path")
	}
}

func TestLintESLint_PassingAndFailing(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		wantOK     bool
		wantOutput string
	}{
		{
			name: "passing",
			script: `#!/bin/sh
exit 0
`,
			wantOK:     true,
			wantOutput: "",
		},
		{
			name: "failing",
			script: `#!/bin/sh
echo "error: missing semicolon"
exit 1
`,
			wantOK:     false,
			wantOutput: "error: missing semicolon",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStubBinary(t, "eslint", tc.script)
			worktreeDir := t.TempDir()
			entry := lintESLint(context.Background(), worktreeDir, "test.js")

			if entry.Tool != "eslint" {
				t.Errorf("lintESLint Tool = %q, want %q", entry.Tool, "eslint")
			}
			if entry.OK != tc.wantOK {
				t.Errorf("lintESLint OK = %v, want %v", entry.OK, tc.wantOK)
			}
			if entry.Output != tc.wantOutput {
				t.Errorf("lintESLint Output = %q, want %q", entry.Output, tc.wantOutput)
			}
		})
	}
}

func TestLintESLint_PassesNoEslintrcFlag(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	withStubBinary(t, "eslint", `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_FILE"
exit 0
`)
	t.Setenv("ARGS_FILE", argsFile)

	worktreeDir := t.TempDir()
	relPath := "src/app/bad.js"
	entry := lintESLint(context.Background(), worktreeDir, relPath)

	if !entry.OK {
		t.Fatalf("lintESLint failed unexpectedly: %v", entry.Output)
	}

	content, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines in args file, got %d: %q", len(lines), string(content))
	}

	if lines[0] != "--no-eslintrc" {
		t.Errorf("first arg = %q, want --no-eslintrc", lines[0])
	}

	fullPath := filepath.Join(worktreeDir, relPath)
	if lines[1] != fullPath {
		t.Errorf("second arg = %q, want %q", lines[1], fullPath)
	}
}
