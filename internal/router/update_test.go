package router

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

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

func testUpdateConfig() *config.Config {
	cfg := config.Default()
	cfg.Router.MinN = 30
	cfg.Router.MinWilsonLB = 0.9
	cfg.Families = map[string]string{}
	return cfg
}

// seedVerification writes one fully decided (calls, features, replays,
// verifications) chain: enough for router.Update's join query to pick it
// up as one row of store.DecidedVerificationsForRouter.
func seedVerification(t *testing.T, db *sql.DB, turnType, subsystem, frontierModel, localModel string, agree bool) {
	t.Helper()

	callID, err := store.InsertCall(db, store.CallRow{
		TS:    time.Now().UTC().Format(time.RFC3339),
		Model: sql.NullString{String: frontierModel, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	if err := store.UpsertFeature(db, store.FeatureRow{
		CallID:    callID,
		TurnType:  turnType,
		Subsystem: sql.NullString{String: subsystem, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}

	replayID, err := store.InsertReplay(db, store.ReplayRow{
		CallID:    callID,
		Backend:   "ollama",
		Model:     localModel,
		CreatedTS: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}

	a := agree
	if _, err := store.InsertVerification(db, store.VerificationRow{
		ReplayID:  replayID,
		Stage:     "exact",
		Agree:     &a,
		DecidedTS: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("InsertVerification: %v", err)
	}
}

func TestUpdate_FamilyInheritance(t *testing.T) {
	db := openTestDB(t)
	cfg := testUpdateConfig()

	// Two exact frontier versions of the same family, both replayed
	// against the same local model: they must aggregate into ONE
	// router_state row (same category, same family pair), not two,
	// proving a request against the newer version inherits the older
	// version's learned stats.
	seedVerification(t, db, "tool_result_summary", "iznik-server-go", "claude-opus-5", "qwen2.5-coder:7b", true)
	seedVerification(t, db, "tool_result_summary", "iznik-server-go", "claude-opus-5-20260901", "qwen2.5-coder:7b", true)

	result, err := Update(db, cfg)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1 (family inheritance should merge both exact versions into one row): %+v", len(result.Rows), result.Rows)
	}
	row := result.Rows[0]
	if row.Families != "claude-opus>qwen-coder:7b" {
		t.Errorf("Families = %q, want claude-opus>qwen-coder:7b", row.Families)
	}
	if row.N != 2 || row.Agreed != 2 {
		t.Errorf("N/Agreed = %d/%d, want 2/2", row.N, row.Agreed)
	}
	if row.DisabledReason != "" {
		t.Errorf("DisabledReason = %q, want empty (no divergence with only 2 rows)", row.DisabledReason)
	}

	// The lookup a live request for the NEWER version would perform uses
	// exactly this families key.
	liveFamilies := FamilyPair("claude-opus-5-20260901", "qwen2.5-coder:7b", cfg.Families)
	if liveFamilies != row.Families {
		t.Errorf("live FamilyPair() = %q, does not match the persisted row's Families %q", liveFamilies, row.Families)
	}
}

func TestUpdate_DivergenceDetection_FlagsAndRecomputes(t *testing.T) {
	db := openTestDB(t)
	cfg := testUpdateConfig()

	const frontier = "claude-sonnet-4-6"
	const majorityLocal = "qwen2.5-coder:7b"
	const divergentLocal = "qwen3-coder:7b" // same family as majorityLocal (Family() examples)

	if Family(majorityLocal, nil) != Family(divergentLocal, nil) {
		t.Fatalf("test setup assumes %q and %q share a family, got %q and %q",
			majorityLocal, divergentLocal, Family(majorityLocal, nil), Family(divergentLocal, nil))
	}

	// Majority: 90 rows, 88 agree (~97.8%).
	for i := 0; i < 88; i++ {
		seedVerification(t, db, "single_file_edit", "iznik-server-go", frontier, majorityLocal, true)
	}
	for i := 0; i < 2; i++ {
		seedVerification(t, db, "single_file_edit", "iznik-server-go", frontier, majorityLocal, false)
	}
	// Divergent new version: 12 rows, only 5 agree (~41.7%), more than 10
	// points below the family aggregate (~91%).
	for i := 0; i < 5; i++ {
		seedVerification(t, db, "single_file_edit", "iznik-server-go", frontier, divergentLocal, true)
	}
	for i := 0; i < 7; i++ {
		seedVerification(t, db, "single_file_edit", "iznik-server-go", frontier, divergentLocal, false)
	}

	result, err := Update(db, cfg)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1 (one category, one family pair): %+v", len(result.Rows), result.Rows)
	}
	row := result.Rows[0]

	if len(row.Diverged) != 1 || row.Diverged[0].Version != divergentLocal {
		t.Fatalf("Diverged = %+v, want exactly one entry for %q", row.Diverged, divergentLocal)
	}
	if row.Diverged[0].N != 12 {
		t.Errorf("Diverged[0].N = %d, want 12", row.Diverged[0].N)
	}

	// Stats recomputed from the divergent version's rows only.
	if row.N != 12 || row.Agreed != 5 {
		t.Errorf("recomputed N/Agreed = %d/%d, want 12/5 (the divergent version's own rows, not the pooled 102)", row.N, row.Agreed)
	}
	if row.DisabledReason == "" {
		t.Error("DisabledReason is empty, want a divergent_version marker")
	}
	if row.Routable {
		t.Error("Routable = true, want false: a flagged version disables routing for the category")
	}

	// Persisted, not just returned.
	persisted, err := store.AllRouterState(db)
	if err != nil {
		t.Fatalf("AllRouterState: %v", err)
	}
	if len(persisted) != 1 || persisted[0].N != 12 || persisted[0].DisabledReason == "" {
		t.Errorf("persisted router_state = %+v, want the recomputed divergent-only row", persisted)
	}
}

func TestUpdate_NoDivergence_UsesFullFamilyAggregate(t *testing.T) {
	db := openTestDB(t)
	cfg := testUpdateConfig()

	for i := 0; i < 99; i++ {
		seedVerification(t, db, "question_answer", "docs", "claude-haiku-4-5", "gemini-2.5-flash", true)
	}
	seedVerification(t, db, "question_answer", "docs", "claude-haiku-4-5", "gemini-2.5-flash", false)

	result, err := Update(db, cfg)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1", len(result.Rows))
	}
	row := result.Rows[0]
	if row.N != 100 || row.Agreed != 99 {
		t.Errorf("N/Agreed = %d/%d, want 100/99", row.N, row.Agreed)
	}
	if row.DisabledReason != "" {
		t.Errorf("DisabledReason = %q, want empty", row.DisabledReason)
	}
	if len(row.Diverged) != 0 {
		t.Errorf("Diverged = %+v, want none", row.Diverged)
	}
	if !row.Routable {
		t.Errorf("Routable = false, want true: n=42 >= 30, wilson_lb should clear 0.9 at ~95%% agreement (lb=%v)", row.WilsonLB)
	}
}

func TestUpdate_SeparateCategoriesProduceSeparateRows(t *testing.T) {
	db := openTestDB(t)
	cfg := testUpdateConfig()

	seedVerification(t, db, "single_file_edit", "iznik-server-go", "claude-sonnet-4-6", "qwen2.5-coder:7b", true)
	seedVerification(t, db, "question_answer", "docs", "claude-sonnet-4-6", "qwen2.5-coder:7b", true)

	result, err := Update(db, cfg)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2: %+v", len(result.Rows), result.Rows)
	}
}
