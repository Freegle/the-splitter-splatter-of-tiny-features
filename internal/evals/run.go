package evals

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/backend"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
	"github.com/freegle/splitter/internal/verify"
)

// bandStage marks an eval_results row that fell in the verification
// cascade's middle band: DESIGN.md "judge stage is skipped, middle band
// counts as not passed, stage records 'band'" (internal/verify.Result
// itself always reports "ast" for a banded row, per that package's own
// stage-naming decision; eval run overrides it to "band" here since, for
// scoring purposes, there is no fourth cascade stage in verifications, but
// eval_results has no such enum constraint).
const bandStage = "band"

// replayFunc sends req to a backend under test and returns its response as
// a complete Anthropic message JSON plus the call's token usage.
type replayFunc func(ctx context.Context, req anthropic.MessagesRequest) (responseJSON []byte, tokensIn, tokensOut int64, err error)

// RunOptions controls one `eval run` invocation.
type RunOptions struct {
	// Backend names a config.Config.Backends key, or the special value
	// "anthropic" for the native Anthropic Messages API client.
	Backend string
	// Model overrides the backend's configured model. Required when
	// Backend is "anthropic" (there is no default anthropic model).
	Model string
	// MaxTokens hard-caps total tokens_in+tokens_out spend for the run when
	// positive; once reached, every remaining task is recorded
	// ladder_skipped and no further backend calls are made.
	MaxTokens int64
}

// ScorecardEntry is one dimension value's tally within one eval run.
type ScorecardEntry struct {
	N      int
	Passed int
}

// Scorecard groups scored (non-skipped) tasks' pass/fail outcome by every
// characteristics dimension DESIGN.md names: language, layer, nature,
// difficulty, turn_type, framework, spec_clarity, size bucket and cutoff
// segment.
type Scorecard struct {
	ByDimension map[string]map[string]ScorecardEntry
}

func newScorecard() *Scorecard {
	return &Scorecard{ByDimension: map[string]map[string]ScorecardEntry{}}
}

func (s *Scorecard) record(dimension, value string, passed bool) {
	if value == "" {
		value = "-"
	}
	m := s.ByDimension[dimension]
	if m == nil {
		m = map[string]ScorecardEntry{}
		s.ByDimension[dimension] = m
	}
	e := m[value]
	e.N++
	if passed {
		e.Passed++
	}
	m[value] = e
}

// Regression is one task that passed in the comparison prior run but did
// not pass in this one.
type Regression struct {
	TaskID   int64
	ShortSHA string
	Brief    string
}

// RunSummary reports what one `eval run` invocation did.
type RunSummary struct {
	RunID        int64
	Backend      string
	Model        string
	TasksTotal   int
	TasksScored  int
	TasksPassed  int
	TasksSkipped int
	TokensIn     int64
	TokensOut    int64
	Ladder       map[string]TrackSummary
	Scorecard    *Scorecard
	BriefSources map[string]int
	PriorModel   string // "" when there is no prior run to compare against
	Regressions  []Regression
}

