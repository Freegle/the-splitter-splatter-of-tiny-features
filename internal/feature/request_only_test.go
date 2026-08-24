package feature

import (
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

func assistantMsg(content ...anthropic.ContentBlock) anthropic.Message {
	return anthropic.Message{Role: "assistant", Content: content}
}

func TestRequestOnly_TurnType(t *testing.T) {
	tests := []struct {
		name string
		req  anthropic.MessagesRequest
		want string
	}{
		{
			name: "plan mode active in system prompt",
			req: anthropic.MessagesRequest{
				System:   []byte(`"You are Claude Code. Plan mode is active."`),
				Messages: []anthropic.Message{userMsg(textBlock("what should we do?"))},
			},
			want: TurnPlan,
		},
		{
			name: "last user message carries a tool_result",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					userMsg(textBlock("please fix this")),
					assistantMsg(editToolUse("Edit", "/repo/a.go")),
					userMsg(toolResultBlock("tu_1", "ok")),
				},
			},
			want: TurnToolResultSummary,
		},
		{
			name: "last user message is plain text",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{userMsg(textBlock("what does this function do?"))},
			},
			want: TurnQuestionAnswer,
		},
		{
			name: "no user message at all",
			req:  anthropic.MessagesRequest{System: []byte(`"system only"`)},
			want: TurnOther,
		},
		{
			name: "never classifies as an edit turn_type, even when history is full of edits",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					userMsg(textBlock("fix both files")),
					assistantMsg(editToolUse("Edit", "/repo/a.go"), editToolUse("Edit", "/repo/b.go")),
					userMsg(textBlock("thanks, what next?")),
				},
			},
			want: TurnQuestionAnswer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := RequestOnly(tt.req, "/repo")
			if got != tt.want {
				t.Errorf("RequestOnly() turn_type = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestOnly_Subsystem(t *testing.T) {
	tests := []struct {
		name string
		req  anthropic.MessagesRequest
		want string
	}{
		{
			name: "no assistant message yet: no subsystem",
			req:  anthropic.MessagesRequest{Messages: []anthropic.Message{userMsg(textBlock("hi"))}},
			want: "",
		},
		{
			name: "subsystem from the most recent assistant edit in history",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					userMsg(textBlock("fix the go handler")),
					assistantMsg(editToolUse("Edit", "/repo/iznik-server-go/handlers/foo.go")),
					userMsg(toolResultBlock("tu_1", "ok")),
				},
			},
			want: "iznik-server-go",
		},
		{
			name: "later assistant turn's subsystem wins over an earlier one",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					userMsg(textBlock("fix the go handler")),
					assistantMsg(editToolUse("Edit", "/repo/iznik-server-go/handlers/foo.go")),
					userMsg(toolResultBlock("tu_1", "ok")),
					assistantMsg(editToolUse("Edit", "/repo/iznik-nuxt3/components/Bar.vue")),
					userMsg(toolResultBlock("tu_2", "ok")),
				},
			},
			want: "iznik-nuxt3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := RequestOnly(tt.req, "/repo")
			if got != tt.want {
				t.Errorf("RequestOnly() subsystem = %q, want %q", got, tt.want)
			}
		})
	}
}
