package evals

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/store"
)

// scoreTask creates an eval run for model and records its single result for taskID.
func scoreTask(t *testing.T, db *sql.DB, taskID int64, model string, passed sql.NullInt64) {
	t.Helper()
	runID, err := store.InsertEvalRun(db, "2025-01-01T00:00:00Z", "backend", model)
	if err != nil {
		t.Fatalf("InsertEvalRun for %q: %v", model, err)
	}
	_, err = store.InsertEvalResult(db, store.EvalResultRow{
		EvalRunID:    runID,
		EvalTaskID:   taskID,
		Passed:       passed,
		Stage:        sql.NullString{String: "final", Valid: true},
		ResponseZstd: []byte{},
	})
	if err != nil {
		t.Fatalf("InsertEvalResult for run %d: %v", runID, err)
	}
}

// insertTask inserts an eval task, defaulting the fields every row needs, and
// returns its id. Callers set only what their case cares about.
func insertTask(t *testing.T, db *sql.DB, row store.EvalTaskRow) int64 {
	t.Helper()
	if row.CreatedTS == "" {
		row.CreatedTS = "2025-01-01T00:00:00Z"
	}
	if row.Origin == "" {
		row.Origin = OriginManual
	}
	if row.RequestZstd == nil {
		row.RequestZstd = []byte{}
	}
	id, _, err := store.InsertEvalTask(db, row)
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}
	return id
}

func TestList_EmptyDatabase(t *testing.T) {
	db := openHarvestTestDB(t)

	rows, err := List(db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no rows, got %d", len(rows))
	}
}

