// Package replay implements Phase 3's replay worker: for each logged call
// eligible for replay, send the translated request to a replay backend,
// record the local response, and run the verification cascade
// (internal/verify) against the frontier response that was originally
// captured for the same call.
package replay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/backend"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
	"github.com/freegle/splitter/internal/verify"
)

// staleWorktreeAge is how old a leftover /tmp/splitter-verify-* directory
// must be before a replay run's startup sweep removes it (DESIGN.md: "a
// splitter replay startup sweep that deletes stale ... older than 1h").
const staleWorktreeAge = time.Hour

// Options controls one replay run.
type Options struct {
	// Backend names the replay backend (a key of config.Config.Backends).
	// Empty uses cfg.Replay.Backend.
	Backend string
	// Limit caps how many calls this run replays. Non-positive uses
	// cfg.Replay.BatchSize.
	Limit int
	// Force bypasses the idle gate.
	Force bool
}

// Summary reports what one replay run did.
type Summary struct {
	Backend       string
	Model         string
	CallsSelected int
	RepliesOK     int
	RepliesError  int
	StageExact    int
	StageAST      int
	Agreed        int
	Disagreed     int
	Banded        int
	CascadeErrors int
	// SweptWorktrees lists the stale worktree base directories the
	// startup sweep removed before this run's own replay work began.
	SweptWorktrees []string
}

// Run executes one replay batch: a startup sweep of stale worktrees left
// by a killed prior run, the idle gate (unless opts.Force), call
// selection, per-call translation and backend dispatch (a per-call
// failure is recorded and does not abort the batch), and the verification
// cascade for every newly created replay.
func Run(ctx context.Context, db *sql.DB, cfg *config.Config, opts Options) (*Summary, error) {
	swept, err := verify.Sweep(cfg.RepoPath, staleWorktreeAge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "splitter: replay: worktree sweep: %v\n", err)
	}

	if !opts.Force {
		if err := checkIdleGate(db, cfg.Replay.IdleMinutes); err != nil {
			return nil, err
		}
	}

	backendName := opts.Backend
	if backendName == "" {
		backendName = cfg.Replay.Backend
	}
	bcfg, ok := cfg.Backends[backendName]
	if !ok {
		return nil, fmt.Errorf("unknown replay backend %q", backendName)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = cfg.Replay.BatchSize
	}

	candidates, err := store.SelectReplayCandidates(db, backendName, bcfg.Model, limit)
	if err != nil {
		return nil, fmt.Errorf("selecting replay candidates: %w", err)
	}

	summary := &Summary{
		Backend:        backendName,
		Model:          bcfg.Model,
		CallsSelected:  len(candidates),
		SweptWorktrees: swept,
	}

	client := &backend.Client{BaseURL: bcfg.BaseURL, APIKeyEnv: bcfg.APIKeyEnv, Model: bcfg.Model}
	verifier := verify.New(verify.Config{
		Thresholds:             cfg.Thresholds,
		Tests:                  cfg.Tests,
		MaxConcurrentWorktrees: cfg.Replay.MaxConcurrentWorktrees,
	})

	for _, c := range candidates {
		replayID, localMsg, err := sendReplay(ctx, db, client, backendName, bcfg.Model, c)
		if err != nil {
			summary.RepliesError++
			fmt.Fprintf(os.Stderr, "splitter: replay: call %d: %v\n", c.CallID, err)
			continue
		}
		summary.RepliesOK++

		frontierMsg, err := store.Decompress(c.FrontierResponseZstd)
		if err != nil {
			summary.CascadeErrors++
			fmt.Fprintf(os.Stderr, "splitter: replay: call %d: decompressing frontier response: %v\n", c.CallID, err)
			continue
		}

		if err := runCascade(ctx, db, verifier, cfg.RepoPath, c, replayID, frontierMsg, localMsg, summary); err != nil {
			summary.CascadeErrors++
			fmt.Fprintf(os.Stderr, "splitter: replay: call %d: verification cascade: %v\n", c.CallID, err)
		}
	}

	return summary, nil
}

// checkIdleGate refuses to run when the newest live-proxy call is younger
// than idleMinutes. No proxy calls logged yet always passes.
func checkIdleGate(db *sql.DB, idleMinutes int) error {
	newest, ok, err := store.NewestProxyCallTS(db)
	if err != nil {
		return fmt.Errorf("checking idle gate: %w", err)
	}
	if !ok {
		return nil
	}
	idle := time.Duration(idleMinutes) * time.Minute
	if since := time.Since(newest); since < idle {
		return fmt.Errorf(
			"refusing to run: newest proxy call was %s ago, idle_minutes is %d (use -force to override)",
			since.Round(time.Second), idleMinutes,
		)
	}
	return nil
}

