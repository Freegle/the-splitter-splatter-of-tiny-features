package agentic

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/config"
)

// newArenaTestWorktree creates a linked git worktree of repoPath checked
// out at commit, either detached (mirroring the real eval-arena's usual
// state) or on a fresh branch (mirroring a worktree left on a branch). It
// registers cleanup to remove the worktree.
func newArenaTestWorktree(t *testing.T, repoPath, commit string, detach bool) string {
	t.Helper()
	worktreeDir := filepath.Join(t.TempDir(), "arena")

	var args []string
	if detach {
		args = []string{"worktree", "add", "--detach", worktreeDir, commit}
	} else {
		args = []string{"worktree", "add", "-b", "arena-branch", worktreeDir, commit}
	}
	runGit(t, repoPath, args...)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktreeDir).Run()
	})
	return worktreeDir
}

func writeArenaEnv(t *testing.T, worktreeDir, project string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(worktreeDir, ".env"), []byte("COMPOSE_PROJECT_NAME="+project+"\n"), 0o644); err != nil {
		t.Fatalf("writing arena .env: %v", err)
	}
}

func TestVerifyArenaIsWorktree_RefusesMainCheckout(t *testing.T) {
	repoPath, _ := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})

	if err := verifyArenaIsWorktree(context.Background(), repoPath); err == nil {
		t.Fatal("expected an error refusing the main checkout, got nil")
	}
}

func TestVerifyArenaIsWorktree_AcceptsLinkedWorktree(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	worktreeDir := newArenaTestWorktree(t, repoPath, commit, true)

	if err := verifyArenaIsWorktree(context.Background(), worktreeDir); err != nil {
		t.Fatalf("verifyArenaIsWorktree on a real linked worktree: %v", err)
	}
}

func TestVerifyArenaIsWorktree_RefusesNonGitDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := verifyArenaIsWorktree(context.Background(), dir); err == nil {
		t.Fatal("expected an error for a non-git directory, got nil")
	}
}

