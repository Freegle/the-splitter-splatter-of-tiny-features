package agentic

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/backend"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
	"github.com/freegle/splitter/internal/store"
)

// greetRepoFiles builds a small Go package with one buggy function
// (Greet ignores its name parameter) and one already-passing test in a
// sibling file (TestUtilAlwaysTrue), the fixture every fail-to-pass /
// regression scenario below shares.
func greetRepoFiles() map[string]string {
	return map[string]string{
		"greet/greet.go":     "package greet\n\nfunc Greet(name string) string {\n\treturn \"Hello, World!\"\n}\n",
		"greet/util.go":      "package greet\n\nfunc AlwaysTrue() bool { return true }\n",
		"greet/util_test.go": "package greet\n\nimport \"testing\"\n\nfunc TestUtilAlwaysTrue(t *testing.T) {\n\tif !AlwaysTrue() {\n\t\tt.Fatal(\"expected true\")\n\t}\n}\n",
	}
}

const greetHeldOutTestContent = "package greet\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif got := Greet(\"Alice\"); got != \"Hello, Alice!\" {\n\t\tt.Fatalf(\"got %q\", got)\n\t}\n}\n"

func buildGreetHoldoutTask(t *testing.T, commit string) (store.EvalTaskRow, string) {
	t.Helper()
	holdout := evals.HoldoutPayload{
		Files:   []evals.HoldoutFile{{Path: "greet/greet_test.go", IsNew: true, Content: greetHeldOutTestContent}},
		TestCmd: "go test -json ./greet/...",
	}
	holdoutJSON, err := json.Marshal(holdout)
	if err != nil {
		t.Fatalf("marshaling holdout payload: %v", err)
	}
	holdoutZstd, err := store.Compress(holdoutJSON)
	if err != nil {
		t.Fatalf("compressing holdout payload: %v", err)
	}
	task := store.EvalTaskRow{
		RepoHead:         sql.NullString{String: commit, Valid: true},
		Brief:            "Fix Greet so it greets the caller by name instead of always saying World.",
		Origin:           evals.OriginHistory,
		HoldoutTestsZstd: holdoutZstd,
	}
	return task, holdout.TestCmd
}

func TestRunOneTask_FailToPassHappyPath(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, greetRepoFiles())
	task, testCmd := buildGreetHoldoutTask(t, commit)

	// The model: checks the failing state, applies the correct fix,
	// re-checks, then reports done. Four backend turns.
	replay := scriptedReplay(t, [][]byte{
		buildAssistantResponse(t, "", toolCallSpec{Name: toolRunTests}),
		buildAssistantResponse(t, "", toolCallSpec{Name: toolEdit, Input: map[string]any{
			"file_path": "greet/greet.go", "old_string": `return "Hello, World!"`, "new_string": `return "Hello, " + name + "!"`,
		}}),
		buildAssistantResponse(t, "", toolCallSpec{Name: toolRunTests}),
		buildAssistantResponse(t, "the tests pass now"),
	})

	cfg := &config.Config{RepoPath: repoPath}
	runner := CommandRunner{UseUnshare: false, Timeout: 30 * time.Second}
	bounds := TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute}

	outcome := runOneTask(context.Background(), cfg, replay, false, runner, "model-under-test", task, testCmd, bounds, false)

	if outcome.Error != "" {
		t.Fatalf("unexpected outcome.Error: %s", outcome.Error)
	}
	if outcome.Turns != 4 {
		t.Errorf("Turns = %d, want 4", outcome.Turns)
	}
	if outcome.TestsRan != 1 || outcome.TestsPassed != 1 {
		t.Errorf("TestsRan/TestsPassed = %d/%d, want 1/1", outcome.TestsRan, outcome.TestsPassed)
	}
	if outcome.Regressions != 0 {
		t.Errorf("Regressions = %d, want 0", outcome.Regressions)
	}
	if !outcome.Passed {
		t.Errorf("Passed = false, want true")
	}
	if !outcome.AgenticReady {
		t.Errorf("AgenticReady = false, want true")
	}
	if len(outcome.TranscriptZstd) == 0 {
		t.Errorf("expected a non-empty transcript")
	}
}

