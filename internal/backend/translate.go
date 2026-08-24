package backend

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// defaultMaxTokens is used for ToOpenAI when the Anthropic request carries
// no max_tokens (Anthropic requires it, so this only matters for requests
// synthesised or edited by earlier splitter stages).
const defaultMaxTokens = 4096

// imageOmittedMarker replaces every Anthropic image block: OpenAI-compatible
// chat completions backends in scope here are text-only.
const imageOmittedMarker = "[image omitted]"

// anthropicMessage is the shape FromOpenAI marshals its result into. It
// mirrors the message skeleton AssembleSSE produces so a translated
// response and a captured one are interchangeable.
type anthropicMessage struct {
	ID         string                   `json:"id,omitempty"`
	Type       string                   `json:"type,omitempty"`
	Role       string                   `json:"role,omitempty"`
	Model      string                   `json:"model,omitempty"`
	Content    []anthropic.ContentBlock `json:"content"`
	StopReason string                   `json:"stop_reason,omitempty"`
	Usage      anthropic.Usage          `json:"usage"`
}

// ToOpenAI translates an Anthropic Messages API request into an
// OpenAI-compatible chat completions request:
//
//   - system (string or content-block array) becomes the first message,
//     role "system", with its text blocks concatenated.
//   - a user message's tool_result blocks become separate role "tool"
//     messages carrying tool_call_id; its remaining text/image blocks
//     become a role "user" message, in the original block order.
//   - an assistant message's text blocks concatenate into one message's
//     content; its tool_use blocks become that same message's tool_calls,
//     with input JSON-encoded into the arguments string.
//   - thinking blocks are dropped.
//   - image blocks become the text imageOmittedMarker.
//   - Anthropic tools become OpenAI function tools (input_schema becomes
//     parameters verbatim).
//   - temperature is always 0, for reproducibility.
//   - max_tokens passes through, defaulting to defaultMaxTokens when the
//     source request has none.
func ToOpenAI(req anthropic.MessagesRequest, model string) ChatRequest {
	out := ChatRequest{
		Model:       model,
		Temperature: 0,
		MaxTokens:   req.MaxTokens,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultMaxTokens
	}

	if sys := systemText(req.System); sys != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: "system", Content: sys})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "assistant":
			out.Messages = append(out.Messages, assistantChatMessage(m))
		case "user":
			out.Messages = append(out.Messages, userChatMessages(m)...)
		}
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, ChatTool{
			Type: "function",
			Function: ChatFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	return out
}

// systemText extracts the concatenated text of an Anthropic request's
// System field, which is either a bare JSON string or an array of content
// blocks on the wire. Non-text blocks (there is no defined non-text system
// block type today) are ignored.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}

	var blocks []anthropic.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == anthropic.BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// assistantChatMessage translates one Anthropic assistant message into a
// single OpenAI assistant message: all its text blocks concatenate into
// Content, all its tool_use blocks become ToolCalls, thinking blocks are
// dropped, and image blocks (unusual on an assistant turn but not
// disallowed on the wire) become the omitted marker text.
func assistantChatMessage(m anthropic.Message) ChatMessage {
	out := ChatMessage{Role: "assistant"}
	var textParts []string

	for _, b := range m.Content {
		switch b.Type {
		case anthropic.BlockText:
			textParts = append(textParts, b.Text)
		case anthropic.BlockImage:
			textParts = append(textParts, imageOmittedMarker)
		case anthropic.BlockToolUse:
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			out.ToolCalls = append(out.ToolCalls, ChatToolCall{
				ID:   b.ID,
				Type: "function",
				Function: ChatToolCallFunc{
					Name:      b.Name,
					Arguments: args,
				},
			})
		case anthropic.BlockThinking:
			// Dropped: not meaningful to a non-Anthropic backend.
		}
	}

	out.Content = strings.Join(textParts, "\n")
	return out
}

// userChatMessages translates one Anthropic user message into zero or more
// OpenAI messages, preserving block order: consecutive text/image blocks
// accumulate into a role "user" message emitted just before the next
// tool_result (or at the end of the source message), and each tool_result
// block becomes its own role "tool" message.
func userChatMessages(m anthropic.Message) []ChatMessage {
	var out []ChatMessage
	var textParts []string

	flush := func() {
		if len(textParts) == 0 {
			return
		}
		out = append(out, ChatMessage{Role: "user", Content: strings.Join(textParts, "\n")})
		textParts = nil
	}

	for _, b := range m.Content {
		switch b.Type {
		case anthropic.BlockText:
			textParts = append(textParts, b.Text)
		case anthropic.BlockImage:
			textParts = append(textParts, imageOmittedMarker)
		case anthropic.BlockToolResult:
			flush()
			out = append(out, ChatMessage{
				Role:       "tool",
				Content:    toolResultText(b.ToolContent),
				ToolCallID: b.ToolUseID,
			})
		case anthropic.BlockThinking:
			// Dropped: never valid on a user turn, tolerated defensively.
		}
	}
	flush()

	return out
}

// toolResultText extracts the text of a tool_result block's content, which
// is either a bare JSON string or an array of content blocks on the wire.
// Image blocks within it become the omitted marker text.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return string(raw)
	}

	var blocks []anthropic.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case anthropic.BlockText:
			parts = append(parts, b.Text)
		case anthropic.BlockImage:
			parts = append(parts, imageOmittedMarker)
		}
	}
	return strings.Join(parts, "\n")
}

// FromOpenAI translates an OpenAI-compatible chat completions response into
// a complete Anthropic message JSON: choices[0].message.content becomes a
// text block, each tool_calls entry becomes a tool_use block with its
// arguments JSON-decoded into Input and its id preserved, usage maps
// prompt_tokens/completion_tokens to input/output tokens, and finish_reason
// maps stop/tool_calls/length to end_turn/tool_use/max_tokens.
func FromOpenAI(resp ChatResponse) ([]byte, error) {
	content := []anthropic.ContentBlock{}
	stopReason := mapFinishReason("")

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		stopReason = mapFinishReason(choice.FinishReason)

		if choice.Message.Content != "" {
			content = append(content, anthropic.ContentBlock{
				Type: anthropic.BlockText,
				Text: choice.Message.Content,
			})
		}

		for _, tc := range choice.Message.ToolCalls {
			content = append(content, anthropic.ContentBlock{
				Type:  anthropic.BlockToolUse,
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: decodeToolArguments(tc.Function.Arguments),
			})
		}
	}

	msg := anthropicMessage{
		ID:         resp.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      resp.Model,
		Content:    content,
		StopReason: stopReason,
		Usage: anthropic.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}

	b, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling translated anthropic message: %w", err)
	}
	return b, nil
}

// decodeToolArguments parses an OpenAI tool call's JSON-encoded arguments
// string into a tool_use block's Input. An empty string becomes "{}"; a
// string that is not valid JSON (a malformed backend response) is preserved
// as a JSON string instead of breaking the translated message, matching
// how internal/anthropic.AssembleSSE preserves unparseable tool_use input.
func decodeToolArguments(arguments string) json.RawMessage {
	if arguments == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(arguments)) {
		return json.RawMessage(arguments)
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return json.RawMessage(`"[unparseable tool arguments]"`)
	}
	return encoded
}

// mapFinishReason maps an OpenAI finish_reason to an Anthropic stop_reason.
// Anything other than "tool_calls" or "length" (including "stop" and an
// unrecognised or empty value) maps to "end_turn".
func mapFinishReason(reason string) string {
	switch reason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}
