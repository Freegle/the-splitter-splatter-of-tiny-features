package agentic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
	"github.com/freegle/splitter/internal/store"
)

// gradingTimeout bounds one baseline/final/model-triggered run_tests
// invocation.
const gradingTimeout = 10 * time.Minute

// sandboxSweepAge is how old a leftover sandbox directory must be before
// Run's startup sweep removes it, matching internal/verify's own sweep age.
const sandboxSweepAge = time.Hour

// RunOptions controls one `eval-agentic` invocation.
type RunOptions struct {
	// Backend names a config.Config.Backends key, or "anthropic" for the
	// native Anthropic Messages API client (see evals.BuildRunBackend).
	Backend string
	// Model overrides the backend's configured model.
	Model string
	// Limit caps how many agentic-gradable tasks this run attempts, applied
	// after ladder scheduling (so the easiest tasks run first). 0 = no cap.
	Limit int
	// AllowNetwork skips unshare -rn network denial and marks every result
	// untrusted (CheatFlagAllowNetwork), per DESIGN.md: for debugging, or
	// as the only way to proceed when unshare is unavailable.
	AllowNetwork bool
	// MaxTokens hard-caps total tokens_in+tokens_out spend for the run.
	MaxTokens int64
	// Arena selects arena mode (DESIGN.md "Agentic eval v2: the real
	// loop"): the tool loop and grading run against a live FreegleDocker
	// worktree's real Docker containers via [evals].arena_path /
	// arena_status_port, instead of v1's ephemeral, unshare-network-denied
	// worktree sandbox.
	Arena bool
}

// TrackTally is one ladder track's agentic outcome tally, the scorecard
// eval-agentic prints (DESIGN.md task brief: "tests_ran/tests_passed/
// regressions/cheat-flag counts by track").
type TrackTally struct {
	Tasks        int
	Passed       int
	TestsRan     int
	TestsPassed  int
	Regressions  int
	CheatFlagged int
	// ModelRanTests counts tasks where the model itself called run_tests
	// at least once during the loop: Edward's "ran the tests at all"
	// capability, tracked separately from the harness's own grading passes
	// (TestsRan/TestsPassed), which run regardless of the model's own
	// behaviour.
	ModelRanTests int
}

// RunSummary reports what one eval-agentic invocation did.
type RunSummary struct {
	RunID                     int64
	Backend, Model            string
	TasksTotal, TasksScored   int
	TasksPassed, TasksSkipped int
	TokensIn, TokensOut       int64
	Ladder                    map[string]evals.TrackSummary
	ByTrack                   map[string]*TrackTally
	SweptSandboxes            []string
	NotGradedNoTestCommand    int
	AllowNetwork              bool
	Arena                     bool
}