// sendReplay decompresses and translates one candidate's request, sends it
// to the backend, and records a replays row. On success it returns the new
// row's id and the local response's assembled Anthropic message JSON. On
// any failure it has already recorded a replays row with its Error column
// set (per-call error tolerance) and returns a non-nil error purely so the
// caller can count and log it; the caller does not retry or abort the
// batch.
func sendReplay(ctx context.Context, db *sql.DB, client *backend.Client, backendName, model string, c store.ReplayCandidate) (int64, []byte, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	start := time.Now()

	reqJSON, err := store.Decompress(c.RequestZstd)
	if err != nil {
		err = fmt.Errorf("decompressing request: %w", err)
		recordReplayError(db, c.CallID, backendName, model, now, err)
		return 0, nil, err
	}

	var req anthropic.MessagesRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		err = fmt.Errorf("decoding request: %w", err)
		recordReplayError(db, c.CallID, backendName, model, now, err)
		return 0, nil, err
	}

	openaiReq := backend.ToOpenAI(req, model)
	resp, err := client.Complete(ctx, &openaiReq)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		err = fmt.Errorf("backend call: %w", err)
		recordReplayError(db, c.CallID, backendName, model, now, err)
		return 0, nil, err
	}

	localMsg, err := backend.FromOpenAI(*resp)
	if err != nil {
		err = fmt.Errorf("translating response: %w", err)
		recordReplayError(db, c.CallID, backendName, model, now, err)
		return 0, nil, err
	}

	compressed, err := store.Compress(localMsg)
	if err != nil {
		err = fmt.Errorf("compressing response: %w", err)
		recordReplayError(db, c.CallID, backendName, model, now, err)
		return 0, nil, err
	}

	id, err := store.InsertReplay(db, store.ReplayRow{
		CallID:       c.CallID,
		Backend:      backendName,
		Model:        model,
		ResponseZstd: compressed,
		LatencyMs:    latency,
		CreatedTS:    now,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("recording replay: %w", err)
	}
	return id, localMsg, nil
}

// recordReplayError inserts a replays row carrying cause's message and no
// response, so a failed call never gets re-selected by
// store.SelectReplayCandidates (it now has a replay for this
// backend/model) and the failure is visible in the store.
func recordReplayError(db *sql.DB, callID int64, backendName, model, ts string, cause error) {
	if _, err := store.InsertReplay(db, store.ReplayRow{
		CallID:    callID,
		Backend:   backendName,
		Model:     model,
		Error:     cause.Error(),
		CreatedTS: ts,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "splitter: replay: call %d: recording replay error row: %v\n", callID, err)
	}
}

// runCascade runs the verification cascade for one new replay and writes
// its verifications row, plus a queued judge_items row when the cascade
// lands in the middle band.
func runCascade(ctx context.Context, db *sql.DB, verifier *verify.Verifier, repoPath string, c store.ReplayCandidate, replayID int64, frontierMsg, localMsg []byte, summary *Summary) error {
	result, err := verifier.Verify(ctx, repoPath, c.RepoHead, frontierMsg, localMsg, c.TurnType)
	if err != nil {
		return err
	}

	switch result.Stage {
	case verify.StageExact:
		summary.StageExact++
	case verify.StageAST:
		summary.StageAST++
	}

	row := store.VerificationRow{
		ReplayID:      replayID,
		Stage:         result.Stage,
		Similarity:    result.Similarity,
		FrontierLint:  result.FrontierLint,
		LocalLint:     result.LocalLint,
		FrontierTests: result.FrontierTests,
		LocalTests:    result.LocalTests,
		Agree:         result.Agree,
	}
	if result.Agree != nil {
		row.DecidedTS = time.Now().UTC().Format(time.RFC3339)
		if *result.Agree {
			summary.Agreed++
		} else {
			summary.Disagreed++
		}
	} else {
		summary.Banded++
	}

	verificationID, err := store.InsertVerification(db, row)
	if err != nil {
		return fmt.Errorf("recording verification: %w", err)
	}

	if result.Agree == nil {
		if _, err := store.InsertJudgeItem(db, verificationID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("queueing judge item: %w", err)
		}
	}

	return nil
}
