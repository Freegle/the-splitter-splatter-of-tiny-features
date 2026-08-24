// Package evals implements the eval library (DESIGN.md "Eval library"): a
// growing set of tasks pinned to a git commit plus a brief, harvested from
// live capture or seeded from git history, scored against any backend with
// the same verification cascade Phase 3 uses (internal/verify), and climbed
// rung by rung so a weak model stops wasting tokens on tasks far beyond it.
package evals

// Origin values for eval_tasks.origin.
const (
	OriginDisagreement  = "disagreement"
	OriginEscalation    = "escalation"
	OriginErrorFollowup = "error_followup"
	OriginClean         = "clean"
	OriginHistory       = "history"
	OriginManual        = "manual"
)

// Difficulty values for eval_tasks.difficulty. The empty string means
// unknown (stored as SQL NULL): difficulty is set only from mechanical
// evidence, never guessed.
const (
	DifficultyChallenging = "challenging"
	DifficultySimple      = "simple"
)

// Brief source values, recorded in the characteristics JSON's brief_source
// field (DESIGN.md "Brief derivation"). Discourse-sourced briefs are part
// of DESIGN.md but out of scope for this pass (see DECISIONS.md), so
// BriefSourceDiscourse is not produced by this package yet.
const (
	BriefSourceSession           = "session"
	BriefSourceCall              = "call"
	BriefSourceCommitSubject     = "commit_subject"
	BriefSourceReverseEngineered = "reverse_engineered"
	BriefSourceManual            = "manual"
)

// Localization values, recorded in the characteristics JSON: whether the
// task hands the model its touched files (given) or requires the agent to
// find them (discovered).
const (
	LocalizationGiven      = "given"
	LocalizationDiscovered = "discovered"
)

// Reverse-brief state, recorded in characteristics.reverse_brief.status.
const (
	ReverseBriefSubmitted = "submitted"
	ReverseBriefDone      = "done"
	ReverseBriefErrored   = "errored"
)
