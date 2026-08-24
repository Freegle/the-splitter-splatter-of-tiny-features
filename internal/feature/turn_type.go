package feature

import (
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// turn_type values, computed by ClassifyTurnType.
const (
	TurnToolResultSummary = "tool_result_summary"
	TurnSingleFileEdit    = "single_file_edit"
	TurnMultiFileEdit     = "multi_file_edit"
	TurnPlan              = "plan"
	TurnQuestionAnswer    = "question_answer"
	TurnOther             = "other"
)

// ClassifyTurnType applies the turn_type rules to a decoded request and its
// response's content blocks, in priority order, first match wins:
//
//  1. the response has >=2 tool_use blocks among the edit-family tools
//     targeting >=2 distinct file paths -> multi_file_edit.
//  2. the response has exactly 1 distinct file among those tools ->
//     single_file_edit.
//  3. the response contains a tool_use named ExitPlanMode, or the system
//     prompt contains "plan mode is active" -> plan.
//  4. the last user message contains a tool_result block and the response
//     is text-only -> tool_result_summary.
//  5. the response is text-only and the last user message is plain text
//     (no tool_result block) -> question_answer.
//  6. otherwise -> other.
func ClassifyTurnType(req anthropic.MessagesRequest, respBlocks []anthropic.ContentBlock) string {
	editBlocks := editFamilyBlocks(respBlocks)
	distinctFiles := distinctFilePathSet(editBlocks)

	if len(editBlocks) >= 2 && len(distinctFiles) >= 2 {
		return TurnMultiFileEdit
	}
	if len(distinctFiles) == 1 {
		return TurnSingleFileEdit
	}
	if hasExitPlanMode(respBlocks) || systemMentionsPlanMode(req.System) {
		return TurnPlan
	}

	lastUser, hasUser := lastUserMessage(req.Messages)
	textOnly := isTextOnly(respBlocks)
	if hasUser && messageHasToolResult(lastUser) && textOnly {
		return TurnToolResultSummary
	}
	if hasUser && textOnly && !messageHasToolResult(lastUser) {
		return TurnQuestionAnswer
	}
	return TurnOther
}

// editFamilyBlocks returns the elements of blocks that are tool_use blocks
// naming an edit-family tool.
func editFamilyBlocks(blocks []anthropic.ContentBlock) []anthropic.ContentBlock {
	var out []anthropic.ContentBlock
	for _, b := range blocks {
		if b.Type == anthropic.BlockToolUse && editFamilyTools[b.Name] {
			out = append(out, b)
		}
	}
	return out
}

// distinctFilePathSet returns the set of distinct file paths named by
// blocks' edit-family tool_use inputs.
func distinctFilePathSet(blocks []anthropic.ContentBlock) map[string]bool {
	set := map[string]bool{}
	for _, b := range blocks {
		if fp := editFilePath(b); fp != "" {
			set[fp] = true
		}
	}
	return set
}

// hasExitPlanMode reports whether blocks contains a tool_use block named
// ExitPlanMode.
func hasExitPlanMode(blocks []anthropic.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == anthropic.BlockToolUse && b.Name == "ExitPlanMode" {
			return true
		}
	}
	return false
}

// systemMentionsPlanMode reports whether the request's system prompt text
// contains "plan mode is active" (case-insensitive).
func systemMentionsPlanMode(system []byte) bool {
	return strings.Contains(strings.ToLower(textFromStringOrBlocks(system)), "plan mode is active")
}

// lastUserMessage returns the last message in msgs with role "user", and
// whether one was found.
func lastUserMessage(msgs []anthropic.Message) (anthropic.Message, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i], true
		}
	}
	return anthropic.Message{}, false
}

// messageHasToolResult reports whether m contains a tool_result block.
func messageHasToolResult(m anthropic.Message) bool {
	for _, b := range m.Content {
		if b.Type == anthropic.BlockToolResult {
			return true
		}
	}
	return false
}

// isTextOnly reports whether blocks contains at least one text block and no
// block of any type other than text or thinking. Extended thinking precedes
// the answer and does not disqualify a response from being text-only.
func isTextOnly(blocks []anthropic.ContentBlock) bool {
	sawText := false
	for _, b := range blocks {
		switch b.Type {
		case anthropic.BlockText:
			sawText = true
		case anthropic.BlockThinking:
			// Does not affect text-only status.
		default:
			return false
		}
	}
	return sawText
}
