// Package agentic implements Phase 5's agentic eval mode (DESIGN.md
// "Agentic eval mode"): a bounded tool loop that drives a candidate model
// through read/list/grep/edit/write/run_tests over a real, network-denied
// git worktree of the target repo, graded fail-to-pass (SWE-bench style)
// against a task's held-out tests. It reuses internal/verify's worktree
// naming/sweep pattern (a distinct temp prefix, since an agentic sandbox is
// one worktree per task, not the two-sided frontier/local pair
// internal/verify builds for cascade scoring) and internal/evals' backend
// resolution and ladder scheduling, rather than duplicating either.
package agentic

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

// agenticTempPrefix names every ephemeral sandbox base directory created
// under os.TempDir(), so both teardown and the startup sweep can find them
// unambiguously. Distinct from internal/verify's "splitter-verify-" prefix.
const agenticTempPrefix = "splitter-agentic-"

// sandboxWorkDirName is the single worktree checkout's directory name
// inside a sandbox's base directory.
const sandboxWorkDirName = "work"

// Sandbox is one task's ephemeral git worktree.
type Sandbox struct {
	RepoPath string // the source repo, for git worktree plumbing
	Base     string // .../splitter-agentic-<pid>-<rand>
	Dir      string // Base/work, the actual worktree checkout
	RepoHead string
}

// newSandboxBase returns a fresh base directory path under os.TempDir(),
// named splitter-agentic-<pid>-<rand>. It does not create the directory:
// `git worktree add` creates its target directory itself.
func newSandboxBase() (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generating sandbox directory suffix: %w", err)
	}
	name := fmt.Sprintf("%s%d-%s", agenticTempPrefix, os.Getpid(), hex.EncodeToString(suffix[:]))
	return filepath.Join(os.TempDir(), name), nil
}

// NewSandbox creates a fresh, detached git worktree of repoPath at
// repoHead. Callers must call Teardown (typically via defer) once done.
func NewSandbox(ctx context.Context, repoPath, repoHead string) (*Sandbox, error) {
	base, err := newSandboxBase()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, sandboxWorkDirName)
	if err := addWorktree(ctx, repoPath, dir, repoHead); err != nil {
		_ = os.RemoveAll(base)
		return nil, fmt.Errorf("creating sandbox worktree: %w", err)
	}
	return &Sandbox{RepoPath: repoPath, Base: base, Dir: dir, RepoHead: repoHead}, nil
}

// Teardown removes the sandbox's worktree and base directory and prunes
// repoPath's worktree administrative list. Safe to call on a partially
// constructed Sandbox and safe to call more than once.
func (s *Sandbox) Teardown() {
	if s == nil {
		return
	}
	removeWorktree(s.RepoPath, s.Dir)
	_ = os.RemoveAll(s.Base)
	pruneWorktrees(s.RepoPath)
}

// addWorktree runs `git worktree add --detach dir commitish` in repoPath.
// Mirrors internal/verify's own helper of the same shape: this package
// builds a single worktree per sandbox rather than a frontier/local pair,
// so the two packages' lifecycle code is not directly shared, but the git
// plumbing itself is intentionally identical (see DECISIONS.md).
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
// removes the directory itself (best-effort: neither git being unaware of
// it nor the directory already being gone is an error worth surfacing,
// since callers use this purely for cleanup).
func removeWorktree(repoPath, dir string) {
	if repoPath != "" {
		cmd := exec.Command("git", "worktree", "remove", "--force", dir)
		cmd.Dir = repoPath
		_ = cmd.Run()
	}
	_ = os.RemoveAll(dir)
}

// pruneWorktrees runs `git worktree prune` in repoPath.
func pruneWorktrees(repoPath string) {
	if repoPath == "" {
		return
	}
	cmd := exec.Command("git", "worktree", "prune")
	cmd.Dir = repoPath
	_ = cmd.Run()
}

// Sweep removes stale ephemeral sandbox directories (matched by the
// agenticTempPrefix naming scheme) under os.TempDir() whose modification
// time is older than olderThan: the startup safety net for a sandbox whose
// owning process was killed before its own defer-based Teardown could run.
// `splitter eval-agentic` calls it at the start of every run. It returns
// the base directories it removed.
func Sweep(repoPath string, olderThan time.Duration) ([]string, error) {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("reading temp directory: %w", err)
	}

	cutoff := time.Now().Add(-olderThan)
	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), agenticTempPrefix) {
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
		removeWorktree(repoPath, filepath.Join(full, sandboxWorkDirName))
		if err := os.RemoveAll(full); err != nil {
			continue
		}
		removed = append(removed, full)
	}

	pruneWorktrees(repoPath)
	return removed, nil
}

