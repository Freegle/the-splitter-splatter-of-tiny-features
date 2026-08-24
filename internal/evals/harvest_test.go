package evals

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

func openHarvestTestDB(t *testing.T) *sql.DB {
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

func editResponseJSON(t *testing.T, filePath, oldString, newString string) []byte {
	t.Helper()
	input, err := json.Marshal(struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}{filePath, oldString, newString})
	if err != nil {
		t.Fatalf("marshaling edit input: %v", err)
	}
	msg := struct {
		Content []anthropic.ContentBlock `json:"content"`
	}{
		Content: []anthropic.ContentBlock{{Type: anthropic.BlockToolUse, ID: "tu1", Name: "Edit", Input: input}},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshaling response: %v", err)
	}
	return b
}

// harvestFixtureCall inserts a call, an optional features row, and returns
// the call id.
type harvestFixtureCall struct {
	filePath         string
	turnType         string
	hadErrorFollowup sql.NullInt64
}

func insertHarvestCall(t *testing.T, db *sql.DB, f harvestFixtureCall) int64 {
	t.Helper()

	reqJSON, err := json.Marshal(userTextRequest("do the thing"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	reqCompressed, err := store.Compress(reqJSON)
	if err != nil {
		t.Fatalf("compress request: %v", err)
	}
	respJSON := editResponseJSON(t, f.filePath, "old text", "new text")
	respCompressed, err := store.Compress(respJSON)
	if err != nil {
		t.Fatalf("compress response: %v", err)
	}

	callID, err := store.InsertCall(db, store.CallRow{
		TS:           time.Now().UTC().Format(time.RFC3339),
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  reqCompressed,
		ResponseZstd: respCompressed,
		RepoHead:     sql.NullString{String: "deadbeef", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	if err := store.UpsertFeature(db, store.FeatureRow{
		CallID:           callID,
		TurnType:         f.turnType,
		FilesTouched:     `["` + f.filePath + `"]`,
		Subsystem:        sql.NullString{String: "internal", Valid: true},
		HadErrorFollowup: f.hadErrorFollowup,
	}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}
	return callID
}

func insertHarvestReplayAndVerification(t *testing.T, db *sql.DB, callID int64, agree bool) {
	t.Helper()
	replayID, err := store.InsertReplay(db, store.ReplayRow{
		CallID:    callID,
		Backend:   "ollama",
		Model:     "qwen2.5-coder:7b",
		CreatedTS: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}
	a := agree
	if _, err := store.InsertVerification(db, store.VerificationRow{
		ReplayID:   replayID,
		Stage:      "ast",
		Similarity: 0.5,
		Agree:      &a,
		DecidedTS:  time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("InsertVerification: %v", err)
	}
}

func TestHarvest_DisagreementDifficultyFollowsErrorFollowup(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := config.Default()

	challengingCall := insertHarvestCall(t, db, harvestFixtureCall{
		filePath: "internal/foo.go", turnType: "single_file_edit",
		hadErrorFollowup: sql.NullInt64{Int64: 1, Valid: true},
	})
	insertHarvestReplayAndVerification(t, db, challengingCall, false)

	unknownCall := insertHarvestCall(t, db, harvestFixtureCall{
		filePath: "internal/bar.go", turnType: "single_file_edit",
		hadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	})
	insertHarvestReplayAndVerification(t, db, unknownCall, false)

	summary, err := Harvest(db, cfg, 0)
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if summary.Disagreements != 2 {
		t.Fatalf("Disagreements = %d, want 2", summary.Disagreements)
	}
	// challengingCall's had_error_followup=1 also makes it its own
	// error_followup-origin task (a call legitimately appears under more
	// than one origin, deduped per (call_id, origin) pair, not per call).
	if summary.ErrorFollowups != 1 {
		t.Fatalf("ErrorFollowups = %d, want 1", summary.ErrorFollowups)
	}
	if summary.Inserted != 3 {
		t.Fatalf("Inserted = %d, want 3 (2 disagreement + 1 error_followup)", summary.Inserted)
	}

	tasks, err := store.EvalTasksByOrigin(db, OriginDisagreement)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	byCall := map[int64]store.EvalTaskRow{}
	for _, task := range tasks {
		byCall[task.CallID.Int64] = task
	}

	if got := byCall[challengingCall].Difficulty.String; got != DifficultyChallenging {
		t.Errorf("challenging call task difficulty = %q, want %q", got, DifficultyChallenging)
	}
	if got := byCall[unknownCall].Difficulty; got.Valid {
		t.Errorf("unknown call task difficulty = %q, want NULL (unknown)", got.String)
	}
}

func TestHarvest_ErrorFollowupOrigin(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := config.Default()

	callID := insertHarvestCall(t, db, harvestFixtureCall{
		filePath: "internal/foo.go", turnType: "single_file_edit",
		hadErrorFollowup: sql.NullInt64{Int64: 1, Valid: true},
	})

	summary, err := Harvest(db, cfg, 0)
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if summary.ErrorFollowups != 1 {
		t.Errorf("ErrorFollowups = %d, want 1", summary.ErrorFollowups)
	}

	tasks, err := store.EvalTasksByOrigin(db, OriginErrorFollowup)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	if len(tasks) != 1 || tasks[0].CallID.Int64 != callID {
		t.Fatalf("expected exactly one error_followup task for call %d, got %+v", callID, tasks)
	}
	if tasks[0].Difficulty.String != DifficultyChallenging {
		t.Errorf("difficulty = %q, want %q", tasks[0].Difficulty.String, DifficultyChallenging)
	}
	if tasks[0].TurnType.String != "single_file_edit" {
		t.Errorf("turn_type = %q, want single_file_edit", tasks[0].TurnType.String)
	}
}

func TestHarvest_EscalationOrigin(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := config.Default()

	callID := insertHarvestCall(t, db, harvestFixtureCall{
		filePath: "internal/foo.go", turnType: "single_file_edit",
		hadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	})
	if _, err := db.Exec(`INSERT INTO router_decisions (ts, call_id, decision) VALUES (?, ?, 'escalated')`,
		time.Now().UTC().Format(time.RFC3339), callID); err != nil {
		t.Fatalf("inserting router_decisions row: %v", err)
	}

	summary, err := Harvest(db, cfg, 0)
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if summary.Escalations != 1 {
		t.Errorf("Escalations = %d, want 1", summary.Escalations)
	}

	tasks, err := store.EvalTasksByOrigin(db, OriginEscalation)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Difficulty.String != DifficultyChallenging {
		t.Fatalf("expected one challenging escalation task, got %+v", tasks)
	}
}

// TestHarvest_CleanSamplingExcludesDisagreeingCalls confirms -include-clean
// only samples single_file_edit, had_error_followup=0 calls whose
// verifications (if any) all agreed.
func TestHarvest_CleanSamplingExcludesDisagreeingCalls(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := config.Default()

	cleanNoVerification := insertHarvestCall(t, db, harvestFixtureCall{
		filePath: "internal/a.go", turnType: "single_file_edit",
		hadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	})

	cleanWithAgreeingVerification := insertHarvestCall(t, db, harvestFixtureCall{
		filePath: "internal/b.go", turnType: "single_file_edit",
		hadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	})
	insertHarvestReplayAndVerification(t, db, cleanWithAgreeingVerification, true)

	dirtyWithDisagreeingVerification := insertHarvestCall(t, db, harvestFixtureCall{
		filePath: "internal/c.go", turnType: "single_file_edit",
		hadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	})
	insertHarvestReplayAndVerification(t, db, dirtyWithDisagreeingVerification, false)

	summary, err := Harvest(db, cfg, 10)
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if summary.CleanSampled != 2 {
		t.Errorf("CleanSampled = %d, want 2 (dirtyWithDisagreeingVerification must be excluded)", summary.CleanSampled)
	}

	tasks, err := store.EvalTasksByOrigin(db, OriginClean)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	seen := map[int64]bool{}
	for _, task := range tasks {
		seen[task.CallID.Int64] = true
		if task.Difficulty.String != DifficultySimple {
			t.Errorf("clean task difficulty = %q, want %q", task.Difficulty.String, DifficultySimple)
		}
	}
	if !seen[cleanNoVerification] || !seen[cleanWithAgreeingVerification] {
		t.Error("expected both clean calls to be sampled")
	}
	if seen[dirtyWithDisagreeingVerification] {
		t.Error("a call with a disagreeing verification must never be sampled as clean")
	}
}

// TestHarvest_DedupOnRerun confirms re-running Harvest never duplicates a
// (call_id, origin) pair.
func TestHarvest_DedupOnRerun(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := config.Default()

	// had_error_followup=0 so this call is eligible under exactly one
	// origin (disagreement), isolating the dedup check to that one pair.
	callID := insertHarvestCall(t, db, harvestFixtureCall{
		filePath: "internal/foo.go", turnType: "single_file_edit",
		hadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	})
	insertHarvestReplayAndVerification(t, db, callID, false)

	first, err := Harvest(db, cfg, 0)
	if err != nil {
		t.Fatalf("first Harvest: %v", err)
	}
	if first.Inserted == 0 {
		t.Fatal("expected the first harvest to insert at least one task")
	}

	second, err := Harvest(db, cfg, 0)
	if err != nil {
		t.Fatalf("second Harvest: %v", err)
	}
	if second.Inserted != 0 {
		t.Errorf("second Harvest Inserted = %d, want 0", second.Inserted)
	}
	if second.Deduped != first.Inserted {
		t.Errorf("second Harvest Deduped = %d, want %d", second.Deduped, first.Inserted)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM eval_tasks WHERE call_id = ?`, callID).Scan(&count); err != nil {
		t.Fatalf("counting eval_tasks: %v", err)
	}
	if count != 1 {
		t.Errorf("eval_tasks rows for call %d = %d, want 1", callID, count)
	}
}

func TestHarvest_SessionBriefUsedWhenAvailable(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := config.Default()

	sessionInitial := userTextRequest("Please make the ripple cap apply to the recipient, not the post.")
	sessionInitialJSON, err := json.Marshal(sessionInitial)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sessionInitialCompressed, err := store.Compress(sessionInitialJSON)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if _, err := store.InsertCall(db, store.CallRow{
		TS:          "2026-08-24T09:00:00Z",
		SessionID:   sql.NullString{String: "sess-harvest", Valid: true},
		RequestZstd: sessionInitialCompressed,
	}); err != nil {
		t.Fatalf("InsertCall (session initial): %v", err)
	}

	respJSON := editResponseJSON(t, "internal/rippling/cap.go", "old", "new")
	respCompressed, err := store.Compress(respJSON)
	if err != nil {
		t.Fatalf("compress response: %v", err)
	}
	taskReq := userTextRequest("apply the fix now")
	taskReqJSON, err := json.Marshal(taskReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	taskReqCompressed, err := store.Compress(taskReqJSON)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	callID, err := store.InsertCall(db, store.CallRow{
		TS:           "2026-08-24T09:05:00Z",
		SessionID:    sql.NullString{String: "sess-harvest", Valid: true},
		RequestZstd:  taskReqCompressed,
		ResponseZstd: respCompressed,
	})
	if err != nil {
		t.Fatalf("InsertCall (task): %v", err)
	}
	if err := store.UpsertFeature(db, store.FeatureRow{
		CallID: callID, TurnType: "single_file_edit", FilesTouched: `["internal/rippling/cap.go"]`,
		HadErrorFollowup: sql.NullInt64{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}

	summary, err := Harvest(db, cfg, 0)
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if summary.ErrorFollowups != 1 {
		t.Fatalf("ErrorFollowups = %d, want 1", summary.ErrorFollowups)
	}

	tasks, err := store.EvalTasksByOrigin(db, OriginErrorFollowup)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	want := "Please make the ripple cap apply to the recipient, not the post."
	if tasks[0].Brief != want {
		t.Errorf("brief = %q, want %q (the session's initiating message)", tasks[0].Brief, want)
	}
	c := ParseCharacteristics(tasks[0].Characteristics.String)
	if c.BriefSource != BriefSourceSession {
		t.Errorf("brief_source = %q, want %q", c.BriefSource, BriefSourceSession)
	}
	if c.Localization != LocalizationDiscovered {
		t.Errorf("localization = %q, want %q", c.Localization, LocalizationDiscovered)
	}
}