// Run replays every active eval task against the named backend/model,
// scores each with internal/verify's reusable cascade (judge stage
// skipped: a middle band counts as not passed, stage "band"), climbs the
// per-track ladder (internal/evals.Ladder), and returns a full summary.
// Writes one eval_runs row up front (so eval_results rows can reference
// it) and one eval_results row per active task (passed=NULL, error=
// "ladder_skipped" for a task the ladder or -max-tokens budget skipped).
func Run(ctx context.Context, db *sql.DB, cfg *config.Config, opts RunOptions) (*RunSummary, error) {
	if opts.Backend == "" {
		return nil, fmt.Errorf("eval run: -backend is required")
	}

	model, doReplay, err := buildRunBackend(cfg, opts)
	if err != nil {
		return nil, err
	}

	tasks, err := store.ActiveEvalTasks(db)
	if err != nil {
		return nil, fmt.Errorf("loading active eval tasks: %w", err)
	}

	runID, err := store.InsertEvalRun(db, time.Now().UTC().Format(time.RFC3339), opts.Backend, model)
	if err != nil {
		return nil, fmt.Errorf("inserting eval run: %w", err)
	}

	// The comparison basis is the most recent run of a different model
	// strictly before this one, found now (using the new row's own id as
	// the exclusive upper bound) so it can never match the row just
	// inserted above.
	priorRun, err := store.MostRecentPriorRunOtherModel(db, runID, model)
	if err != nil {
		return nil, fmt.Errorf("finding prior run for comparison: %w", err)
	}

	verifier := verify.New(verify.Config{
		Thresholds:             cfg.Thresholds,
		Tests:                  cfg.Tests,
		MaxConcurrentWorktrees: cfg.Replay.MaxConcurrentWorktrees,
	})
	ladder := NewLadder(cfg.Evals)
	scorecard := newScorecard()
	briefSources := map[string]int{}

	summary := &RunSummary{RunID: runID, Backend: opts.Backend, Model: model, TasksTotal: len(tasks)}

	scheduled := scheduleTasks(tasks, cfg.Evals.LadderTrack)

	var tokensIn, tokensOut int64
	budgetExceeded := false

	for _, st := range scheduled {
		t := st.task
		c := ParseCharacteristics(t.Characteristics.String)
		if c.BriefSource != "" {
			briefSources[c.BriefSource]++
		}

		if budgetExceeded || !ladder.Allowed(st.track, st.rung) {
			if _, err := store.InsertEvalResult(db, store.EvalResultRow{
				EvalRunID:  runID,
				EvalTaskID: t.ID,
				Error:      sql.NullString{String: "ladder_skipped", Valid: true},
			}); err != nil {
				return nil, fmt.Errorf("recording ladder_skipped result for task %d: %w", t.ID, err)
			}
			summary.TasksSkipped++
			continue
		}

		passed, stage, similarity, resultErr, respCompressed, tin, tout, err := scoreOneTask(ctx, verifier, cfg, doReplay, t)
		if err != nil {
			return nil, fmt.Errorf("scoring task %d: %w", t.ID, err)
		}
		tokensIn += tin
		tokensOut += tout

		if _, err := store.InsertEvalResult(db, store.EvalResultRow{
			EvalRunID:    runID,
			EvalTaskID:   t.ID,
			Passed:       sql.NullInt64{Int64: boolToInt64(passed), Valid: true},
			Stage:        sql.NullString{String: stage, Valid: stage != ""},
			Similarity:   sql.NullFloat64{Float64: similarity, Valid: stage != ""},
			ResponseZstd: respCompressed,
			Error:        sql.NullString{String: resultErr, Valid: resultErr != ""},
		}); err != nil {
			return nil, fmt.Errorf("recording eval result for task %d: %w", t.ID, err)
		}

		ladder.Record(st.track, st.rung, passed)
		summary.TasksScored++
		if passed {
			summary.TasksPassed++
		}
		recordScorecard(scorecard, t, c, model, cfg, passed)

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
	summary.Scorecard = scorecard
	summary.BriefSources = briefSources

	if priorRun != nil {
		summary.PriorModel = priorRun.Model
		regressions, err := computeRegressions(db, priorRun.ID, runID)
		if err != nil {
			return nil, fmt.Errorf("computing regressions: %w", err)
		}
		summary.Regressions = regressions
	}

	return summary, nil
}

// scheduledTask pairs one active task with its ladder track/rung.
type scheduledTask struct {
	task  store.EvalTaskRow
	track string
	rung  int
}

// scheduleTasks computes each task's ladder track/rung and sorts by
// (track, rung, id) ascending, the order eval run climbs.
func scheduleTasks(tasks []store.EvalTaskRow, ladderTrack string) []scheduledTask {
	out := make([]scheduledTask, 0, len(tasks))
	for _, t := range tasks {
		c := ParseCharacteristics(t.Characteristics.String)
		track := Track(ladderTrack, t.Language.String, t.Layer.String)
		rung := Rung(t.Difficulty.String, t.TurnType.String, c.Size.ContextBytes)
		out = append(out, scheduledTask{task: t, track: track, rung: rung})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].track != out[j].track {
			return out[i].track < out[j].track
		}
		if out[i].rung != out[j].rung {
			return out[i].rung < out[j].rung
		}
		return out[i].task.ID < out[j].task.ID
	})
	return out
}

// scoreOneTask replays task against doReplay and scores the result with
// verifier, per the eval run contract: a backend call failure or a
// verification error both count as not passed with the error text
// recorded (there is no "unscored" outcome for an attempted task, only
// ladder_skipped for a task never attempted at all).
func scoreOneTask(ctx context.Context, verifier *verify.Verifier, cfg *config.Config, doReplay replayFunc, t store.EvalTaskRow) (passed bool, stage string, similarity float64, resultErr string, respCompressed []byte, tokensIn, tokensOut int64, err error) {
	reqJSON, derr := store.Decompress(t.RequestZstd)
	if derr != nil {
		return false, "", 0, fmt.Sprintf("decompressing request: %v", derr), nil, 0, 0, nil
	}
	var req anthropic.MessagesRequest
	if jerr := json.Unmarshal(reqJSON, &req); jerr != nil {
		return false, "", 0, fmt.Sprintf("decoding request: %v", jerr), nil, 0, 0, nil
	}

	respJSON, tin, tout, callErr := doReplay(ctx, req)
	if callErr != nil {
		return false, "", 0, fmt.Sprintf("backend call: %v", callErr), nil, tin, tout, nil
	}

	respCompressed, cerr := store.Compress(respJSON)
	if cerr != nil {
		return false, "", 0, "", nil, tin, tout, fmt.Errorf("compressing response: %w", cerr)
	}

	if len(t.ReferenceResponseZstd) == 0 {
		return false, "", 0, "task has no reference response to score against", respCompressed, tin, tout, nil
	}
	refJSON, derr := store.Decompress(t.ReferenceResponseZstd)
	if derr != nil {
		return false, "", 0, fmt.Sprintf("decompressing reference response: %v", derr), respCompressed, tin, tout, nil
	}

	vres, verr := verifier.Verify(ctx, cfg.RepoPath, t.RepoHead.String, refJSON, respJSON, t.TurnType.String)
	if verr != nil {
		return false, "", 0, fmt.Sprintf("verification cascade: %v", verr), respCompressed, tin, tout, nil
	}

	if vres.Agree == nil {
		return false, bandStage, vres.Similarity, "", respCompressed, tin, tout, nil
	}
	return *vres.Agree, vres.Stage, vres.Similarity, "", respCompressed, tin, tout, nil
}

