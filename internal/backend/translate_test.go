package backend

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

func rawInput(t *testing.T, jsonText string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(jsonText)) {
		t.Fatalf("test fixture is not valid JSON: %s", jsonText)
	}
	return json.RawMessage(jsonText)
}

func TestToOpenAI(t *testing.T) {
	tests := []struct {
		name string
		req  anthropic.MessagesRequest
		want ChatRequest
	}{
		{
			name: "plain string system concatenated as first message",
			req: anthropic.MessagesRequest{
				System: rawInput(t, `"You are a helpful assistant."`),
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hi"}}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "system", Content: "You are a helpful assistant."},
					{Role: "user", Content: "hi"},
				},
			},
		},
		{
			name: "block-array system, text blocks concatenated",
			req: anthropic.MessagesRequest{
				System: rawInput(t, `[{"type":"text","text":"Part one."},{"type":"text","text":"Part two."}]`),
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hi"}}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "system", Content: "Part one.\nPart two."},
					{Role: "user", Content: "hi"},
				},
			},
		},
		{
			name: "no system block produces no system message",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hi"}}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "user", Content: "hi"},
				},
			},
		},
		{
			name: "user tool_result becomes role tool with tool_call_id",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{
						{Type: anthropic.BlockToolResult, ToolUseID: "toolu_1", ToolContent: rawInput(t, `"file written"`)},
					}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "tool", Content: "file written", ToolCallID: "toolu_1"},
				},
			},
		},
		{
			name: "user message mixing text and tool_result keeps block order",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{
						{Type: anthropic.BlockToolResult, ToolUseID: "toolu_1", ToolContent: rawInput(t, `"ok"`)},
						{Type: anthropic.BlockText, Text: "also please check this"},
					}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "tool", Content: "ok", ToolCallID: "toolu_1"},
					{Role: "user", Content: "also please check this"},
				},
			},
		},
		{
			name: "assistant tool_use becomes tool_calls with JSON-encoded arguments",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "assistant", Content: []anthropic.ContentBlock{
						{Type: anthropic.BlockText, Text: "Editing now."},
						{Type: anthropic.BlockToolUse, ID: "toolu_9", Name: "Edit", Input: rawInput(t, `{"file_path":"a.go"}`)},
					}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{
						Role:    "assistant",
						Content: "Editing now.",
						ToolCalls: []ChatToolCall{
							{ID: "toolu_9", Type: "function", Function: ChatToolCallFunc{Name: "Edit", Arguments: `{"file_path":"a.go"}`}},
						},
					},
				},
			},
		},
		{
			name: "assistant tool_use with empty input encodes as empty object",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "assistant", Content: []anthropic.ContentBlock{
						{Type: anthropic.BlockToolUse, ID: "toolu_1", Name: "ExitPlanMode"},
					}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{
						Role: "assistant",
						ToolCalls: []ChatToolCall{
							{ID: "toolu_1", Type: "function", Function: ChatToolCallFunc{Name: "ExitPlanMode", Arguments: "{}"}},
						},
					},
				},
			},
		},
		{
			name: "thinking blocks dropped from assistant message",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "assistant", Content: []anthropic.ContentBlock{
						{Type: anthropic.BlockThinking, Thinking: "internal reasoning"},
						{Type: anthropic.BlockText, Text: "final answer"},
					}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "assistant", Content: "final answer"},
				},
			},
		},
		{
			name: "image blocks replaced by omitted marker text",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{
						{Type: anthropic.BlockText, Text: "look at this"},
						{Type: anthropic.BlockImage, Source: rawInput(t, `{"type":"base64","media_type":"image/png","data":"AAAA"}`)},
					}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "user", Content: "look at this\n[image omitted]"},
				},
			},
		},
		{
			name: "image inside a tool_result also replaced by the marker",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{
						{Type: anthropic.BlockToolResult, ToolUseID: "toolu_1", ToolContent: rawInput(t, `[{"type":"text","text":"screenshot:"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`)},
					}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "tool", Content: "screenshot:\n[image omitted]", ToolCallID: "toolu_1"},
				},
			},
		},
		{
			name: "anthropic tools become OpenAI function tools",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hi"}}},
				},
				Tools: []anthropic.Tool{
					{Name: "Edit", Description: "Edit a file", InputSchema: rawInput(t, `{"type":"object","properties":{"file_path":{"type":"string"}}}`)},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   defaultMaxTokens,
				Messages: []ChatMessage{
					{Role: "user", Content: "hi"},
				},
				Tools: []ChatTool{
					{Type: "function", Function: ChatFunction{
						Name:        "Edit",
						Description: "Edit a file",
						Parameters:  rawInput(t, `{"type":"object","properties":{"file_path":{"type":"string"}}}`),
					}},
				},
			},
		},
		{
			name: "max_tokens passes through when set",
			req: anthropic.MessagesRequest{
				MaxTokens: 8192,
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hi"}}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   8192,
				Messages: []ChatMessage{
					{Role: "user", Content: "hi"},
				},
			},
		},
		{
			name: "max_tokens defaults to 4096 when absent",
			req: anthropic.MessagesRequest{
				Messages: []anthropic.Message{
					{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hi"}}},
				},
			},
			want: ChatRequest{
				Model:       "target",
				Temperature: 0,
				MaxTokens:   4096,
				Messages: []ChatMessage{
					{Role: "user", Content: "hi"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToOpenAI(tt.req, "target")
			if !reflect.DeepEqual(got, tt.want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				wantJSON, _ := json.MarshalIndent(tt.want, "", "  ")
				t.Errorf("ToOpenAI mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
			}
		})
	}
}

// decodedAnthropicMessage mirrors the shape FromOpenAI marshals, used to
// decode its output for assertions.
type decodedAnthropicMessage struct {
	ID         string                   `json:"id"`
	Type       string                   `json:"type"`
	Role       string                   `json:"role"`
	Model      string                   `json:"model"`
	Content    []anthropic.ContentBlock `json:"content"`
	StopReason string                   `json:"stop_reason"`
	Usage      anthropic.Usage          `json:"usage"`
}

func TestFromOpenAI(t *testing.T) {
	tests := []struct {
		name  string
		resp  ChatResponse
		check func(t *testing.T, got decodedAnthropicMessage)
	}{
		{
			name: "text content becomes a text block",
			resp: ChatResponse{
				ID:    "chatcmpl-1",
				Model: "qwen2.5-coder:7b",
				Choices: []ChatChoice{
					{Message: ChatMessage{Role: "assistant", Content: "hello there"}, FinishReason: "stop"},
				},
				Usage: ChatUsage{PromptTokens: 10, CompletionTokens: 3},
			},
			check: func(t *testing.T, got decodedAnthropicMessage) {
				if len(got.Content) != 1 || got.Content[0].Type != anthropic.BlockText || got.Content[0].Text != "hello there" {
					t.Errorf("Content = %+v, want one text block %q", got.Content, "hello there")
				}
				if got.StopReason != "end_turn" {
					t.Errorf("StopReason = %q, want end_turn (stop maps to end_turn)", got.StopReason)
				}
				if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 3 {
					t.Errorf("Usage = %+v, want input 10 output 3", got.Usage)
				}
			},
		},
		{
			name: "tool_calls become tool_use blocks with decoded arguments and preserved ids",
			resp: ChatResponse{
				Choices: []ChatChoice{
					{
						Message: ChatMessage{
							Role: "assistant",
							ToolCalls: []ChatToolCall{
								{ID: "call_1", Type: "function", Function: ChatToolCallFunc{Name: "Edit", Arguments: `{"file_path":"a.go","old_string":"x","new_string":"y"}`}},
								{ID: "call_2", Type: "function", Function: ChatToolCallFunc{Name: "ExitPlanMode", Arguments: ""}},
							},
						},
						FinishReason: "tool_calls",
					},
				},
			},
			check: func(t *testing.T, got decodedAnthropicMessage) {
				if len(got.Content) != 2 {
					t.Fatalf("len(Content) = %d, want 2", len(got.Content))
				}
				b1 := got.Content[0]
				if b1.Type != anthropic.BlockToolUse || b1.ID != "call_1" || b1.Name != "Edit" {
					t.Errorf("Content[0] = %+v", b1)
				}
				var decoded struct {
					FilePath  string `json:"file_path"`
					OldString string `json:"old_string"`
					NewString string `json:"new_string"`
				}
				if err := json.Unmarshal(b1.Input, &decoded); err != nil {
					t.Fatalf("Content[0].Input does not parse: %v (%s)", err, b1.Input)
				}
				if decoded.FilePath != "a.go" || decoded.OldString != "x" || decoded.NewString != "y" {
					t.Errorf("decoded arguments = %+v", decoded)
				}

				b2 := got.Content[1]
				if b2.Type != anthropic.BlockToolUse || b2.ID != "call_2" || b2.Name != "ExitPlanMode" {
					t.Errorf("Content[1] = %+v", b2)
				}
				if string(b2.Input) != "{}" {
					t.Errorf("Content[1].Input = %s, want {} for empty arguments", b2.Input)
				}

				if got.StopReason != "tool_use" {
					t.Errorf("StopReason = %q, want tool_use (tool_calls maps to tool_use)", got.StopReason)
				}
			},
		},
		{
			name: "malformed arguments string preserved as a JSON string instead of breaking",
			resp: ChatResponse{
				Choices: []ChatChoice{
					{
						Message: ChatMessage{
							Role: "assistant",
							ToolCalls: []ChatToolCall{
								{ID: "call_1", Type: "function", Function: ChatToolCallFunc{Name: "Edit", Arguments: `{not valid json`}},
							},
						},
						FinishReason: "tool_calls",
					},
				},
			},
			check: func(t *testing.T, got decodedAnthropicMessage) {
				if len(got.Content) != 1 {
					t.Fatalf("len(Content) = %d, want 1", len(got.Content))
				}
				var asString string
				if err := json.Unmarshal(got.Content[0].Input, &asString); err != nil {
					t.Fatalf("malformed arguments should decode as a JSON string: %v (%s)", err, got.Content[0].Input)
				}
				if asString != `{not valid json` {
					t.Errorf("wrapped string = %q, want the raw malformed arguments preserved", asString)
				}
			},
		},
		{
			name: "finish_reason length maps to max_tokens",
			resp: ChatResponse{
				Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "cut off"}, FinishReason: "length"}},
			},
			check: func(t *testing.T, got decodedAnthropicMessage) {
				if got.StopReason != "max_tokens" {
					t.Errorf("StopReason = %q, want max_tokens", got.StopReason)
				}
			},
		},
		{
			name: "unrecognised finish_reason defaults to end_turn",
			resp: ChatResponse{
				Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "x"}, FinishReason: "content_filter"}},
			},
			check: func(t *testing.T, got decodedAnthropicMessage) {
				if got.StopReason != "end_turn" {
					t.Errorf("StopReason = %q, want end_turn for an unrecognised finish_reason", got.StopReason)
				}
			},
		},
		{
			name: "no choices produces empty content and default stop reason",
			resp: ChatResponse{Choices: nil},
			check: func(t *testing.T, got decodedAnthropicMessage) {
				if len(got.Content) != 0 {
					t.Errorf("Content = %+v, want empty", got.Content)
				}
				if got.StopReason != "end_turn" {
					t.Errorf("StopReason = %q, want end_turn", got.StopReason)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := FromOpenAI(tt.resp)
			if err != nil {
				t.Fatalf("FromOpenAI: %v", err)
			}
			if !json.Valid(raw) {
				t.Fatalf("FromOpenAI did not return valid JSON: %s", raw)
			}
			var got decodedAnthropicMessage
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decoding FromOpenAI output: %v\n%s", err, raw)
			}
			if got.Type != "message" || got.Role != "assistant" {
				t.Errorf("type/role = %q/%q, want message/assistant", got.Type, got.Role)
			}
			tt.check(t, got)
		})
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"", "end_turn"},
		{"content_filter", "end_turn"},
	}
	for _, tt := range tests {
		if got := mapFinishReason(tt.reason); got != tt.want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}