func TestReadComposeProjectName(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{"present", "COMPOSE_PROJECT_NAME=freegle-eval-arena\n", "freegle-eval-arena", false},
		{"with other keys", "PORT_STATUS=12090\nCOMPOSE_PROJECT_NAME=freegle-eval-arena\nOTHER=x\n", "freegle-eval-arena", false},
		{"missing key", "PORT_STATUS=12090\n", "", true},
		{"empty value", "COMPOSE_PROJECT_NAME=\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(tt.content), 0o644); err != nil {
				t.Fatalf("writing .env: %v", err)
			}
			got, err := readComposeProjectName(dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got project %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readComposeProjectName: %v", err)
			}
			if got != tt.want {
				t.Errorf("project = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadComposeProjectName_MissingFile(t *testing.T) {
	if _, err := readComposeProjectName(t.TempDir()); err == nil {
		t.Fatal("expected an error for a missing .env, got nil")
	}
}

// TestResolveArenaConfig_NeverDefaultsToBareMainProjectName is the safety
// test the task brief asks for: a fixture .env naming
// COMPOSE_PROJECT_NAME=freegle-eval-arena must resolve to exactly that
// project, never the bare main-instance name "freegle" some other tool
// (status-nuxt itself) would silently fall back to.
func TestResolveArenaConfig_NeverDefaultsToBareMainProjectName(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	worktreeDir := newArenaTestWorktree(t, repoPath, commit, true)
	writeArenaEnv(t, worktreeDir, "freegle-eval-arena")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	port := serverPort(t, server)

	cfg := config.Default()
	cfg.Evals.ArenaPath = worktreeDir
	cfg.Evals.ArenaStatusPort = port

	arena, err := ResolveArenaConfig(cfg)
	if err != nil {
		t.Fatalf("ResolveArenaConfig: %v", err)
	}
	if arena.Project != "freegle-eval-arena" {
		t.Fatalf("Project = %q, want freegle-eval-arena", arena.Project)
	}

	for _, subsystem := range []string{"iznik-server-go", "iznik-nuxt3", "iznik-batch"} {
		service, workdir, ok := arenaContainerFor(subsystem)
		if !ok {
			t.Fatalf("arenaContainerFor(%q) not ok", subsystem)
		}
		args := arenaDockerExecArgs(arena.Project, service, workdir, "true")
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "freegle-"+service) && !strings.Contains(joined, "freegle-eval-arena-"+service) {
			t.Errorf("subsystem %s: constructed args %v used the bare main project name, not freegle-eval-arena", subsystem, args)
		}
		if !strings.Contains(joined, "freegle-eval-arena-"+service) {
			t.Errorf("subsystem %s: constructed args %v did not target freegle-eval-arena-%s", subsystem, args, service)
		}
	}
}

func TestResolveArenaConfig_RefusesMissingArenaPath(t *testing.T) {
	cfg := config.Default()
	cfg.Evals.ArenaPath = filepath.Join(t.TempDir(), "does-not-exist")
	cfg.Evals.ArenaStatusPort = 1

	if _, err := ResolveArenaConfig(cfg); err == nil {
		t.Fatal("expected an error for a missing arena_path, got nil")
	}
}

func TestResolveArenaConfig_RefusesUnconfiguredArenaPath(t *testing.T) {
	cfg := config.Default()
	if _, err := ResolveArenaConfig(cfg); err == nil {
		t.Fatal("expected an error when arena_path is not configured, got nil")
	}
}

func TestResolveArenaConfig_RefusesMainCheckoutPath(t *testing.T) {
	repoPath, _ := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	writeArenaEnv(t, repoPath, "freegle-eval-arena")

	cfg := config.Default()
	cfg.Evals.ArenaPath = repoPath
	cfg.Evals.ArenaStatusPort = 1

	if _, err := ResolveArenaConfig(cfg); err == nil {
		t.Fatal("expected an error refusing the main checkout path, got nil")
	}
}

func TestResolveArenaConfig_RefusesBareMainProjectNameInEnv(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	worktreeDir := newArenaTestWorktree(t, repoPath, commit, true)
	writeArenaEnv(t, worktreeDir, "freegle")

	cfg := config.Default()
	cfg.Evals.ArenaPath = worktreeDir
	cfg.Evals.ArenaStatusPort = 1

	if _, err := ResolveArenaConfig(cfg); err == nil {
		t.Fatal("expected an error refusing COMPOSE_PROJECT_NAME=freegle, got nil")
	}
}

func TestResolveArenaConfig_RefusesUnreachableStatusAPI(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	worktreeDir := newArenaTestWorktree(t, repoPath, commit, true)
	writeArenaEnv(t, worktreeDir, "freegle-eval-arena")

	cfg := config.Default()
	cfg.Evals.ArenaPath = worktreeDir
	cfg.Evals.ArenaStatusPort = unusedPort(t)

	if _, err := ResolveArenaConfig(cfg); err == nil {
		t.Fatal("expected an error for an unreachable status API, got nil")
	}
}

func TestResolveArenaConfig_Succeeds(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	worktreeDir := newArenaTestWorktree(t, repoPath, commit, true)
	writeArenaEnv(t, worktreeDir, "freegle-eval-arena")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()

	cfg := config.Default()
	cfg.Evals.ArenaPath = worktreeDir
	cfg.Evals.ArenaStatusPort = serverPort(t, server)

	arena, err := ResolveArenaConfig(cfg)
	if err != nil {
		t.Fatalf("ResolveArenaConfig: %v", err)
	}
	if arena.Path != worktreeDir || arena.Project != "freegle-eval-arena" {
		t.Errorf("arena = %+v", arena)
	}
}

func TestNewArenaSandbox_RefusesDirtyArena(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	worktreeDir := newArenaTestWorktree(t, repoPath, commit, true)
	if err := os.WriteFile(filepath.Join(worktreeDir, "main.go"), []byte("package main\n// dirty\n"), 0o644); err != nil {
		t.Fatalf("dirtying worktree: %v", err)
	}

	if _, err := NewArenaSandbox(context.Background(), worktreeDir, commit); err == nil {
		t.Fatal("expected an error for a dirty arena worktree, got nil")
	}
}

func TestNewArenaSandbox_ChecksOutTaskCommitAndTeardownRestoresBranch(t *testing.T) {
	repoPath, firstCommit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n// v1\n"})
	writeRepoFile(t, repoPath, "main.go", "package main\n// v2\n")
	runGit(t, repoPath, "add", "-A")
	runGit(t, repoPath, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "v2")

	worktreeDir := newArenaTestWorktree(t, repoPath, firstCommit, false) // on branch "arena-branch", at firstCommit

	head, err := gitOutput(context.Background(), worktreeDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("reading initial HEAD: %v", err)
	}
	if strings.TrimSpace(head) != firstCommit {
		t.Fatalf("worktree HEAD = %s, want %s", strings.TrimSpace(head), firstCommit)
	}

	secondCommit := strings.TrimSpace(runGit(t, repoPath, "rev-parse", "main"))

	sandbox, err := NewArenaSandbox(context.Background(), worktreeDir, secondCommit)
	if err != nil {
		t.Fatalf("NewArenaSandbox: %v", err)
	}

	gotHead, err := gitOutput(context.Background(), worktreeDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("reading checked-out HEAD: %v", err)
	}
	if strings.TrimSpace(gotHead) != secondCommit {
		t.Fatalf("HEAD after NewArenaSandbox = %s, want task commit %s", strings.TrimSpace(gotHead), secondCommit)
	}
	content, err := os.ReadFile(filepath.Join(worktreeDir, "main.go"))
	if err != nil || !strings.Contains(string(content), "v2") {
		t.Fatalf("expected the task commit's content (v2) in the sandbox, got %q, err=%v", content, err)
	}

	if err := sandbox.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	branch, err := gitOutput(context.Background(), worktreeDir, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		t.Fatalf("reading restored branch: %v", err)
	}
	if strings.TrimSpace(branch) != "arena-branch" {
		t.Fatalf("branch after Teardown = %q, want arena-branch", strings.TrimSpace(branch))
	}
	restoredHead, err := gitOutput(context.Background(), worktreeDir, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(restoredHead) != firstCommit {
		t.Fatalf("HEAD after Teardown = %s, want original %s (err=%v)", strings.TrimSpace(restoredHead), firstCommit, err)
	}
}

func TestNewArenaSandbox_TeardownRestoresDetachedHeadEvenOnError(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, map[string]string{"main.go": "package main\n"})
	worktreeDir := newArenaTestWorktree(t, repoPath, commit, true) // detached at commit

	sandbox, err := NewArenaSandbox(context.Background(), worktreeDir, commit)
	if err != nil {
		t.Fatalf("NewArenaSandbox: %v", err)
	}

	// Simulate the rest of a task's lifecycle failing: an untracked file
	// created (e.g. a held-out test) and a tracked file modified (a model
	// edit), neither committed, matching the state Teardown must clean up
	// even though nothing "succeeded".
	if err := os.WriteFile(filepath.Join(worktreeDir, "untracked_new_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("writing untracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "main.go"), []byte("package main\n// model edit\n"), 0o644); err != nil {
		t.Fatalf("modifying tracked file: %v", err)
	}

	// The caller's own defer runs regardless of what happened above.
	if err := sandbox.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	dirty, err := arenaIsDirty(context.Background(), worktreeDir)
	if err != nil {
		t.Fatalf("arenaIsDirty: %v", err)
	}
	if dirty {
		t.Error("arena worktree still dirty after Teardown")
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, "untracked_new_test.go")); !os.IsNotExist(err) {
		t.Errorf("untracked file survived Teardown: err=%v", err)
	}
	restoredHead, err := gitOutput(context.Background(), worktreeDir, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(restoredHead) != commit {
		t.Fatalf("HEAD after Teardown = %s, want original %s (err=%v)", strings.TrimSpace(restoredHead), commit, err)
	}
}

func TestArenaDockerExecArgs_PerSubsystemContainerAndWorkdir(t *testing.T) {
	tests := []struct {
		subsystem   string
		wantService string
		wantWorkdir string
	}{
		{"iznik-server-go", "apiv2", "/app"},
		{"iznik-nuxt3", "modtools-dev-local", "/app"},
		{"iznik-batch", "batch", ""},
	}
	for _, tt := range tests {
		t.Run(tt.subsystem, func(t *testing.T) {
			service, workdir, ok := arenaContainerFor(tt.subsystem)
			if !ok || service != tt.wantService || workdir != tt.wantWorkdir {
				t.Fatalf("arenaContainerFor(%q) = (%q, %q, %v), want (%q, %q, true)", tt.subsystem, service, workdir, ok, tt.wantService, tt.wantWorkdir)
			}

			args := arenaDockerExecArgs("freegle-eval-arena", service, workdir, "go test ./...")
			wantContainer := "freegle-eval-arena-" + tt.wantService
			found := false
			for _, a := range args {
				if a == wantContainer {
					found = true
				}
			}
			if !found {
				t.Errorf("args %v do not contain container %q", args, wantContainer)
			}
			if tt.wantWorkdir != "" {
				joined := strings.Join(args, " ")
				if !strings.Contains(joined, "-w "+tt.wantWorkdir) {
					t.Errorf("args %v missing -w %s", args, tt.wantWorkdir)
				}
			}
		})
	}
}

// TestArenaDockerExecArgs_NeverEmitsNetworkConnect guards the isolation
// rule that grading a task must never grant the arena's containers any new
// network reach: docker exec runs a command inside an already-running
// container, never `docker network connect`.
func TestArenaDockerExecArgs_NeverEmitsNetworkConnect(t *testing.T) {
	for _, subsystem := range []string{"iznik-server-go", "iznik-nuxt3", "iznik-batch"} {
		service, workdir, _ := arenaContainerFor(subsystem)
		args := arenaDockerExecArgs("freegle-eval-arena", service, workdir, "go test ./... && curl http://example.com")
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "network") && strings.Contains(joined, "connect") {
			t.Errorf("subsystem %s: constructed args look like a network connect: %v", subsystem, args)
		}
		if args[0] != "exec" {
			t.Errorf("subsystem %s: docker subcommand = %q, want exec", subsystem, args[0])
		}
	}
}