// recordScorecard tallies one scored task's outcome into every dimension
// scorecard's report groups by.
func recordScorecard(sc *Scorecard, t store.EvalTaskRow, c Characteristics, model string, cfg *config.Config, passed bool) {
	sc.record("language", t.Language.String, passed)
	sc.record("layer", t.Layer.String, passed)
	sc.record("nature", t.Nature.String, passed)
	sc.record("difficulty", t.Difficulty.String, passed)
	sc.record("turn_type", t.TurnType.String, passed)
	sc.record("framework", c.Framework, passed)
	sc.record("spec_clarity", c.SpecClarity, passed)
	sc.record("size_bucket", c.Size.SizeBucket(), passed)
	sc.record("cutoff_segment", CutoffSegment(c.TaskDate, model, cfg.ModelCutoffs, cfg.Families), passed)
}

// computeRegressions lists every task that passed in priorRunID but did
// not pass in currentRunID, sorted by task id.
func computeRegressions(db *sql.DB, priorRunID, currentRunID int64) ([]Regression, error) {
	priorResults, err := store.EvalResultsForRun(db, priorRunID)
	if err != nil {
		return nil, fmt.Errorf("loading prior run results: %w", err)
	}
	priorPassed := make(map[int64]bool, len(priorResults))
	for _, r := range priorResults {
		if r.Passed.Valid {
			priorPassed[r.EvalTaskID] = r.Passed.Int64 == 1
		}
	}

	currentResults, err := store.EvalResultsForRun(db, currentRunID)
	if err != nil {
		return nil, fmt.Errorf("loading current run results: %w", err)
	}

	var regressions []Regression
	for _, r := range currentResults {
		if !r.Passed.Valid || r.Passed.Int64 == 1 {
			continue
		}
		wasPassed, existed := priorPassed[r.EvalTaskID]
		if !existed || !wasPassed {
			continue
		}
		task, err := store.GetEvalTask(db, r.EvalTaskID)
		if err != nil {
			return nil, fmt.Errorf("loading regressed task %d: %w", r.EvalTaskID, err)
		}
		c := ParseCharacteristics(task.Characteristics.String)
		regressions = append(regressions, Regression{TaskID: task.ID, ShortSHA: shortSHA(*task, c), Brief: task.Brief})
	}
	sort.Slice(regressions, func(i, j int) bool { return regressions[i].TaskID < regressions[j].TaskID })
	return regressions, nil
}

// buildRunBackend resolves opts into a resolved model name and a
// replayFunc: the special "anthropic" backend name selects the native
// Anthropic Messages API client (DESIGN.md: "for eval runs only, live
// routing never uses it"), any other name looks up cfg.Backends.
func buildRunBackend(cfg *config.Config, opts RunOptions) (model string, doReplay replayFunc, err error) {
	if opts.Backend == "anthropic" {
		if opts.Model == "" {
			return "", nil, fmt.Errorf("-model is required when -backend anthropic")
		}
		model = opts.Model
		client := &backend.AnthropicClient{BaseURL: cfg.Upstream, APIKeyEnv: cfg.Judge.APIKeyEnv, Model: model}
		doReplay = func(ctx context.Context, req anthropic.MessagesRequest) ([]byte, int64, int64, error) {
			msg, err := client.Complete(ctx, req)
			if err != nil {
				return nil, 0, 0, err
			}
			var withUsage struct {
				Usage anthropic.Usage `json:"usage"`
			}
			_ = json.Unmarshal(msg, &withUsage)
			return msg, int64(withUsage.Usage.InputTokens), int64(withUsage.Usage.OutputTokens), nil
		}
		return model, doReplay, nil
	}

	bcfg, ok := cfg.Backends[opts.Backend]
	if !ok {
		return "", nil, fmt.Errorf("unknown backend %q", opts.Backend)
	}
	model = bcfg.Model
	if opts.Model != "" {
		model = opts.Model
	}
	client := &backend.Client{BaseURL: bcfg.BaseURL, APIKeyEnv: bcfg.APIKeyEnv, Model: model}
	doReplay = func(ctx context.Context, req anthropic.MessagesRequest) ([]byte, int64, int64, error) {
		openaiReq := backend.ToOpenAI(req, model)
		resp, err := client.Complete(ctx, &openaiReq)
		if err != nil {
			return nil, 0, 0, err
		}
		msg, err := backend.FromOpenAI(*resp)
		if err != nil {
			return nil, 0, 0, err
		}
		return msg, int64(resp.Usage.PromptTokens), int64(resp.Usage.CompletionTokens), nil
	}
	return model, doReplay, nil
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
