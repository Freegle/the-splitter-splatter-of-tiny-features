package feature

import "github.com/freegle/splitter/internal/anthropic"

// RequestOnly infers turn_type and subsystem for a live request, before its
// response exists: the input Phase 4 live routing needs to look up a
// router_state category, cheaply, from the incoming request alone.
//
// It cannot classify single_file_edit or multi_file_edit: those turn_type
// values describe which tools the pending response calls, which is not
// knowable before that response exists (unlike ClassifyTurnType, which
// reads them off an already-captured response). Given only req, it
// returns:
//
//   - "plan" when the system prompt contains "plan mode is active"
//     (case-insensitive), the one ClassifyTurnType rule that is itself
//     entirely request-derived.
//   - "tool_result_summary" when the last message is from the user and
//     carries a tool_result block: the shape every summary-after-a-tool-call
//     turn has, regardless of what the upcoming response turns out to be.
//   - "question_answer" when the last message is from the user and is
//     plain text (no tool_result block).
//   - "other" otherwise (no user message at all).
//
// subsystem is the top-level path segment of the most recently touched
// file across every assistant message already present in req's history
// (Claude Code resends the full session transcript on every turn, so an
// earlier turn's edits are visible here even though the response to req
// itself is unknown): the file the session is currently working in, ""
// when the history names none.
func RequestOnly(req anthropic.MessagesRequest, repoPath string) (turnType, subsystem string) {
	if systemMentionsPlanMode(req.System) {
		return TurnPlan, requestSubsystem(req, repoPath)
	}

	lastUser, hasUser := lastUserMessage(req.Messages)
	switch {
	case hasUser && messageHasToolResult(lastUser):
		turnType = TurnToolResultSummary
	case hasUser:
		turnType = TurnQuestionAnswer
	default:
		turnType = TurnOther
	}
	return turnType, requestSubsystem(req, repoPath)
}

// requestSubsystem returns Subsystem applied to the most recently touched
// file across every assistant message in req.Messages, in message order
// (so a later assistant turn's edits take precedence over an earlier one's
// when both touched different top-level areas).
func requestSubsystem(req anthropic.MessagesRequest, repoPath string) string {
	var lastTouched string
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		if touched := FilesTouched(m.Content, repoPath); len(touched) > 0 {
			lastTouched = touched[len(touched)-1]
		}
	}
	if lastTouched == "" {
		return ""
	}
	return Subsystem([]string{lastTouched})
}
