package judge

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/store"
)

// openTestDB opens and migrates a fresh splitter store in t.TempDir().
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "splitter.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

// insertQueuedFixture inserts a full chain (call, features, replay,
// middle-band verification, queued judge_items row) mirroring what
// internal/replay and internal/verify write in production, and returns
// the queued judge_items row's id.
func insertQueuedFixture(t *testing.T, db *sql.DB, requestJSON, frontierRespJSON, localRespJSON string) int64 {
	t.Helper()

	reqZstd, err := store.Compress([]byte(requestJSON))
	if err != nil {
		t.Fatalf("Compress request: %v", err)
	}
	frontierZstd, err := store.Compress([]byte(frontierRespJSON))
	if err != nil {
		t.Fatalf("Compress frontier response: %v", err)
	}

	callID, err := store.InsertCall(db, store.CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-1", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  reqZstd,
		ResponseZstd: frontierZstd,
		Source:       "proxy",
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	if err := store.UpsertFeature(db, store.FeatureRow{
		CallID:    callID,
		TurnType:  "single_file_edit",
		Subsystem: sql.NullString{String: "internal", Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}

	localZstd, err := store.Compress([]byte(localRespJSON))
	if err != nil {
		t.Fatalf("Compress local response: %v", err)
	}
	replayID, err := store.InsertReplay(db, store.ReplayRow{
		CallID:       callID,
		Backend:      "ollama",
		Model:        "qwen2.5-coder:7b",
		ResponseZstd: localZstd,
		CreatedTS:    "2026-08-24T12:01:00Z",
	})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}

	verificationID, err := store.InsertVerification(db, store.VerificationRow{
		ReplayID:   replayID,
		Stage:      "ast",
		Similarity: 0.7,
	})
	if err != nil {
		t.Fatalf("InsertVerification: %v", err)
	}

	judgeItemID, err := store.InsertJudgeItem(db, verificationID, "2026-08-24T12:02:00Z")
	if err != nil {
		t.Fatalf("InsertJudgeItem: %v", err)
	}
	return judgeItemID
}

func TestSubmit_NoQueuedItems(t *testing.T) {
	db := openTestDB(t)

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	result, err := Submit(context.Background(), db, Config{Upstream: server.URL, Model: "claude-haiku-4-5", MaxContextChars: 8000})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.ItemCount != 0 {
		t.Errorf("ItemCount = %d, want 0", result.ItemCount)
	}
	if called {
		t.Error("Submit made an HTTP call despite there being no queued items")
	}
}

func TestSubmit_BuildsPromptAndFlipsStatus(t *testing.T) {
	db := openTestDB(t)

	longRequest := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"` + strings.Repeat("a", 200) + `"}]}`
	id1 := insertQueuedFixture(t, db, longRequest,
		`{"content":[{"type":"text","text":"frontier answer one"}]}`,
		`{"content":[{"type":"text","text":"local answer one"}]}`)
	id2 := insertQueuedFixture(t, db, `{"model":"claude-sonnet-4-6","messages":[]}`,
		`{"content":[{"type":"text","text":"frontier answer two"}]}`,
		`{"content":[{"type":"text","text":"local answer two"}]}`)

	var gotBody batchRequestBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("server: decoding body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msgbatch_sub1","processing_status":"in_progress"}`))
	}))
	defer server.Close()

	cfg := Config{Upstream: server.URL, Model: "claude-haiku-4-5", MaxContextChars: 20}
	result, err := Submit(context.Background(), db, cfg)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.ItemCount != 2 {
		t.Fatalf("ItemCount = %d, want 2", result.ItemCount)
	}
	if result.BatchID != "msgbatch_sub1" {
		t.Errorf("BatchID = %q, want msgbatch_sub1", result.BatchID)
	}

	if len(gotBody.Requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2", len(gotBody.Requests))
	}
	wantCustomIDs := map[string]bool{
		"ji-" + strconv.FormatInt(id1, 10): false,
		"ji-" + strconv.FormatInt(id2, 10): false,
	}
	for _, req := range gotBody.Requests {
		if _, ok := wantCustomIDs[req.CustomID]; !ok {
			t.Errorf("unexpected custom_id %q", req.CustomID)
			continue
		}
		wantCustomIDs[req.CustomID] = true
		if len(req.Params.Messages) != 1 {
			t.Fatalf("requests custom_id %s: %d messages, want 1", req.CustomID, len(req.Params.Messages))
		}
		content := req.Params.Messages[0].Content
		if !strings.Contains(content, judgeInstruction) {
			t.Errorf("custom_id %s: prompt missing the JSON-only instruction", req.CustomID)
		}
		if req.CustomID == "ji-"+strconv.FormatInt(id1, 10) {
			if !strings.Contains(content, "frontier answer one") || !strings.Contains(content, "local answer one") {
				t.Errorf("custom_id %s: prompt missing frontier/local response text:\n%s", req.CustomID, content)
			}
			if strings.Contains(content, strings.Repeat("a", 200)) {
				t.Errorf("custom_id %s: long request context was not truncated:\n%s", req.CustomID, content)
			}
			if !strings.Contains(content, truncatedSuffix) {
				t.Errorf("custom_id %s: prompt does not show the truncation marker", req.CustomID)
			}
		}
	}
	for id, seen := range wantCustomIDs {
		if !seen {
			t.Errorf("custom_id %s was never sent", id)
		}
	}

	// Both queued items must now be gone from the queue and marked submitted.
	remaining, err := store.QueuedJudgeItems(db)
	if err != nil {
		t.Fatalf("QueuedJudgeItems: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("QueuedJudgeItems after Submit = %+v, want empty", remaining)
	}

	refs, err := store.JudgeItemsForBatch(db, result.JudgeBatchRowID)
	if err != nil {
		t.Fatalf("JudgeItemsForBatch: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("JudgeItemsForBatch(%d) = %+v, want 2 entries", result.JudgeBatchRowID, refs)
	}
}