func TestRunOneTask_RegressionIntroducedByModel(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, greetRepoFiles())
	task, testCmd := buildGreetHoldoutTask(t, commit)

	// The model fixes the held-out test but also breaks the pre-existing,
	// previously-passing TestUtilAlwaysTrue in the same package.
	replay := scriptedReplay(t, [][]byte{
		buildAssistantResponse(t, "", toolCallSpec{Name: toolEdit, Input: map[string]any{
			"file_path": "greet/greet.go", "old_string": `return "Hello, World!"`, "new_string": `return "Hello, " + name + "!"`,
		}}),
		buildAssistantResponse(t, "", toolCallSpec{Name: toolEdit, Input: map[string]any{
			"file_path": "greet/util.go", "old_string": "return true", "new_string": "return false",
		}}),
		buildAssistantResponse(t, "done"),
	})

	cfg := &config.Config{RepoPath: repoPath}
	runner := CommandRunner{UseUnshare: false, Timeout: 30 * time.Second}
	bounds := TaskBounds{MaxTurns: 20, MaxTaskTokens: 200000, WallClock: time.Minute}

	outcome := runOneTask(context.Background(), cfg, replay, false, runner, "model-under-test", task, testCmd, bounds, false)

	if outcome.Error != "" {
		t.Fatalf("unexpected outcome.Error: %s", outcome.Error)
	}
	if outcome.TestsRan != 1 || outcome.TestsPassed != 1 {
		t.Errorf("TestsRan/TestsPassed = %d/%d, want 1/1 (the held-out test itself is fixed)", outcome.TestsRan, outcome.TestsPassed)
	}
	if outcome.Regressions == 0 {
		t.Errorf("Regressions = 0, want > 0 (TestUtilAlwaysTrue was broken)")
	}
	if outcome.Passed {
		t.Errorf("Passed = true, want false (a regression must fail the task overall)")
	}
}

func TestRunOneTask_MaxTurnsBoundFailsWithReason(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, greetRepoFiles())
	task, testCmd := buildGreetHoldoutTask(t, commit)

	var responses [][]byte
	for i := 0; i < 10; i++ {
		responses = append(responses, buildAssistantResponse(t, "", toolCallSpec{Name: toolRunTests}))
	}
	replay := scriptedReplay(t, responses)

	cfg := &config.Config{RepoPath: repoPath}
	runner := CommandRunner{UseUnshare: false, Timeout: 30 * time.Second}
	bounds := TaskBounds{MaxTurns: 2, MaxTaskTokens: 200000, WallClock: time.Minute}

	outcome := runOneTask(context.Background(), cfg, replay, false, runner, "model-under-test", task, testCmd, bounds, false)

	if outcome.Passed {
		t.Errorf("Passed = true, want false for a max_turns bound failure")
	}
	if outcome.Error == "" {
		t.Errorf("expected outcome.Error to record the bound-exceeded reason")
	}
	if outcome.Turns != 2 {
		t.Errorf("Turns = %d, want exactly the bound (2)", outcome.Turns)
	}
}

func TestRunOneTask_NoTestCommandFailsCleanly(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, greetRepoFiles())
	task, _ := buildGreetHoldoutTask(t, commit)

	cfg := &config.Config{RepoPath: repoPath}
	runner := CommandRunner{UseUnshare: false, Timeout: 30 * time.Second}
	outcome := runOneTask(context.Background(), cfg, nil, false, runner, "model-under-test", task, "", TaskBounds{MaxTurns: 5, MaxTaskTokens: 1000, WallClock: time.Minute}, false)

	if outcome.Error == "" {
		t.Errorf("expected an error when no test command is available")
	}
	if outcome.Passed {
		t.Errorf("Passed = true, want false")
	}
}