func TestNewArenaTaskEnv_UnknownSubsystemErrors(t *testing.T) {
	arena := &ArenaConfig{Path: "/arena", Project: "freegle-eval-arena", BaseURL: "http://localhost:1"}
	if _, err := newArenaTaskEnv(arena, "some-unknown-subsystem"); err == nil {
		t.Fatal("expected an error for an unknown subsystem, got nil")
	}
}

func TestNewArenaTaskEnv_KnownSubsystemWiresLane(t *testing.T) {
	arena := &ArenaConfig{Path: "/arena", Project: "freegle-eval-arena", BaseURL: "http://localhost:1"}
	env, err := newArenaTaskEnv(arena, "iznik-server-go")
	if err != nil {
		t.Fatalf("newArenaTaskEnv: %v", err)
	}
	if env.LaneName != "go" {
		t.Errorf("LaneName = %q, want go", env.LaneName)
	}
	if env.ArenaPath != "/arena" {
		t.Errorf("ArenaPath = %q, want /arena", env.ArenaPath)
	}
}

func TestParseLaneFailures(t *testing.T) {
	tests := []struct {
		name string
		lane string
		logs string
		want []string
	}{
		{
			name: "go top-level failure only, subtest excluded",
			lane: "go",
			logs: "=== RUN   TestFoo\n--- FAIL: TestFoo (0.00s)\n    --- FAIL: TestFoo/sub (0.00s)\n--- PASS: TestBar (0.00s)\n",
			want: []string{"TestFoo"},
		},
		{
			name: "go no failures",
			lane: "go",
			logs: "--- PASS: TestBar (0.00s)\nok  \tpkg\t0.010s\n",
			want: nil,
		},
		{
			name: "vitest verbose failures",
			lane: "vitest",
			logs: "  ✓ renders (3ms)\n  × handles click (12ms)\n  ✗ another one\n",
			want: []string{"handles click", "another one"},
		},
		{
			name: "laravel phpunit failure list",
			lane: "laravel",
			logs: "There were 2 failures:\n\n1) Tests\\Unit\\FooTest::testBar\nFailed asserting\n\n2) Tests\\Feature\\BazTest::testQux\nSomething\n",
			want: []string{"Tests\\Unit\\FooTest::testBar", "Tests\\Feature\\BazTest::testQux"},
		},
		{
			name: "unknown lane",
			lane: "spatial",
			logs: "--- FAIL: TestFoo\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseLaneFailures(tt.lane, tt.logs)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseLaneFailures = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ParseLaneFailures[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLaneRegressions(t *testing.T) {
	got := laneRegressions([]string{"TestHeldOut", "TestOther"}, []string{"TestHeldOut"})
	if got != 1 {
		t.Errorf("laneRegressions = %d, want 1", got)
	}
	if got := laneRegressions(nil, []string{"TestHeldOut"}); got != 0 {
		t.Errorf("laneRegressions(nil) = %d, want 0", got)
	}
}

// fakeLaneServer builds an httptest server implementing enough of
// status-nuxt's POST /api/tests/<lane> + GET /api/tests/<lane>/status
// contract to drive LaneRunner: it reports "running" for the first
// runningPolls status checks, then terminates with the given result.
func fakeLaneServer(t *testing.T, runningPolls int, finalStatus string, success bool, logs string) (*httptest.Server, *int32) {
	t.Helper()
	var polls int32
	var started int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			atomic.AddInt32(&started, 1)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"})
		case strings.HasSuffix(r.URL.Path, "/status"):
			n := atomic.AddInt32(&polls, 1)
			resp := laneStatusResponse{Status: "running"}
			if int(n) > runningPolls {
				resp = laneStatusResponse{Status: finalStatus, Success: success, Logs: logs}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &started
}

func TestLaneRunner_Run_PollsUntilCompleteAndReturnsFailureNames(t *testing.T) {
	server, started := fakeLaneServer(t, 2, "completed", false, "--- FAIL: TestBroken\n--- PASS: TestOK\n")
	lane := LaneRunner{BaseURL: server.URL, PollInterval: time.Millisecond}

	result, err := lane.Run(context.Background(), "go", 5*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(started) != 1 {
		t.Errorf("lane started %d times, want 1", atomic.LoadInt32(started))
	}
	if result.Success {
		t.Error("Success = true, want false")
	}
	if len(result.FailureNames) != 1 || result.FailureNames[0] != "TestBroken" {
		t.Errorf("FailureNames = %v, want [TestBroken]", result.FailureNames)
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false")
	}
}

func TestLaneRunner_Run_TimesOut(t *testing.T) {
	server, _ := fakeLaneServer(t, 1000000, "completed", true, "")
	lane := LaneRunner{BaseURL: server.URL, PollInterval: time.Millisecond}

	result, err := lane.Run(context.Background(), "go", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.TimedOut {
		t.Error("TimedOut = false, want true")
	}
}

func TestLaneRunner_Run_TreatsConflictAsAlreadyRunning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)
		case strings.HasSuffix(r.URL.Path, "/status"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(laneStatusResponse{Status: "completed", Success: true})
		}
	}))
	defer server.Close()

	lane := LaneRunner{BaseURL: server.URL, PollInterval: time.Millisecond}
	result, err := lane.Run(context.Background(), "go", 5*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Success {
		t.Error("Success = false, want true (a 409 start should not fail the run)")
	}
}

// TestRunOneArenaTask_FailToPassHappyPath mirrors v1's
// TestRunOneTask_FailToPassHappyPath, using a faked TestExecutor (plain
// CommandRunner, no unshare, no docker) standing in for ArenaRunner: this
// exercises runOneArenaTask's own orchestration (ArenaSandbox lifecycle,
// held-out application, fail-to-pass scoring) end to end. The full-lane
// check is skipped (LaneName "") so it needs no status API fixture.
func TestRunOneArenaTask_FailToPassHappyPath(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, greetRepoFiles())
	arenaPath := newArenaTestWorktree(t, repoPath, commit, true)
	task, testCmd := buildGreetHoldoutTask(t, commit)

	replay := scriptedReplay(t, [][]byte{
		buildAssistantResponse(t, "", toolCallSpec{Name: toolRunTests}),
		buildAssistantResponse(t, "", toolCallSpec{Name: toolEdit, Input: map[string]any{
			"file_path": "greet/greet.go", "old_string": `return "Hello, World!"`, "new_string": `return "Hello, " + name + "!"`,
		}}),
		buildAssistantResponse(t, "", toolCallSpec{Name: toolRunTests}),
		buildAssistantResponse(t, "the tests pass now"),
	})

	cfg := &config.Config{}
	env := ArenaTaskEnv{
		ArenaPath: arenaPath,
		Tests:     CommandRunner{UseUnshare: false, Timeout: 30 * time.Second},
	}
	bounds := TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute}

	outcome := runOneArenaTask(context.Background(), cfg, replay, "model-under-test", task, testCmd, bounds, env)

	if outcome.Error != "" {
		t.Fatalf("unexpected outcome.Error: %s", outcome.Error)
	}
	if outcome.TestsRan != 1 || outcome.TestsPassed != 1 {
		t.Errorf("TestsRan/TestsPassed = %d/%d, want 1/1", outcome.TestsRan, outcome.TestsPassed)
	}
	if !outcome.Passed {
		t.Error("Passed = false, want true")
	}
	if !outcome.AgenticReady {
		t.Error("AgenticReady = false, want true")
	}

	// The arena must be left exactly as it started: the whole point of
	// reusing it across tasks.
	dirty, err := arenaIsDirty(context.Background(), arenaPath)
	if err != nil {
		t.Fatalf("arenaIsDirty: %v", err)
	}
	if dirty {
		t.Error("arena left dirty after a completed task")
	}
	restoredHead, err := gitOutput(context.Background(), arenaPath, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(restoredHead) != commit {
		t.Fatalf("arena HEAD after task = %s, want original %s (err=%v)", strings.TrimSpace(restoredHead), commit, err)
	}
}

