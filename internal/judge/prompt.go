package judge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// judgeInstruction is the exact JSON-only instruction appended to every
// judge prompt, from DESIGN.md.
const judgeInstruction = `Answer ONLY JSON {"equivalent": bool, "confidence": 0-1, "reason": "one line"}`

// truncatedSuffix marks a request context BuildPrompt cut short.
const truncatedSuffix = "...[truncated]"

// BuildPrompt assembles the single user-turn judge prompt: the request
// context (truncated to maxContextChars runes), the frontier response, the
// local response, then the JSON-only instruction, each under its own
// labelled section. Only the request context is truncated; both responses
// are included in full.
func BuildPrompt(requestContext, frontierResponse, localResponse string, maxContextChars int) string {
	var b strings.Builder
	b.WriteString("Request context:\n")
	b.WriteString(truncateRunes(requestContext, maxContextChars))
	b.WriteString("\n\nFrontier response:\n")
	b.WriteString(frontierResponse)
	b.WriteString("\n\nLocal response:\n")
	b.WriteString(localResponse)
	b.WriteString("\n\n")
	b.WriteString(judgeInstruction)
	return b.String()
}

// truncateRunes returns s unchanged when it has maxRunes runes or fewer, or
// when maxRunes <= 0 (no limit); otherwise it returns the first maxRunes
// runes plus truncatedSuffix. Rune based so a multi-byte character is
// never split.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + truncatedSuffix
}

// responseMessage is the subset of a captured or replayed Anthropic
// message ExtractResponseText reads: only its content blocks matter here.
type responseMessage struct {
	Content []anthropic.ContentBlock `json:"content"`
}

// ExtractResponseText renders a complete Anthropic message JSON (as stored
// in calls.response_zstd or replays.response_zstd, once decompressed) as
// the plain text a judge prompt embeds: each text block's text, and each
// tool_use block rendered as "tool_use <name>(<input JSON>)", one per line
// in the message's original block order. A message that fails to decode
// falls back to its raw bytes, so a malformed stored message still
// produces a prompt rather than an error.
func ExtractResponseText(messageJSON []byte) string {
	var msg responseMessage
	if err := json.Unmarshal(messageJSON, &msg); err != nil {
		return string(messageJSON)
	}

	var parts []string
	for _, b := range msg.Content {
		switch b.Type {
		case anthropic.BlockText:
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case anthropic.BlockToolUse:
			input := "{}"
			if len(b.Input) > 0 {
				input = string(b.Input)
			}
			parts = append(parts, fmt.Sprintf("tool_use %s(%s)", b.Name, input))
		}
	}
	return strings.Join(parts, "\n")
}
