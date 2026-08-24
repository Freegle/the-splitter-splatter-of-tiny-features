package judge

import (
	"strings"
	"testing"
)

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"shorter than limit unchanged", "hello", 10, "hello"},
		{"exactly at limit unchanged", "hello", 5, "hello"},
		{"zero means unlimited", "hello world", 0, "hello world"},
		{"negative means unlimited", "hello world", -1, "hello world"},
		{"longer than limit truncated with suffix", "hello world", 5, "hello" + truncatedSuffix},
		{"multi-byte runes not split", "héllo wörld", 6, "héllo " + truncatedSuffix},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateRunes(tt.s, tt.max); got != tt.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
		})
	}
}

func TestBuildPrompt(t *testing.T) {
	longContext := strings.Repeat("x", 100)
	frontier := "frontier says " + strings.Repeat("y", 100)
	local := "local says " + strings.Repeat("z", 100)

	prompt := BuildPrompt(longContext, frontier, local, 10)

	contextIdx := strings.Index(prompt, "x")
	frontierIdx := strings.Index(prompt, frontier)
	localIdx := strings.Index(prompt, local)
	instructionIdx := strings.Index(prompt, judgeInstruction)

	if contextIdx == -1 || frontierIdx == -1 || localIdx == -1 || instructionIdx == -1 {
		t.Fatalf("prompt missing an expected section:\n%s", prompt)
	}
	if !(contextIdx < frontierIdx && frontierIdx < localIdx && localIdx < instructionIdx) {
		t.Errorf("prompt sections out of order: context=%d frontier=%d local=%d instruction=%d\n%s",
			contextIdx, frontierIdx, localIdx, instructionIdx, prompt)
	}

	if strings.Contains(prompt, strings.Repeat("x", 100)) {
		t.Error("request context was not truncated in the prompt")
	}
	if !strings.Contains(prompt, truncatedSuffix) {
		t.Error("prompt does not show the truncation marker for the request context")
	}
	if !strings.Contains(prompt, frontier) {
		t.Error("frontier response was truncated or altered, want it included in full")
	}
	if !strings.Contains(prompt, local) {
		t.Error("local response was truncated or altered, want it included in full")
	}
}

func TestExtractResponseText(t *testing.T) {
	tests := []struct {
		name        string
		messageJSON string
		want        string
	}{
		{
			name:        "text only",
			messageJSON: `{"content":[{"type":"text","text":"hello there"}]}`,
			want:        "hello there",
		},
		{
			name:        "tool_use only",
			messageJSON: `{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"a.go"}}]}`,
			want:        `tool_use Edit({"file_path":"a.go"})`,
		},
		{
			name:        "text then tool_use, order preserved",
			messageJSON: `{"content":[{"type":"text","text":"I'll edit this file."},{"type":"tool_use","name":"Write","input":{"file_path":"b.go","content":"x"}}]}`,
			want:        "I'll edit this file.\n" + `tool_use Write({"file_path":"b.go","content":"x"})`,
		},
		{
			name:        "empty content",
			messageJSON: `{"content":[]}`,
			want:        "",
		},
		{
			name:        "malformed JSON falls back to raw bytes",
			messageJSON: `not json at all`,
			want:        "not json at all",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractResponseText([]byte(tt.messageJSON)); got != tt.want {
				t.Errorf("ExtractResponseText(%s) = %q, want %q", tt.messageJSON, got, tt.want)
			}
		})
	}
}