func TestRunOneArenaTask_LaneRegressionFailsTheTask(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, greetRepoFiles())
	arenaPath := newArenaTestWorktree(t, repoPath, commit, true)
	task, testCmd := buildGreetHoldoutTask(t, commit)

	replay := scriptedReplay(t, [][]byte{
		buildAssistantResponse(t, "", toolCallSpec{Name: toolEdit, Input: map[string]any{
			"file_path": "greet/greet.go", "old_string": `return "Hello, World!"`, "new_string": `return "Hello, " + name + "!"`,
		}}),
		buildAssistantResponse(t, "done"),
	})

	// The full-lane run reports a failure unrelated to the held-out test:
	// a real regression the scoped grading pass (limited to ./greet/...)
	// would never see.
	server, _ := fakeLaneServer(t, 0, "completed", false, "--- FAIL: TestSomethingElseBroke\n")

	cfg := &config.Config{}
	env := ArenaTaskEnv{
		ArenaPath:   arenaPath,
		Tests:       CommandRunner{UseUnshare: false, Timeout: 30 * time.Second},
		Lane:        LaneRunner{BaseURL: server.URL, PollInterval: time.Millisecond},
		LaneName:    "go",
		LaneTimeout: 5 * time.Second,
	}
	bounds := TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute}

	outcome := runOneArenaTask(context.Background(), cfg, replay, "model-under-test", task, testCmd, bounds, env)

	if outcome.Passed {
		t.Error("Passed = true, want false: the full lane reported a regression")
	}
	if outcome.Regressions == 0 {
		t.Error("Regressions = 0, want > 0")
	}
	if !strings.Contains(outcome.Error, "TestSomethingElseBroke") {
		t.Errorf("Error = %q, want it to name the regressing test", outcome.Error)
	}
}

