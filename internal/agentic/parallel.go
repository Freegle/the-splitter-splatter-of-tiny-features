// Parallel arena-mode dispatch: DESIGN.md "Agentic eval v2" arena sittings
// split their scheduled tasks into two pools. The arena pool (a derivable
// test command) executes in the shared arena's own containers and must run
// strictly serially, since the checkout holds exactly one commit at a
// time. The judge pool (no test command: nothing executes) grades the
// candidate's actual working-tree diff instead, so each task can run in
// its own throwaway worktree of the target repo and the whole pool can run
// with bounded concurrency, entirely off the arena. Run drives both pools
// at once, so one sitting's wall time is the max of the two, not the sum.
package agentic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
	"github.com/freegle/splitter/internal/store"
)

// defaultParallelJudgeTasks mirrors config.EvalsConfig.ParallelJudgeTasks's
// documented default, applied when the config value is unset or invalid.
const defaultParallelJudgeTasks = 3

// runState coordinates the bookkeeping shared by every task in one
// eval-agentic run: the ladder (evals.Ladder documents itself as unsafe
// for concurrent use), the run's DB writes, its token budget, and its
// summary tallies. dispatch and complete each take rs.mu, so a run's
// arena pool (runSerialPool) and judge pool (runConcurrentPool) can share
// one runState safely even though they run as two concurrent pools; v1's
// single serial pool uses it the same way, just with no real contention.
type runState struct {
	mu             sync.Mutex
	db             *sql.DB
	runID          int64
	ladder         *evals.Ladder
	maxTokens      int64
	tokensIn       int64
	tokensOut      int64
	budgetExceeded bool
	summary        *RunSummary
}

func newRunState(db *sql.DB, runID int64, ladder *evals.Ladder, maxTokens int64, summary *RunSummary) *runState {
	return &runState{db: db, runID: runID, ladder: ladder, maxTokens: maxTokens, summary: summary}
}

// dispatch reports whether task st should be run: false when the run's
// token budget is already exceeded or the ladder has abandoned st's rung,
// in which case dispatch itself records the ladder_skipped result (the
// only result row a skipped task ever gets) and counts it. Once the budget
// trips, every dispatch call from either pool sees it immediately (the
// mutex both pools share); a task already past this check when the budget
// trips is left to finish, per the run-level "in-flight tasks finish" rule.
func (rs *runState) dispatch(st evals.ScheduledTask) (bool, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.budgetExceeded || !rs.ladder.Allowed(st.Track, st.Rung) {
		if _, err := store.InsertEvalResult(rs.db, store.EvalResultRow{
			EvalRunID: rs.runID, EvalTaskID: st.Task.ID, Mode: "agentic",
			Error: sql.NullString{String: "ladder_skipped", Valid: true},
		}); err != nil {
			return false, fmt.Errorf("recording ladder_skipped result for task %d: %w", st.Task.ID, err)
		}
		rs.summary.TasksSkipped++
		return false, nil
	}
	return true, nil
}

// complete records one task's finished outcome: the agentic_ready update,
// the eval_results row, token accounting against the run's budget, the
// ladder update, and the summary tallies. Safe to call from multiple
// goroutines (runConcurrentPool's workers); each scheduled task must call
// it exactly once, whether it came from the arena pool, the judge pool, or
// v1's single pool.
func (rs *runState) complete(st evals.ScheduledTask, outcome taskOutcome) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	task := st.Task
	if err := store.UpdateEvalTaskAgenticReady(rs.db, task.ID, outcome.AgenticReady); err != nil {
		return fmt.Errorf("updating agentic_ready for task %d: %w", task.ID, err)
	}

	row := store.EvalResultRow{
		EvalRunID: rs.runID, EvalTaskID: task.ID, Mode: "agentic",
		Passed:         sql.NullInt64{Int64: boolToInt64(outcome.Passed), Valid: true},
		Turns:          sql.NullInt64{Int64: int64(outcome.Turns), Valid: true},
		TestsRan:       sql.NullInt64{Int64: int64(outcome.TestsRan), Valid: true},
		TestsPassed:    sql.NullInt64{Int64: int64(outcome.TestsPassed), Valid: true},
		Regressions:    sql.NullInt64{Int64: int64(outcome.Regressions), Valid: true},
		TranscriptZstd: outcome.TranscriptZstd,
		CheatFlags:     sql.NullString{String: encodeCheatFlags(outcome.CheatFlags), Valid: len(outcome.CheatFlags) > 0},
		Error:          sql.NullString{String: outcome.Error, Valid: outcome.Error != ""},
		JudgeVerdict:   sql.NullString{String: outcome.JudgeVerdictJSON, Valid: outcome.JudgeVerdictJSON != ""},
	}
	if _, err := store.InsertEvalResult(rs.db, row); err != nil {
		return fmt.Errorf("recording eval result for task %d: %w", task.ID, err)
	}

	rs.tokensIn += outcome.TokensIn
	rs.tokensOut += outcome.TokensOut
	rs.ladder.Record(st.Track, st.Rung, outcome.Passed)
	rs.summary.TasksScored++
	if outcome.Passed {
		rs.summary.TasksPassed++
	}
	tallyTrack(rs.summary.ByTrack, st.Track, outcome)

	if rs.maxTokens > 0 && rs.tokensIn+rs.tokensOut >= rs.maxTokens {
		rs.budgetExceeded = true
	}
	return nil
}

