package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// roundTrip runs SynthesizeSSE then AssembleSSE on messageJSON and returns
// the reassembled message plus the usage and stop reason AssembleSSE
// reports, failing the test on any error from either step.
func roundTrip(t *testing.T, messageJSON []byte) (assembledMessage, Usage, string) {
	t.Helper()

	sse, err := SynthesizeSSE(messageJSON)
	if err != nil {
		t.Fatalf("SynthesizeSSE: %v", err)
	}

	got, usage, stopReason, err := AssembleSSE(sse)
	if err != nil {
		t.Fatalf("AssembleSSE(SynthesizeSSE(...)): %v\nsse:\n%s", err, sse)
	}

	var msg assembledMessage
	if err := json.Unmarshal(got, &msg); err != nil {
		t.Fatalf("reassembled message does not parse: %v\n%s", err, got)
	}
	return msg, usage, stopReason
}

func TestSynthesizeSSE_RoundTrip_TextAndToolUse(t *testing.T) {
	source := assembledMessage{
		ID:    "msg_01ABC",
		Type:  "message",
		Role:  "assistant",
		Model: "qwen2.5-coder:7b",
		Content: []ContentBlock{
			{Type: BlockText, Text: "Sure, I'll edit that file."},
			{Type: BlockToolUse, ID: "toolu_01XYZ", Name: "Edit", Input: json.RawMessage(`{"file_path":"a.go","old_string":"foo","new_string":"bar"}`)},
		},
		StopReason: "tool_use",
		Usage:      Usage{InputTokens: 25, OutputTokens: 42},
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshaling source message: %v", err)
	}

	msg, usage, stopReason := roundTrip(t, sourceJSON)

	if msg.ID != source.ID || msg.Role != source.Role || msg.Model != source.Model {
		t.Errorf("skeleton = %+v, want id/role/model matching %+v", msg, source)
	}
	if stopReason != "tool_use" {
		t.Errorf("stopReason = %q, want tool_use", stopReason)
	}
	if usage.InputTokens != 25 {
		t.Errorf("usage.InputTokens = %d, want 25", usage.InputTokens)
	}
	if usage.OutputTokens != 42 {
		t.Errorf("usage.OutputTokens = %d, want 42", usage.OutputTokens)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(msg.Content))
	}

	textBlock := msg.Content[0]
	if textBlock.Type != BlockText || textBlock.Text != source.Content[0].Text {
		t.Errorf("Content[0] = %+v, want text %q", textBlock, source.Content[0].Text)
	}

	toolBlock := msg.Content[1]
	if toolBlock.Type != BlockToolUse || toolBlock.ID != "toolu_01XYZ" || toolBlock.Name != "Edit" {
		t.Errorf("Content[1] = %+v, want tool_use toolu_01XYZ/Edit", toolBlock)
	}
	var gotInput, wantInput map[string]string
	if err := json.Unmarshal(toolBlock.Input, &gotInput); err != nil {
		t.Fatalf("Content[1].Input does not parse: %v (%s)", err, toolBlock.Input)
	}
	if err := json.Unmarshal(source.Content[1].Input, &wantInput); err != nil {
		t.Fatalf("test setup: source input does not parse: %v", err)
	}
	if gotInput["file_path"] != wantInput["file_path"] || gotInput["old_string"] != wantInput["old_string"] || gotInput["new_string"] != wantInput["new_string"] {
		t.Errorf("tool_use input = %+v, want %+v", gotInput, wantInput)
	}
}

func TestSynthesizeSSE_RoundTrip_ThinkingBlock(t *testing.T) {
	source := assembledMessage{
		ID:    "msg_02",
		Type:  "message",
		Role:  "assistant",
		Model: "m",
		Content: []ContentBlock{
			{Type: BlockThinking, Thinking: "Let me think about this.", Signature: "sig-abc"},
			{Type: BlockText, Text: "Done."},
		},
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 10, OutputTokens: 5},
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshaling source message: %v", err)
	}

	msg, _, stopReason := roundTrip(t, sourceJSON)

	if stopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", stopReason)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(msg.Content))
	}
	if msg.Content[0].Type != BlockThinking || msg.Content[0].Thinking != "Let me think about this." || msg.Content[0].Signature != "sig-abc" {
		t.Errorf("Content[0] = %+v", msg.Content[0])
	}
	if msg.Content[1].Type != BlockText || msg.Content[1].Text != "Done." {
		t.Errorf("Content[1] = %+v", msg.Content[1])
	}
}

func TestSynthesizeSSE_RoundTrip_EmptyContent(t *testing.T) {
	source := assembledMessage{
		ID:         "msg_03",
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		Content:    []ContentBlock{},
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 3, OutputTokens: 0},
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshaling source message: %v", err)
	}

	msg, usage, stopReason := roundTrip(t, sourceJSON)

	if stopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", stopReason)
	}
	if usage.InputTokens != 3 {
		t.Errorf("usage.InputTokens = %d, want 3", usage.InputTokens)
	}
	if len(msg.Content) != 0 {
		t.Errorf("len(Content) = %d, want 0", len(msg.Content))
	}
}

func TestSynthesizeSSE_InvalidJSON_ReturnsError(t *testing.T) {
	if _, err := SynthesizeSSE([]byte("not json")); err == nil {
		t.Fatal("expected an error decoding invalid message JSON")
	}
}

func TestSynthesizeSSE_EventFraming(t *testing.T) {
	source := assembledMessage{
		ID:         "msg_04",
		Type:       "message",
		Role:       "assistant",
		Model:      "m",
		Content:    []ContentBlock{{Type: BlockText, Text: "hi"}},
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 1, OutputTokens: 1},
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshaling source message: %v", err)
	}

	sse, err := SynthesizeSSE(sourceJSON)
	if err != nil {
		t.Fatalf("SynthesizeSSE: %v", err)
	}

	wantEvents := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	for _, want := range wantEvents {
		if !strings.Contains(string(sse), "event: "+want) {
			t.Errorf("SSE output missing %q event\n%s", want, sse)
		}
	}
}
