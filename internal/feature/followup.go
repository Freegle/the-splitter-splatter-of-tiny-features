package feature

import (
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// errorFollowupPrefixChars bounds how much of a tool_result's text is
// inspected for an error signal.
const errorFollowupPrefixChars = 200

// HasErrorFollowup reports whether nextReq, the next call in the same
// session after the call that produced filesTouched, shows the cheap
// error-followup signal:
//
//   - a tool_result block with is_error true, or
//   - a tool_result block whose text starts with "error" (case-insensitive)
//     within its first errorFollowupPrefixChars characters, or
//   - an edit-family tool_use targeting one of the same repo-relative files
//     this response edited.
func HasErrorFollowup(filesTouched []string, nextReq anthropic.MessagesRequest, repoPath string) bool {
	touched := make(map[string]bool, len(filesTouched))
	for _, f := range filesTouched {
		touched[f] = true
	}

	for _, msg := range nextReq.Messages {
		for _, b := range msg.Content {
			switch b.Type {
			case anthropic.BlockToolResult:
				if b.IsError || toolResultTextLooksLikeError(b.ToolContent) {
					return true
				}
			case anthropic.BlockToolUse:
				if !editFamilyTools[b.Name] {
					continue
				}
				fp := editFilePath(b)
				if fp != "" && touched[repoRelative(fp, repoPath)] {
					return true
				}
			}
		}
	}
	return false
}

// toolResultTextLooksLikeError reports whether raw's text, truncated to
// errorFollowupPrefixChars, starts with "error" once trimmed and
// lowercased.
func toolResultTextLooksLikeError(raw []byte) bool {
	text := textFromStringOrBlocks(raw)
	if len(text) > errorFollowupPrefixChars {
		text = text[:errorFollowupPrefixChars]
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "error")
}