func TestList_TaskWithNoResults(t *testing.T) {
	db := openHarvestTestDB(t)
	taskID := insertTask(t, db, store.EvalTaskRow{
		Brief:           "fix login bug",
		RepoHead:        sql.NullString{String: "abc123def456", Valid: true},
		Characteristics: sql.NullString{String: `{"framework":"go"}`, Valid: true},
	})

	rows, err := List(db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	row := rows[0]
	if row.ID != taskID {
		t.Errorf("row ID = %d, want %d", row.ID, taskID)
	}
	if row.Origin != OriginManual {
		t.Errorf("origin = %q, want %q", row.Origin, OriginManual)
	}
	if row.Brief != "fix login bug" {
		t.Errorf("brief = %q, want %q", row.Brief, "fix login bug")
	}
	if len(row.PassRates) != 0 {
		t.Errorf("expected empty PassRates, got %v", row.PassRates)
	}
}

func TestList_PassRateAggregation(t *testing.T) {
	db := openHarvestTestDB(t)
	taskID := insertTask(t, db, store.EvalTaskRow{
		Brief:           "fix login bug",
		Characteristics: sql.NullString{String: `{"framework":"go"}`, Valid: true},
	})

	scoreTask(t, db, taskID, "qwen-coder:7b", sql.NullInt64{Int64: 1, Valid: true})
	scoreTask(t, db, taskID, "qwen-coder:7b", sql.NullInt64{Int64: 1, Valid: true})
	scoreTask(t, db, taskID, "qwen-coder:7b", sql.NullInt64{Int64: 0, Valid: true})

	rows, err := List(db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	prs := rows[0].PassRates
	if len(prs) != 1 {
		t.Fatalf("expected 1 model in PassRates, got %d", len(prs))
	}
	expected := PassRate{Passed: 2, Total: 3}
	if prs["qwen-coder:7b"] != expected {
		t.Errorf("PassRates[\"qwen-coder:7b\"] = %+v, want %+v", prs["qwen-coder:7b"], expected)
	}
}

func TestList_PassRatesAreSeparatedByModel(t *testing.T) {
	db := openHarvestTestDB(t)
	taskID := insertTask(t, db, store.EvalTaskRow{
		Brief:           "fix login bug",
		Characteristics: sql.NullString{String: `{"framework":"go"}`, Valid: true},
	})

	scoreTask(t, db, taskID, "model-a", sql.NullInt64{Int64: 1, Valid: true})
	scoreTask(t, db, taskID, "model-b", sql.NullInt64{Int64: 0, Valid: true})
	scoreTask(t, db, taskID, "model-b", sql.NullInt64{Int64: 0, Valid: true})

	rows, err := List(db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	prs := rows[0].PassRates
	if len(prs) != 2 {
		t.Fatalf("expected 2 models in PassRates, got %d: %v", len(prs), prs)
	}
	if prs["model-a"] != (PassRate{Passed: 1, Total: 1}) {
		t.Errorf("model-a rate = %+v, want {Passed:1 Total:1}", prs["model-a"])
	}
	if prs["model-b"] != (PassRate{Passed: 0, Total: 2}) {
		t.Errorf("model-b rate = %+v, want {Passed:0 Total:2}", prs["model-b"])
	}
}

func TestList_UndecidedResultsAreExcludedFromPassRate(t *testing.T) {
	db := openHarvestTestDB(t)
	taskID := insertTask(t, db, store.EvalTaskRow{
		Brief:           "fix login bug",
		Characteristics: sql.NullString{String: `{"framework":"go"}`, Valid: true},
	})

	scoreTask(t, db, taskID, "model-x", sql.NullInt64{Int64: 1, Valid: true})
	// Undecided result: Passed.Valid == false
	scoreTask(t, db, taskID, "model-x", sql.NullInt64{})

	rows, err := List(db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	prs := rows[0].PassRates
	if len(prs) != 1 {
		t.Fatalf("expected 1 model in PassRates, got %d: %v", len(prs), prs)
	}
	expected := PassRate{Passed: 1, Total: 1}
	if prs["model-x"] != expected {
		t.Errorf("PassRates[\"model-x\"] = %+v, want %+v (undecided should not count)", prs["model-x"], expected)
	}
}

func TestList_ShortSHA(t *testing.T) {
	tests := []struct {
		name      string
		commitSHA string
		repoHead  string
		wantShort string
	}{
		{
			name:      "commit_sha wins and truncates",
			commitSHA: "abcdef1234567890",
			repoHead:  "9999999999999999",
			wantShort: "abcdef12",
		},
		{
			name:      "repo_head used when no commit_sha",
			commitSHA: "",
			repoHead:  "1234567890abcdef",
			wantShort: "12345678",
		},
		{
			name:      "short repo_head unchanged",
			commitSHA: "",
			repoHead:  "abc",
			wantShort: "abc",
		},
		{
			name:      "neither set yields empty",
			commitSHA: "",
			repoHead:  "",
			wantShort: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openHarvestTestDB(t)
			characteristicsJSON := ""
			if tt.commitSHA != "" {
				characteristicsJSON = (&Characteristics{CommitSHA: tt.commitSHA}).JSON()
			}
			insertTask(t, db, store.EvalTaskRow{
				Brief:           "fix login bug",
				RepoHead:        sql.NullString{String: tt.repoHead, Valid: tt.repoHead != ""},
				Characteristics: sql.NullString{String: characteristicsJSON, Valid: true},
			})

			rows, err := List(db)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("expected 1 row, got %d", len(rows))
			}

			row := rows[0]
			if row.ShortSHA != tt.wantShort {
				t.Errorf("ShortSHA = %q, want %q", row.ShortSHA, tt.wantShort)
			}
			// For the first case, ensure we didn't accidentally use repo_head prefix
			if tt.commitSHA != "" && tt.repoHead != "" {
				repoPrefix := tt.repoHead[:8]
				if row.ShortSHA == repoPrefix {
					t.Errorf("ShortSHA should prefer commit_sha, but got repo_head prefix %q", repoPrefix)
				}
			}
		})
	}
}

func TestList_CharacteristicsSummaryIsIncluded(t *testing.T) {
	db := openHarvestTestDB(t)
	insertTask(t, db, store.EvalTaskRow{
		Brief:           "fix login bug",
		Language:        sql.NullString{String: "go", Valid: true},
		Nature:          sql.NullString{String: "bugfix", Valid: true},
		Characteristics: sql.NullString{String: `{"framework":"go"}`, Valid: true},
	})

	rows, err := List(db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	row := rows[0]
	expectedSummary := "go/-/bugfix/-"
	if row.Characteristics != expectedSummary {
		t.Errorf("Characteristics = %q, want %q", row.Characteristics, expectedSummary)
	}
}

func TestList_LoadFailureIsReported(t *testing.T) {
	db := openHarvestTestDB(t)
	db.Close()

	rows, err := List(db)
	if err == nil {
		t.Fatal("expected error for closed database")
	}
	if rows != nil {
		t.Errorf("expected nil rows on error, got %d rows", len(rows))
	}
	if !strings.Contains(err.Error(), "loading eval tasks") {
		t.Errorf("error message %q should mention 'loading eval tasks'", err.Error())
	}
}
