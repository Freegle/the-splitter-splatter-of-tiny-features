package evals

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/backend"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

func TestApplyMaxAnswerTokensFloor(t *testing.T) {
	cases := []struct {
		name        string
		storedValue int
		cfgValue    int
		want        int
	}{
		{"absent max_tokens floored to configured value", 0, 32000, 32000},
		{"absent max_tokens floored to default when config unset", 0, 0, defaultMaxAnswerTokens},
		{"low max_tokens raised to the floor", 500, 16384, 16384},
		{"explicit larger value kept unchanged", 50000, 16384, 50000},
		{"value equal to the floor kept unchanged", 16384, 16384, 16384},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := anthropic.MessagesRequest{MaxTokens: c.storedValue}
			applyMaxAnswerTokensFloor(&req, config.EvalsConfig{MaxAnswerTokens: c.cfgValue})
			if req.MaxTokens != c.want {
				t.Errorf("MaxTokens = %d, want %d", req.MaxTokens, c.want)
			}
		})
	}
}

// TestRun_FloorsMaxTokensOnDispatch inserts one task whose stored request
// has no max_tokens and one whose stored request already asks for more
// than the configured floor, then asserts eval run's actual dispatched
// backend requests reflect the floor: the first arrives at the fake
// backend with max_answer_tokens (16384), the second keeps its own larger
// value. This is the run-time safety net DECISIONS.md records: it fixes
// already-seeded tasks without re-seeding them.
func TestRun_FloorsMaxTokensOnDispatch(t *testing.T) {
	repoPath, commit := runTestRepo(t, map[string]string{"x.go": "package x\n\nfunc F() string { return \"old\" }\n"})
	db := openRunTestDB(t)

	taskNoMaxTokens := insertRawEditTask(t, db, commit, "x.go", "old", "new-a", 0)
	taskExplicitMaxTokens := insertRawEditTask(t, db, commit, "x.go", "old", "new-b", 50000)

	seenMaxTokens := map[int64]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backend.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding chat request: %v", err)
		}
		text := chatRequestUserText(req)
		switch {
		case strings.Contains(text, "new-a"):
			seenMaxTokens[taskNoMaxTokens] = req.MaxTokens
		case strings.Contains(text, "new-b"):
			seenMaxTokens[taskExplicitMaxTokens] = req.MaxTokens
		}
		writeChatResponse(t, w, "x.go", "old", "old")
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.RepoPath = repoPath
	cfg.Evals.MaxAnswerTokens = 16384
	cfg.Backends = map[string]config.BackendConfig{"fake": {BaseURL: server.URL, Model: "fake-model"}}

	if _, err := Run(context.Background(), db, cfg, RunOptions{Backend: "fake"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := seenMaxTokens[taskNoMaxTokens]; got != 16384 {
		t.Errorf("task with no stored max_tokens dispatched with max_tokens=%d, want 16384", got)
	}
	if got := seenMaxTokens[taskExplicitMaxTokens]; got != 50000 {
		t.Errorf("task with an explicit larger max_tokens dispatched with max_tokens=%d, want it kept at 50000", got)
	}
}

// insertRawEditTask inserts one active eval task whose request names
// oldStr/newStr in filePath and carries maxTokens (0 = absent, matching a
// pre-this-floor seeded task).
func insertRawEditTask(t *testing.T, db *sql.DB, repoHead, filePath, oldStr, newStr string, maxTokens int) int64 {
	t.Helper()

	userText := "Edit " + filePath + ": replace '" + oldStr + "' with '" + newStr + "'."
	req := anthropic.MessagesRequest{
		Messages:  []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: userText}}}},
		Tools:     seedToolDefs,
		MaxTokens: maxTokens,
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	reqCompressed, err := store.Compress(reqJSON)
	if err != nil {
		t.Fatalf("compress request: %v", err)
	}

	refJSON, err := buildSeedReferenceMessage([]anthropic.ContentBlock{editToolUseBlock("toolu_ref", filePath, []diffHunk{{Old: oldStr, New: newStr}})})
	if err != nil {
		t.Fatalf("buildSeedReferenceMessage: %v", err)
	}
	refCompressed, err := store.Compress(refJSON)
	if err != nil {
		t.Fatalf("compress reference: %v", err)
	}

	c := Characteristics{Size: Size{Files: 1, ContextBytes: len(reqJSON)}, BriefSource: BriefSourceManual}
	id, _, err := store.InsertEvalTask(db, store.EvalTaskRow{
		CreatedTS:             "2026-08-24T00:00:00Z",
		RepoHead:              sql.NullString{String: repoHead, Valid: true},
		Brief:                 userText,
		TurnType:              sql.NullString{String: "single_file_edit", Valid: true},
		RequestZstd:           reqCompressed,
		ReferenceResponseZstd: refCompressed,
		Origin:                OriginManual,
		Characteristics:       sql.NullString{String: c.JSON(), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}
	return id
}