// Run selects every active, agentic-gradable eval task (a history-origin
// task with a Go-derived held-out test command, or a harvested live task
// whose subsystem has a configured [tests] command), climbs the shared
// ladder (evals.ScheduleTasks/NewLadder) driving each through the bounded
// tool loop in a fresh sandbox, and scores it fail-to-pass.
func Run(ctx context.Context, db *sql.DB, cfg *config.Config, opts RunOptions) (*RunSummary, error) {
	if opts.Backend == "" {
		return nil, fmt.Errorf("eval-agentic: -backend is required")
	}

	var arena *ArenaConfig
	useUnshare := !opts.AllowNetwork
	var swept []string
	if opts.Arena {
		var err error
		arena, err = ResolveArenaConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("eval-agentic -arena: %w", err)
		}
	} else {
		if useUnshare && !UnshareAvailable() {
			return nil, fmt.Errorf("unshare -rn is unavailable on this machine; pass -allow-network to run anyway (marks every result untrusted)")
		}
		var err error
		swept, err = Sweep(cfg.RepoPath, sandboxSweepAge)
		if err != nil {
			return nil, fmt.Errorf("sweeping stale sandboxes: %w", err)
		}
	}

	model, doReplay, err := evals.BuildRunBackend(cfg, evals.RunOptions{Backend: opts.Backend, Model: opts.Model})
	if err != nil {
		return nil, err
	}

	tasks, testCmds, notGraded, err := selectAgenticTasks(db, cfg)
	if err != nil {
		return nil, fmt.Errorf("selecting agentic-gradable tasks: %w", err)
	}

	scheduled := evals.ScheduleTasks(tasks, cfg.Evals.LadderTrack)
	if opts.Limit > 0 && len(scheduled) > opts.Limit {
		scheduled = scheduled[:opts.Limit]
	}

	runID, err := store.InsertEvalRun(db, time.Now().UTC().Format(time.RFC3339), opts.Backend, model)
	if err != nil {
		return nil, fmt.Errorf("inserting eval run: %w", err)
	}

	ladder := evals.NewLadder(cfg.Evals)
	bounds := BoundsFromConfig(cfg.Evals)
	runner := CommandRunner{UseUnshare: useUnshare, Timeout: gradingTimeout}

	summary := &RunSummary{
		RunID: runID, Backend: opts.Backend, Model: model, TasksTotal: len(scheduled),
		Ladder: nil, ByTrack: map[string]*TrackTally{}, SweptSandboxes: swept,
		NotGradedNoTestCommand: notGraded, AllowNetwork: opts.AllowNetwork, Arena: opts.Arena,
	}

	var tokensIn, tokensOut int64
	budgetExceeded := false

	for _, st := range scheduled {
		task := st.Task

		if budgetExceeded || !ladder.Allowed(st.Track, st.Rung) {
			if _, err := store.InsertEvalResult(db, store.EvalResultRow{
				EvalRunID: runID, EvalTaskID: task.ID, Mode: "agentic",
				Error: sql.NullString{String: "ladder_skipped", Valid: true},
			}); err != nil {
				return nil, fmt.Errorf("recording ladder_skipped result for task %d: %w", task.ID, err)
			}
			summary.TasksSkipped++
			continue
		}

		var outcome taskOutcome
		if opts.Arena {
			env, envErr := newArenaTaskEnv(arena, task.Subsystem.String)
			if envErr != nil {
				outcome = taskOutcome{Error: envErr.Error()}
			} else {
				outcome = runOneArenaTask(ctx, cfg, doReplay, model, task, testCmds[task.ID], bounds, env)
			}
		} else {
			outcome = runOneTask(ctx, cfg, doReplay, useUnshare, runner, model, task, testCmds[task.ID], bounds, opts.AllowNetwork)
		}

		if err := store.UpdateEvalTaskAgenticReady(db, task.ID, outcome.AgenticReady); err != nil {
			return nil, fmt.Errorf("updating agentic_ready for task %d: %w", task.ID, err)
		}

		row := store.EvalResultRow{
			EvalRunID: runID, EvalTaskID: task.ID, Mode: "agentic",
			Passed:         sql.NullInt64{Int64: boolToInt64(outcome.Passed), Valid: true},
			Turns:          sql.NullInt64{Int64: int64(outcome.Turns), Valid: true},
			TestsRan:       sql.NullInt64{Int64: int64(outcome.TestsRan), Valid: true},
			TestsPassed:    sql.NullInt64{Int64: int64(outcome.TestsPassed), Valid: true},
			Regressions:    sql.NullInt64{Int64: int64(outcome.Regressions), Valid: true},
			TranscriptZstd: outcome.TranscriptZstd,
			CheatFlags:     sql.NullString{String: encodeCheatFlags(outcome.CheatFlags), Valid: len(outcome.CheatFlags) > 0},
			Error:          sql.NullString{String: outcome.Error, Valid: outcome.Error != ""},
		}
		if _, err := store.InsertEvalResult(db, row); err != nil {
			return nil, fmt.Errorf("recording eval result for task %d: %w", task.ID, err)
		}

		tokensIn += outcome.TokensIn
		tokensOut += outcome.TokensOut
		ladder.Record(st.Track, st.Rung, outcome.Passed)
		summary.TasksScored++
		if outcome.Passed {
			summary.TasksPassed++
		}
		tallyTrack(summary.ByTrack, st.Track, outcome)

		if opts.MaxTokens > 0 && tokensIn+tokensOut >= opts.MaxTokens {
			budgetExceeded = true
		}
	}

	ladderSummary := ladder.Summary()
	ladderJSON, err := json.Marshal(ladderSummary)
	if err != nil {
		return nil, fmt.Errorf("marshaling ladder summary: %w", err)
	}
	if err := store.UpdateEvalRunSummary(db, runID, summary.TasksScored, summary.TasksPassed, string(ladderJSON), tokensIn, tokensOut); err != nil {
		return nil, fmt.Errorf("updating eval run summary: %w", err)
	}

	summary.TokensIn = tokensIn
	summary.TokensOut = tokensOut
	summary.Ladder = ladderSummary
	return summary, nil
}