// prepTimeout bounds one dependency-prep subprocess (go mod download, npm
// ci): these run with network access, before the sandbox is handed to the
// model, and can legitimately take a while on a cold cache.
const prepTimeout = 5 * time.Minute

// PrepDependencies best-effort installs/warms dependencies from whatever
// lockfiles are present in dir (a Go module's go.sum via `go mod download`,
// an npm project's package-lock.json via `npm ci`), and, when extraDir is
// non-empty, also checks dir/extraDir for its own go.mod (a subsystem
// living in its own module within a monorepo). It runs with the ambient
// network (no unshare): DESIGN.md "Prep online: install/warm dependencies
// ... before the model is involved". ok is false when any prep step failed;
// callers record this via store.UpdateEvalTaskAgenticReady. A sandbox with
// no recognised lockfiles at all is trivially ready (ok=true, detail="").
func PrepDependencies(ctx context.Context, dir, extraDir string) (ok bool, detail string) {
	var errs []string
	ran := false

	if fileExists(filepath.Join(dir, "go.mod")) {
		ran = true
		if err := runPlain(ctx, dir, "go", "mod", "download"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if extraDir != "" && extraDir != "." {
		modDir := filepath.Join(dir, extraDir)
		if fileExists(filepath.Join(modDir, "go.mod")) {
			ran = true
			if err := runPlain(ctx, modDir, "go", "mod", "download"); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if fileExists(filepath.Join(dir, "package-lock.json")) {
		ran = true
		if err := runPlain(ctx, dir, "npm", "ci"); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if !ran || len(errs) == 0 {
		return true, ""
	}
	return false, strings.Join(errs, "; ")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runPlain(ctx context.Context, dir, name string, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, prepTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parkGit renames dir's .git entry (a worktree's is a file pointing at the
// main repo's gitdir, but this tolerates a plain directory too) out of the
// way for the duration of one run_tests invocation, so a test-spawned git
// command fails closed rather than reading real history (DESIGN.md
// "Leakage containment": ".git parked during run_tests"). The returned
// restore func puts it back; callers invoke it via defer immediately after
// a successful park. A sandbox with no .git entry (already parked, or
// never had one) is a no-op: restore does nothing.
func parkGit(dir string) (restore func(), err error) {
	gitPath := filepath.Join(dir, ".git")
	if _, statErr := os.Lstat(gitPath); statErr != nil {
		return func() {}, nil
	}
	parkedPath := gitPath + ".parked"
	if err := os.Rename(gitPath, parkedPath); err != nil {
		return nil, fmt.Errorf("parking .git: %w", err)
	}
	return func() { _ = os.Rename(parkedPath, gitPath) }, nil
}

// UnshareAvailable probes whether `unshare -rn` can actually run on this
// machine (the binary exists and a network namespace can be created).
func UnshareAvailable() bool {
	if _, err := exec.LookPath("unshare"); err != nil {
		return false
	}
	cmd := exec.Command("unshare", "-rn", "true")
	return cmd.Run() == nil
}

// CommandRunner runs one shell command in a sandbox, optionally
// network-denied via `unshare -rn`.
type CommandRunner struct {
	UseUnshare bool
	Timeout    time.Duration
}

// Run runs command via `sh -c` in dir (wrapped in `unshare -rn --` first
// when UseUnshare), bounded by Timeout. ok reports whether the command
// exited zero; err is non-nil only when the command could not be run at
// all (e.g. the shell itself failed to start), not for a nonzero exit,
// which is a normal graded outcome (ok=false, err=nil).
func (r CommandRunner) Run(ctx context.Context, dir, command string) (output string, ok bool, err error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if r.UseUnshare {
		cmd = exec.CommandContext(cctx, "unshare", "-rn", "--", "sh", "-c", command)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", command)
	}
	cmd.Dir = dir

	outBytes, runErr := cmd.CombinedOutput()
	output = string(outBytes)
	if runErr == nil {
		return output, true, nil
	}
	if _, isExit := runErr.(*exec.ExitError); isExit {
		return output, false, nil
	}
	return output, false, fmt.Errorf("running command: %w", runErr)
}

// runTestsWithParking runs command in dir with .git parked for the
// duration (restored afterwards regardless of outcome), the shape every
// run_tests invocation (model-triggered or the harness's own baseline/final
// grading passes) uses.
func runTestsWithParking(ctx context.Context, runner CommandRunner, dir, command string) (output string, ok bool, err error) {
	restore, err := parkGit(dir)
	if err != nil {
		return "", false, err
	}
	defer restore()
	return runner.Run(ctx, dir, command)
}