// TestRun_EndToEnd exercises the exported Run entry point against a real
// SQLite store: one history-origin holdout task and one harvested-style
// live task whose subsystem has a configured [tests] command, asserting
// both are selected, scored, and recorded.
func TestRun_EndToEnd(t *testing.T) {
	repoPath, commit := newAgenticTestRepo(t, greetRepoFiles())

	db, err := store.Open(filepath.Join(t.TempDir(), "splitter.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	holdout := evals.HoldoutPayload{
		Files:   []evals.HoldoutFile{{Path: "greet/greet_test.go", IsNew: true, Content: greetHeldOutTestContent}},
		TestCmd: "go test -json ./greet/...",
	}
	holdoutJSON, err := json.Marshal(holdout)
	if err != nil {
		t.Fatalf("marshal holdout: %v", err)
	}
	holdoutZstd, err := store.Compress(holdoutJSON)
	if err != nil {
		t.Fatalf("compress holdout: %v", err)
	}
	reqZstd, err := store.Compress([]byte(`{"model":"synthesized","messages":[]}`))
	if err != nil {
		t.Fatalf("compress request: %v", err)
	}

	if _, _, err := store.InsertEvalTask(db, store.EvalTaskRow{
		CreatedTS:        time.Now().UTC().Format(time.RFC3339),
		RepoHead:         sql.NullString{String: commit, Valid: true},
		Brief:            "Fix Greet so it greets the caller by name.",
		Origin:           evals.OriginHistory,
		RequestZstd:      reqZstd,
		HoldoutTestsZstd: holdoutZstd,
		Language:         sql.NullString{String: "go", Valid: true},
	}); err != nil {
		t.Fatalf("InsertEvalTask (history): %v", err)
	}

	// A harvested-style live task: no holdout payload, but its subsystem
	// has a configured [tests] command, so selectAgenticTasks must still
	// pick it up via cfg.Tests.
	if _, _, err := store.InsertEvalTask(db, store.EvalTaskRow{
		CreatedTS:   time.Now().UTC().Format(time.RFC3339),
		Brief:       "Some live task in the greet subsystem.",
		Origin:      evals.OriginClean,
		RequestZstd: reqZstd,
		Subsystem:   sql.NullString{String: "greet", Valid: true},
		Language:    sql.NullString{String: "go", Valid: true},
	}); err != nil {
		t.Fatalf("InsertEvalTask (live): %v", err)
	}

	// A fake OpenAI-compatible backend that always answers with plain text
	// and no tool calls: both tasks' loops finish immediately (turn 1), so
	// each is graded on whatever state the sandbox already has (the
	// history task's held-out test stays failing; the live task's
	// pre-existing suite, untouched, passes).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backend.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding chat request: %v", err)
		}
		resp := backend.ChatResponse{
			ID:      "chatcmpl-1",
			Choices: []backend.ChatChoice{{Message: backend.ChatMessage{Role: "assistant", Content: "no changes needed"}, FinishReason: "stop"}},
			Usage:   backend.ChatUsage{PromptTokens: 5, CompletionTokens: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encoding fake chat response: %v", err)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.RepoPath = repoPath
	cfg.Tests = map[string]string{"greet": "go test -json ./greet/..."}
	cfg.Backends = map[string]config.BackendConfig{"fake": {BaseURL: server.URL, Model: "fake-model"}}

	summary, err := Run(context.Background(), db, cfg, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if summary.TasksTotal != 2 {
		t.Fatalf("TasksTotal = %d, want 2", summary.TasksTotal)
	}
	if summary.TasksScored != 2 {
		t.Errorf("TasksScored = %d, want 2", summary.TasksScored)
	}

	results, err := store.EvalResultsForRun(db, summary.RunID)
	if err != nil {
		t.Fatalf("EvalResultsForRun: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Mode != "agentic" {
			t.Errorf("result mode = %q, want agentic", r.Mode)
		}
	}
}