func tallyTrack(byTrack map[string]*TrackTally, track string, outcome taskOutcome) {
	tt, ok := byTrack[track]
	if !ok {
		tt = &TrackTally{}
		byTrack[track] = tt
	}
	tt.Tasks++
	if outcome.Passed {
		tt.Passed++
	}
	tt.TestsRan += outcome.TestsRan
	tt.TestsPassed += outcome.TestsPassed
	tt.Regressions += outcome.Regressions
	if len(outcome.CheatFlags) > 0 {
		tt.CheatFlagged++
	}
	if outcome.ModelRanTests > 0 {
		tt.ModelRanTests++
	}
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// selectAgenticTasks returns every active task eligible for agentic
// grading, plus the shell command to grade each with, keyed by task id.
// notGraded counts active history tasks that carry a holdout payload with
// no derivable test command (non-Go held-out tests, see DECISIONS.md):
// stored for future use but not selected here.
func selectAgenticTasks(db *sql.DB, cfg *config.Config) (tasks []store.EvalTaskRow, testCmds map[int64]string, notGraded int, err error) {
	testCmds = map[int64]string{}

	holdoutTasks, err := store.AgenticGradableEvalTasks(db)
	if err != nil {
		return nil, nil, 0, err
	}
	seen := map[int64]bool{}
	for _, t := range holdoutTasks {
		holdoutJSON, derr := store.Decompress(t.HoldoutTestsZstd)
		if derr != nil {
			continue
		}
		payload, derr := evals.DecodeHoldoutPayload(holdoutJSON)
		if derr != nil {
			continue
		}
		if payload.TestCmd == "" {
			notGraded++
			continue
		}
		testCmds[t.ID] = payload.TestCmd
		tasks = append(tasks, t)
		seen[t.ID] = true
	}

	active, err := store.ActiveEvalTasks(db)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, t := range active {
		if seen[t.ID] || len(t.HoldoutTestsZstd) > 0 {
			continue
		}
		if !t.Subsystem.Valid || t.Subsystem.String == "" {
			continue
		}
		cmd, ok := cfg.Tests[t.Subsystem.String]
		if !ok || strings.TrimSpace(cmd) == "" {
			continue
		}
		testCmds[t.ID] = cmd
		tasks = append(tasks, t)
	}

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks, testCmds, notGraded, nil
}

// taskOutcome is one task's fully graded agentic run.
type taskOutcome struct {
	Passed                bool
	TestsRan, TestsPassed int
	Regressions           int
	Turns                 int
	TokensIn, TokensOut   int64
	CheatFlags            []CheatFlag
	Error                 string
	TranscriptZstd        []byte
	AgenticReady          bool
	// ModelRanTests is how many times the model itself invoked run_tests
	// during the loop (ToolExecutor.TestsRanByModel), independent of the
	// harness's own baseline/final grading passes.
	ModelRanTests int
}

// heldOutContext is what a history-origin task's held-out payload
// contributes to grading: the test names fail-to-pass grading targets, the
// paths to exclude from suspect_copy comparison, and (when a reference
// response is stored) the withheld reference content suspect_copy needs,
// snapshotted from sandboxDir before the model's loop runs. The zero value
// (Names/ExcludeTestPaths/RefMsgJSON all empty/nil) is what a non-history
// task gets: every caller treats it as "nothing to hold out or compare".
type heldOutContext struct {
	Names            []string
	ExcludeTestPaths map[string]bool
	RefMsgJSON       []byte
	ParentSnapshot   map[string]string
	DiffLines        int
}

// prepareHeldOutContext applies task's held-out test files onto sandboxDir
// and builds the suspect_copy comparison basis, shared by v1's runOneTask
// and arena mode's runOneArenaTask. It is a no-op returning the zero
// heldOutContext for a task with no holdout payload.
func prepareHeldOutContext(task store.EvalTaskRow, sandboxDir string) (heldOutContext, error) {
	if len(task.HoldoutTestsZstd) == 0 {
		return heldOutContext{}, nil
	}

	holdoutJSON, err := store.Decompress(task.HoldoutTestsZstd)
	if err != nil {
		return heldOutContext{}, fmt.Errorf("decompressing holdout payload: %w", err)
	}
	payload, err := evals.DecodeHoldoutPayload(holdoutJSON)
	if err != nil {
		return heldOutContext{}, fmt.Errorf("decoding holdout payload: %w", err)
	}

	hc := heldOutContext{ExcludeTestPaths: map[string]bool{}}
	for _, f := range payload.Files {
		if err := applyHoldoutFile(sandboxDir, f); err != nil {
			return heldOutContext{}, fmt.Errorf("applying held-out tests: %w", err)
		}
		hc.ExcludeTestPaths[f.Path] = true
	}
	hc.Names = HeldOutTestNames(payload)

	if refJSON, derr := store.Decompress(task.ReferenceResponseZstd); derr == nil {
		hc.RefMsgJSON = refJSON
		var refMsg struct {
			Content []anthropic.ContentBlock `json:"content"`
		}
		if json.Unmarshal(refJSON, &refMsg) == nil {
			order, _ := referenceEditsByPath(refMsg.Content, hc.ExcludeTestPaths)
			hc.ParentSnapshot = snapshotReferenceFiles(sandboxDir, order)
		}
	}
	c := evals.ParseCharacteristics(task.Characteristics.String)
	hc.DiffLines = c.Size.DiffLines
	return hc, nil
}

// detectSuspectCopyForTask runs the suspect_copy detector for a
// history-origin task (nil for any other task, or one with no withheld
// reference edits, or one whose task_date is not trusted post-cutoff for
// model: DESIGN.md "Leakage containment" only trusts this detector against
// tasks a model could not have memorised).
func detectSuspectCopyForTask(task store.EvalTaskRow, cfg *config.Config, model, sandboxDir string, hc heldOutContext) *CheatFlag {
	if hc.RefMsgJSON == nil {
		return nil
	}
	c := evals.ParseCharacteristics(task.Characteristics.String)
	if evals.CutoffSegment(c.TaskDate, model, cfg.ModelCutoffs, cfg.Families) != evals.SegmentPostCutoff {
		return nil
	}
	return EvaluateSuspectCopy(sandboxDir, hc.RefMsgJSON, hc.ExcludeTestPaths, hc.ParentSnapshot, hc.DiffLines)
}

// runOneTask runs one task's full sandbox lifecycle: create, prep deps,
// apply held-out tests (history origin), grade baseline, run the tool
// loop, grade final, score, detect cheating, tear down. It never returns a
// hard error: any failure is recorded in the returned outcome's Error
// field, matching internal/evals.Run's "one task's failure never aborts
// the batch" convention.
func runOneTask(ctx context.Context, cfg *config.Config, doReplay evals.ReplayFunc, useUnshare bool, runner CommandRunner, model string, task store.EvalTaskRow, testCmd string, bounds TaskBounds, allowNetwork bool) taskOutcome {
	if testCmd == "" {
		return taskOutcome{Error: "no test command available for this task"}
	}

	sandbox, err := NewSandbox(ctx, cfg.RepoPath, task.RepoHead.String)
	if err != nil {
		return taskOutcome{Error: fmt.Sprintf("creating sandbox: %v", err)}
	}
	defer sandbox.Teardown()

	ready, prepDetail := PrepDependencies(ctx, sandbox.Dir, task.Subsystem.String)
	if !ready {
		return taskOutcome{Error: "sandbox dependency prep failed: " + prepDetail, AgenticReady: false}
	}

	hc, err := prepareHeldOutContext(task, sandbox.Dir)
	if err != nil {
		return taskOutcome{Error: err.Error(), AgenticReady: true}
	}

	baseline, err := RunGrading(ctx, runner, sandbox.Dir, testCmd)
	if err != nil {
		return taskOutcome{Error: "baseline grading: " + err.Error(), AgenticReady: true}
	}

	exec := NewToolExecutor(sandbox.Dir, runner, testCmd)
	loopResult := RunLoop(ctx, doReplay, exec, task.Brief, bounds)

	transcriptZstd, cerr := store.Compress(loopResult.TranscriptJSON)
	if cerr != nil {
		transcriptZstd = nil
	}

	final, err := RunGrading(ctx, runner, sandbox.Dir, testCmd)
	if err != nil {
		return taskOutcome{
			Error: "final grading: " + err.Error(), AgenticReady: true,
			Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
			CheatFlags: exec.CheatFlags(), TranscriptZstd: transcriptZstd, ModelRanTests: exec.TestsRanByModel(),
		}
	}

	testsRan, testsPassed, regressions := ScoreFailToPass(baseline, final, hc.Names)

	cheatFlags := exec.CheatFlags()
	if flag := detectSuspectCopyForTask(task, cfg, model, sandbox.Dir, hc); flag != nil {
		cheatFlags = append(cheatFlags, *flag)
	}
	if allowNetwork {
		cheatFlags = append(cheatFlags, CheatFlag{Type: CheatFlagAllowNetwork, Detail: "network denial bypassed via -allow-network"})
	}

	errText := loopResult.Error
	passed := !loopResult.bounded() && testsRan > 0 && testsPassed == testsRan && regressions == 0

	return taskOutcome{
		Passed: passed, TestsRan: testsRan, TestsPassed: testsPassed, Regressions: regressions,
		Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
		CheatFlags: cheatFlags, Error: errText, TranscriptZstd: transcriptZstd, AgenticReady: true,
		ModelRanTests: exec.TestsRanByModel(),
	}
}

// applyHoldoutFile writes one held-out test file's post-commit state onto
// the sandbox: full content for a new file, sequential old_string ->
// new_string hunk replacement over the existing (parent-tree) content
// otherwise. Mirrors internal/verify's single-occurrence Edit semantics.
func applyHoldoutFile(sandboxDir string, f evals.HoldoutFile) error {
	full := filepath.Join(sandboxDir, f.Path)
	if f.IsNew {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("creating parent directory for %s: %w", f.Path, err)
		}
		return os.WriteFile(full, []byte(f.Content), 0o644)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("reading %s: %w", f.Path, err)
	}
	content := string(data)
	for _, h := range f.Hunks {
		if !strings.Contains(content, h.Old) {
			return fmt.Errorf("%s: old_string not found while applying held-out test hunk", f.Path)
		}
		content = strings.Replace(content, h.Old, h.New, 1)
	}
	return os.WriteFile(full, []byte(content), 0o644)
}
