package feature

import (
	"encoding/json"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

func userMsg(content ...anthropic.ContentBlock) anthropic.Message {
	return anthropic.Message{Role: "user", Content: content}
}

func textBlock(s string) anthropic.ContentBlock {
	return anthropic.ContentBlock{Type: anthropic.BlockText, Text: s}
}

func toolResultBlock(id, text string) anthropic.ContentBlock {
	return anthropic.ContentBlock{
		Type:        anthropic.BlockToolResult,
		ToolUseID:   id,
		ToolContent: json.RawMessage(`"` + text + `"`),
	}
}

func editToolUse(name, filePath string) anthropic.ContentBlock {
	input, _ := json.Marshal(map[string]string{"file_path": filePath})
	return anthropic.ContentBlock{Type: anthropic.BlockToolUse, Name: name, Input: input}
}

func toolUse(name string) anthropic.ContentBlock {
	return anthropic.ContentBlock{Type: anthropic.BlockToolUse, Name: name, Input: json.RawMessage("{}")}
}

func TestClassifyTurnType(t *testing.T) {
	tests := []struct {
		name string
		req  anthropic.MessagesRequest
		resp []anthropic.ContentBlock
		want string
	}{
		{
			name: "rule1: two edit blocks two distinct files",
			req:  anthropic.MessagesRequest{Messages: []anthropic.Message{userMsg(textBlock("please fix both files"))}},
			resp: []anthropic.ContentBlock{
				editToolUse("Edit", "/repo/a.go"),
				editToolUse("Edit", "/repo/b.go"),
			},
			want: TurnMultiFileEdit,
		},
		{
			name: "rule1: MultiEdit blocks on two files",
			req:  anthropic.MessagesRequest{Messages: []anthropic.Message{userMsg(textBlock("refactor these"))}},
			resp: []anthropic.ContentBlock{
				editToolUse("MultiEdit", "/repo/a.go"),
				editToolUse("MultiEdit", "/repo/b.go"),
			},
			want: TurnMultiFileEdit,
		},
		{
			name: "rule2: two edit blocks same file stays single",
			req:  anthropic.MessagesRequest{Messages: []anthropic.Message{userMsg(textBlock("fix this file"))}},
			resp: []anthropic.ContentBlock{
				editToolUse("Edit", "/repo/a.go"),
				editToolUse("Edit", "/repo/a.go"),
			},
			want: TurnSingleFileEdit,
		},
		{
			name: "rule2: single MultiEdit block one file",
			req:  anthropic.MessagesRequest{Messages: []anthropic.Message{userMsg(textBlock("clean up a.go"))}},
			resp: []anthropic.ContentBlock{
				editToolUse("MultiEdit", "/repo/a.go"),
			},
			want: TurnSingleFileEdit,
		},
		{
			name: "rule2: single Write block",
			req:  anthropic.MessagesRequest{Messages: []anthropic.Message{userMsg(textBlock("create a new file"))}},
			resp: []anthropic.ContentBlock{
				editToolUse("Write", "/repo/new.go"),
			},
			want: TurnSingleFileEdit,
		},
		{
			name: "rule3: ExitPlanMode tool_use",
			req:  anthropic.MessagesRequest{Messages: []anthropic.Message{userMsg(textBlock("how should I approach this"))}},
			resp: []anthropic.ContentBlock{
				textBlock("Here is my plan:"),
				toolUse("ExitPlanMode"),
			},
			want: TurnPlan,
		},
		{
			name: "rule3: system prompt mentions plan mode active",
			req: anthropic.MessagesRequest{
				System:   json.RawMessage(`"You are in an interactive session. Plan mode is active. Do not edit files."`),
				Messages: []anthropic.Message{userMsg(textBlock("what should we do"))},
			},
			resp: []anthropic.ContentBlock{textBlock("I would start by...")},
			want: TurnPlan,
		},
		{
			name: "rule4: tool_result followed by text summary",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					userMsg(toolResultBlock("toolu_1", "file contents here")),
				},
			},
			resp: []anthropic.ContentBlock{textBlock("The file contains a Go handler for X.")},
			want: TurnToolResultSummary,
		},
		{
			name: "rule4: tool_result is_error true still summarised as text",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					userMsg(anthropic.ContentBlock{Type: anthropic.BlockToolResult, IsError: true, ToolContent: json.RawMessage(`"Error: file not found"`)}),
				},
			},
			resp: []anthropic.ContentBlock{textBlock("That file does not exist; let me check the directory listing instead.")},
			want: TurnToolResultSummary,
		},
		{
			name: "rule5: plain question and answer",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{userMsg(textBlock("What does this function do?"))},
			},
			resp: []anthropic.ContentBlock{textBlock("It parses the config file and returns a struct.")},
			want: TurnQuestionAnswer,
		},
		{
			name: "rule5: short question",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{userMsg(textBlock("yes"))},
			},
			resp: []anthropic.ContentBlock{textBlock("Great, I'll proceed with that approach.")},
			want: TurnQuestionAnswer,
		},
		{
			name: "rule6: non-edit tool_use is other",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{userMsg(textBlock("run the tests"))},
			},
			resp: []anthropic.ContentBlock{
				textBlock("Running the test suite now."),
				toolUse("Bash"),
			},
			want: TurnOther,
		},
		{
			name: "rule6: empty response content",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{userMsg(textBlock("hello"))},
			},
			resp: nil,
			want: TurnOther,
		},
		{
			name: "rule6: no user message at all",
			req:  anthropic.MessagesRequest{Messages: nil},
			resp: []anthropic.ContentBlock{textBlock("hello")},
			want: TurnOther,
		},
		{
			name: "thinking block does not break text-only question_answer",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{userMsg(textBlock("why does this fail intermittently?"))},
			},
			resp: []anthropic.ContentBlock{
				{Type: anthropic.BlockThinking, Thinking: "This looks like a race condition..."},
				textBlock("It is likely a race condition between the two goroutines."),
			},
			want: TurnQuestionAnswer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyTurnType(tt.req, tt.resp)
			if got != tt.want {
				t.Errorf("ClassifyTurnType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSystemMentionsPlanMode_ArrayOfBlocks(t *testing.T) {
	sys, _ := json.Marshal([]anthropic.ContentBlock{
		{Type: anthropic.BlockText, Text: "General instructions."},
		{Type: anthropic.BlockText, Text: "PLAN MODE IS ACTIVE right now."},
	})
	if !systemMentionsPlanMode(sys) {
		t.Error("expected plan mode mention to be detected in block array system prompt (case-insensitive)")
	}
}

func TestSystemMentionsPlanMode_NoMention(t *testing.T) {
	sys := json.RawMessage(`"You are a helpful coding assistant."`)
	if systemMentionsPlanMode(sys) {
		t.Error("did not expect plan mode mention")
	}
}
