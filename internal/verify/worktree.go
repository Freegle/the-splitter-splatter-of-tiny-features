package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// verifyTempPrefix names every ephemeral comparison worktree base
// directory created under os.TempDir(), so both teardown and the startup
// sweep can find them unambiguously.
const verifyTempPrefix = "splitter-verify-"

// semaphore bounds how many worktree-based cascades run at once.
type semaphore chan struct{}

func newSemaphore(n int) semaphore {
	if n < 1 {
		n = 1
	}
	return make(semaphore, n)
}

func (s semaphore) acquire(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s semaphore) release() {
	<-s
}

// newWorktreeBase returns a fresh base directory path under os.TempDir()
// named splitter-verify-<pid>-<rand>, per DESIGN.md's naming scheme. It
// does not create the directory: `git worktree add` creates its target
// directory itself.
func newWorktreeBase() (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generating worktree directory suffix: %w", err)
	}
	name := fmt.Sprintf("%s%d-%s", verifyTempPrefix, os.Getpid(), hex.EncodeToString(suffix[:]))
	return filepath.Join(os.TempDir(), name), nil
}

// joinWorktree returns the per-side directory (base/frontier or
// base/local) under a worktree base directory.
func joinWorktree(base, side string) string {
	return filepath.Join(base, side)
}

// addWorktree runs `git worktree add --detach dir commitish` in repoPath.
func addWorktree(ctx context.Context, repoPath, dir, commitish string) error {
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "--detach", dir, commitish)
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add %s at %s: %w: %s", dir, commitish, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// removeWorktree tells git to forget dir as a worktree of repoPath, then
// removes the directory itself (best-effort: git may already be unaware
// of it, or the directory may already be gone, neither is an error worth
// surfacing here since callers use this purely for cleanup).
func removeWorktree(repoPath, dir string) {
	if repoPath != "" {
		cmd := exec.Command("git", "worktree", "remove", "--force", dir)
		cmd.Dir = repoPath
		_ = cmd.Run()
	}
	_ = os.RemoveAll(dir)
}

// pruneWorktrees runs `git worktree prune` in repoPath, dropping any
// worktree administrative entries left behind by a directory that was
// removed by means other than `git worktree remove`.
func pruneWorktrees(repoPath string) {
	if repoPath == "" {
		return
	}
	cmd := exec.Command("git", "worktree", "prune")
	cmd.Dir = repoPath
	_ = cmd.Run()
}

// teardownWorktrees removes both per-side worktree directories, the base
// directory that held them, and prunes repoPath's worktree list. It is
// called via defer from the moment the base directory is chosen, so a
// failure partway through worktree creation still cleans up whatever was
// created.
func teardownWorktrees(repoPath, base string, sideDirs ...string) {
	for _, dir := range sideDirs {
		removeWorktree(repoPath, dir)
	}
	_ = os.RemoveAll(base)
	pruneWorktrees(repoPath)
}

// Sweep removes stale ephemeral comparison worktree directories (matched
// by the verifyTempPrefix naming scheme) under os.TempDir() whose
// modification time is older than olderThan. This is the startup safety
// net for a worktree whose owning process was killed (e.g. SIGKILL) before
// its own defer-based teardown could run: `splitter replay` calls it at
// the start of every run. It returns the base directories it removed.
func Sweep(repoPath string, olderThan time.Duration) ([]string, error) {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("reading temp directory: %w", err)
	}

	cutoff := time.Now().Add(-olderThan)
	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), verifyTempPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		full := filepath.Join(os.TempDir(), entry.Name())
		removeWorktree(repoPath, joinWorktree(full, "frontier"))
		removeWorktree(repoPath, joinWorktree(full, "local"))
		if err := os.RemoveAll(full); err != nil {
			continue
		}
		removed = append(removed, full)
	}

	pruneWorktrees(repoPath)
	return removed, nil
}
