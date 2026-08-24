package router

import (
	"math"
	"testing"
)

// TestWilsonLowerBound_KnownValues checks the formula against hand-computed
// reference values at z=1.96 (DESIGN.md's pinned confidence level), each
// derived independently of this package's own implementation.
func TestWilsonLowerBound_KnownValues(t *testing.T) {
	tests := []struct {
		name      string
		agreed, n int
		want      float64
		tolerance float64
	}{
		// n=1, x=1 (single observed success): (1 + z^2/2 - z*sqrt(z^2/4)) /
		// (1+z^2) = 1/(1+3.8416) = 1/4.8416.
		{"n1_x1", 1, 1, 0.20655, 0.001},
		// n=10, x=10 (all agree): numerator collapses to exactly 1 (see
		// DECISIONS.md derivation), giving 1/1.38416.
		{"n10_x10", 10, 10, 0.72246, 0.001},
		// n=100, x=50 (p=0.5): the textbook Wilson score example (Evan
		// Miller, "How Not To Sort By Average Rating").
		{"n100_x50", 50, 100, 0.4038, 0.001},
		// n=0: no evidence, no confidence.
		{"n0", 0, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WilsonLowerBound(tt.agreed, tt.n, WilsonZ95)
			if math.Abs(got-tt.want) > tt.tolerance {
				t.Errorf("WilsonLowerBound(%d, %d, %v) = %v, want %v (+/- %v)", tt.agreed, tt.n, WilsonZ95, got, tt.want, tt.tolerance)
			}
		})
	}
}

func TestWilsonLowerBound_MonotonicInAgreement(t *testing.T) {
	low := WilsonLowerBound(15, 30, WilsonZ95)
	high := WilsonLowerBound(29, 30, WilsonZ95)
	if !(high > low) {
		t.Errorf("WilsonLowerBound should increase with agreement rate: n=30 x=15 -> %v, n=30 x=29 -> %v", low, high)
	}
}

func TestWilsonLowerBound_NeverNegativeOrAboveOne(t *testing.T) {
	for n := 0; n <= 5; n++ {
		for agreed := 0; agreed <= n; agreed++ {
			got := WilsonLowerBound(agreed, n, WilsonZ95)
			if got < 0 || got > 1 {
				t.Errorf("WilsonLowerBound(%d, %d, ...) = %v, want in [0,1]", agreed, n, got)
			}
		}
	}
}

func TestRoutable_Edges(t *testing.T) {
	const minN = 30
	const minLB = 0.9

	tests := []struct {
		name           string
		n              int
		wilsonLB       float64
		disabledReason string
		want           bool
	}{
		{"n below minN, lb high", 29, 0.95, "", false},
		{"n at minN, lb high", 30, 0.95, "", true},
		{"lb just below minLB", 30, 0.899, "", false},
		{"lb exactly at minLB", 30, 0.9, "", true},
		{"lb above minLB", 30, 0.95, "", true},
		{"disabled reason forces false even when n/lb pass", 30, 0.95, "escalation", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Routable(tt.n, tt.wilsonLB, tt.disabledReason, minN, minLB)
			if got != tt.want {
				t.Errorf("Routable(n=%d, lb=%v, reason=%q, minN=%d, minLB=%v) = %v, want %v",
					tt.n, tt.wilsonLB, tt.disabledReason, minN, minLB, got, tt.want)
			}
		})
	}
}
