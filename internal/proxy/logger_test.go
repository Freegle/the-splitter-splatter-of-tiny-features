package proxy

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/store"
)

func openTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "splitter.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestExtractUsage_ValidResponse(t *testing.T) {
	body := []byte(`{"content":[],"usage":{"input_tokens":10,"output_tokens":4}}`)
	usage := extractUsage(body)
	if usage.InputTokens != 10 || usage.OutputTokens != 4 {
		t.Errorf("extractUsage() = %+v, want input=10 output=4", usage)
	}
}

func TestExtractUsage_MalformedJSON(t *testing.T) {
	usage := extractUsage([]byte("not json"))
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("extractUsage(malformed) = %+v, want zero value", usage)
	}
}

func TestAppendErr(t *testing.T) {
	if got := appendErr("", errors.New("first")); got != "first" {
		t.Errorf("appendErr(\"\", first) = %q", got)
	}
	if got := appendErr("first", errors.New("second")); got != "first; second" {
		t.Errorf("appendErr(first, second) = %q", got)
	}
}

func TestOptionalString(t *testing.T) {
	if ns := optionalString(""); ns.Valid {
		t.Error("optionalString(\"\") should be invalid")
	}
	ns := optionalString("hi")
	if !ns.Valid || ns.String != "hi" {
		t.Errorf("optionalString(hi) = %+v", ns)
	}
}

func TestCallLogger_ProcessInsertsRow(t *testing.T) {
	db, _ := openTestDB(t)
	l := newCallLogger(db, "", 4)
	l.start()

	rec := &captureRecord{
		ts:           time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		sessionID:    "sess-1",
		model:        "claude-x",
		stream:       false,
		requestBody:  []byte(`{"model":"claude-x"}`),
		responseBody: []byte(`{"content":[],"usage":{"input_tokens":3,"output_tokens":2}}`),
		isSSE:        false,
		latencyMs:    12,
		status:       200,
	}
	l.enqueue(rec)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	row, err := store.GetCall(db, 1)
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if row.SessionID.String != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", row.SessionID.String)
	}
	if row.InputTokens.Int64 != 3 || row.OutputTokens.Int64 != 2 {
		t.Errorf("tokens = %d/%d, want 3/2", row.InputTokens.Int64, row.OutputTokens.Int64)
	}
}

func TestCallLogger_Close_TimesOutOnSlowDrain(t *testing.T) {
	db, _ := openTestDB(t)
	l := newCallLogger(db, "", 4)
	l.testDelay = 200 * time.Millisecond
	l.start()

	l.enqueue(&captureRecord{
		ts:           time.Now(),
		requestBody:  []byte(`{}`),
		responseBody: []byte(`{}`),
		status:       200,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := l.close(ctx); err == nil {
		t.Fatal("close returned nil, want a timeout error for a slow drain")
	}
}

func TestCallLogger_Enqueue_DropsWhenChannelFull(t *testing.T) {
	db, _ := openTestDB(t)
	l := newCallLogger(db, "", 1)
	// Fill the channel without starting the consumer goroutine so every
	// enqueue beyond capacity 1 is guaranteed to hit the full-channel path.
	l.enqueue(&captureRecord{requestBody: []byte(`{}`), responseBody: []byte(`{}`)})
	l.enqueue(&captureRecord{requestBody: []byte(`{}`), responseBody: []byte(`{}`)})
	l.enqueue(&captureRecord{requestBody: []byte(`{}`), responseBody: []byte(`{}`)})

	if got := l.dropped.Load(); got != 2 {
		t.Errorf("dropped = %d, want 2", got)
	}
}

func TestCallLogger_NilDB_SkipsInsertWithoutError(t *testing.T) {
	l := newCallLogger(nil, "", 4)
	l.start()

	l.enqueue(&captureRecord{
		ts:           time.Now(),
		requestBody:  []byte(`{}`),
		responseBody: []byte(`{}`),
		status:       200,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCallLogger_ProcessRecoversFromPanic(t *testing.T) {
	db, _ := openTestDB(t)
	l := newCallLogger(db, "", 4)
	l.testPanicOnce = true
	l.start()

	// The first record's processing panics (injected via testPanicOnce);
	// the logger goroutine must recover and keep consuming, so the second
	// record still reaches the database despite the first being dropped.
	l.enqueue(&captureRecord{ts: time.Now(), requestBody: []byte(`{}`), responseBody: []byte(`{}`), status: 200})
	l.enqueue(&captureRecord{ts: time.Now(), sessionID: "after-panic", requestBody: []byte(`{}`), responseBody: []byte(`{}`), status: 200})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The panicking record's insert never ran, so the only row in the
	// table is the one that follows it, whichever id SQLite assigned it.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM calls").Scan(&count); err != nil {
		t.Fatalf("counting calls: %v", err)
	}
	if count != 1 {
		t.Fatalf("calls row count = %d, want exactly 1 (the record after the panic)", count)
	}

	var sessionID string
	if err := db.QueryRow("SELECT session_id FROM calls LIMIT 1").Scan(&sessionID); err != nil {
		t.Fatalf("reading session_id: %v", err)
	}
	if sessionID != "after-panic" {
		t.Errorf("SessionID = %q, want after-panic", sessionID)
	}
}
