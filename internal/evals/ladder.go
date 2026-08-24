package evals

import (
	"math"
	"strconv"

	"github.com/freegle/splitter/internal/config"
)

// wilsonZ is the z-score for a 95% confidence interval, used for both the
// ladder's futility stop and (for consistency) anywhere else this package
// needs a Wilson bound. internal/router is expected to define its own
// router-scoped Wilson helper for router_state (DESIGN.md's min_wilson_lb
// rule); this one is local to the ladder's stop_wilson_upper rule.
const wilsonZ = 1.96

// Rung computes a task's ladder rung, per DESIGN.md: three difficulty
// bands (simple, unknown, challenging) each split into two size bands
// (small: single_file_edit and context under 8KB, or larger/multi-file
// otherwise), giving rungs 1-6 from easiest to hardest. difficulty is ""
// for unknown.
func Rung(difficulty, turnType string, contextBytes int) int {
	small := turnType == "single_file_edit" && contextBytes < 8192
	switch difficulty {
	case DifficultySimple:
		if small {
			return 1
		}
		return 2
	case DifficultyChallenging:
		if small {
			return 5
		}
		return 6
	default:
		if small {
			return 3
		}
		return 4
	}
}

// Track computes a task's ladder track from its language/layer and the
// configured ladder_track mode: "language" (default) or "layer" use that
// dimension directly ("mixed" language is its own track, an empty layer is
// its own track too), "none" collapses every task onto one global track.
func Track(ladderTrack, language, layer string) string {
	switch ladderTrack {
	case "layer":
		return layer
	case "none":
		return ""
	default:
		return language
	}
}

// wilsonBounds returns the two-sided Wilson score interval for passed
// successes out of n trials at confidence z. n == 0 returns the widest
// possible interval [0,1], so a caller gating on the upper bound never
// mistakes "no data yet" for "definitely failing".
func wilsonBounds(passed, n int, z float64) (lower, upper float64) {
	if n <= 0 {
		return 0, 1
	}
	nf := float64(n)
	phat := float64(passed) / nf
	denom := 1 + z*z/nf
	center := phat + z*z/(2*nf)
	margin := z * math.Sqrt(phat*(1-phat)/nf+z*z/(4*nf*nf))
	lower = (center - margin) / denom
	upper = (center + margin) / denom
	if lower < 0 {
		lower = 0
	}
	if upper > 1 {
		upper = 1
	}
	return lower, upper
}

// RungSummary is one track/rung's scored outcome, part of the eval_runs
// ladder JSON.
type RungSummary struct {
	N      int `json:"n"`
	Passed int `json:"passed"`
}

// TrackSummary is one track's ladder outcome for one eval run.
type TrackSummary struct {
	// StopRung is the lowest rung this track abandoned, 0 when the track
	// was never abandoned (every rung with tasks ran to completion).
	StopRung int `json:"stop_rung,omitempty"`
	// Reason names why StopRung was abandoned: "futility" or
	// "wilson_upper", empty when StopRung is 0.
	Reason string `json:"reason,omitempty"`
	// Rungs is keyed by rung number as a decimal string (JSON object keys
	// must be strings).
	Rungs map[string]RungSummary `json:"rungs"`
}

// rungState is one track/rung's running tally as tasks are scored.
type rungState struct {
	n, passed        int
	consecutiveFails int
}

// trackState is one track's ladder state across every rung seen so far.
type trackState struct {
	rungs       map[int]*rungState
	abandonFrom int // 0 = not abandoned; else the lowest abandoned rung
	abandonWhy  string
}

// Ladder tracks per-track, per-rung pass/fail state during one eval run and
// decides, after each scored task, whether a track's climb should stop.
// One Ladder is used for exactly one eval run; it is not safe for
// concurrent use.
type Ladder struct {
	cfg    config.EvalsConfig
	tracks map[string]*trackState
}

// NewLadder builds a Ladder from cfg. Zero-valued fields in cfg (an unset
// [evals] section) fall back to DESIGN.md's stated defaults, so a caller
// that forgets to set them still gets sane futility stopping rather than a
// ladder that never stops or stops after zero tasks.
func NewLadder(cfg config.EvalsConfig) *Ladder {
	if cfg.StopWilsonUpper <= 0 {
		cfg.StopWilsonUpper = 0.2
	}
	if cfg.StopMinN <= 0 {
		cfg.StopMinN = 8
	}
	if cfg.FutilityConsecutiveFails <= 0 {
		cfg.FutilityConsecutiveFails = 6
	}
	return &Ladder{cfg: cfg, tracks: map[string]*trackState{}}
}

func (l *Ladder) track(name string) *trackState {
	ts, ok := l.tracks[name]
	if !ok {
		ts = &trackState{rungs: map[int]*rungState{}}
		l.tracks[name] = ts
	}
	return ts
}

// Allowed reports whether a task at (track, rung) should be attempted: true
// unless that track has already abandoned rung or a lower one.
func (l *Ladder) Allowed(track string, rung int) bool {
	ts := l.track(track)
	return ts.abandonFrom == 0 || rung < ts.abandonFrom
}

// Record updates track's rung tally with one scored task's outcome, then
// re-evaluates the stop conditions for that rung: the Wilson UPPER bound
// dropping below stop_wilson_upper with at least stop_min_n scored, or
// futility_consecutive_fails consecutive zero-pass failures. Either
// abandons this rung and every higher rung in the track (a lower
// already-abandoned rung is never re-abandoned to a higher value).
func (l *Ladder) Record(track string, rung int, passed bool) {
	ts := l.track(track)
	rs, ok := ts.rungs[rung]
	if !ok {
		rs = &rungState{}
		ts.rungs[rung] = rs
	}
	rs.n++
	if passed {
		rs.passed++
		rs.consecutiveFails = 0
	} else {
		rs.consecutiveFails++
	}

	if ts.abandonFrom != 0 && rung >= ts.abandonFrom {
		return
	}

	why := ""
	if rs.consecutiveFails >= l.cfg.FutilityConsecutiveFails {
		why = "futility"
	} else if rs.n >= l.cfg.StopMinN {
		_, upper := wilsonBounds(rs.passed, rs.n, wilsonZ)
		if upper < l.cfg.StopWilsonUpper {
			why = "wilson_upper"
		}
	}
	if why != "" && (ts.abandonFrom == 0 || rung < ts.abandonFrom) {
		ts.abandonFrom = rung
		ts.abandonWhy = why
	}
}

// Summary returns every track's outcome, for eval_runs.ladder.
func (l *Ladder) Summary() map[string]TrackSummary {
	out := make(map[string]TrackSummary, len(l.tracks))
	for name, ts := range l.tracks {
		rungs := make(map[string]RungSummary, len(ts.rungs))
		for rung, rs := range ts.rungs {
			rungs[strconv.Itoa(rung)] = RungSummary{N: rs.n, Passed: rs.passed}
		}
		out[name] = TrackSummary{StopRung: ts.abandonFrom, Reason: ts.abandonWhy, Rungs: rungs}
	}
	return out
}
