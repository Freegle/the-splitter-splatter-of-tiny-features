package router

import "math"

// WilsonZ95 is the z-score DESIGN.md pins for router_state's confidence
// bound ("Wilson lower bound at z=1.96").
const WilsonZ95 = 1.96

// WilsonLowerBound returns the lower bound of the Wilson score confidence
// interval for agreed successes out of n trials, at the given z-score. It
// returns 0 for n <= 0 (no evidence, no confidence).
func WilsonLowerBound(agreed, n int, z float64) float64 {
	if n <= 0 {
		return 0
	}
	phat := float64(agreed) / float64(n)
	nf := float64(n)
	z2 := z * z

	denominator := 1 + z2/nf
	center := phat + z2/(2*nf)
	spread := z * math.Sqrt((phat*(1-phat)+z2/(4*nf))/nf)

	lb := (center - spread) / denominator
	if lb < 0 {
		return 0
	}
	if lb > 1 {
		return 1
	}
	return lb
}

// Routable applies the routability rule: a category is routable when it
// has no disabling reason, its sample size meets minN, and its Wilson
// lower bound meets minWilsonLB. All three conditions use inclusive (>=)
// comparisons, so a value exactly at a threshold counts as passing.
func Routable(n int, wilsonLB float64, disabledReason string, minN int, minWilsonLB float64) bool {
	return disabledReason == "" && n >= minN && wilsonLB >= minWilsonLB
}
