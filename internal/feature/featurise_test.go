package feature

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/freegle/splitter/internal/store"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "splitter.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

// testRequest/testResponse are minimal JSON-shaped test fixtures matching
// the wire format decodeRequest/decodeResponseBlocks read; lowercase field
// names match the wire, so plain map literals in tests stay readable.
type testRequest struct {
	System   string           `json:"system,omitempty"`
	Messages []map[string]any `json:"messages"`
}

type testResponse struct {
	Content []map[string]any `json:"content"`
}

func userText(text string) map[string]any {
	return map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}}
}

func userToolResult(toolUseID, text string, isError bool) map[string]any {
	return map[string]any{"role": "user", "content": []map[string]any{
		{"type": "tool_result", "tool_use_id": toolUseID, "is_error": isError, "content": text},
	}}
}

func textContent(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func editToolContent(filePath string) map[string]any {
	return map[string]any{
		"type": "tool_use", "id": "toolu_1", "name": "Edit",
		"input": map[string]any{"file_path": filePath, "old_string": "a", "new_string": "b"},
	}
}

// insertCall inserts a calls row with the given request/response JSON
// (compressed) and returns its id.
func insertCall(t *testing.T, db *sql.DB, sessionID, ts string, req testRequest, resp testResponse) int64 {
	t.Helper()

	reqJSON, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}
	respJSON, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshaling response: %v", err)
	}
	reqZstd, err := store.Compress(reqJSON)
	if err != nil {
		t.Fatalf("compressing request: %v", err)
	}
	respZstd, err := store.Compress(respJSON)
	if err != nil {
		t.Fatalf("compressing response: %v", err)
	}

	id, err := store.InsertCall(db, store.CallRow{
		TS:           ts,
		SessionID:    sql.NullString{String: sessionID, Valid: sessionID != ""},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  reqZstd,
		ResponseZstd: respZstd,
		InputTokens:  sql.NullInt64{Int64: 1000, Valid: true},
		OutputTokens: sql.NullInt64{Int64: 200, Valid: true},
		Status:       sql.NullInt64{Int64: 200, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	return id
}

func TestRun_FeaturisesNewCall(t *testing.T) {
	db := openTestDB(t)

	id := insertCall(t, db, "sess-1", "2026-08-24T10:00:00Z",
		testRequest{Messages: []map[string]any{userText("please edit the file")}},
		testResponse{Content: []map[string]any{editToolContent("/repo/internal/feature/foo.go")}},
	)

	processed, err := Run(db, "/repo", false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if processed != 1 {
		t.Fatalf("processed = %d, want 1", processed)
	}

	f, err := store.GetFeature(db, id)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	if f.TurnType != TurnSingleFileEdit {
		t.Errorf("TurnType = %q, want %q", f.TurnType, TurnSingleFileEdit)
	}
	if f.FilesTouched != `["internal/feature/foo.go"]` {
		t.Errorf("FilesTouched = %q, want [\"internal/feature/foo.go\"]", f.FilesTouched)
	}
	if !f.Subsystem.Valid || f.Subsystem.String != "internal" {
		t.Errorf("Subsystem = %+v, want internal", f.Subsystem)
	}
	if !f.ContextTokens.Valid || f.ContextTokens.Int64 != 1000 {
		t.Errorf("ContextTokens = %+v, want 1000", f.ContextTokens)
	}
	if !f.OutputTokens.Valid || f.OutputTokens.Int64 != 200 {
		t.Errorf("OutputTokens = %+v, want 200", f.OutputTokens)
	}
	if f.HadErrorFollowup.Valid {
		t.Errorf("HadErrorFollowup = %+v, want NULL (no next call yet)", f.HadErrorFollowup)
	}
}

func TestRun_IsIdempotent(t *testing.T) {
	db := openTestDB(t)
	id := insertCall(t, db, "sess-1", "2026-08-24T10:00:00Z",
		testRequest{Messages: []map[string]any{userText("what does this do?")}},
		testResponse{Content: []map[string]any{textContent("It parses config.")}},
	)

	if _, err := Run(db, "/repo", false); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := Run(db, "/repo", false); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM features WHERE call_id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("counting features rows: %v", err)
	}
	if count != 1 {
		t.Errorf("features row count = %d, want 1 (idempotent upsert)", count)
	}
}

func TestRun_FillsInErrorFollowupOnceNextCallArrives(t *testing.T) {
	db := openTestDB(t)

	id1 := insertCall(t, db, "sess-1", "2026-08-24T10:00:00Z",
		testRequest{Messages: []map[string]any{userText("edit foo.go")}},
		testResponse{Content: []map[string]any{editToolContent("/repo/foo.go")}},
	)

	processed, err := Run(db, "/repo", false)
	if err != nil {
		t.Fatalf("Run (first pass): %v", err)
	}
	if processed != 1 {
		t.Fatalf("processed = %d, want 1", processed)
	}

	f, err := store.GetFeature(db, id1)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	if f.HadErrorFollowup.Valid {
		t.Fatal("expected HadErrorFollowup NULL before the next call exists")
	}

	// The next call in the session reports a tool_result error.
	insertCall(t, db, "sess-1", "2026-08-24T10:01:00Z",
		testRequest{Messages: []map[string]any{userToolResult("toolu_1", "Error: syntax error", true)}},
		testResponse{Content: []map[string]any{textContent("Let me try a different approach.")}},
	)

	processed, err = Run(db, "/repo", false)
	if err != nil {
		t.Fatalf("Run (second pass): %v", err)
	}
	// call 2 gains a features row, and call 1's row is refreshed for its
	// now-resolved had_error_followup: two rows processed.
	if processed != 2 {
		t.Fatalf("processed = %d, want 2", processed)
	}

	f, err = store.GetFeature(db, id1)
	if err != nil {
		t.Fatalf("GetFeature after second pass: %v", err)
	}
	if !f.HadErrorFollowup.Valid || f.HadErrorFollowup.Int64 != 1 {
		t.Errorf("HadErrorFollowup = %+v, want 1", f.HadErrorFollowup)
	}

	// A third pass with no new calls reprocesses call 2: it is still the
	// last call in its session, so its own had_error_followup stays NULL
	// (unknown until a further call arrives) and it stays in the pending
	// set on every run. That reprocessing is idempotent: call 1, whose
	// followup already resolved, is not touched again.
	processed, err = Run(db, "/repo", false)
	if err != nil {
		t.Fatalf("Run (third pass): %v", err)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1 (call 2's followup is still unresolved)", processed)
	}

	f, err = store.GetFeature(db, id1)
	if err != nil {
		t.Fatalf("GetFeature after third pass: %v", err)
	}
	if !f.HadErrorFollowup.Valid || f.HadErrorFollowup.Int64 != 1 {
		t.Errorf("call 1 HadErrorFollowup after third pass = %+v, want 1 (unchanged)", f.HadErrorFollowup)
	}
}

func TestRun_RefreshReprocessesEverything(t *testing.T) {
	db := openTestDB(t)
	insertCall(t, db, "", "2026-08-24T10:00:00Z",
		testRequest{Messages: []map[string]any{userText("hello")}},
		testResponse{Content: []map[string]any{textContent("hi there")}},
	)
	insertCall(t, db, "", "2026-08-24T10:01:00Z",
		testRequest{Messages: []map[string]any{userText("edit bar.go")}},
		testResponse{Content: []map[string]any{editToolContent("/repo/bar.go")}},
	)

	if _, err := Run(db, "/repo", false); err != nil {
		t.Fatalf("initial Run: %v", err)
	}

	processed, err := Run(db, "/repo", true)
	if err != nil {
		t.Fatalf("Run --refresh: %v", err)
	}
	if processed != 2 {
		t.Errorf("processed = %d, want 2 (refresh reprocesses every call)", processed)
	}
}

func TestRun_SkipsCallsWithNoCapturedResponse(t *testing.T) {
	db := openTestDB(t)
	id, err := store.InsertCall(db, store.CallRow{TS: "2026-08-24T10:00:00Z"})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	processed, err := Run(db, "/repo", false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if processed != 0 {
		t.Errorf("processed = %d, want 0", processed)
	}

	if _, err := store.GetFeature(db, id); err == nil {
		t.Error("expected no features row for a call with no captured response")
	}
}
