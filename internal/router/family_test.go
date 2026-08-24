package router

import "testing"

func TestFamily_DesignExamples(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-opus-5", "claude-opus"},
		{"claude-opus-5-20260101", "claude-opus"},
		{"claude-sonnet-4-6", "claude-sonnet"},
		{"claude-haiku-4-5", "claude-haiku"},
		{"claude-haiku-4-5@20260115", "claude-haiku"},
		{"qwen2.5-coder:7b", "qwen-coder:7b"},
		{"qwen3-coder:7b", "qwen-coder:7b"},
		{"Qwen/Qwen2.5-Coder-32B-Instruct", "qwen-coder-32b-instruct"},
		{"gemini-2.5-flash", "gemini-flash"},
		{"gpt-4o-mini", "gpt-4o-mini"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := Family(tt.model, nil); got != tt.want {
				t.Errorf("Family(%q, nil) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestFamily_OverrideTakesPrecedence(t *testing.T) {
	overrides := map[string]string{"claude-opus-4-6-20260101": "claude-opus-custom"}

	if got := Family("claude-opus-4-6-20260101", overrides); got != "claude-opus-custom" {
		t.Errorf("Family with override = %q, want claude-opus-custom", got)
	}
	// A model not named in overrides still falls through to the rules.
	if got := Family("claude-opus-5", overrides); got != "claude-opus" {
		t.Errorf("Family for unrelated model with overrides set = %q, want claude-opus", got)
	}
}

func TestFamily_EmptyAndNilOverrides(t *testing.T) {
	if got := Family("claude-sonnet-4-6", nil); got != "claude-sonnet" {
		t.Errorf("Family with nil overrides = %q, want claude-sonnet", got)
	}
	if got := Family("claude-sonnet-4-6", map[string]string{}); got != "claude-sonnet" {
		t.Errorf("Family with empty overrides = %q, want claude-sonnet", got)
	}
}

func TestCategory(t *testing.T) {
	if got := Category("single_file_edit", "iznik-server-go"); got != "single_file_edit|iznik-server-go" {
		t.Errorf("Category() = %q, want single_file_edit|iznik-server-go", got)
	}
}

func TestFamilyPair(t *testing.T) {
	got := FamilyPair("claude-sonnet-4-6", "qwen2.5-coder:7b", nil)
	want := "claude-sonnet>qwen-coder:7b"
	if got != want {
		t.Errorf("FamilyPair() = %q, want %q", got, want)
	}
}
