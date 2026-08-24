package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/store"

	_ "modernc.org/sqlite"
)

// The fixture transcripts under ../../testdata/transcripts/fixture-project
// are invented but plausible: they mirror the real Claude Code JSONL shape
// observed in ~/.claude/projects/*/*.jsonl (a "type" per line, "user"/
// "assistant" lines carrying a "message" object, an assistant message's
// usage block, isSidechain for subagent turns) without being copied from
// any actual session, and are deliberately small and hand-editable so the
// skip-counting behaviour below is exercised on purpose:
//   - fixture-session-0001.jsonl: one malformed JSON line, one full
//     single_file_edit exchange (two assistant turns), one sidechain
//     (subagent) assistant turn, one assistant turn with empty content,
//     one assistant turn with no timestamp, and one non-message
//     "attachment" line.
//   - no-session-field-0002.jsonl: a plain question/answer exchange whose
//     lines omit "sessionId" entirely, to exercise the filename fallback.
const fixtureTranscriptDir = "../../testdata/transcripts"

func TestImportHistoryCommand_Registered(t *testing.T) {
	if _, ok := commands["import-history"]; !ok {
		t.Fatal(`"import-history" command not registered`)
	}
}

func TestImportTranscriptFile_SkipsAndImportsAsDocumented(t *testing.T) {
	db, dbPath := openImportTestDB(t)
	defer os.Remove(dbPath)

	path := filepath.Join(fixtureTranscriptDir, "fixture-project", "fixture-session-0001.jsonl")
	sum, err := importTranscriptFile(db, path)
	if err != nil {
		t.Fatalf("importTranscriptFile: %v", err)
	}

	if sum.Imported != 2 {
		t.Errorf("Imported = %d, want 2", sum.Imported)
	}
	if sum.SkippedSidechain != 1 {
		t.Errorf("SkippedSidechain = %d, want 1", sum.SkippedSidechain)
	}
	// One malformed top-level JSON line, one assistant turn with empty
	// content, one assistant turn with no timestamp: three unparseable.
	if sum.SkippedUnparseable != 3 {
		t.Errorf("SkippedUnparseable = %d, want 3", sum.SkippedUnparseable)
	}
	// AssistantTurns counts every non-sidechain assistant line seen
	// (msg_1, msg_2, msg_4 empty-content, msg_5 no-timestamp); the
	// sidechain turn (msg_3) is excluded from this count entirely, not
	// just from Imported.
	if sum.AssistantTurns != 4 {
		t.Errorf("AssistantTurns = %d, want 4", sum.AssistantTurns)
	}
}

func TestImportTranscriptFile_SessionIDFallsBackToFilename(t *testing.T) {
	db, dbPath := openImportTestDB(t)
	defer os.Remove(dbPath)

	path := filepath.Join(fixtureTranscriptDir, "fixture-project", "no-session-field-0002.jsonl")
	sum, err := importTranscriptFile(db, path)
	if err != nil {
		t.Fatalf("importTranscriptFile: %v", err)
	}
	if sum.Imported != 1 {
		t.Fatalf("Imported = %d, want 1", sum.Imported)
	}

	row := queryOneImportedCall(t, db)
	if !row.SessionID.Valid || row.SessionID.String != "no-session-field-0002" {
		t.Errorf("SessionID = %+v, want valid \"no-session-field-0002\"", row.SessionID)
	}
	if row.Source != "import" {
		t.Errorf("Source = %q, want import", row.Source)
	}
}

