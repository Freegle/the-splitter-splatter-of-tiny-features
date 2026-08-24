package verify

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// messageContent decodes just the content array of a complete Anthropic
// message JSON (the shape internal/anthropic.AssembleSSE and
// internal/backend.FromOpenAI both produce).
type messageContent struct {
	Content []anthropic.ContentBlock `json:"content"`
}

// concatenatedContent extracts the text a message conveys: every text
// block's text, and every tool_use block's name plus its raw input JSON,
// concatenated in content order. Other block types (tool_result, thinking,
// image, unknown) never appear in an assistant response and are ignored.
func concatenatedContent(msgJSON []byte) (string, error) {
	var msg messageContent
	if err := json.Unmarshal(msgJSON, &msg); err != nil {
		return "", fmt.Errorf("decoding message content: %w", err)
	}
	var sb strings.Builder
	for _, b := range msg.Content {
		switch b.Type {
		case anthropic.BlockText:
			sb.WriteString(b.Text)
			sb.WriteString("\n")
		case anthropic.BlockToolUse:
			sb.WriteString(b.Name)
			sb.WriteString(" ")
			sb.Write(b.Input)
			sb.WriteString("\n")
		}
	}
	return sb.String(), nil
}

// normalizeWhitespace collapses every run of whitespace to a single space
// and trims the result. Used by the cascade's exact-match stage and as the
// shared basis for non-edit token similarity.
func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