func TestRunOneArenaTask_LaneTimeoutFailsTheTask(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, greetRepoFiles())
	arenaPath := newArenaTestWorktree(t, repoPath, commit, true)
	task, testCmd := buildGreetHoldoutTask(t, commit)

	replay := scriptedReplay(t, [][]byte{
		buildAssistantResponse(t, "", toolCallSpec{Name: toolEdit, Input: map[string]any{
			"file_path": "greet/greet.go", "old_string": `return "Hello, World!"`, "new_string": `return "Hello, " + name + "!"`,
		}}),
		buildAssistantResponse(t, "done"),
	})

	server, _ := fakeLaneServer(t, 1000000, "completed", true, "")

	cfg := &config.Config{}
	env := ArenaTaskEnv{
		ArenaPath:   arenaPath,
		Tests:       CommandRunner{UseUnshare: false, Timeout: 30 * time.Second},
		Lane:        LaneRunner{BaseURL: server.URL, PollInterval: time.Millisecond},
		LaneName:    "go",
		LaneTimeout: 20 * time.Millisecond,
	}
	bounds := TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute}

	outcome := runOneArenaTask(context.Background(), cfg, replay, "model-under-test", task, testCmd, bounds, env)

	if outcome.Passed {
		t.Error("Passed = true, want false: the lane timed out")
	}
	if !strings.Contains(outcome.Error, "timed out") {
		t.Errorf("Error = %q, want it to mention the lane timing out", outcome.Error)
	}
}

