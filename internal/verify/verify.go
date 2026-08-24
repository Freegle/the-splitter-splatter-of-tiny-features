// Package verify implements Phase 3's verification cascade: given a
// captured frontier response and a replayed local response for the same
// call, decide whether they agree, cheapest check first. It is a reusable
// entry point: internal/replay drives it for every new replay, and the
// eval library (internal/evals) drives the same cascade to score eval
// runs, sharing worktree teardown, lint dispatch and similarity scoring.
package verify

import (
	"context"
	"fmt"
	"strings"

	"github.com/freegle/splitter/internal/config"
)

// Cascade stage names, stored verbatim in verifications.stage. A row
// reaching the middle band (Result.Agree == nil) still stores "ast": it
// was the AST/similarity stage that produced the score, judge arbitration
// (a later, separate command) is what may eventually decide it, at which
// point the stage is updated to "judge".
const (
	StageExact = "exact"
	StageAST   = "ast"
)

// editTurnTypes are the turn_type values whose responses are compared by
// applying file edits in ephemeral worktrees rather than by raw text
// similarity.
var editTurnTypes = map[string]bool{
	"single_file_edit": true,
	"multi_file_edit":  true,
}

// Config configures a Verifier.
type Config struct {
	// Thresholds are the cascade's similarity thresholds (default plus
	// per language/turn_type overrides).
	Thresholds config.ThresholdsConfig
	// Tests maps a subsystem name to an optional shell command run in each
	// comparison worktree. A subsystem with no entry (or the zero value
	// map) never runs a test command.
	Tests map[string]string
	// MaxConcurrentWorktrees bounds how many worktree-based cascades this
	// Verifier runs at once, regardless of how many goroutines call Verify
	// concurrently. Values less than 1 are treated as 1.
	MaxConcurrentWorktrees int
}

// Result is the outcome of running the cascade on one frontier/local
// response pair.
type Result struct {
	// Stage is the cascade stage that produced Similarity: StageExact or
	// StageAST.
	Stage string
	// Similarity is in [0,1]. 1 for an exact-match agreement.
	Similarity float64
	// FrontierLint and LocalLint are JSON (an array of {tool,ok,output}
	// entries, each capped to keep the whole string under 2KB), populated
	// only for edit turns whose worktrees actually ran a linter. Empty
	// when no lint was run.
	FrontierLint string
	LocalLint    string
	// FrontierTests and LocalTests are JSON ({command,ok,output}, capped),
	// populated only when a [tests] command was configured for the edited
	// files' subsystem and actually ran. Empty when no test command ran.
	FrontierTests string
	LocalTests    string
	// Agree is the cascade's decision: true/false when Similarity cleared
	// a threshold outright, nil when it fell in the middle band and the
	// pair should be queued for judge arbitration instead.
	Agree *bool
}

// Verifier runs the verification cascade. One Verifier is safe to share
// across goroutines and across many Verify calls: its worktree semaphore
// bounds concurrent worktree-based cascades regardless of how many callers
// invoke Verify at once.
type Verifier struct {
	cfg Config
	sem semaphore
}

// New builds a Verifier from cfg.
func New(cfg Config) *Verifier {
	return &Verifier{cfg: cfg, sem: newSemaphore(cfg.MaxConcurrentWorktrees)}
}

// Verify runs the cascade comparing frontierMsg (the captured frontier
// response) against localMsg (the replayed local response), both complete
// Anthropic message JSON (the shape internal/anthropic.AssembleSSE and
// internal/backend.FromOpenAI produce), for a call of the given turnType.
// repoPath is the target repository and repoHead the commit the call was
// captured against; both are required to build comparison worktrees for an
// edit turn, everything else ignores them.
func (v *Verifier) Verify(ctx context.Context, repoPath, repoHead string, frontierMsg, localMsg []byte, turnType string) (*Result, error) {
	frontierText, err := concatenatedContent(frontierMsg)
	if err != nil {
		return nil, fmt.Errorf("reading frontier response: %w", err)
	}
	localText, err := concatenatedContent(localMsg)
	if err != nil {
		return nil, fmt.Errorf("reading local response: %w", err)
	}

	normFrontier := normalizeWhitespace(frontierText)
	normLocal := normalizeWhitespace(localText)
	if normFrontier == normLocal {
		agree := true
		return &Result{Stage: StageExact, Similarity: 1, Agree: &agree}, nil
	}

	language := ""
	var res *Result
	if editTurnTypes[turnType] && repoPath != "" && repoHead != "" {
		res, language, err = v.verifyEditTurn(ctx, repoPath, repoHead, frontierMsg, localMsg)
		if err != nil {
			return nil, err
		}
	}
	if res == nil {
		// Not an edit turn, or an edit turn with no repo HEAD recorded at
		// capture time (best-effort fallback): token-level similarity over
		// the same normalised text used for the exact-match check above.
		res = &Result{Stage: StageAST, Similarity: tokenSimilarity(normFrontier, normLocal)}
	}

	high, low := thresholdsFor(v.cfg.Thresholds, language, turnType)
	switch {
	case res.Similarity >= high:
		agree := true
		res.Agree = &agree
	case res.Similarity <= low:
		agree := false
		res.Agree = &agree
	default:
		res.Agree = nil
	}
	return res, nil
}

