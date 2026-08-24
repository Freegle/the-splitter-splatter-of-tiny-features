// Package anthropic holds the subset of Anthropic Messages API types
// splitter needs, plus assembly of a complete message from a captured SSE
// stream.
package anthropic

import (
	"encoding/json"
	"fmt"
)

// Known content block types. Any other value seen on the wire is treated as
// unknown and preserved verbatim (see ContentBlock).
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
	BlockThinking   = "thinking"
	BlockImage      = "image"
)

// Usage is the Messages API token accounting block.
type Usage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// Tool is one entry of MessagesRequest.Tools.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ContentBlock is one element of a message's content array. Fields for the
// known block types (text, tool_use, tool_result, thinking, image) are
// populated directly for convenient reading. A block of any other type is
// decoded with Raw set to its exact original bytes and is re-emitted from
// Raw verbatim on marshal, so unknown block types are never dropped by a
// decode/encode round trip.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID   string          `json:"tool_use_id,omitempty"`
	ToolContent json.RawMessage `json:"content,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// image
	Source json.RawMessage `json:"source,omitempty"`

	// Raw holds the original bytes for a block of an unrecognised type.
	// Empty for known types, which marshal from the fields above instead.
	Raw json.RawMessage `json:"-"`
}

// contentBlockAlias has the same fields as ContentBlock but none of its
// methods, so it can be used inside UnmarshalJSON/MarshalJSON without
// recursing.
type contentBlockAlias ContentBlock

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var a contentBlockAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("decoding content block: %w", err)
	}
	*b = ContentBlock(a)
	switch b.Type {
	case BlockText, BlockToolUse, BlockToolResult, BlockThinking, BlockImage:
		b.Raw = nil
	default:
		b.Raw = append(json.RawMessage(nil), data...)
	}
	return nil
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	return json.Marshal(contentBlockAlias(b))
}

// Message is one turn in MessagesRequest.Messages. Content is normalised to
// a slice of ContentBlock: a bare wire-format string becomes a single text
// block.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decoding message: %w", err)
	}
	m.Role = raw.Role

	if len(raw.Content) == 0 {
		m.Content = nil
		return nil
	}
	if raw.Content[0] == '"' {
		var text string
		if err := json.Unmarshal(raw.Content, &text); err != nil {
			return fmt.Errorf("decoding string message content: %w", err)
		}
		m.Content = []ContentBlock{{Type: BlockText, Text: text}}
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw.Content, &blocks); err != nil {
		return fmt.Errorf("decoding message content blocks: %w", err)
	}
	m.Content = blocks
	return nil
}

// MessagesRequest is the subset of a POST /v1/messages request body
// splitter inspects or translates to another backend's format.
type MessagesRequest struct {
	Model string `json:"model"`

	// System is either a plain string or an array of content blocks on the
	// wire; kept raw so both shapes round-trip without loss.
	System json.RawMessage `json:"system,omitempty"`

	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Stream    bool      `json:"stream,omitempty"`

	// Metadata is passed through raw; callers that need e.g. metadata.user_id
	// decode it further themselves.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}
