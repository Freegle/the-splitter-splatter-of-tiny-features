package replay_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/replay"
	"github.com/freegle/splitter/internal/store"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "splitter.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func testConfig(backendURL string) *config.Config {
	cfg := config.Default()
	cfg.RepoPath = ""
	cfg.Replay.Backend = "testbackend"
	cfg.Replay.IdleMinutes = 30
	cfg.Replay.MaxConcurrentWorktrees = 2
	cfg.Replay.BatchSize = 100
	cfg.Backends = map[string]config.BackendConfig{
		"testbackend": {BaseURL: backendURL, Model: "test-model"},
	}
	return cfg
}

type testCallOpts struct {
	TurnType     string
	RepoHead     string
	RequestJSON  []byte
	ResponseJSON []byte
	TS           time.Time
	Source       string
}

func insertTestCall(t *testing.T, db *sql.DB, opts testCallOpts) int64 {
	t.Helper()

	reqZstd, err := store.Compress(opts.RequestJSON)
	if err != nil {
		t.Fatalf("compressing request: %v", err)
	}
	respZstd, err := store.Compress(opts.ResponseJSON)
	if err != nil {
		t.Fatalf("compressing response: %v", err)
	}

	ts := opts.TS
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	source := opts.Source
	if source == "" {
		source = "proxy"
	}

	id, err := store.InsertCall(db, store.CallRow{
		TS:           ts.Format(time.RFC3339),
		Model:        sql.NullString{String: "claude-test", Valid: true},
		RequestZstd:  reqZstd,
		ResponseZstd: respZstd,
		RepoHead:     sql.NullString{String: opts.RepoHead, Valid: opts.RepoHead != ""},
		Source:       source,
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	if opts.TurnType != "" {
		if err := store.UpsertFeature(db, store.FeatureRow{
			CallID:       id,
			TurnType:     opts.TurnType,
			FilesTouched: "[]",
		}); err != nil {
			t.Fatalf("UpsertFeature: %v", err)
		}
	}
	return id
}

func testRequestJSON(t *testing.T, userText string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":      "claude-test",
		"system":     "You are a helpful assistant.",
		"max_tokens": 200,
		"messages": []map[string]any{
			{"role": "user", "content": userText},
		},
	})
	if err != nil {
		t.Fatalf("marshaling request fixture: %v", err)
	}
	return b
}

func testResponseJSON(t *testing.T, text string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
	if err != nil {
		t.Fatalf("marshaling response fixture: %v", err)
	}
	return b
}

func newBackendServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func fixedTextHandler(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "resp1",
			"model": "test-model",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}
}

// selectiveHandler fails any request whose body contains failMarker, and
// otherwise succeeds with a fixed reply text.
func selectiveHandler(failMarker string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), failMarker) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		fixedTextHandler("agreed reply")(w, r)
	}
}

func TestRun_IdleGate_RefusesWithoutForce(t *testing.T) {
	db := openTestDB(t)
	insertTestCall(t, db, testCallOpts{
		TurnType:     "question_answer",
		TS:           time.Now().UTC(),
		RequestJSON:  testRequestJSON(t, "hello"),
		ResponseJSON: testResponseJSON(t, "hi"),
	})

	cfg := testConfig("http://unused.invalid")

	_, err := replay.Run(context.Background(), db, cfg, replay.Options{})
	if err == nil {
		t.Fatal("expected the idle gate to refuse a run with a fresh proxy call and no -force")
	}
}

func TestRun_IdleGate_ForceOverridesRefusal(t *testing.T) {
	db := openTestDB(t)
	insertTestCall(t, db, testCallOpts{
		TurnType:     "question_answer",
		TS:           time.Now().UTC(),
		RequestJSON:  testRequestJSON(t, "What is 6*7?"),
		ResponseJSON: testResponseJSON(t, "42"),
	})

	srv := newBackendServer(t, fixedTextHandler("42"))
	cfg := testConfig(srv.URL)

	summary, err := replay.Run(context.Background(), db, cfg, replay.Options{Force: true})
	if err != nil {
		t.Fatalf("Run with -force: %v", err)
	}
	if summary.RepliesOK != 1 {
		t.Errorf("RepliesOK = %d, want 1", summary.RepliesOK)
	}
}

