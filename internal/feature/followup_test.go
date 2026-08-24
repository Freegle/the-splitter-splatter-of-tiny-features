package feature

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

func TestHasErrorFollowup(t *testing.T) {
	tests := []struct {
		name         string
		filesTouched []string
		nextReq      anthropic.MessagesRequest
		repoPath     string
		want         bool
	}{
		{
			name:         "tool_result is_error true",
			filesTouched: []string{"a.go"},
			nextReq: anthropic.MessagesRequest{Messages: []anthropic.Message{
				userMsg(anthropic.ContentBlock{Type: anthropic.BlockToolResult, IsError: true, ToolContent: json.RawMessage(`"boom"`)}),
			}},
			want: true,
		},
		{
			name:         "tool_result text starts with Error",
			filesTouched: []string{"a.go"},
			nextReq: anthropic.MessagesRequest{Messages: []anthropic.Message{
				userMsg(toolResultBlock("t1", "Error: file changed since last read")),
			}},
			want: true,
		},
		{
			name:         "tool_result text starts with lowercase error:",
			filesTouched: []string{"a.go"},
			nextReq: anthropic.MessagesRequest{Messages: []anthropic.Message{
				userMsg(toolResultBlock("t1", "error: unexpected token")),
			}},
			want: true,
		},
		{
			name:         "tool_result text error appears after 200 chars is not caught",
			filesTouched: []string{"a.go"},
			nextReq: anthropic.MessagesRequest{Messages: []anthropic.Message{
				userMsg(toolResultBlock("t1", strings.Repeat("x", 250)+" error later")),
			}},
			want: false,
		},
		{
			name:         "clean tool_result no error",
			filesTouched: []string{"a.go"},
			nextReq: anthropic.MessagesRequest{Messages: []anthropic.Message{
				userMsg(toolResultBlock("t1", "file updated successfully")),
			}},
			want: false,
		},
		{
			name:         "edit tool_use retargets same file",
			filesTouched: []string{"internal/feature/foo.go"},
			repoPath:     "/repo",
			nextReq: anthropic.MessagesRequest{Messages: []anthropic.Message{
				userMsg(textBlock("try again")),
				{Role: "assistant", Content: []anthropic.ContentBlock{editToolUse("Edit", "/repo/internal/feature/foo.go")}},
			}},
			want: true,
		},
		{
			name:         "edit tool_use targets a different file",
			filesTouched: []string{"internal/feature/foo.go"},
			repoPath:     "/repo",
			nextReq: anthropic.MessagesRequest{Messages: []anthropic.Message{
				{Role: "assistant", Content: []anthropic.ContentBlock{editToolUse("Edit", "/repo/internal/feature/bar.go")}},
			}},
			want: false,
		},
		{
			name:         "no signal at all",
			filesTouched: []string{"a.go"},
			nextReq: anthropic.MessagesRequest{Messages: []anthropic.Message{
				userMsg(textBlock("looks good, thanks")),
			}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasErrorFollowup(tt.filesTouched, tt.nextReq, tt.repoPath)
			if got != tt.want {
				t.Errorf("HasErrorFollowup() = %v, want %v", got, tt.want)
			}
		})
	}
}