func TestArenaRunner_RunTests_WaitsForFileSyncThenExecs(t *testing.T) {
	var slept time.Duration
	var execedArgs []string
	runner := ArenaRunner{
		Project: "freegle-eval-arena", Service: "apiv2", Workdir: "/app",
		Execer: fakeDockerExecer(func(ctx context.Context, args []string) (string, bool, error) {
			execedArgs = args
			return "ok", true, nil
		}),
		Sleep: func(d time.Duration) { slept = d },
	}

	output, ok, err := runner.RunTests(context.Background(), "/unused", "go test ./...")
	if err != nil || !ok || output != "ok" {
		t.Fatalf("RunTests = (%q, %v, %v)", output, ok, err)
	}
	if slept != arenaFileSyncDelay {
		t.Errorf("slept %v, want the file-sync delay %v", slept, arenaFileSyncDelay)
	}
	if len(execedArgs) == 0 || execedArgs[0] != "exec" {
		t.Errorf("execedArgs = %v, want a docker exec invocation", execedArgs)
	}
}

// fakeDockerExecer adapts a function literal to DockerExecer.
type fakeDockerExecer func(ctx context.Context, args []string) (string, bool, error)

func (f fakeDockerExecer) Exec(ctx context.Context, args []string) (string, bool, error) {
	return f(ctx, args)
}

// serverPort extracts the numeric port an httptest.Server is listening on.
func serverPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing httptest server URL %s: %v", server.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parsing port from httptest server URL %s: %v", server.URL, err)
	}
	return port
}

// unusedPort returns a port number nothing is currently listening on, by
// opening and immediately closing a listener on port 0.
func unusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding an unused port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestArenaWorktreeDiffCapturesEditsAndNewFiles(t *testing.T) {
	dir := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "-q")
	mustGit("config", "user.email", "t@t")
	mustGit("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit("add", "-A")
	mustGit("commit", "-q", "-m", "base")

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := arenaWorktreeDiff(dir)
	if err != nil {
		t.Fatalf("arenaWorktreeDiff: %v", err)
	}
	for _, want := range []string{"-one", "+two", "new.txt", "+brand new"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}
