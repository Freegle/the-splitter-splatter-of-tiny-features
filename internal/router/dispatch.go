package router

// dualDispatchHashConstant is Knuth's 64-bit multiplicative hashing
// constant (the odd integer nearest 2^64/phi), used to spread sequential
// ordinals evenly across the dual-dispatch decision so consecutive calls
// do not cluster into or out of the shadowed bucket.
const dualDispatchHashConstant = 11400714819323198485

// IsDualDispatchOrdinal reports whether ordinal (the count of routable
// decisions made so far, see LiveRouter.NextOrdinal) selects dual-dispatch
// shadow serving, at pctOf100 percent. DESIGN.md's default configuration
// (5%) is the literal "hash(call ordinal) % 20 == 0"; this generalises it
// to any configured percentage via hash(ordinal) % 100 < pct, which is
// exactly equivalent to "% 20 == 0" when pct is 5. pctOf100 <= 0 disables
// dual dispatch entirely; pctOf100 >= 100 selects every ordinal.
func IsDualDispatchOrdinal(ordinal int64, pctOf100 int) bool {
	if pctOf100 <= 0 {
		return false
	}
	if pctOf100 >= 100 {
		return true
	}
	h := uint64(ordinal) * dualDispatchHashConstant
	return (h % 100) < uint64(pctOf100)
}
