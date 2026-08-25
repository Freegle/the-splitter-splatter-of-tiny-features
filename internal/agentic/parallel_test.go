package agentic

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/backend"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
	"github.com/freegle/splitter/internal/store"
)

// TestPartitionArenaJudge exercises the pure split: a non-empty testCmds
// entry puts a task in the arena pool, an empty or missing entry puts it in
// the judge pool, and each pool preserves the input order.
func TestPartitionArenaJudge(t *testing.T) {
	sched := func(id int64) evals.ScheduledTask {
		return evals.ScheduledTask{Task: store.EvalTaskRow{ID: id}, Track: "go", Rung: 1}
	}
	scheduled := []evals.ScheduledTask{sched(1), sched(2), sched(3), sched(4)}
	testCmds := map[int64]string{
		1: "go test ./...",
		2: "", // explicitly empty: judge pool
		// 3, 4 absent entirely: judge pool
	}

	arenaPool, judgePool := partitionArenaJudge(scheduled, testCmds)

	if len(arenaPool) != 1 || arenaPool[0].Task.ID != 1 {
		t.Fatalf("arenaPool = %+v, want exactly task 1", arenaPool)
	}
	if len(judgePool) != 3 || judgePool[0].Task.ID != 2 || judgePool[1].Task.ID != 3 || judgePool[2].Task.ID != 4 {
		t.Fatalf("judgePool = %+v, want tasks 2,3,4 in order", judgePool)
	}
}

// inFlightTracker counts how many fake task runs are concurrently
// in-flight, the shared counter TestParallelPools_* uses to prove the
// arena pool never overlaps (Overlapped stays false) and the judge pool
// genuinely runs concurrently (Max reaches above 1).
type inFlightTracker struct {
	mu         sync.Mutex
	active     int
	max        int
	overlapped bool
}

func (o *inFlightTracker) enter() {
	o.mu.Lock()
	o.active++
	if o.active > o.max {
		o.max = o.active
	}
	if o.active > 1 {
		o.overlapped = true
	}
	o.mu.Unlock()
}

func (o *inFlightTracker) exit() {
	o.mu.Lock()
	o.active--
	o.mu.Unlock()
}

// fakeTrackedRunOne returns a runOneFunc that records its in-flight span on
// tracker (sleeping delay while "running", to widen the window a
// concurrency bug would need to land in), then reports a fixed outcome:
// enough for testing runSerialPool/runConcurrentPool's own orchestration
// without any real sandbox, git or backend call involved.
func fakeTrackedRunOne(tracker *inFlightTracker, delay time.Duration, tokensIn, tokensOut int64) runOneFunc {
	return func(ctx context.Context, st evals.ScheduledTask) taskOutcome {
		tracker.enter()
		defer tracker.exit()
		time.Sleep(delay)
		return taskOutcome{Passed: true, TokensIn: tokensIn, TokensOut: tokensOut, AgenticReady: true}
	}
}

// parallelTestFixture opens a fresh migrated store and inserts one eval_run
// plus n bare eval_tasks rows (satisfying eval_results' foreign keys),
// returning a ScheduledTask per task, all on the same track/rung: with
// every fake outcome below Passed, the ladder's futility/Wilson stops
// never trigger (n never reaches its default stop_min_n of 8), so the
// ladder cannot confound the budget-cap assertions this fixture supports.
func parallelTestFixture(t *testing.T, n int) (*sql.DB, int64, []evals.ScheduledTask) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "splitter.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	runID, err := store.InsertEvalRun(db, "2026-08-25T00:00:00Z", "fake", "fake-model")
	if err != nil {
		t.Fatalf("InsertEvalRun: %v", err)
	}

	reqZstd, err := store.Compress([]byte(`{}`))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}

	var scheduled []evals.ScheduledTask
	for i := 0; i < n; i++ {
		id, inserted, err := store.InsertEvalTask(db, store.EvalTaskRow{
			CreatedTS: "2026-08-25T00:00:00Z", Brief: "fixture task", Origin: evals.OriginManual, RequestZstd: reqZstd,
		})
		if err != nil {
			t.Fatalf("InsertEvalTask: %v", err)
		}
		if !inserted {
			t.Fatalf("InsertEvalTask: task %d unexpectedly conflicted", i)
		}
		scheduled = append(scheduled, evals.ScheduledTask{Task: store.EvalTaskRow{ID: id}, Track: "t", Rung: 1})
	}
	return db, runID, scheduled
}

