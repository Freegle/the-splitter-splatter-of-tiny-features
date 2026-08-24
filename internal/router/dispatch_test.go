package router

import "testing"

func TestIsDualDispatchOrdinal_ApproximatesConfiguredPercent(t *testing.T) {
	const total = 100000
	const pct = 5
	hits := 0
	for i := int64(0); i < total; i++ {
		if IsDualDispatchOrdinal(i, pct) {
			hits++
		}
	}
	gotPct := float64(hits) / float64(total) * 100
	if gotPct < 4.5 || gotPct > 5.5 {
		t.Errorf("dual dispatch hit rate = %.2f%%, want close to 5%%", gotPct)
	}
}

func TestIsDualDispatchOrdinal_ZeroOrNegativePctDisables(t *testing.T) {
	for _, ordinal := range []int64{0, 1, 20, 100, 12345} {
		if IsDualDispatchOrdinal(ordinal, 0) {
			t.Errorf("IsDualDispatchOrdinal(%d, 0) = true, want false", ordinal)
		}
		if IsDualDispatchOrdinal(ordinal, -5) {
			t.Errorf("IsDualDispatchOrdinal(%d, -5) = true, want false", ordinal)
		}
	}
}

func TestIsDualDispatchOrdinal_HundredPctSelectsEverything(t *testing.T) {
	for _, ordinal := range []int64{0, 1, 20, 100, 12345} {
		if !IsDualDispatchOrdinal(ordinal, 100) {
			t.Errorf("IsDualDispatchOrdinal(%d, 100) = false, want true", ordinal)
		}
	}
}

// findShadowOrdinal returns the smallest ordinal >= from that
// IsDualDispatchOrdinal selects at pct, the "force ordinal" mechanism live
// router tests use to deterministically hit the shadow branch: the hash is
// pure and deterministic, so a caller can always find one instead of
// needing IsDualDispatchOrdinal to accept an override.
func findShadowOrdinal(t *testing.T, from int64, pct int) int64 {
	t.Helper()
	for ordinal := from; ordinal < from+10000; ordinal++ {
		if IsDualDispatchOrdinal(ordinal, pct) {
			return ordinal
		}
	}
	t.Fatalf("no dual-dispatch ordinal found within 10000 of %d at pct=%d", from, pct)
	return 0
}

func TestFindShadowOrdinal_IsDeterministicAndSelects(t *testing.T) {
	ordinal := findShadowOrdinal(t, 0, 5)
	if !IsDualDispatchOrdinal(ordinal, 5) {
		t.Fatalf("findShadowOrdinal returned %d, which IsDualDispatchOrdinal does not select", ordinal)
	}
}
