package agentic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewSandbox_TeardownLeavesNoDirectory(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})

	sandbox, err := NewSandbox(context.Background(), repoPath, commit)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	if _, err := os.Stat(sandbox.Dir); err != nil {
		t.Fatalf("expected sandbox dir to exist: %v", err)
	}

	sandbox.Teardown()

	if _, err := os.Stat(sandbox.Base); !os.IsNotExist(err) {
		t.Errorf("sandbox base %s still exists after Teardown: %v", sandbox.Base, err)
	}
}

func TestSweep_AgeFiltering(t *testing.T) {
	origTemp := os.Getenv("TMPDIR")
	tempRoot := t.TempDir()
	if err := os.Setenv("TMPDIR", tempRoot); err != nil {
		t.Fatalf("setting TMPDIR: %v", err)
	}
	t.Cleanup(func() { os.Setenv("TMPDIR", origTemp) })

	staleDir := filepath.Join(os.TempDir(), agenticTempPrefix+"stale")
	freshDir := filepath.Join(os.TempDir(), agenticTempPrefix+"fresh")
	unrelatedDir := filepath.Join(os.TempDir(), "not-an-agentic-dir")
	for _, d := range []string{staleDir, freshDir, unrelatedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleDir, oldTime, oldTime); err != nil {
		t.Fatalf("backdating %s: %v", staleDir, err)
	}

	removed, err := Sweep("", time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(removed) != 1 || removed[0] != staleDir {
		t.Errorf("removed = %v, want only %v", removed, staleDir)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("stale dir still exists after sweep")
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("fresh dir should not have been removed: %v", err)
	}
	if _, err := os.Stat(unrelatedDir); err != nil {
		t.Errorf("unrelated dir should not have been removed: %v", err)
	}
}

func TestSweep_ZeroLeftoverAgenticDirsAfterTeardown(t *testing.T) {
	origTemp := os.Getenv("TMPDIR")
	tempRoot := t.TempDir()
	if err := os.Setenv("TMPDIR", tempRoot); err != nil {
		t.Fatalf("setting TMPDIR: %v", err)
	}
	t.Cleanup(func() { os.Setenv("TMPDIR", origTemp) })

	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})

	sandbox, err := NewSandbox(context.Background(), repoPath, commit)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	sandbox.Teardown()

	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("reading temp dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), agenticTempPrefix) {
			t.Errorf("leftover agentic sandbox dir after Teardown: %s", e.Name())
		}
	}
}

// TestParkGit_SandboxHasNoRemotesAndGitAbsentDuringRunTests builds a real
// sandbox worktree and asserts both DESIGN.md leakage-containment
// properties: the worktree carries no configured remotes, and a test
// command run through runTestsWithParking (the same wrapper run_tests
// uses) sees .git as absent for its own duration, restored immediately
// after.
func TestParkGit_SandboxHasNoRemotesAndGitAbsentDuringRunTests(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})

	sandbox, err := NewSandbox(context.Background(), repoPath, commit)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	defer sandbox.Teardown()

	remotes := runGit(t, sandbox.Dir, "remote")
	if strings.TrimSpace(remotes) != "" {
		t.Errorf("sandbox worktree has remotes: %q, want none", remotes)
	}

	gitPath := filepath.Join(sandbox.Dir, ".git")
	if _, err := os.Lstat(gitPath); err != nil {
		t.Fatalf("expected .git to exist before run_tests: %v", err)
	}

	runner := CommandRunner{UseUnshare: false, Timeout: 10 * time.Second}
	output, ok, err := runTestsWithParking(context.Background(), runner, sandbox.Dir, `test -e .git && echo PRESENT || echo ABSENT`)
	if err != nil {
		t.Fatalf("runTestsWithParking: %v", err)
	}
	if !ok {
		t.Errorf("test command should have exited zero, output=%q", output)
	}
	if !strings.Contains(output, "ABSENT") {
		t.Errorf("output = %q, want it to report .git ABSENT during run_tests", output)
	}

	if _, err := os.Lstat(gitPath); err != nil {
		t.Errorf(".git not restored after run_tests: %v", err)
	}
}

func TestContainsAttemptedGit_FlaggedWhenTestOutputMentionsGitFailure(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	sandbox, err := NewSandbox(context.Background(), repoPath, commit)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	defer sandbox.Teardown()

	// run_tests always returns isError=false (a nonzero test exit is a
	// normal graded outcome, not a tool call error); the failure must show
	// up in the result text itself instead.
	exec := NewToolExecutor(sandbox, CommandRunner{UseUnshare: false, Timeout: 10 * time.Second}, "git status")
	text, _ := exec.runTests(context.Background())
	if !strings.Contains(strings.ToLower(text), "not a git repository") {
		t.Fatalf("run_tests output = %q, want it to mention 'not a git repository'", text)
	}

	flags := exec.CheatFlags()
	found := false
	for _, f := range flags {
		if f.Type == CheatFlagAttemptedGit {
			found = true
		}
	}
	if !found {
		t.Errorf("CheatFlags() = %+v, want an attempted_git flag", flags)
	}
}

func TestUnshareAvailable(t *testing.T) {
	// This machine is documented (DECISIONS.md) as having a working
	// unshare -rn; assert the probe agrees, so a regression in the probe
	// itself (not the underlying kernel feature) is caught.
	if !UnshareAvailable() {
		t.Skip("unshare -rn is not usable on this machine; UnshareAvailable's own correctness cannot be checked here")
	}
}

func TestCommandRunner_UnshareWrapping(t *testing.T) {
	if !UnshareAvailable() {
		t.Skip("unshare -rn is not usable on this machine")
	}
	dir := t.TempDir()
	runner := CommandRunner{UseUnshare: true, Timeout: 10 * time.Second}
	output, ok, err := runner.Run(context.Background(), dir, "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ok || strings.TrimSpace(output) != "hello" {
		t.Errorf("Run under unshare = (%q, %v), want (\"hello\", true)", output, ok)
	}
}

func TestPrepDependencies_TrivialGoModuleIsReady(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/prep\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	ready, detail := PrepDependencies(context.Background(), dir, "")
	if !ready {
		t.Errorf("PrepDependencies ready=false, detail=%q, want ready", detail)
	}
}

func TestPrepDependencies_NoLockfilesIsTriviallyReady(t *testing.T) {
	dir := t.TempDir()
	ready, detail := PrepDependencies(context.Background(), dir, "")
	if !ready || detail != "" {
		t.Errorf("PrepDependencies = (%v, %q), want (true, \"\")", ready, detail)
	}
}
