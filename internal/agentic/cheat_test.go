package agentic

import "testing"

func TestTokenSimilarity(t *testing.T) {
	cases := []struct {
		name    string
		a, b    string
		wantLow float64 // similarity must be >= wantLow
		wantHi  float64 // similarity must be <= wantHi
	}{
		{"identical", "func Greet() string { return \"hi\" }", "func Greet() string { return \"hi\" }", 1, 1},
		{"both empty", "", "", 1, 1},
		{"completely different", "func A() {}", "package unrelated import fmt var x = 1", 0, 0.3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokenSimilarity(c.a, c.b)
			if got < c.wantLow || got > c.wantHi {
				t.Errorf("tokenSimilarity(%q, %q) = %v, want in [%v,%v]", c.a, c.b, got, c.wantLow, c.wantHi)
			}
		})
	}
}

func TestSuspectCopyThreshold_RisesAsPatchesShrink(t *testing.T) {
	tiny := suspectCopyThreshold(2)
	small := suspectCopyThreshold(8)
	normal := suspectCopyThreshold(50)
	if !(tiny > small && small > normal) {
		t.Errorf("thresholds not strictly decreasing with size: tiny=%v small=%v normal=%v", tiny, small, normal)
	}
	if tiny <= 1 {
		t.Errorf("a trivial (<=3 line) patch should be exempt (threshold above 1, unreachable), got %v", tiny)
	}
}

func TestDetectSuspectCopy(t *testing.T) {
	cases := []struct {
		name       string
		similarity float64
		diffLines  int
		wantFlag   bool
	}{
		{"near-verbatim large patch flags", 0.99, 50, true},
		{"below threshold does not flag", 0.5, 50, false},
		{"trivial patch never flags even at 1.0", 1.0, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DetectSuspectCopy(c.similarity, c.diffLines)
			if (got != nil) != c.wantFlag {
				t.Errorf("DetectSuspectCopy(%v, %d) = %+v, want flag=%v", c.similarity, c.diffLines, got, c.wantFlag)
			}
			if got != nil && got.Type != CheatFlagSuspectCopy {
				t.Errorf("flag type = %q, want %q", got.Type, CheatFlagSuspectCopy)
			}
		})
	}
}
