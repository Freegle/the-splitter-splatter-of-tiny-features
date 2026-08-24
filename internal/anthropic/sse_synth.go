package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// sseSourceMessage is the shape SynthesizeSSE decodes its input from: the
// same message shape AssembleSSE produces and internal/backend.FromOpenAI
// produces, so either can feed a synthesized stream.
type sseSourceMessage struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

// SynthesizeSSE builds a valid Anthropic SSE event stream that represents
// messageJSON (a complete message, in the shape AssembleSSE returns):
// message_start (skeleton plus input usage), then for each content block a
// content_block_start/delta/stop trio (a tool_use block's input is sent as
// one input_json_delta), then message_delta (stop_reason and output
// usage), then message_stop. It is the inverse of AssembleSSE: running
// AssembleSSE on the result reconstructs a message equivalent to the input.
// Used by Phase 4 live routing to answer a client request that asked for
// streaming from a non-streaming backend response.
func SynthesizeSSE(messageJSON []byte) ([]byte, error) {
	var msg sseSourceMessage
	if err := json.Unmarshal(messageJSON, &msg); err != nil {
		return nil, fmt.Errorf("decoding message for SSE synthesis: %w", err)
	}

	var buf bytes.Buffer

	if err := writeMessageStart(&buf, msg); err != nil {
		return nil, err
	}
	for i, block := range msg.Content {
		if err := writeContentBlock(&buf, i, block); err != nil {
			return nil, err
		}
	}
	if err := writeMessageDelta(&buf, msg); err != nil {
		return nil, err
	}
	if err := writeEvent(&buf, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// writeMessageStart emits the message_start event. The nested message's
// stop_reason and stop_sequence are always null (not yet known this early
// in a real stream) and its content array is always empty (content is
// rebuilt from the content_block_* events that follow); only input-side
// usage fields are carried, matching what a real message_start reports
// before any output has been generated.
func writeMessageStart(buf *bytes.Buffer, msg sseSourceMessage) error {
	skeleton := struct {
		ID           string         `json:"id"`
		Type         string         `json:"type"`
		Role         string         `json:"role"`
		Model        string         `json:"model"`
		Content      []ContentBlock `json:"content"`
		StopReason   *string        `json:"stop_reason"`
		StopSequence *string        `json:"stop_sequence"`
		Usage        Usage          `json:"usage"`
	}{
		ID:      msg.ID,
		Type:    firstNonEmpty(msg.Type, "message"),
		Role:    firstNonEmpty(msg.Role, "assistant"),
		Model:   msg.Model,
		Content: []ContentBlock{},
		Usage: Usage{
			InputTokens:              msg.Usage.InputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		},
	}
	return writeEvent(buf, "message_start", map[string]any{
		"type":    "message_start",
		"message": skeleton,
	})
}

// writeContentBlock emits the content_block_start/delta*/stop trio for one
// content block at the given index.
func writeContentBlock(buf *bytes.Buffer, index int, block ContentBlock) error {
	if err := writeEvent(buf, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": startShape(block),
	}); err != nil {
		return err
	}

	for _, delta := range deltaPayloads(block) {
		if err := writeEvent(buf, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": delta,
		}); err != nil {
			return err
		}
	}

	return writeEvent(buf, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
}

// startShape returns the content_block payload for a content_block_start
// event: for the block types that stream incrementally (text, tool_use,
// thinking) it is the empty skeleton a real stream opens with, since the
// following deltas carry the actual content. Any other block type
// (tool_result, image, or an unknown/raw type) has no defined streaming
// delta shape, so it is sent whole here and no delta events follow;
// AssembleSSE accepts a content_block_start carrying a complete block.
func startShape(block ContentBlock) any {
	switch block.Type {
	case BlockText:
		return ContentBlock{Type: BlockText, Text: ""}
	case BlockToolUse:
		return ContentBlock{Type: BlockToolUse, ID: block.ID, Name: block.Name, Input: json.RawMessage("{}")}
	case BlockThinking:
		return ContentBlock{Type: BlockThinking, Thinking: "", Signature: ""}
	default:
		return block
	}
}

// deltaPayloads returns the content_block_delta "delta" payloads for one
// block, in emission order. A tool_use block's entire input is sent as one
// input_json_delta. A thinking block emits a thinking_delta followed by a
// signature_delta only when a signature is present. Block types with no
// incremental shape (see startShape) emit no deltas.
func deltaPayloads(block ContentBlock) []map[string]any {
	switch block.Type {
	case BlockText:
		if block.Text == "" {
			return nil
		}
		return []map[string]any{
			{"type": "text_delta", "text": block.Text},
		}
	case BlockToolUse:
		input := block.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return []map[string]any{
			{"type": "input_json_delta", "partial_json": string(input)},
		}
	case BlockThinking:
		var deltas []map[string]any
		if block.Thinking != "" {
			deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": block.Thinking})
		}
		if block.Signature != "" {
			deltas = append(deltas, map[string]any{"type": "signature_delta", "signature": block.Signature})
		}
		return deltas
	default:
		return nil
	}
}

// writeMessageDelta emits the message_delta event carrying the final
// stop_reason and output-side usage.
func writeMessageDelta(buf *bytes.Buffer, msg sseSourceMessage) error {
	var stopReason *string
	if msg.StopReason != "" {
		stopReason = &msg.StopReason
	}
	return writeEvent(buf, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": msg.StopSequence,
		},
		"usage": Usage{OutputTokens: msg.Usage.OutputTokens},
	})
}

// writeEvent appends one SSE block ("event: <name>\ndata: <json>\n\n") to
// buf.
func writeEvent(buf *bytes.Buffer, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling %s event for SSE synthesis: %w", event, err)
	}
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return nil
}

// firstNonEmpty returns a, or b when a is empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
