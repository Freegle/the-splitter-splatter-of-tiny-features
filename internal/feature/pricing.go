package feature

import "github.com/freegle/splitter/internal/router"

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
	// Without this entry deepseek falls back to opus-level pricing and the
	// spend report overstates its cost roughly 35x (see BACKENDS.md). The
	// "-v-" is what router.Family leaves of the "v4" version marker.
	"deepseek-v-flash": {InputPerMTok: 0.14, OutputPerMTok: 0.28},
}

// fallbackPricing prices any model whose family is not in pricingTable.
var fallbackPricing = Pricing{InputPerMTok: 5, OutputPerMTok: 25}

// PricingFor returns the pricing to use for model, keyed by its normalised
// model family.
func PricingFor(model string) Pricing {
	if p, ok := pricingTable[pricingFamily(model)]; ok {
		return p
	}
	return fallbackPricing
}

// pricingFamily normalises an exact model id to its family via
// internal/router.Family (DESIGN.md "Model families"), with no per-model
// overrides: PricingFor's signature carries no config, and pricing only
// ever needs the logged frontier (Claude) model's family, which never
// depends on the [families] override table used by live routing.
func pricingFamily(model string) string {
	return router.Family(model, nil)
}