func TestRun_IdleGate_PassesWhenNewestCallIsOldEnough(t *testing.T) {
	db := openTestDB(t)
	insertTestCall(t, db, testCallOpts{
		TurnType:     "question_answer",
		TS:           time.Now().UTC().Add(-2 * time.Hour),
		RequestJSON:  testRequestJSON(t, "What is 6*7?"),
		ResponseJSON: testResponseJSON(t, "42"),
	})

	srv := newBackendServer(t, fixedTextHandler("42"))
	cfg := testConfig(srv.URL)

	if _, err := replay.Run(context.Background(), db, cfg, replay.Options{}); err != nil {
		t.Fatalf("Run should not be refused by the idle gate: %v", err)
	}
}

func TestRun_IdleGate_IgnoresImportedCalls(t *testing.T) {
	db := openTestDB(t)
	// An imported (non-proxy) call, however fresh, never counts as live
	// traffic for the idle gate.
	insertTestCall(t, db, testCallOpts{
		TurnType:     "question_answer",
		TS:           time.Now().UTC(),
		RequestJSON:  testRequestJSON(t, "What is 6*7?"),
		ResponseJSON: testResponseJSON(t, "42"),
		Source:       "import",
	})

	srv := newBackendServer(t, fixedTextHandler("42"))
	cfg := testConfig(srv.URL)

	if _, err := replay.Run(context.Background(), db, cfg, replay.Options{}); err != nil {
		t.Fatalf("Run should not be refused by the idle gate on imported-only traffic: %v", err)
	}
}

