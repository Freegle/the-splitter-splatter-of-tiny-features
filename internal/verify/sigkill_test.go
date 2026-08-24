package verify

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestSweep_RemovesSigkilledWorktreeLeftover proves the startup sweep
// (Sweep) is the safety net for a worktree whose owning process never got
// to run its own defer-based teardown: it spawns a child copy of this test
// binary that creates a real ephemeral worktree, reports its base
// directory, and blocks; SIGKILLs it (no deferred cleanup runs); then
// asserts Sweep finds and removes the leftover. The age check is bypassed
// by passing olderThan = 0.
func TestSweep_RemovesSigkilledWorktreeLeftover(t *testing.T) {
	repoPath, commit := newTestRepo(t, "main.go", "package main\n")

	cmd := exec.Command(os.Args[0], "-test.run=^TestVerifyWorktreeHelperProcess$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "HELPER_REPO="+repoPath, "HELPER_COMMIT="+commit)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("wiring helper stdout: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("wiring helper stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper process: %v", err)
	}

	reader := bufio.NewReader(stdout)
	line, readErr := reader.ReadString('\n')
	if readErr != nil {
		errOut, _ := io.ReadAll(stderr)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("reading helper readiness line: %v (stderr: %s)", readErr, errOut)
	}
	base := strings.TrimSpace(line)
	if base == "" {
		t.Fatalf("helper reported an empty worktree base")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL helper process: %v", err)
	}
	_ = cmd.Wait()

	if _, err := os.Stat(base); err != nil {
		t.Fatalf("expected leftover worktree base %s to exist before sweep: %v", base, err)
	}

	removed, err := Sweep(repoPath, 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	found := false
	for _, r := range removed {
		if r == base {
			found = true
		}
	}
	if !found {
		t.Errorf("Sweep did not report removing %s; removed=%v", base, removed)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Errorf("leftover worktree base %s still exists after sweep: %v", base, err)
	}
}

// TestVerifyWorktreeHelperProcess is not a real test: run under the
// GO_WANT_HELPER_PROCESS=1 env var (set only by
// TestSweep_RemovesSigkilledWorktreeLeftover), it creates one ephemeral
// worktree, prints its base directory, and blocks indefinitely so the
// parent can SIGKILL it before any teardown runs. Under a normal `go test`
// invocation the env var is unset and it returns immediately, a no-op.
func TestVerifyWorktreeHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	repoPath := os.Getenv("HELPER_REPO")
	commit := os.Getenv("HELPER_COMMIT")

	base, err := newWorktreeBase()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := addWorktree(context.Background(), repoPath, joinWorktree(base, "frontier"), commit); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Println(base)
	os.Stdout.Sync()
	time.Sleep(time.Hour)
}
