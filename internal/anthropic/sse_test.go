package anthropic

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// realisticStream is a captured-shaped SSE stream: a text block, a tool_use
// block whose input arrives across two input_json_delta chunks, a ping
// keepalive interleaved, and usage split across message_start (input) and
// message_delta (output).
const realisticStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01ABC","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Sure, I'll edit "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"that file."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01XYZ","name":"Edit","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\": \"a.go\","}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"old_string\": \"foo\", \"new_string\": \"bar\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAssembleSSE_RealisticStream(t *testing.T) {
	messageJSON, usage, stopReason, err := AssembleSSE([]byte(realisticStream))
	if err != nil {
		t.Fatalf("AssembleSSE: %v", err)
	}
	if stopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", stopReason)
	}
	if usage.InputTokens != 25 {
		t.Errorf("usage.InputTokens = %d, want 25 (from message_start)", usage.InputTokens)
	}
	if usage.OutputTokens != 42 {
		t.Errorf("usage.OutputTokens = %d, want 42 (from message_delta)", usage.OutputTokens)
	}

	var got assembledMessage
	if err := json.Unmarshal(messageJSON, &got); err != nil {
		t.Fatalf("messageJSON does not parse: %v\n%s", err, messageJSON)
	}
	if got.ID != "msg_01ABC" || got.Role != "assistant" || got.Model != "claude-sonnet-4-6" {
		t.Errorf("message skeleton = %+v", got)
	}
	if len(got.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(got.Content))
	}

	textBlock := got.Content[0]
	if textBlock.Type != BlockText {
		t.Errorf("Content[0].Type = %q, want text", textBlock.Type)
	}
	if textBlock.Text != "Sure, I'll edit that file." {
		t.Errorf("Content[0].Text = %q", textBlock.Text)
	}

	toolBlock := got.Content[1]
	if toolBlock.Type != BlockToolUse {
		t.Errorf("Content[1].Type = %q, want tool_use", toolBlock.Type)
	}
	if toolBlock.ID != "toolu_01XYZ" || toolBlock.Name != "Edit" {
		t.Errorf("Content[1] id/name = %q/%q", toolBlock.ID, toolBlock.Name)
	}
	var input struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(toolBlock.Input, &input); err != nil {
		t.Fatalf("tool_use input does not parse as JSON: %v (raw: %s)", err, toolBlock.Input)
	}
	if input.FilePath != "a.go" || input.OldString != "foo" || input.NewString != "bar" {
		t.Errorf("tool_use input = %+v", input)
	}
}

func TestAssembleSSE_UnknownEventAndDeltaTypesTolerated(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"before "}}

event: some_future_event
data: {"type":"some_future_event","foo":"bar"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"some_future_delta","stuff":"x"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"after"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

	messageJSON, _, stopReason, err := AssembleSSE([]byte(stream))
	if err != nil {
		t.Fatalf("AssembleSSE should tolerate unknown event/delta types, got err: %v", err)
	}
	if stopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", stopReason)
	}

	var got assembledMessage
	if err := json.Unmarshal(messageJSON, &got); err != nil {
		t.Fatalf("messageJSON does not parse: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(got.Content))
	}
	if got.Content[0].Text != "before after" {
		t.Errorf("Content[0].Text = %q, want %q (unknown event/delta should not corrupt accumulation)", got.Content[0].Text, "before after")
	}
}

func TestAssembleSSE_TruncatedStream_ReturnsPartialAndErr(t *testing.T) {
	// Cut the realistic stream off mid tool_use input, before its
	// content_block_stop, message_delta or message_stop ever arrive.
	cut := strings.Index(realisticStream, `event: content_block_stop
data: {"type":"content_block_stop","index":1}`)
	if cut < 0 {
		t.Fatal("test fixture setup: could not find cut point")
	}
	truncated := realisticStream[:cut]

	messageJSON, usage, _, err := AssembleSSE([]byte(truncated))
	if err == nil {
		t.Fatal("expected an error for a truncated stream")
	}
	if !errors.Is(err, ErrTruncatedStream) {
		t.Errorf("err = %v, want wrapping ErrTruncatedStream", err)
	}
	if usage.InputTokens != 25 {
		t.Errorf("usage.InputTokens = %d, want 25 (captured before truncation)", usage.InputTokens)
	}
	// message_delta never arrived, so output_tokens stays at message_start's
	// placeholder value.
	if usage.OutputTokens != 1 {
		t.Errorf("usage.OutputTokens = %d, want 1 (message_delta never arrived)", usage.OutputTokens)
	}

	if len(messageJSON) == 0 {
		t.Fatal("messageJSON should not be empty for a partial assembly")
	}
	var got assembledMessage
	if err := json.Unmarshal(messageJSON, &got); err != nil {
		t.Fatalf("partial messageJSON does not parse as JSON: %v\n%s", err, messageJSON)
	}
	if len(got.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2 (text block plus the unfinished tool_use)", len(got.Content))
	}
	if got.Content[0].Text != "Sure, I'll edit that file." {
		t.Errorf("Content[0].Text = %q, text block should survive truncation of a later block", got.Content[0].Text)
	}

	// The accumulated partial_json ("{\"file_path\": \"a.go\",\"old_string\": \"foo\", \"new_string\": \"bar\"}")
	// is actually complete valid JSON in this fixture's cut point (the cut
	// falls after both deltas), so assert on the input being present as an
	// object rather than requiring it to be malformed.
	if len(got.Content[1].Input) == 0 {
		t.Error("truncated tool_use block should still carry whatever input was accumulated")
	}
}