// TestParallelPools_ArenaSerialJudgeConcurrentSharedBudget drives the
// arena pool (runSerialPool) and the judge pool (runConcurrentPool)
// concurrently against one shared runState, exactly as Run does in arena
// mode, using fake runOne closures instead of real sandboxes so the
// concurrency properties are deterministic and fast to check:
//   - the arena pool's two tasks never overlap (serialism, required because
//     they would share one arena checkout in the real harness)
//   - the judge pool's four tasks, at parallelism 2, do overlap (real
//     concurrency, not just a bounded-but-still-serial queue)
//   - every task is recorded exactly once, whether scored or skipped
//   - the shared token budget stops dispatch once exceeded, and does so
//     deterministically here: each task's fixed cost equals the budget, so
//     the first admission in either pool trips it for every later one.
func TestParallelPools_ArenaSerialJudgeConcurrentSharedBudget(t *testing.T) {
	db, runID, tasks := parallelTestFixture(t, 6)
	arenaPool, judgePool := tasks[:2], tasks[2:]

	const perTaskCost = int64(1000) // 500 in + 500 out
	const maxTokens = int64(1000)   // one task's cost already trips the budget
	const delay = 30 * time.Millisecond

	arenaTracker := &inFlightTracker{}
	judgeTracker := &inFlightTracker{}
	arenaRunOne := fakeTrackedRunOne(arenaTracker, delay, perTaskCost/2, perTaskCost/2)
	judgeRunOne := fakeTrackedRunOne(judgeTracker, delay, perTaskCost/2, perTaskCost/2)

	ladder := evals.NewLadder(config.EvalsConfig{})
	summary := &RunSummary{ByTrack: map[string]*TrackTally{}}
	rs := newRunState(db, runID, ladder, maxTokens, summary)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := runSerialPool(context.Background(), rs, arenaPool, arenaRunOne); err != nil {
			errs <- err
		}
	}()
	go func() {
		defer wg.Done()
		if err := runConcurrentPool(context.Background(), rs, judgePool, 2, judgeRunOne); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("pool error: %v", err)
		}
	}

	if arenaTracker.overlapped {
		t.Errorf("arena pool tasks overlapped: max in-flight would exceed 1")
	}
	if arenaTracker.max > 1 {
		t.Errorf("arena pool max in-flight = %d, want 1 (strict serialism)", arenaTracker.max)
	}
	if judgeTracker.max <= 1 {
		t.Errorf("judge pool max in-flight = %d, want > 1 (real concurrency)", judgeTracker.max)
	}

	// Deterministic given the fixture's numbers (see the function comment):
	// the first admission in each pool (one arena task, two judge tasks)
	// happens before anything can have completed, so all three run; their
	// own cost alone trips the shared budget, so every later dispatch in
	// either pool is skipped.
	if summary.TasksScored != 3 {
		t.Errorf("TasksScored = %d, want 3", summary.TasksScored)
	}
	if summary.TasksSkipped != 3 {
		t.Errorf("TasksSkipped = %d, want 3", summary.TasksSkipped)
	}
	if rs.tokensIn+rs.tokensOut != 3*perTaskCost {
		t.Errorf("tokens spent = %d, want %d (exactly the 3 completed tasks)", rs.tokensIn+rs.tokensOut, 3*perTaskCost)
	}

	results, err := store.EvalResultsForRun(db, runID)
	if err != nil {
		t.Fatalf("EvalResultsForRun: %v", err)
	}
	if len(results) != 6 {
		t.Fatalf("len(results) = %d, want 6 (every task recorded exactly once)", len(results))
	}
	seen := map[int64]int{}
	scored, skipped := 0, 0
	for _, r := range results {
		seen[r.EvalTaskID]++
		if r.Error.Valid && r.Error.String == "ladder_skipped" {
			skipped++
		} else {
			scored++
		}
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("task %d recorded %d times, want exactly 1", id, count)
		}
	}
	if scored != 3 || skipped != 3 {
		t.Errorf("db rows: scored=%d skipped=%d, want 3/3", scored, skipped)
	}
}