func TestRun_SelectionExcludesIneligibleCalls(t *testing.T) {
	db := openTestDB(t)
	oldTS := time.Now().UTC().Add(-2 * time.Hour)

	// Excluded: turn_type "other".
	insertTestCall(t, db, testCallOpts{
		TurnType: "other", TS: oldTS,
		RequestJSON: testRequestJSON(t, "a"), ResponseJSON: testResponseJSON(t, "a"),
	})

	// Excluded: already has a replay row for this exact backend/model.
	alreadyID := insertTestCall(t, db, testCallOpts{
		TurnType: "question_answer", TS: oldTS,
		RequestJSON: testRequestJSON(t, "b"), ResponseJSON: testResponseJSON(t, "b"),
	})
	if _, err := store.InsertReplay(db, store.ReplayRow{
		CallID: alreadyID, Backend: "testbackend", Model: "test-model", CreatedTS: oldTS.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seeding existing replay: %v", err)
	}

	// Eligible.
	insertTestCall(t, db, testCallOpts{
		TurnType: "question_answer", TS: oldTS,
		RequestJSON: testRequestJSON(t, "c"), ResponseJSON: testResponseJSON(t, "c"),
	})

	srv := newBackendServer(t, fixedTextHandler("reply"))
	cfg := testConfig(srv.URL)

	summary, err := replay.Run(context.Background(), db, cfg, replay.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.CallsSelected != 1 {
		t.Errorf("CallsSelected = %d, want 1", summary.CallsSelected)
	}
}

func TestRun_LimitOverridesBatchSize(t *testing.T) {
	db := openTestDB(t)
	oldTS := time.Now().UTC().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		insertTestCall(t, db, testCallOpts{
			TurnType: "question_answer", TS: oldTS,
			RequestJSON: testRequestJSON(t, "q"), ResponseJSON: testResponseJSON(t, "a"),
		})
	}

	srv := newBackendServer(t, fixedTextHandler("a"))
	cfg := testConfig(srv.URL)
	cfg.Replay.BatchSize = 100

	summary, err := replay.Run(context.Background(), db, cfg, replay.Options{Limit: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.CallsSelected != 2 {
		t.Errorf("CallsSelected = %d, want 2 (the -limit override)", summary.CallsSelected)
	}
}

func TestRun_PerCallErrorTolerance(t *testing.T) {
	db := openTestDB(t)
	oldTS := time.Now().UTC().Add(-2 * time.Hour)
	insertTestCall(t, db, testCallOpts{
		TurnType: "question_answer", TS: oldTS,
		RequestJSON: testRequestJSON(t, "FAIL_ME"), ResponseJSON: testResponseJSON(t, "x"),
	})
	insertTestCall(t, db, testCallOpts{
		TurnType: "question_answer", TS: oldTS,
		RequestJSON: testRequestJSON(t, "succeed"), ResponseJSON: testResponseJSON(t, "agreed reply"),
	})

	srv := newBackendServer(t, selectiveHandler("FAIL_ME"))
	cfg := testConfig(srv.URL)

	summary, err := replay.Run(context.Background(), db, cfg, replay.Options{})
	if err != nil {
		t.Fatalf("a per-call backend failure must not abort the batch: %v", err)
	}
	if summary.RepliesError != 1 {
		t.Errorf("RepliesError = %d, want 1", summary.RepliesError)
	}
	if summary.RepliesOK != 1 {
		t.Errorf("RepliesOK = %d, want 1", summary.RepliesOK)
	}

	var errCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM replays WHERE error IS NOT NULL`).Scan(&errCount); err != nil {
		t.Fatalf("querying replays: %v", err)
	}
	if errCount != 1 {
		t.Errorf("replays rows with error set = %d, want 1", errCount)
	}
}

func TestRun_EndToEnd_ExactMatchRecordsVerification(t *testing.T) {
	db := openTestDB(t)
	insertTestCall(t, db, testCallOpts{
		TurnType:     "question_answer",
		TS:           time.Now().UTC().Add(-2 * time.Hour),
		RequestJSON:  testRequestJSON(t, "What is 6*7?"),
		ResponseJSON: testResponseJSON(t, "42"),
	})

	srv := newBackendServer(t, fixedTextHandler("42"))
	cfg := testConfig(srv.URL)

	summary, err := replay.Run(context.Background(), db, cfg, replay.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.StageExact != 1 || summary.Agreed != 1 {
		t.Errorf("summary = %+v, want one exact-stage agreement", summary)
	}

	var stage string
	var agree sql.NullInt64
	var similarity float64
	if err := db.QueryRow(`SELECT stage, similarity, agree FROM verifications`).Scan(&stage, &similarity, &agree); err != nil {
		t.Fatalf("querying verifications: %v", err)
	}
	if stage != "exact" || similarity != 1 || !agree.Valid || agree.Int64 != 1 {
		t.Errorf("verification row = stage=%s similarity=%v agree=%v, want exact/1/1", stage, similarity, agree)
	}
}

func TestRun_MiddleBand_QueuesJudgeItem(t *testing.T) {
	db := openTestDB(t)
	insertTestCall(t, db, testCallOpts{
		TurnType:     "question_answer",
		TS:           time.Now().UTC().Add(-2 * time.Hour),
		RequestJSON:  testRequestJSON(t, "describe the weather today in detail please"),
		ResponseJSON: testResponseJSON(t, "the quick brown fox jumps over the lazy dog today"),
	})

	// The local reply differs from the frontier's by one token out of ten
	// (similarity 0.9); thresholds are set so that lands in the band.
	srv := newBackendServer(t, fixedTextHandler("the quick brown fox jumps over the lazy cat today"))
	cfg := testConfig(srv.URL)
	cfg.Thresholds.DefaultHigh = 0.95
	cfg.Thresholds.DefaultLow = 0.5

	summary, err := replay.Run(context.Background(), db, cfg, replay.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Banded != 1 {
		t.Errorf("Banded = %d, want 1", summary.Banded)
	}

	var verificationID int64
	var agree sql.NullInt64
	var decidedTS sql.NullString
	if err := db.QueryRow(`SELECT id, agree, decided_ts FROM verifications`).Scan(&verificationID, &agree, &decidedTS); err != nil {
		t.Fatalf("querying verifications: %v", err)
	}
	if agree.Valid {
		t.Errorf("agree = %v, want NULL for a middle-band row", agree.Int64)
	}
	if decidedTS.Valid {
		t.Errorf("decided_ts = %q, want NULL while judge arbitration is pending", decidedTS.String)
	}

	var customID, status string
	if err := db.QueryRow(`SELECT custom_id, status FROM judge_items WHERE verification_id = ?`, verificationID).Scan(&customID, &status); err != nil {
		t.Fatalf("querying judge_items: %v", err)
	}
	if status != "queued" {
		t.Errorf("judge_items.status = %q, want queued", status)
	}
	wantCustomID := "ji-"
	if !strings.HasPrefix(customID, wantCustomID) || customID == wantCustomID {
		t.Errorf("judge_items.custom_id = %q, want a non-empty ji-<id> value", customID)
	}
}
