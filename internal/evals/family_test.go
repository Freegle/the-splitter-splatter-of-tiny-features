package evals

import "testing"

func TestFamily(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-opus-5", "claude-opus"},
		{"claude-opus-5-20260101", "claude-opus"},
		{"claude-sonnet-4-6", "claude-sonnet"},
		{"claude-haiku-4-5", "claude-haiku"},
		{"claude-haiku-4-5@20260101", "claude-haiku"},
		{"qwen2.5-coder:7b", "qwen-coder:7b"},
		{"qwen3-coder:7b", "qwen-coder:7b"},
		{"Qwen/Qwen2.5-Coder-32B-Instruct", "qwen-coder-32b-instruct"},
		{"gemini-2.5-flash", "gemini-flash"},
		{"gpt-4o-mini", "gpt-4o-mini"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := Family(tt.model, nil); got != tt.want {
				t.Errorf("Family(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestFamily_ExactOverrideWins(t *testing.T) {
	overrides := map[string]string{"claude-opus-4-6-20260101": "claude-opus-custom"}
	if got := Family("claude-opus-4-6-20260101", overrides); got != "claude-opus-custom" {
		t.Errorf("Family with override = %q, want claude-opus-custom", got)
	}
	// A model not in overrides still falls through to the normal algorithm.
	if got := Family("claude-opus-5", overrides); got != "claude-opus" {
		t.Errorf("Family without matching override = %q, want claude-opus", got)
	}
}

func TestModelCutoff(t *testing.T) {
	cutoffs := map[string]string{
		"claude-sonnet":     "2026-02",
		"deepseek-v4-flash": "2026-01",
	}

	t.Run("exact match wins over family", func(t *testing.T) {
		c, ok := ModelCutoff("deepseek-v4-flash", cutoffs, nil)
		if !ok || c != "2026-01" {
			t.Errorf("ModelCutoff = %q, %v, want 2026-01, true", c, ok)
		}
	})

	t.Run("family match when no exact entry", func(t *testing.T) {
		c, ok := ModelCutoff("claude-sonnet-4-6", cutoffs, nil)
		if !ok || c != "2026-02" {
			t.Errorf("ModelCutoff = %q, %v, want 2026-02, true", c, ok)
		}
	})

	t.Run("unknown model", func(t *testing.T) {
		_, ok := ModelCutoff("gpt-4o-mini", cutoffs, nil)
		if ok {
			t.Error("expected ModelCutoff to report unknown for a model with no configured cutoff")
		}
	})
}

func TestCutoffSegment(t *testing.T) {
	cutoffs := map[string]string{"claude-sonnet": "2026-02"}

	tests := []struct {
		name     string
		taskDate string
		model    string
		want     string
	}{
		{"before cutoff month", "2026-01-15T00:00:00Z", "claude-sonnet-4-6", SegmentPreCutoff},
		{"same month as cutoff counts as post", "2026-02-01T00:00:00Z", "claude-sonnet-4-6", SegmentPostCutoff},
		{"after cutoff month", "2026-06-01T00:00:00Z", "claude-sonnet-4-6", SegmentPostCutoff},
		{"no cutoff configured for model", "2026-01-01T00:00:00Z", "gpt-4o-mini", SegmentUnknownCutoff},
		{"empty task date", "", "claude-sonnet-4-6", SegmentUnknownCutoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CutoffSegment(tt.taskDate, tt.model, cutoffs, nil); got != tt.want {
				t.Errorf("CutoffSegment(%q, %q) = %q, want %q", tt.taskDate, tt.model, got, tt.want)
			}
		})
	}
}
