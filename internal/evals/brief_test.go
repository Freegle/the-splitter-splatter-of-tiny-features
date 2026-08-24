package evals

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/store"
)

func openBriefTestDB(t *testing.T) *sql.DB {
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

func insertBriefCall(t *testing.T, db *sql.DB, sessionID, ts string, req anthropic.MessagesRequest) int64 {
	t.Helper()
	reqJSON, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}
	compressed, err := store.Compress(reqJSON)
	if err != nil {
		t.Fatalf("compressing request: %v", err)
	}
	id, err := store.InsertCall(db, store.CallRow{
		TS:          ts,
		SessionID:   sql.NullString{String: sessionID, Valid: sessionID != ""},
		RequestZstd: compressed,
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	return id
}

func userTextRequest(text string) anthropic.MessagesRequest {
	return anthropic.MessagesRequest{
		Messages: []anthropic.Message{
			{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: text}}},
		},
	}
}

// TestDeriveBrief_SessionWalkBack confirms the EARLIEST call's user text
// wins over every later call in the same session, including one whose own
// last user message is a tool_result-only turn (the natural shape of a
// mid-session call).
func TestDeriveBrief_SessionWalkBack(t *testing.T) {
	db := openBriefTestDB(t)

	insertBriefCall(t, db, "sess-1", "2026-08-24T09:00:00Z", userTextRequest("Please fix the postcard reply link so it opens the right chat."))
	insertBriefCall(t, db, "sess-1", "2026-08-24T09:05:00Z", anthropic.MessagesRequest{
		Messages: []anthropic.Message{
			{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockToolResult, ToolUseID: "t1", ToolContent: json.RawMessage(`"file contents here"`)}}},
		},
	})
	thirdCallReq := userTextRequest("this text on the third call must not be picked, the earliest call wins")
	thirdCallID := insertBriefCall(t, db, "sess-1", "2026-08-24T09:10:00Z", thirdCallReq)

	thirdReqJSON, err := json.Marshal(thirdCallReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	brief, source, err := DeriveBrief(db, "sess-1", thirdReqJSON)
	if err != nil {
		t.Fatalf("DeriveBrief: %v", err)
	}
	if source != BriefSourceSession {
		t.Errorf("source = %q, want %q", source, BriefSourceSession)
	}
	want := "Please fix the postcard reply link so it opens the right chat."
	if brief != want {
		t.Errorf("brief = %q, want %q", brief, want)
	}
	_ = thirdCallID
}

// TestDeriveBrief_FallbackToOwnCallWhenNoSession confirms the fallback
// path: no session id at all falls back to the call's own last plain-text
// user block, truncated to 120 characters.
func TestDeriveBrief_FallbackToOwnCallWhenNoSession(t *testing.T) {
	db := openBriefTestDB(t)

	longText := ""
	for len(longText) < 200 {
		longText += "word "
	}
	ownReq := userTextRequest(longText)
	ownReqJSON, err := json.Marshal(ownReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	brief, source, err := DeriveBrief(db, "", ownReqJSON)
	if err != nil {
		t.Fatalf("DeriveBrief: %v", err)
	}
	if source != BriefSourceCall {
		t.Errorf("source = %q, want %q", source, BriefSourceCall)
	}
	if len([]rune(brief)) > callFallbackBriefChars {
		t.Errorf("brief length %d exceeds cap %d", len([]rune(brief)), callFallbackBriefChars)
	}
}

// TestDeriveBrief_FallbackWhenSessionHasNoPlainText confirms that a
// session whose earliest call carries no plain-text user block (only tool
// results) falls back to the task call's own text, rather than an empty
// or session-sourced brief.
func TestDeriveBrief_FallbackWhenSessionHasNoPlainText(t *testing.T) {
	db := openBriefTestDB(t)

	insertBriefCall(t, db, "sess-2", "2026-08-24T09:00:00Z", anthropic.MessagesRequest{
		Messages: []anthropic.Message{
			{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockToolResult, ToolUseID: "t1", ToolContent: json.RawMessage(`"result"`)}}},
		},
	})
	ownReq := userTextRequest("this call's own text should be used as the fallback")
	ownReqJSON, err := json.Marshal(ownReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	brief, source, err := DeriveBrief(db, "sess-2", ownReqJSON)
	if err != nil {
		t.Fatalf("DeriveBrief: %v", err)
	}
	if source != BriefSourceCall {
		t.Errorf("source = %q, want %q", source, BriefSourceCall)
	}
	if brief != "this call's own text should be used as the fallback" {
		t.Errorf("brief = %q", brief)
	}
}

func TestLastPlainTextUserBlock(t *testing.T) {
	req := anthropic.MessagesRequest{
		Messages: []anthropic.Message{
			{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "first"}}},
			{Role: "assistant", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "reply"}}},
			{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockToolResult, ToolUseID: "t1"}}},
		},
	}
	text, ok := lastPlainTextUserBlock(req)
	if !ok || text != "first" {
		t.Errorf("lastPlainTextUserBlock = %q, %v, want \"first\", true", text, ok)
	}
}
