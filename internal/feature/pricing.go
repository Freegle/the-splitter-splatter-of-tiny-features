package feature

import "strings"

// Pricing holds one model family's per-million-token cost in USD.
type Pricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Cost estimates the USD cost of inputTokens/outputTokens at this pricing.
func (p Pricing) Cost(inputTokens, outputTokens int64) float64 {
	return float64(inputTokens)/1_000_000*p.InputPerMTok + float64(outputTokens)/1_000_000*p.OutputPerMTok
}

// pricingTable holds the known frontier model families' pricing, keyed by
// family (not exact model id) so a new same-family version prices the same
// as the version this table was written against, until measured otherwise.
var pricingTable = map[string]Pricing{
	"claude-opus":   {InputPerMTok: 5, OutputPerMTok: 25},
	"claude-sonnet": {InputPerMTok: 3, OutputPerMTok: 15},
	"claude-haiku":  {InputPerMTok: 1, OutputPerMTok: 5},
}

// fallbackPricing prices any model whose family is not in pricingTable.
var fallbackPricing = Pricing{InputPerMTok: 5, OutputPerMTok: 25}

// PricingFor returns the pricing to use for model, keyed by its normalised
// model family. This is a small local stand-in for internal/router.Family,
// which arrives with the router component (DESIGN.md "Model families"):
// the router may later rewire this lookup to call that function instead of
// pricingFamily without changing PricingFor's signature or callers.
func PricingFor(model string) Pricing {
	if p, ok := pricingTable[pricingFamily(model)]; ok {
		return p
	}
	return fallbackPricing
}

// pricingFamily normalises an exact Claude model id to its family: strips a
// trailing -YYYYMMDD/@YYYYMMDD date suffix, then drops dash-separated
// segments that are pure version numbers (digits and dots only), keeping
// variant words like "opus"/"sonnet"/"haiku". Good enough for pricing
// lookups against frontier models; local/replay backend model ids (ollama,
// together, gemini, openai) simply miss pricingTable and take
// fallbackPricing, since PricingFor prices the logged frontier model only.
func pricingFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = stripTrailingDateSuffix(m)

	parts := strings.Split(m, "-")
	kept := parts[:0]
	for _, p := range parts {
		if isVersionSegment(p) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "-")
}

// stripTrailingDateSuffix removes a trailing "-YYYYMMDD" or "@YYYYMMDD"
// segment (8 digits), if present.
func stripTrailingDateSuffix(m string) string {
	for _, sep := range []string{"-", "@"} {
		idx := strings.LastIndex(m, sep)
		if idx < 0 {
			continue
		}
		suffix := m[idx+1:]
		if len(suffix) == 8 && isVersionSegment(suffix) {
			return m[:idx]
		}
	}
	return m
}

// isVersionSegment reports whether s consists only of digits and dots (a
// generation number like "5" or "4.5"), making it non-empty and not a
// variant word.
func isVersionSegment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