// runOneFunc runs exactly one scheduled task's sandbox lifecycle and
// returns its outcome. runSerialPool and runConcurrentPool are agnostic to
// which pool (v1's single pool, the arena pool, or the judge pool) it
// drives; only the closure Run builds differs.
type runOneFunc func(ctx context.Context, st evals.ScheduledTask) taskOutcome

// runSerialPool processes tasks one at a time, in schedule order: v1
// mode's entire task set, or arena mode's test-carrying pool, which must
// never overlap because every task in it shares the one arena checkout.
func runSerialPool(ctx context.Context, rs *runState, tasks []evals.ScheduledTask, runOne runOneFunc) error {
	for _, st := range tasks {
		proceed, err := rs.dispatch(st)
		if err != nil {
			return err
		}
		if !proceed {
			continue
		}
		outcome := runOne(ctx, st)
		if err := rs.complete(st, outcome); err != nil {
			return err
		}
	}
	return nil
}

// runConcurrentPool processes tasks with up to parallelism running at
// once: arena mode's judge pool, where each task gets its own ephemeral
// worktree, so nothing requires the serial pool's one-at-a-time guarantee.
// Dispatch order follows the tasks slice (the ladder's climb order);
// completion order does not, and the first hard error from any worker (a
// DB write failure, not a task-level failure, which is always recorded as
// an outcome) stops the pool and is returned once every already-started
// worker has finished.
func runConcurrentPool(ctx context.Context, rs *runState, tasks []evals.ScheduledTask, parallelism int, runOne runOneFunc) error {
	if parallelism < 1 {
		parallelism = 1
	}
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	errs := make(chan error, len(tasks))

	for _, st := range tasks {
		st := st
		sem <- struct{}{}

		proceed, err := rs.dispatch(st)
		if err != nil {
			<-sem
			errs <- err
			break
		}
		if !proceed {
			<-sem
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			outcome := runOne(ctx, st)
			if err := rs.complete(st, outcome); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// partitionArenaJudge splits an arena-mode run's scheduled tasks into the
// arena pool (testCmds holds a non-empty command: it executes in the
// arena's containers, so its tasks must run strictly serially) and the
// judge pool (no entry, or an empty command: nothing executes, so its
// tasks run off the arena, with bounded concurrency). Relative order
// within each pool follows scheduled's own order (the ladder climb order).
func partitionArenaJudge(scheduled []evals.ScheduledTask, testCmds map[int64]string) (arenaPool, judgePool []evals.ScheduledTask) {
	for _, st := range scheduled {
		if testCmds[st.Task.ID] != "" {
			arenaPool = append(arenaPool, st)
		} else {
			judgePool = append(judgePool, st)
		}
	}
	return arenaPool, judgePool
}

// judgeGradedOutcome grades a task with no derivable test command by
// judging the sandbox's actual working-tree diff against the withheld
// reference response: tests are the strongest grader where they exist
// (runOneArenaTask), not the only one (Edward, DECISIONS.md "composite
// arena grading"). Shared by runOneJudgeTask; sandboxDir is whatever
// ephemeral worktree the caller built the tool loop against.
func judgeGradedOutcome(ctx context.Context, cfg *config.Config, model string, task store.EvalTaskRow, sandboxDir string, exec *ToolExecutor, loopResult *LoopResult, transcriptZstd []byte, hc heldOutContext) taskOutcome {
	diff, derr := arenaWorktreeDiff(sandboxDir)
	if derr != nil {
		return taskOutcome{
			Error: "capturing candidate diff: " + derr.Error(), AgenticReady: true,
			Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
			CheatFlags: exec.CheatFlags(), TranscriptZstd: transcriptZstd, ModelRanTests: exec.TestsRanByModel(),
		}
	}

	judgeVerdictJSON := ""
	judgePassed := false
	switch {
	case strings.TrimSpace(diff) == "":
		judgeVerdictJSON = `{"equivalent":false,"confidence":1,"reason":"candidate made no change to the working tree"}`
	default:
		refJSON, rerr := store.Decompress(task.ReferenceResponseZstd)
		if rerr != nil {
			return taskOutcome{Error: "decompressing reference: " + rerr.Error(), AgenticReady: true}
		}
		verdict, jerr := evals.JudgeCandidateChange(ctx, cfg, cfg.Evals.JudgeModel, task.Brief, refJSON, diff)
		if jerr != nil {
			return taskOutcome{
				Error: "judging candidate diff: " + jerr.Error(), AgenticReady: true,
				Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
				CheatFlags: exec.CheatFlags(), TranscriptZstd: transcriptZstd, ModelRanTests: exec.TestsRanByModel(),
			}
		}
		judgePassed = verdict.Agree()
		if vj, merr := json.Marshal(verdict); merr == nil {
			judgeVerdictJSON = string(vj)
		}
	}

	cheatFlags := exec.CheatFlags()
	if flag := detectSuspectCopyForTask(task, cfg, model, sandboxDir, hc); flag != nil {
		cheatFlags = append(cheatFlags, *flag)
	}

	return taskOutcome{
		Passed: judgePassed, Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
		CheatFlags: cheatFlags, Error: loopResult.Error, TranscriptZstd: transcriptZstd, AgenticReady: true,
		ModelRanTests: exec.TestsRanByModel(), JudgeVerdictJSON: judgeVerdictJSON,
	}
}

// runOneJudgeTask runs one judge-pool task (no derivable test command, so
// nothing executes) in its own ephemeral worktree of the target repo (v1's
// Sandbox/Teardown; never the shared arena checkout), so it can run
// concurrently with every other judge-pool task and with the arena pool's
// serial queue without contending for anything. Otherwise identical to
// runOneArenaTask's own judge-diff-grading: the same tool loop,
// subsystem-rooted tools with StripPrefix, working-tree diff capture,
// evals.JudgeCandidateChange grading and cheat detectors. It never returns
// a hard error: any failure is recorded in the returned outcome's Error
// field, matching runOneTask's and runOneArenaTask's convention.
func runOneJudgeTask(ctx context.Context, cfg *config.Config, doReplay evals.ReplayFunc, model string, task store.EvalTaskRow, bounds TaskBounds) taskOutcome {
	sandbox, err := NewSandbox(ctx, cfg.RepoPath, task.RepoHead.String)
	if err != nil {
		return taskOutcome{Error: fmt.Sprintf("creating sandbox: %v", err)}
	}
	defer sandbox.Teardown()

	hc, err := prepareHeldOutContext(task, sandbox.Dir)
	if err != nil {
		return taskOutcome{Error: err.Error(), AgenticReady: true}
	}

	toolRoot := sandbox.Dir
	if task.Subsystem.Valid && task.Subsystem.String != "" {
		toolRoot = filepath.Join(sandbox.Dir, task.Subsystem.String)
	}
	exec := NewToolExecutor(toolRoot, noExecTests{}, "")
	if task.Subsystem.Valid {
		exec.StripPrefix = task.Subsystem.String + "/"
	}
	loopResult := RunLoop(ctx, doReplay, exec, task.Brief, bounds)

	transcriptZstd, cerr := store.Compress(loopResult.TranscriptJSON)
	if cerr != nil {
		transcriptZstd = nil
	}

	return judgeGradedOutcome(ctx, cfg, model, task, sandbox.Dir, exec, loopResult, transcriptZstd, hc)
}
