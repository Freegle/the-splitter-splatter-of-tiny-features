package feature

import "testing"

func TestPricingFamily(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-opus-5", "claude-opus"},
		{"claude-opus-5-20260101", "claude-opus"},
		{"claude-sonnet-4-6", "claude-sonnet"},
		{"claude-haiku-4-5", "claude-haiku"},
		{"claude-haiku-4-5@20260115", "claude-haiku"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := pricingFamily(tt.model); got != tt.want {
				t.Errorf("pricingFamily(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestPricingFor(t *testing.T) {
	tests := []struct {
		model string
		want  Pricing
	}{
		{"claude-opus-5", Pricing{InputPerMTok: 5, OutputPerMTok: 25}},
		{"claude-opus-5-20260101", Pricing{InputPerMTok: 5, OutputPerMTok: 25}},
		{"claude-sonnet-4-6", Pricing{InputPerMTok: 3, OutputPerMTok: 15}},
		{"claude-sonnet-5", Pricing{InputPerMTok: 3, OutputPerMTok: 15}},
		{"claude-haiku-4-5", Pricing{InputPerMTok: 1, OutputPerMTok: 5}},
		{"qwen2.5-coder:7b", fallbackPricing},
		{"gpt-4o-mini", fallbackPricing},
		{"", fallbackPricing},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := PricingFor(tt.model); got != tt.want {
				t.Errorf("PricingFor(%q) = %+v, want %+v", tt.model, got, tt.want)
			}
		})
	}
}

func TestPricingFor_NewSameFamilyVersionInheritsPricing(t *testing.T) {
	// A hypothetical future Sonnet version prices the same as today's,
	// until it is measured otherwise (DESIGN.md "Model families").
	got := PricingFor("claude-sonnet-6-20270301")
	want := PricingFor("claude-sonnet-4-6")
	if got != want {
		t.Errorf("PricingFor(future sonnet) = %+v, want %+v (same family pricing)", got, want)
	}
}

func TestPricing_Cost(t *testing.T) {
	p := Pricing{InputPerMTok: 3, OutputPerMTok: 15}
	got := p.Cost(1_000_000, 500_000)
	want := 3.0 + 7.5
	if got != want {
		t.Errorf("Cost() = %v, want %v", got, want)
	}
}

func TestPricingForDeepSeekFlash(t *testing.T) {
	p := PricingFor("deepseek-v4-flash")
	if p.InputPerMTok != 0.14 || p.OutputPerMTok != 0.28 {
		t.Fatalf("deepseek-v4-flash priced %+v, want 0.14/0.28 (family key mismatch means it fell back to opus pricing)", p)
	}
}
