package feature

import (
	"sort"

	"github.com/freegle/splitter/internal/store"
)

// SpendSummary is one turn_type's aggregated call count, token totals and
// estimated cost for splitter report spend.
type SpendSummary struct {
	TurnType      string
	Calls         int
	ContextTokens int64
	OutputTokens  int64
	CostUSD       float64
}

// Spend aggregates rows, one per featurised call, into a SpendSummary per
// turn_type. Each row is priced by its own model before summing, so a mix
// of frontier and locally-served calls is costed correctly. The result is
// sorted by descending cost (ties broken by turn_type name), so the
// biggest spend line appears first.
func Spend(rows []store.SpendRow) []SpendSummary {
	byType := map[string]*SpendSummary{}
	var order []string
	for _, r := range rows {
		s, ok := byType[r.TurnType]
		if !ok {
			s = &SpendSummary{TurnType: r.TurnType}
			byType[r.TurnType] = s
			order = append(order, r.TurnType)
		}
		s.Calls++
		s.ContextTokens += r.ContextTokens
		s.OutputTokens += r.OutputTokens
		s.CostUSD += PricingFor(r.Model).Cost(r.ContextTokens, r.OutputTokens)
	}

	out := make([]SpendSummary, 0, len(order))
	for _, t := range order {
		out = append(out, *byType[t])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		return out[i].TurnType < out[j].TurnType
	})
	return out
}
