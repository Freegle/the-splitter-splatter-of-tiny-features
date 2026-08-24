package agentic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
)

func TestRunLoop_ModelStopsWithoutToolCall(t *testing.T) {
	e, _ := newTestExecutor(t, "")
	replay := scriptedReplay(t, [][]byte{
		buildAssistantResponse(t, "done, no changes needed"),
	})

	result := RunLoop(context.Background(), replay, e, "do nothing", TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute})

	if result.StopReason != StopModelDone {
		t.Errorf("StopReason = %q, want %q", result.StopReason, StopModelDone)
	}
	if result.Turns != 1 {
		t.Errorf("Turns = %d, want 1", result.Turns)
	}
	if result.bounded() {
		t.Errorf("a model-finished loop should not be considered bounded/failed")
	}
	if !strings.Contains(string(result.TranscriptJSON), "done, no changes needed") {
		t.Errorf("transcript missing the model's final text: %s", result.TranscriptJSON)
	}
}

func TestRunLoop_ToolCallsThenStop(t *testing.T) {
	e, _ := newTestExecutor(t, "")
	replay := scriptedReplay(t, [][]byte{
		buildAssistantResponse(t, "", toolCallSpec{Name: toolReadFile, Input: map[string]any{"file_path": "greet.go"}}),
		buildAssistantResponse(t, "", toolCallSpec{Name: toolEdit, Input: map[string]any{"file_path": "greet.go", "old_string": `"hi"`, "new_string": `"hello"`}}),
		buildAssistantResponse(t, "done"),
	})

	result := RunLoop(context.Background(), replay, e, "fix the greeting", TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute})

	if result.StopReason != StopModelDone {
		t.Errorf("StopReason = %q, want %q", result.StopReason, StopModelDone)
	}
	if result.Turns != 3 {
		t.Errorf("Turns = %d, want 3", result.Turns)
	}
	if result.TokensIn != 30 || result.TokensOut != 30 {
		t.Errorf("tokens = (%d,%d), want (30,30) accumulated over 3 turns", result.TokensIn, result.TokensOut)
	}
}

func TestRunLoop_MaxTurnsBoundStopsWithReason(t *testing.T) {
	e, _ := newTestExecutor(t, "")
	// Script far more tool-calling turns than the bound allows; the loop
	// must stop itself rather than exhausting the script.
	var responses [][]byte
	for i := 0; i < 10; i++ {
		responses = append(responses, buildAssistantResponse(t, "", toolCallSpec{Name: toolReadFile, Input: map[string]any{"file_path": "greet.go"}}))
	}
	replay := scriptedReplay(t, responses)

	result := RunLoop(context.Background(), replay, e, "keep going forever", TaskBounds{MaxTurns: 3, MaxTaskTokens: 200000, WallClock: time.Minute})

	if result.StopReason != StopMaxTurns {
		t.Errorf("StopReason = %q, want %q", result.StopReason, StopMaxTurns)
	}
	if result.Turns != 3 {
		t.Errorf("Turns = %d, want exactly the bound (3)", result.Turns)
	}
	if result.Error == "" {
		t.Errorf("expected a reason recorded in Error for a bound-exceeded stop")
	}
	if !result.bounded() {
		t.Errorf("a max_turns stop should be considered bounded/failed")
	}
}

func TestRunLoop_MaxTaskTokensBoundStopsWithReason(t *testing.T) {
	e, _ := newTestExecutor(t, "")
	var responses [][]byte
	for i := 0; i < 10; i++ {
		responses = append(responses, buildAssistantResponse(t, "", toolCallSpec{Name: toolReadFile, Input: map[string]any{"file_path": "greet.go"}}))
	}
	replay := scriptedReplay(t, responses)

	// Each scripted response reports 10 input + 10 output tokens; a budget
	// of 25 should stop after the third turn (30 accumulated >= 25).
	result := RunLoop(context.Background(), replay, e, "keep going forever", TaskBounds{MaxTurns: 20, MaxTaskTokens: 25, WallClock: time.Minute})

	if result.StopReason != StopMaxTokens {
		t.Errorf("StopReason = %q, want %q", result.StopReason, StopMaxTokens)
	}
	if !result.bounded() {
		t.Errorf("a max_tokens stop should be considered bounded/failed")
	}
}

func TestRunLoop_RequestsCarryMaxAnswerTokens(t *testing.T) {
	e, _ := newTestExecutor(t, "")
	var seenMaxTokens []int
	replay := func(ctx context.Context, req anthropic.MessagesRequest) ([]byte, int64, int64, error) {
		seenMaxTokens = append(seenMaxTokens, req.MaxTokens)
		return buildAssistantResponse(t, "done"), 10, 10, nil
	}

	RunLoop(context.Background(), replay, e, "brief", TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute, MaxAnswerTokens: 9000})

	if len(seenMaxTokens) != 1 || seenMaxTokens[0] != 9000 {
		t.Errorf("seenMaxTokens = %v, want [9000]", seenMaxTokens)
	}
}

func TestRunLoop_UnsetMaxAnswerTokensFallsBackToDefault(t *testing.T) {
	e, _ := newTestExecutor(t, "")
	var seenMaxTokens []int
	replay := func(ctx context.Context, req anthropic.MessagesRequest) ([]byte, int64, int64, error) {
		seenMaxTokens = append(seenMaxTokens, req.MaxTokens)
		return buildAssistantResponse(t, "done"), 10, 10, nil
	}

	// A zero-valued TaskBounds (as if a caller forgot BoundsFromConfig)
	// must never send max_tokens=0, which internal/backend.ToOpenAI would
	// otherwise quietly reinterpret as its own 4096 default.
	RunLoop(context.Background(), replay, e, "brief", TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute})

	if len(seenMaxTokens) != 1 || seenMaxTokens[0] != defaultMaxAnswerTokens {
		t.Errorf("seenMaxTokens = %v, want [%d]", seenMaxTokens, defaultMaxAnswerTokens)
	}
}

func TestBoundsFromConfig_MaxAnswerTokens(t *testing.T) {
	got := BoundsFromConfig(config.EvalsConfig{MaxAnswerTokens: 32000})
	if got.MaxAnswerTokens != 32000 {
		t.Errorf("MaxAnswerTokens = %d, want 32000", got.MaxAnswerTokens)
	}

	got = BoundsFromConfig(config.EvalsConfig{})
	if got.MaxAnswerTokens != defaultMaxAnswerTokens {
		t.Errorf("MaxAnswerTokens (unset) = %d, want default %d", got.MaxAnswerTokens, defaultMaxAnswerTokens)
	}
}

func TestRunLoop_BackendErrorStopsWithReason(t *testing.T) {
	e, _ := newTestExecutor(t, "")
	replay := func(ctx context.Context, req anthropic.MessagesRequest) ([]byte, int64, int64, error) {
		return nil, 0, 0, errors.New("backend unavailable")
	}
	result := RunLoop(context.Background(), replay, e, "brief", TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute})

	if result.StopReason != StopBackendError {
		t.Errorf("StopReason = %q, want %q", result.StopReason, StopBackendError)
	}
	if result.Turns != 0 {
		t.Errorf("Turns = %d, want 0 (the failing call never completed a turn)", result.Turns)
	}
	if !result.bounded() {
		t.Errorf("a backend_error stop should be considered bounded/failed")
	}
}