// verifyEditTurn builds two ephemeral worktrees of repoPath at repoHead,
// applies each side's edits, lints and (optionally) tests each side, and
// scores their similarity. It returns (nil, "", nil), not an error, when
// neither response contains a recognised edit-family tool call: the
// caller falls back to plain token similarity in that case.
func (v *Verifier) verifyEditTurn(ctx context.Context, repoPath, repoHead string, frontierMsg, localMsg []byte) (*Result, string, error) {
	frontierEdits, err := extractFileEdits(frontierMsg)
	if err != nil {
		return nil, "", fmt.Errorf("extracting frontier edits: %w", err)
	}
	localEdits, err := extractFileEdits(localMsg)
	if err != nil {
		return nil, "", fmt.Errorf("extracting local edits: %w", err)
	}
	if len(frontierEdits) == 0 && len(localEdits) == 0 {
		return nil, "", nil
	}

	if err := v.sem.acquire(ctx); err != nil {
		return nil, "", fmt.Errorf("acquiring worktree semaphore: %w", err)
	}
	defer v.sem.release()

	base, err := newWorktreeBase()
	if err != nil {
		return nil, "", err
	}
	frontierDir := joinWorktree(base, "frontier")
	localDir := joinWorktree(base, "local")
	defer teardownWorktrees(repoPath, base, frontierDir, localDir)

	if err := addWorktree(ctx, repoPath, frontierDir, repoHead); err != nil {
		return nil, "", fmt.Errorf("creating frontier worktree: %w", err)
	}
	if err := addWorktree(ctx, repoPath, localDir, repoHead); err != nil {
		return nil, "", fmt.Errorf("creating local worktree: %w", err)
	}

	frontierTouched, frontierFailures := applyFileEdits(frontierDir, frontierEdits, repoPath)
	localTouched, localFailures := applyFileEdits(localDir, localEdits, repoPath)

	frontierLint := append([]lintEntry{}, frontierFailures...)
	for _, rel := range frontierTouched {
		if e := lintFile(ctx, frontierDir, rel); e.Tool != "" {
			frontierLint = append(frontierLint, e)
		}
	}
	localLint := append([]lintEntry{}, localFailures...)
	for _, rel := range localTouched {
		if e := lintFile(ctx, localDir, rel); e.Tool != "" {
			localLint = append(localLint, e)
		}
	}

	similarity := meanSimilarity(ctx, frontierDir, localDir, frontierTouched, localTouched)

	firstPath := firstOf(frontierTouched, localTouched)
	language := languageForPath(firstPath)
	frontierTests, localTests := v.runTests(ctx, subsystemOf(firstPath), frontierDir, localDir)

	return &Result{
		Stage:         StageAST,
		Similarity:    similarity,
		FrontierLint:  encodeLintEntries(frontierLint),
		LocalLint:     encodeLintEntries(localLint),
		FrontierTests: frontierTests,
		LocalTests:    localTests,
	}, language, nil
}

// runTests runs the [tests] command configured for subsystem, if any, in
// each worktree, returning capped JSON for each side. Both are empty when
// no command is configured for subsystem.
func (v *Verifier) runTests(ctx context.Context, subsystem, frontierDir, localDir string) (string, string) {
	if v.cfg.Tests == nil {
		return "", ""
	}
	cmdStr, ok := v.cfg.Tests[subsystem]
	if !ok || strings.TrimSpace(cmdStr) == "" {
		return "", ""
	}
	frontier := runTestCommand(ctx, frontierDir, cmdStr, 0)
	local := runTestCommand(ctx, localDir, cmdStr, 1)
	return encodeTestResult(frontier), encodeTestResult(local)
}

// thresholdsFor returns the high/low similarity thresholds for language
// and turnType: a configured override for "<language>/<turnType>" when one
// exists, else cfg's configured defaults, falling back to 0.9/0.5 (the
// brief's stated defaults) when cfg carries the zero value (both defaults
// unset).
func thresholdsFor(cfg config.ThresholdsConfig, language, turnType string) (high, low float64) {
	high, low = cfg.DefaultHigh, cfg.DefaultLow
	if high == 0 && low == 0 {
		high, low = 0.9, 0.5
	}
	if language != "" {
		if override, ok := cfg.Overrides[language+"/"+turnType]; ok {
			high, low = override.High, override.Low
		}
	}
	return high, low
}
