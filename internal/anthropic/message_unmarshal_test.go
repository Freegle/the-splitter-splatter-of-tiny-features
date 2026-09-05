package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContentBlock_UnmarshalJSON_HappyPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  ContentBlock
	}{
		{
			name:  "text block",
			input: `{"type":"text","text":"hello"}`,
			want:  ContentBlock{Type: BlockText, Text: "hello"},
		},
		{
			name:  "tool_use block",
			input: `{"type":"tool_use","id":"t1","name":"foo","input":{"k":"v"}}`,
			want: ContentBlock{
				Type:  BlockToolUse,
				ID:    "t1",
				Name:  "foo",
				Input: json.RawMessage(`{"k":"v"}`),
			},
		},
		{
			name:  "tool_result block",
			input: `{"type":"tool_result","tool_use_id":"t1","content":"ok","is_error":true}`,
			want: ContentBlock{
				Type:        BlockToolResult,
				ToolUseID:   "t1",
				ToolContent: json.RawMessage(`"ok"`),
				IsError:     true,
			},
		},
		{
			name:  "thinking block with signature",
			input: `{"type":"thinking","thinking":"reasoning","signature":"sig123"}`,
			want: ContentBlock{
				Type:      BlockThinking,
				Thinking:  "reasoning",
				Signature: "sig123",
			},
		},
		{
			name:  "image block",
			input: `{"type":"image","source":{"media_type":"image/png","data":"base64"}}`,
			want: ContentBlock{
				Type:   BlockImage,
				Source: json.RawMessage(`{"media_type":"image/png","data":"base64"}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b ContentBlock
			if err := b.UnmarshalJSON([]byte(tt.input)); err != nil {
				t.Fatalf("UnmarshalJSON: %v", err)
			}
			if b.Type != tt.want.Type {
				t.Errorf("Type = %q, want %q", b.Type, tt.want.Type)
			}
			if b.Text != tt.want.Text {
				t.Errorf("Text = %q, want %q", b.Text, tt.want.Text)
			}
			if b.ID != tt.want.ID || b.Name != tt.want.Name {
				t.Errorf("tool_use fields: got ID=%q Name=%q, want ID=%q Name=%q", b.ID, b.Name, tt.want.ID, tt.want.Name)
			}
			if string(b.Input) != string(tt.want.Input) {
				t.Errorf("Input = %s, want %s", b.Input, tt.want.Input)
			}
			if b.ToolUseID != tt.want.ToolUseID || b.IsError != tt.want.IsError {
				t.Errorf("tool_result fields: got ToolUseID=%q IsError=%v, want ToolUseID=%q IsError=%v", b.ToolUseID, b.IsError, tt.want.ToolUseID, tt.want.IsError)
			}
			if string(b.ToolContent) != string(tt.want.ToolContent) {
				t.Errorf("ToolContent = %s, want %s", b.ToolContent, tt.want.ToolContent)
			}
			if b.Thinking != tt.want.Thinking || b.Signature != tt.want.Signature {
				t.Errorf("thinking fields: got Thinking=%q Signature=%q, want Thinking=%q Signature=%q", b.Thinking, b.Signature, tt.want.Thinking, tt.want.Signature)
			}
			if string(b.Source) != string(tt.want.Source) {
				t.Errorf("Source = %s, want %s", b.Source, tt.want.Source)
			}
			if len(b.Raw) != 0 {
				t.Errorf("Raw should be nil for known types, got %q", b.Raw)
			}
		})
	}
}

func TestContentBlock_UnmarshalJSON_UnknownType(t *testing.T) {
	input := `{"type":"unknown_type","foo":"bar"}`
	var b ContentBlock
	if err := b.UnmarshalJSON([]byte(input)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if b.Type != "unknown_type" {
		t.Errorf("Type = %q, want unknown_type", b.Type)
	}
	if len(b.Raw) == 0 {
		t.Error("Raw should be set for unknown types")
	}
	var rawMap map[string]any
	if err := json.Unmarshal(b.Raw, &rawMap); err != nil {
		t.Fatalf("Raw does not parse as JSON: %v", err)
	}
	if rawMap["foo"] != "bar" {
		t.Errorf("Raw content mismatch: got %+v", rawMap)
	}
}

func TestContentBlock_UnmarshalJSON_InvalidJSON(t *testing.T) {
	var b ContentBlock
	if err := b.UnmarshalJSON([]byte(`{invalid}`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestMessage_UnmarshalJSON_TextOnly(t *testing.T) {
	input := `{"role":"user","content":"hello world"}`
	var m Message
	if err := m.UnmarshalJSON([]byte(input)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if m.Role != "user" {
		t.Errorf("Role = %q, want user", m.Role)
	}
	if len(m.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(m.Content))
	}
	if m.Content[0].Type != BlockText || m.Content[0].Text != "hello world" {
		t.Errorf("Content[0] = %+v, want text block with hello world", m.Content[0])
	}
}

func TestMessage_UnmarshalJSON_BlockArray(t *testing.T) {
	input := `{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"foo","input":{}}]}`
	var m Message
	if err := m.UnmarshalJSON([]byte(input)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if m.Role != "assistant" {
		t.Errorf("Role = %q, want assistant", m.Role)
	}
	if len(m.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(m.Content))
	}
	if m.Content[0].Type != BlockText || m.Content[0].Text != "hi" {
		t.Errorf("Content[0] = %+v", m.Content[0])
	}
	if m.Content[1].Type != BlockToolUse || m.Content[1].ID != "t1" || m.Content[1].Name != "foo" {
		t.Errorf("Content[1] = %+v", m.Content[1])
	}
}

func TestMessage_UnmarshalJSON_EmptyContent(t *testing.T) {
	input := `{"role":"user","content":[]}`
	var m Message
	if err := m.UnmarshalJSON([]byte(input)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if m.Role != "user" {
		t.Errorf("Role = %q, want user", m.Role)
	}
	if len(m.Content) != 0 {
		t.Errorf("len(Content) = %d, want 0", len(m.Content))
	}
}

func TestMessage_UnmarshalJSON_NullContent(t *testing.T) {
	input := `{"role":"user","content":null}`
	var m Message
	if err := m.UnmarshalJSON([]byte(input)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if m.Role != "user" {
		t.Errorf("Role = %q, want user", m.Role)
	}
	if len(m.Content) != 0 {
		t.Errorf("len(Content) = %d, want 0", len(m.Content))
	}
}

func TestMessage_UnmarshalJSON_InvalidJSON(t *testing.T) {
	var m Message
	if err := m.UnmarshalJSON([]byte(`{bad}`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestMessage_UnmarshalJSON_InvalidContentBlocks(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "content is array of numbers",
			input: `{"role":"user","content":[123]}`,
		},
		{
			name:  "content is object instead of array",
			input: `{"role":"user","content":{"not":"an array"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m Message
			err := m.UnmarshalJSON([]byte(tt.input))
			if err == nil {
				t.Fatal("expected error for invalid content blocks")
			}
			if !strings.Contains(err.Error(), "decoding message content blocks") {
				t.Errorf("error = %q, want to contain 'decoding message content blocks'", err.Error())
			}
		})
	}
}
