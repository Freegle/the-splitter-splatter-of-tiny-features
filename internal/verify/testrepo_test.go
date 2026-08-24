package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRepo builds a real git repository in t.TempDir(): a go.mod, one Go
// source file at goFileRel with goFileContent, committed. It returns the
// repository path and the commit sha, so verify's cascade tests can create
// real ephemeral worktrees against it.
func newTestRepo(t *testing.T, goFileRel, goFileContent string) (repoPath, commit string) {
	t.Helper()
	repoPath = t.TempDir()

	runGit(t, repoPath, "init", "-q")
	runGit(t, repoPath, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--allow-empty", "-q", "-m", "root")

	writeRepoFile(t, repoPath, "go.mod", "module example.com/testrepo\n\ngo 1.21\n")
	writeRepoFile(t, repoPath, goFileRel, goFileContent)

	runGit(t, repoPath, "add", "-A")
	runGit(t, repoPath, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "add "+goFileRel)

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