// TestRun_ArenaModeSplitsArenaAndJudgePools is an end-to-end smoke test of
// Run's own arena/judge wiring: a real target repo, a real (but
// container-less, so no Docker is needed) arena worktree, and a fake
// OpenAI-compatible backend that never edits anything, so every judge-pool
// task's working-tree diff is empty and gets graded without a judge call.
// It exercises the real sandbox lifecycles for both pools (NewArenaSandbox
// for the arena task, NewSandbox for the judge tasks) and asserts teardown
// leaves no stray worktrees behind either the arena or the target repo.
func TestRun_ArenaModeSplitsArenaAndJudgePools(t *testing.T) {
	// .gitignore the .env file writeArenaEnv creates below: the real
	// eval-arena worktree's .env is untracked the same way, so this
	// mirrors production rather than leaving the fixture permanently
	// "dirty" from a file the arena's own lifecycle never touches.
	files := greetRepoFiles()
	files[".gitignore"] = ".env\n"
	repoPath, commit := newAgenticTestRepo(t, files)
	arenaPath := newArenaTestWorktree(t, repoPath, commit, true)
	writeArenaEnv(t, arenaPath, "splitter-parallel-test-arena")

	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer statusServer.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "splitter.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	holdout := evals.HoldoutPayload{
		Files:   []evals.HoldoutFile{{Path: "greet/greet_test.go", IsNew: true, Content: greetHeldOutTestContent}},
		TestCmd: "go test -json ./greet/...",
	}
	holdoutJSON, err := json.Marshal(holdout)
	if err != nil {
		t.Fatalf("marshal holdout: %v", err)
	}
	holdoutZstd, err := store.Compress(holdoutJSON)
	if err != nil {
		t.Fatalf("compress holdout: %v", err)
	}
	reqZstd, err := store.Compress([]byte(`{}`))
	if err != nil {
		t.Fatalf("compress request: %v", err)
	}

	// One arena-pool task: a holdout test command, subsystem left empty so
	// newArenaTaskEnv falls back to noExecTests and no real Docker is
	// touched (arena_test.go's existing runOneArenaTask tests use the same
	// trick, just via a hand-built ArenaTaskEnv rather than the container
	// fallback).
	if _, _, err := store.InsertEvalTask(db, store.EvalTaskRow{
		CreatedTS: time.Now().UTC().Format(time.RFC3339), RepoHead: sql.NullString{String: commit, Valid: true},
		Brief: "arena task: fix Greet", Origin: evals.OriginHistory, RequestZstd: reqZstd, HoldoutTestsZstd: holdoutZstd,
	}); err != nil {
		t.Fatalf("InsertEvalTask (arena): %v", err)
	}
	// Two judge-pool tasks: no holdout, so no test command, so they never
	// touch the arena at all.
	for i := 0; i < 2; i++ {
		if _, _, err := store.InsertEvalTask(db, store.EvalTaskRow{
			CreatedTS: time.Now().UTC().Format(time.RFC3339), RepoHead: sql.NullString{String: commit, Valid: true},
			Brief: "judge task: no-op review", Origin: evals.OriginClean, RequestZstd: reqZstd,
		}); err != nil {
			t.Fatalf("InsertEvalTask (judge %d): %v", i, err)
		}
	}

	// The model never edits anything: the arena task's held-out test stays
	// failing (graded false, which is fine, this test checks wiring, not
	// per-task pass/fail), and both judge tasks' working-tree diffs are
	// empty, so judgeGradedOutcome short-circuits without any judge call.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backend.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding chat request: %v", err)
		}
		resp := backend.ChatResponse{
			ID:      "chatcmpl-1",
			Choices: []backend.ChatChoice{{Message: backend.ChatMessage{Role: "assistant", Content: "no changes needed"}, FinishReason: "stop"}},
			Usage:   backend.ChatUsage{PromptTokens: 5, CompletionTokens: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encoding fake chat response: %v", err)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.RepoPath = repoPath
	cfg.Evals.ArenaPath = arenaPath
	cfg.Evals.ArenaStatusPort = serverPort(t, statusServer)
	cfg.Evals.ParallelJudgeTasks = 2
	cfg.Backends = map[string]config.BackendConfig{"fake": {BaseURL: server.URL, Model: "fake-model"}}

	summary, err := Run(context.Background(), db, cfg, RunOptions{Backend: "fake", Arena: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if summary.TasksTotal != 3 {
		t.Fatalf("TasksTotal = %d, want 3", summary.TasksTotal)
	}
	if summary.TasksScored != 3 {
		t.Fatalf("TasksScored = %d, want 3 (no budget cap set)", summary.TasksScored)
	}

	results, err := store.EvalResultsForRun(db, summary.RunID)
	if err != nil {
		t.Fatalf("EvalResultsForRun: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	seen := map[int64]bool{}
	for _, r := range results {
		if seen[r.EvalTaskID] {
			t.Fatalf("task %d recorded more than once", r.EvalTaskID)
		}
		seen[r.EvalTaskID] = true
		if r.Mode != "agentic" {
			t.Errorf("result mode = %q, want agentic", r.Mode)
		}
	}

	// The arena worktree must be left exactly as it started.
	dirty, err := arenaIsDirty(context.Background(), arenaPath)
	if err != nil {
		t.Fatalf("arenaIsDirty: %v", err)
	}
	if dirty {
		statusOut := runGit(t, arenaPath, "status", "--porcelain")
		t.Errorf("arena left dirty after the run: %q", statusOut)
	}
	restoredHead, err := gitOutput(context.Background(), arenaPath, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(restoredHead) != commit {
		t.Fatalf("arena HEAD after run = %s, want original %s (err=%v)", strings.TrimSpace(restoredHead), commit, err)
	}

	// Teardown leaves no stray worktrees: the target repo should show only
	// its own main checkout plus the (still-registered) arena worktree.
	out := runGit(t, repoPath, "worktree", "list", "--porcelain")
	count := strings.Count(out, "worktree ")
	if count != 2 {
		t.Fatalf("worktree list after run has %d entries, want 2 (main + arena):\n%s", count, out)
	}
}