func TestImportTranscriptFile_RowsRoundTripAndAreFeaturisable(t *testing.T) {
	db, dbPath := openImportTestDB(t)
	defer os.Remove(dbPath)

	path := filepath.Join(fixtureTranscriptDir, "fixture-project", "fixture-session-0001.jsonl")
	if _, err := importTranscriptFile(db, path); err != nil {
		t.Fatalf("importTranscriptFile: %v", err)
	}

	rows, err := db.Query(`SELECT id FROM calls WHERE source = 'import' ORDER BY id`)
	if err != nil {
		t.Fatalf("querying imported calls: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("imported %d calls, want 2", len(ids))
	}

	for _, id := range ids {
		call, err := store.GetCall(db, id)
		if err != nil {
			t.Fatalf("GetCall(%d): %v", id, err)
		}

		reqJSON, err := store.Decompress(call.RequestZstd)
		if err != nil {
			t.Fatalf("decompressing request for call %d: %v", id, err)
		}
		var req anthropic.MessagesRequest
		if err := json.Unmarshal(reqJSON, &req); err != nil {
			t.Fatalf("request for call %d is not valid MessagesRequest JSON: %v", id, err)
		}

		respJSON, err := store.Decompress(call.ResponseZstd)
		if err != nil {
			t.Fatalf("decompressing response for call %d: %v", id, err)
		}
		var resp struct {
			Content []anthropic.ContentBlock `json:"content"`
		}
		if err := json.Unmarshal(respJSON, &resp); err != nil {
			t.Fatalf("response for call %d is not valid message JSON: %v", id, err)
		}
		if len(resp.Content) == 0 {
			t.Fatalf("response for call %d has no content blocks", id)
		}

		// The imported request/response pair must be usable by the same
		// featuriser real proxy-captured rows go through.
		turnType := feature.ClassifyTurnType(req, resp.Content)
		if turnType == "" {
			t.Errorf("ClassifyTurnType for call %d returned empty turn_type", id)
		}
	}

	// The first imported turn (msg_1) has one Edit tool_use against one
	// file: it must classify as single_file_edit specifically, not just
	// "some non-empty turn_type".
	first, err := store.GetCall(db, ids[0])
	if err != nil {
		t.Fatalf("GetCall(%d): %v", ids[0], err)
	}
	reqJSON, _ := store.Decompress(first.RequestZstd)
	respJSON, _ := store.Decompress(first.ResponseZstd)
	var req anthropic.MessagesRequest
	var resp struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	_ = json.Unmarshal(reqJSON, &req)
	_ = json.Unmarshal(respJSON, &resp)
	if got := feature.ClassifyTurnType(req, resp.Content); got != feature.TurnSingleFileEdit {
		t.Errorf("first imported call turn_type = %q, want %q", got, feature.TurnSingleFileEdit)
	}
}

func TestImportTranscriptFile_SecondTurnRequestCarriesPriorHistory(t *testing.T) {
	db, dbPath := openImportTestDB(t)
	defer os.Remove(dbPath)

	path := filepath.Join(fixtureTranscriptDir, "fixture-project", "fixture-session-0001.jsonl")
	if _, err := importTranscriptFile(db, path); err != nil {
		t.Fatalf("importTranscriptFile: %v", err)
	}

	rows, err := db.Query(`SELECT id FROM calls WHERE source = 'import' ORDER BY id`)
	if err != nil {
		t.Fatalf("querying imported calls: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("imported %d calls, want 2", len(ids))
	}

	second, err := store.GetCall(db, ids[1])
	if err != nil {
		t.Fatalf("GetCall(%d): %v", ids[1], err)
	}
	reqJSON, err := store.Decompress(second.RequestZstd)
	if err != nil {
		t.Fatalf("decompressing second call's request: %v", err)
	}
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		t.Fatalf("unmarshaling second call's request: %v", err)
	}

	// The second assistant turn followed: initiating user message, first
	// assistant turn (Edit), tool_result user message. All three should
	// have been carried into its reconstructed request as prior messages.
	if len(req.Messages) != 3 {
		t.Fatalf("second call's request has %d prior messages, want 3", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("Messages[0].Role = %q, want user", req.Messages[0].Role)
	}
	if req.Messages[1].Role != "assistant" {
		t.Errorf("Messages[1].Role = %q, want assistant", req.Messages[1].Role)
	}
	if req.Messages[2].Role != "user" {
		t.Errorf("Messages[2].Role = %q, want user", req.Messages[2].Role)
	}
}

func TestRunImportHistory_EndToEndAcrossBothFixtureFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "splitter.db")
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("db_path = \""+dbPath+"\"\n"), 0o644); err != nil {
		t.Fatalf("writing config fixture: %v", err)
	}

	if err := runImportHistory([]string{"-config", configPath, "-dir", fixtureTranscriptDir}); err != nil {
		t.Fatalf("runImportHistory: %v", err)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening resulting db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM calls WHERE source = 'import'`).Scan(&count); err != nil {
		t.Fatalf("counting imported calls: %v", err)
	}
	// 2 from fixture-session-0001.jsonl + 1 from no-session-field-0002.jsonl.
	if count != 3 {
		t.Errorf("imported call count = %d, want 3", count)
	}

	var otherSource int
	if err := db.QueryRow(`SELECT count(*) FROM calls WHERE source != 'import'`).Scan(&otherSource); err != nil {
		t.Fatalf("counting non-imported calls: %v", err)
	}
	if otherSource != 0 {
		t.Errorf("found %d non-import calls, want 0 (nothing else wrote to this fresh db)", otherSource)
	}
}

func TestRunImportHistory_MissingDirectoryIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "splitter.db")
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("db_path = \""+dbPath+"\"\n"), 0o644); err != nil {
		t.Fatalf("writing config fixture: %v", err)
	}

	emptyDir := filepath.Join(dir, "does-not-exist")
	if err := runImportHistory([]string{"-config", configPath, "-dir", emptyDir}); err != nil {
		t.Fatalf("runImportHistory over a missing directory should not error, got: %v", err)
	}
}

func TestResolveTranscriptDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available in this environment")
	}

	got, err := resolveTranscriptDir("")
	if err != nil {
		t.Fatalf("resolveTranscriptDir(\"\"): %v", err)
	}
	want := filepath.Join(home, ".claude", "projects")
	if got != want {
		t.Errorf("default transcript dir = %q, want %q", got, want)
	}

	overridden, err := resolveTranscriptDir("/tmp/some-other-dir")
	if err != nil {
		t.Fatalf("resolveTranscriptDir(override): %v", err)
	}
	if overridden != "/tmp/some-other-dir" {
		t.Errorf("overridden transcript dir = %q, want /tmp/some-other-dir", overridden)
	}

	tilde, err := resolveTranscriptDir("~/custom-transcripts")
	if err != nil {
		t.Fatalf("resolveTranscriptDir(tilde): %v", err)
	}
	if tilde != filepath.Join(home, "custom-transcripts") {
		t.Errorf("tilde-expanded transcript dir = %q, want %q", tilde, filepath.Join(home, "custom-transcripts"))
	}
}

func TestAppendBounded_CapsHistoryLength(t *testing.T) {
	var history []anthropic.Message
	for i := 0; i < maxHistoryMessages+10; i++ {
		history = appendBounded(history, anthropic.Message{Role: "user"})
	}
	if len(history) != maxHistoryMessages {
		t.Errorf("history length = %d, want %d", len(history), maxHistoryMessages)
	}
}

// openImportTestDB opens and migrates a fresh splitter database backed by
// a temp file (not :memory:, so store.Open's chmod-0600-the-file step is
// exercised the same way it is in production) and returns it along with
// its path for cleanup.
func openImportTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "splitter.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dbPath
}

// queryOneImportedCall fetches the single expected source='import' row,
// failing the test if there is not exactly one.
func queryOneImportedCall(t *testing.T, db *sql.DB) *store.CallRow {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM calls WHERE source = 'import'`).Scan(&id); err != nil {
		t.Fatalf("querying imported call: %v", err)
	}
	row, err := store.GetCall(db, id)
	if err != nil {
		t.Fatalf("GetCall(%d): %v", id, err)
	}
	return row
}
