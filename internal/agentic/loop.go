package agentic

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
)

// TaskBounds bounds one agentic task's tool loop, per DESIGN.md.
type TaskBounds struct {
	MaxTurns      int
	MaxTaskTokens int64
	WallClock     time.Duration
	// MaxAnswerTokens is the max_tokens sent with every request in the
	// loop (see internal/evals.applyMaxAnswerTokensFloor's sibling
	// rationale): a reasoning backend's reasoning tokens bill as output,
	// so a low per-turn max_tokens can be exhausted before the model
	// calls a tool or answers at all.
	MaxAnswerTokens int
}

// defaults, mirrored from DESIGN.md and internal/config.Default's [evals]
// section, applied when a zero-valued config.EvalsConfig is given (an
// unset [evals] section should never disable bounds entirely).
const (
	defaultMaxTurns         = 20
	defaultMaxTaskTokens    = 200000
	defaultWallClockMinutes = 10
	defaultMaxAnswerTokens  = 16384
)

// BoundsFromConfig builds TaskBounds from cfg, falling back to DESIGN.md's
// defaults for any zero-valued field.
func BoundsFromConfig(cfg config.EvalsConfig) TaskBounds {
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	maxTokens := cfg.MaxTaskTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTaskTokens
	}
	wallMinutes := cfg.WallClockMinutes
	if wallMinutes <= 0 {
		wallMinutes = defaultWallClockMinutes
	}
	maxAnswerTokens := cfg.MaxAnswerTokens
	if maxAnswerTokens <= 0 {
		maxAnswerTokens = defaultMaxAnswerTokens
	}
	return TaskBounds{
		MaxTurns: maxTurns, MaxTaskTokens: maxTokens, WallClock: time.Duration(wallMinutes) * time.Minute,
		MaxAnswerTokens: maxAnswerTokens,
	}
}

// systemPrompt is the minimal coding-agent system prompt every agentic
// task's loop carries.
const systemPrompt = `You are a coding agent working inside a sandboxed checkout of a real repository. Use the available tools (read_file, list_dir, grep, edit, write, run_tests) to understand the codebase and make the change described in the task. Call run_tests to check your work, and keep iterating until the tests pass or you are confident no further progress is possible. You have no access to git and no network access beyond these tools.`

// Loop stop reasons, recorded in LoopResult.StopReason.
const (
	StopModelDone    = "model_stopped"
	StopMaxTurns     = "max_turns"
	StopMaxTokens    = "max_tokens"
	StopWallClock    = "wall_clock"
	StopBackendError = "backend_error"
)

// LoopResult is one task's completed tool loop.
type LoopResult struct {
	Turns          int
	TokensIn       int64
	TokensOut      int64
	TranscriptJSON []byte
	StopReason     string
	Error          string
}

// bounded reports whether r stopped because a bound was exceeded rather
// than the model itself finishing: DESIGN.md "exceeding any bound = fail
// with reason recorded".
func (r *LoopResult) bounded() bool {
	switch r.StopReason {
	case StopMaxTurns, StopMaxTokens, StopWallClock, StopBackendError:
		return true
	default:
		return false
	}
}

// RunLoop drives complete (internal/evals.BuildRunBackend's resolved
// backend call, so both OpenAI-compatible and anthropic-kind backends work
// unchanged) through the tool loop against exec's sandbox, starting from
// brief, until the model stops calling tools or a bound in bounds is
// exceeded.
func RunLoop(ctx context.Context, complete evals.ReplayFunc, exec *ToolExecutor, brief string, bounds TaskBounds) *LoopResult {
	deadline := time.Now().Add(bounds.WallClock)
	lctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	systemJSON, _ := json.Marshal(systemPrompt)
	messages := []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: brief}}}}
	tools := toolDefinitions()

	var tokensIn, tokensOut int64
	turns := 0
	stopReason := StopModelDone
	loopErr := ""

loop:
	for {
		switch {
		case turns >= bounds.MaxTurns:
			stopReason = StopMaxTurns
			loopErr = fmt.Sprintf("exceeded max_turns (%d)", bounds.MaxTurns)
			break loop
		case bounds.MaxTaskTokens > 0 && tokensIn+tokensOut >= bounds.MaxTaskTokens:
			stopReason = StopMaxTokens
			loopErr = fmt.Sprintf("exceeded max_task_tokens (%d)", bounds.MaxTaskTokens)
			break loop
		case time.Now().After(deadline):
			stopReason = StopWallClock
			loopErr = "exceeded wall clock budget"
			break loop
		}

		maxAnswerTokens := bounds.MaxAnswerTokens
		if maxAnswerTokens <= 0 {
			maxAnswerTokens = defaultMaxAnswerTokens
		}
		req := anthropic.MessagesRequest{
			System:    json.RawMessage(systemJSON),
			Messages:  messages,
			Tools:     tools,
			MaxTokens: maxAnswerTokens,
		}
		respJSON, tin, tout, err := complete(lctx, req)
		tokensIn += tin
		tokensOut += tout
		if err != nil {
			stopReason = StopBackendError
			loopErr = err.Error()
			break loop
		}
		turns++

		var respMsg struct {
			Content []anthropic.ContentBlock `json:"content"`
		}
		if err := json.Unmarshal(respJSON, &respMsg); err != nil {
			stopReason = StopBackendError
			loopErr = fmt.Sprintf("decoding backend response: %v", err)
			break loop
		}
		messages = append(messages, anthropic.Message{Role: "assistant", Content: respMsg.Content})

		calls := toolUseBlocks(respMsg.Content)
		if len(calls) == 0 {
			break loop
		}

		var resultBlocks []anthropic.ContentBlock
		for _, call := range calls {
			text, isErr := exec.Execute(lctx, call.Name, call.Input)
			contentJSON, _ := json.Marshal(text)
			resultBlocks = append(resultBlocks, anthropic.ContentBlock{
				Type:        anthropic.BlockToolResult,
				ToolUseID:   call.ID,
				ToolContent: json.RawMessage(contentJSON),
				IsError:     isErr,
			})
		}
		messages = append(messages, anthropic.Message{Role: "user", Content: resultBlocks})
	}

	transcriptJSON, _ := json.Marshal(messages)
	return &LoopResult{
		Turns: turns, TokensIn: tokensIn, TokensOut: tokensOut,
		TranscriptJSON: transcriptJSON, StopReason: stopReason, Error: loopErr,
	}
}
