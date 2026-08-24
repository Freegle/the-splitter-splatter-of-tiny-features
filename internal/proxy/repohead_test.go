package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit runs git in dir, failing the test on error. It exists only in test
// setup code: production code never shells out to git (see readRepoHead).
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=splitter-test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=splitter-test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}
}

func TestReadRepoHead_EmptyPath(t *testing.T) {
	if got := readRepoHead(""); got != "" {
		t.Errorf("readRepoHead(\"\") = %q, want empty", got)
	}
}

func TestReadRepoHead_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	if got := readRepoHead(dir); got != "" {
		t.Errorf("readRepoHead(non-repo) = %q, want empty", got)
	}
}

func TestReadRepoHead_PlainRepo(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "initial")
	want := runGit(t, dir, "rev-parse", "HEAD")

	got := readRepoHead(dir)
	if got != want {
		t.Errorf("readRepoHead(plain repo) = %q, want %q", got, want)
	}
}

func TestReadRepoHead_DetachedHEAD(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "initial")
	sha := runGit(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "checkout", "-q", sha)

	got := readRepoHead(dir)
	if got != sha {
		t.Errorf("readRepoHead(detached HEAD) = %q, want %q", got, sha)
	}
}

func TestReadRepoHead_Worktree(t *testing.T) {
	requireGit(t)
	mainDir := t.TempDir()
	runGit(t, mainDir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(mainDir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	runGit(t, mainDir, "add", "a.txt")
	runGit(t, mainDir, "commit", "-q", "-m", "initial")

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	runGit(t, mainDir, "worktree", "add", "-q", "-b", "feature", worktreeDir)
	want := runGit(t, worktreeDir, "rev-parse", "HEAD")

	// Confirm this really is exercising the worktree ".git" FILE path, not
	// a plain directory.
	info, err := os.Stat(filepath.Join(worktreeDir, ".git"))
	if err != nil {
		t.Fatalf("stat worktree .git: %v", err)
	}
	if info.IsDir() {
		t.Fatal("worktree .git is a directory, expected a gitdir-pointer file for this test to be meaningful")
	}

	got := readRepoHead(worktreeDir)
	if got != want {
		t.Errorf("readRepoHead(worktree) = %q, want %q", got, want)
	}
}

func TestReadRepoHead_WorktreeAfterCommitInWorktree(t *testing.T) {
	requireGit(t)
	mainDir := t.TempDir()
	runGit(t, mainDir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(mainDir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	runGit(t, mainDir, "add", "a.txt")
	runGit(t, mainDir, "commit", "-q", "-m", "initial")

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	runGit(t, mainDir, "worktree", "add", "-q", "-b", "feature2", worktreeDir)

	if err := os.WriteFile(filepath.Join(worktreeDir, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("writing second fixture file: %v", err)
	}
	runGit(t, worktreeDir, "add", "b.txt")
	runGit(t, worktreeDir, "commit", "-q", "-m", "second")
	want := runGit(t, worktreeDir, "rev-parse", "HEAD")

	got := readRepoHead(worktreeDir)
	if got != want {
		t.Errorf("readRepoHead(worktree, after its own commit) = %q, want %q", got, want)
	}
}
