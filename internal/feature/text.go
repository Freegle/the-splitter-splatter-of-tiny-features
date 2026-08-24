// Package feature implements the Phase 2 featuriser: it tags each logged
// call with routing-relevant features (turn_type, files_touched,
// subsystem, token counts, had_error_followup) and stores them in the
// features table.
package feature

import (
	"encoding/json"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// textFromStringOrBlocks extracts plain text from a field that is, on the
// wire, either a bare JSON string or an array of content blocks: the shape
// shared by MessagesRequest.System and a tool_result block's content.
// Non-text blocks are ignored. An empty or unparseable value yields "".
func textFromStringOrBlocks(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '"' {
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
