package evals

import (
	"testing"

	"github.com/freegle/splitter/internal/config"
)

func TestLadder_FutilityStopsRungAndHigher(t *testing.T) {
	l := NewLadder(config.EvalsConfig{StopWilsonUpper: 0.2, StopMinN: 100, FutilityConsecutiveFails: 3})

	if !l.Allowed("go", 3) {
		t.Fatal("rung 3 should be allowed before any results are recorded")
	}

	l.Record("go", 3, false)
	l.Record("go", 3, false)
	if !l.Allowed("go", 3) {
		t.Fatal("rung 3 should still be allowed after only 2 consecutive fails (futility threshold is 3)")
	}
	l.Record("go", 3, false)

	if l.Allowed("go", 3) {
		t.Error("rung 3 should be abandoned after 3 consecutive fails")
	}
	if l.Allowed("go", 4) {
		t.Error("rung 4 (higher than the abandoned rung) should also be abandoned")
	}
	if !l.Allowed("go", 2) {
		t.Error("rung 2 (lower than the abandoned rung) should remain allowed")
	}

	summary := l.Summary()
	ts, ok := summary["go"]
	if !ok {
		t.Fatal("expected a summary entry for track \"go\"")
	}
	if ts.StopRung != 3 || ts.Reason != "futility" {
		t.Errorf("TrackSummary = %+v, want StopRung 3, Reason futility", ts)
	}
}

func TestLadder_ConsecutiveFailsResetsOnPass(t *testing.T) {
	l := NewLadder(config.EvalsConfig{StopWilsonUpper: 0.2, StopMinN: 100, FutilityConsecutiveFails: 3})

	l.Record("go", 1, false)
	l.Record("go", 1, false)
	l.Record("go", 1, true) // resets the consecutive-fail counter
	l.Record("go", 1, false)
	l.Record("go", 1, false)

	if !l.Allowed("go", 1) {
		t.Error("rung 1 should still be allowed: no 3 consecutive fails in a row occurred")
	}
}

func TestLadder_WilsonUpperStopsRung(t *testing.T) {
	// FutilityConsecutiveFails is set high enough that 5 fails in a row
	// never triggers it, isolating the Wilson-upper-bound stop condition:
	// 0/5 passes gives a Wilson upper bound of about 0.43, below 0.5.
	l := NewLadder(config.EvalsConfig{StopWilsonUpper: 0.5, StopMinN: 5, FutilityConsecutiveFails: 1000})

	for i := 0; i < 5; i++ {
		l.Record("go", 2, false)
	}

	if l.Allowed("go", 2) {
		t.Error("rung 2 should be abandoned once its Wilson upper bound drops below stop_wilson_upper with n >= stop_min_n")
	}
	summary := l.Summary()
	if summary["go"].Reason != "wilson_upper" {
		t.Errorf("Reason = %q, want wilson_upper", summary["go"].Reason)
	}
}

func TestLadder_TracksAreIndependent(t *testing.T) {
	l := NewLadder(config.EvalsConfig{StopWilsonUpper: 0.2, StopMinN: 100, FutilityConsecutiveFails: 2})

	l.Record("go", 3, false)
	l.Record("go", 3, false)
	if l.Allowed("go", 3) {
		t.Error("go track rung 3 should be abandoned")
	}
	if !l.Allowed("vue", 3) {
		t.Error("vue track should be entirely unaffected by go track's abandonment")
	}
}

func TestTrack(t *testing.T) {
	tests := []struct {
		ladderTrack, language, layer, want string
	}{
		{"language", "go", "backend-api", "go"},
		{"language", "mixed", "backend-api", "mixed"},
		{"layer", "go", "backend-api", "backend-api"},
		{"none", "go", "backend-api", ""},
		{"", "go", "backend-api", "go"}, // unset falls back to language
	}
	for _, tt := range tests {
		if got := Track(tt.ladderTrack, tt.language, tt.layer); got != tt.want {
			t.Errorf("Track(%q, %q, %q) = %q, want %q", tt.ladderTrack, tt.language, tt.layer, got, tt.want)
		}
	}
}

func TestWilsonBounds_NoDataIsWidestInterval(t *testing.T) {
	lower, upper := wilsonBounds(0, 0, wilsonZ)
	if lower != 0 || upper != 1 {
		t.Errorf("wilsonBounds(0,0) = (%v,%v), want (0,1)", lower, upper)
	}
}

func TestWilsonBounds_AllPassedIsNearOne(t *testing.T) {
	_, upper := wilsonBounds(20, 20, wilsonZ)
	if upper < 0.9 {
		t.Errorf("wilsonBounds(20,20) upper = %v, want close to 1", upper)
	}
	lower, _ := wilsonBounds(20, 20, wilsonZ)
	if lower < 0.7 {
		t.Errorf("wilsonBounds(20,20) lower = %v, want a reasonably high lower bound", lower)
	}
}