func TestAssembleSSE_TruncatedMidToolUseDelta_WrapsPartialAsString(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Edit","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\": \"a.go\","}}
`

	messageJSON, _, _, err := AssembleSSE([]byte(stream))
	if !errors.Is(err, ErrTruncatedStream) {
		t.Fatalf("err = %v, want wrapping ErrTruncatedStream", err)
	}

	var got assembledMessage
	if jsonErr := json.Unmarshal(messageJSON, &got); jsonErr != nil {
		t.Fatalf("partial messageJSON does not parse: %v\n%s", jsonErr, messageJSON)
	}
	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(got.Content))
	}
	// The accumulated partial_json is invalid JSON on its own, so it must be
	// preserved as a JSON string rather than break the document.
	var asString string
	if jsonErr := json.Unmarshal(got.Content[0].Input, &asString); jsonErr != nil {
		t.Errorf("unparseable partial tool_use input should be wrapped as a JSON string: %v (raw: %s)", jsonErr, got.Content[0].Input)
	}
	if !strings.Contains(asString, "file_path") {
		t.Errorf("wrapped partial input lost its content: %q", asString)
	}
}

func TestAssembleSSE_EmptyStream_ReturnsTruncatedErr(t *testing.T) {
	messageJSON, _, _, err := AssembleSSE(nil)
	if !errors.Is(err, ErrTruncatedStream) {
		t.Fatalf("err = %v, want wrapping ErrTruncatedStream", err)
	}
	if !json.Valid(messageJSON) {
		t.Errorf("messageJSON should still be valid JSON for an empty stream: %s", messageJSON)
	}
}

func TestAssembleSSE_ErrorEvent_WrapsUpstreamError(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`
	_, _, _, err := AssembleSSE([]byte(stream))
	if err == nil {
		t.Fatal("expected an error for a stream carrying an error event")
	}
	if !errors.Is(err, ErrUpstreamSSEError) {
		t.Errorf("err = %v, want wrapping ErrUpstreamSSEError", err)
	}
	if !strings.Contains(err.Error(), "Overloaded") {
		t.Errorf("err = %v, want it to include the upstream error message", err)
	}
}

func TestAssembleSSE_ThinkingBlockWithSignature(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`
	messageJSON, _, _, err := AssembleSSE([]byte(stream))
	if err != nil {
		t.Fatalf("AssembleSSE: %v", err)
	}
	var got assembledMessage
	if err := json.Unmarshal(messageJSON, &got); err != nil {
		t.Fatalf("messageJSON does not parse: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(got.Content))
	}
	if got.Content[0].Type != BlockThinking {
		t.Errorf("Content[0].Type = %q, want thinking", got.Content[0].Type)
	}
	if got.Content[0].Thinking != "Let me think." {
		t.Errorf("Content[0].Thinking = %q", got.Content[0].Thinking)
	}
	if got.Content[0].Signature != "sig-abc" {
		t.Errorf("Content[0].Signature = %q", got.Content[0].Signature)
	}
}

func TestAssembleSSE_NoEventLine_FallsBackToJSONType(t *testing.T) {
	// Some captures may lack an explicit "event:" line; the JSON body's own
	// "type" field must still drive dispatch.
	stream := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	messageJSON, _, stopReason, err := AssembleSSE([]byte(stream))
	if err != nil {
		t.Fatalf("AssembleSSE: %v", err)
	}
	if stopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", stopReason)
	}
	var got assembledMessage
	if err := json.Unmarshal(messageJSON, &got); err != nil {
		t.Fatalf("messageJSON does not parse: %v", err)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "hi" {
		t.Errorf("Content = %+v", got.Content)
	}
}
