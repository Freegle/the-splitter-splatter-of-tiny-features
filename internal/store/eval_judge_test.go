package store

import (
	"database/sql"
	"testing"
)

func TestUpdateEvalTaskRequest(t *testing.T) {
	db, _ := openTestDB(t)

	id, _, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "task for request update", Origin: "manual",
		RequestZstd: []byte("original"),
	})
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}

	newReq := []byte("updated-request-zstd")
	if err := UpdateEvalTaskRequest(db, id, newReq); err != nil {
		t.Fatalf("UpdateEvalTaskRequest: %v", err)
	}

	got, err := GetEvalTask(db, id)
	if err != nil {
		t.Fatalf("GetEvalTask: %v", err)
	}
	if string(got.RequestZstd) != "updated-request-zstd" {
		t.Errorf("RequestZstd = %q, want %q", got.RequestZstd, newReq)
	}

	// Updating a non-existent ID should not error.
	if err := UpdateEvalTaskRequest(db, 99999, []byte("x")); err != nil {
		t.Errorf("UpdateEvalTaskRequest on non-existent ID returned error: %v", err)
	}
}

func TestUpdateEvalTaskHoldout(t *testing.T) {
	db, _ := openTestDB(t)

	id, _, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "task for holdout update", Origin: "manual",
		RequestZstd: []byte("x"),
	})
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}

	newHoldout := []byte("holdout-tests-zstd")
	if err := UpdateEvalTaskHoldout(db, id, newHoldout); err != nil {
		t.Fatalf("UpdateEvalTaskHoldout: %v", err)
	}

	got, err := GetEvalTask(db, id)
	if err != nil {
		t.Fatalf("GetEvalTask: %v", err)
	}
	if string(got.HoldoutTestsZstd) != "holdout-tests-zstd" {
		t.Errorf("HoldoutTestsZstd = %q, want %q", got.HoldoutTestsZstd, newHoldout)
	}

	// Updating a non-existent ID should not error.
	if err := UpdateEvalTaskHoldout(db, 99999, []byte("x")); err != nil {
		t.Errorf("UpdateEvalTaskHoldout on non-existent ID returned error: %v", err)
	}
}

func TestApplyEvalJudgeVerdict(t *testing.T) {
	db, _ := openTestDB(t)

	taskID, _, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "task for verdict", Origin: "manual",
		RequestZstd: []byte("x"),
	})
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}

	runID, err := InsertEvalRun(db, "2026-08-24T12:00:00Z", "ollama", "m1")
	if err != nil {
		t.Fatalf("InsertEvalRun: %v", err)
	}

	resultID, err := InsertEvalResult(db, EvalResultRow{
		EvalRunID: runID, EvalTaskID: taskID,
		Passed: sql.NullInt64{Int64: 0, Valid: true},
		Stage:  sql.NullString{String: "unit", Valid: true},
		Error:  sql.NullString{}, // empty string treated as NULL in query logic
	})
	if err != nil {
		t.Fatalf("InsertEvalResult: %v", err)
	}

	const verdictJSON = `{"verdict":"pass","reason":"good job"}`
	if err := ApplyEvalJudgeVerdict(db, resultID, 1, verdictJSON); err != nil {
		t.Fatalf("ApplyEvalJudgeVerdict: %v", err)
	}

	var passed int
	var stage string
	var verdict string
	if err := db.QueryRow(`SELECT passed, stage, judge_verdict FROM eval_results WHERE id = ?`, resultID).
		Scan(&passed, &stage, &verdict); err != nil {
		t.Fatalf("querying updated result: %v", err)
	}

	if passed != 1 {
		t.Errorf("passed = %d, want 1", passed)
	}
	if stage != "judge" {
		t.Errorf("stage = %q, want judge", stage)
	}
	if verdict != verdictJSON {
		t.Errorf("judge_verdict = %q, want %q", verdict, verdictJSON)
	}
}

func TestFailedUnjudgedEvalResults(t *testing.T) {
	db, _ := openTestDB(t)

	runID, err := InsertEvalRun(db, "2026-08-24T12:00:00Z", "ollama", "m1")
	if err != nil {
		t.Fatalf("InsertEvalRun: %v", err)
	}

	taskID, _, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "task for failed unjudged", Origin: "manual",
		RequestZstd: []byte("x"),
	})
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}

	// Qualifying row: passed=1, stage="unit" (not exact), error empty/NULL, judge_verdict empty/NULL, response non-nil.
	_, err = InsertEvalResult(db, EvalResultRow{
		EvalRunID: runID, EvalTaskID: taskID,
		Passed:       sql.NullInt64{Int64: 1, Valid: true},
		Stage:        sql.NullString{String: "unit", Valid: true},
		Error:        sql.NullString{}, // empty string -> NULL in DB logic
		JudgeVerdict: sql.NullString{}, // empty string -> NULL in DB logic
		ResponseZstd: []byte("response"),
	})
	if err != nil {
		t.Fatalf("InsertEvalResult (qualifying): %v", err)
	}

	// Excluded: stage = "exact" (different task to avoid unique constraint on run_id+task_id)
	taskID2, _, _ := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "task for exact", Origin: "manual",
		RequestZstd: []byte("x"),
	})
	_, err = InsertEvalResult(db, EvalResultRow{
		EvalRunID: runID, EvalTaskID: taskID2,
		Passed:       sql.NullInt64{Int64: 1, Valid: true},
		Stage:        sql.NullString{String: "exact", Valid: true},
		Error:        sql.NullString{},
		JudgeVerdict: sql.NullString{},
		ResponseZstd: []byte("response"),
	})
	if err != nil {
		t.Fatalf("InsertEvalResult (exact): %v", err)
	}

	// Excluded: error set to non-empty string (different task)
	taskID3, _, _ := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "task for error", Origin: "manual",
		RequestZstd: []byte("x"),
	})
	_, err = InsertEvalResult(db, EvalResultRow{
		EvalRunID: runID, EvalTaskID: taskID3,
		Passed:       sql.NullInt64{Int64: 1, Valid: true},
		Stage:        sql.NullString{String: "unit", Valid: true},
		Error:        sql.NullString{String: "boom", Valid: true},
		JudgeVerdict: sql.NullString{},
		ResponseZstd: []byte("response"),
	})
	if err != nil {
		t.Fatalf("InsertEvalResult (error): %v", err)
	}

	// Excluded: judge_verdict set to non-empty string (different task)
	taskID4, _, _ := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "task for verdict", Origin: "manual",
		RequestZstd: []byte("x"),
	})
	_, err = InsertEvalResult(db, EvalResultRow{
		EvalRunID: runID, EvalTaskID: taskID4,
		Passed:       sql.NullInt64{Int64: 1, Valid: true},
		Stage:        sql.NullString{String: "unit", Valid: true},
		Error:        sql.NullString{},
		JudgeVerdict: sql.NullString{String: "fail", Valid: true},
		ResponseZstd: []byte("response"),
	})
	if err != nil {
		t.Fatalf("InsertEvalResult (verdict): %v", err)
	}

	rows, err := FailedUnjudgedEvalResults(db, runID)
	if err != nil {
		t.Fatalf("FailedUnjudgedEvalResults: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("FailedUnjudgedEvalResults = %d rows, want 1", len(rows))
	} else if rows[0].Passed != 1 || string(rows[0].ResponseZstd) != "response" {
		t.Errorf("qualifying row = %+v, expected Passed=1 ResponseZstd=response", rows[0])
	}

	// Different run ID returns nothing.
	rows, err = FailedUnjudgedEvalResults(db, 99999)
	if err != nil {
		t.Fatalf("FailedUnjudgedEvalResults (other run): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("FailedUnjudgedEvalResults(other run) = %d rows, want 0", len(rows))
	}

	// runID=0 returns all qualifying results.
	rows, err = FailedUnjudgedEvalResults(db, 0)
	if err != nil {
		t.Fatalf("FailedUnjudgedEvalResults (runID=0): %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("FailedUnjudgedEvalResults(runID=0) = %d rows, want 1", len(rows))
	}
}

func TestEvalJudgeQueryErrors(t *testing.T) {
	db, _ := openTestDB(t)
	db.Close()

	if err := UpdateEvalTaskRequest(db, 1, []byte("x")); err == nil {
		t.Error("UpdateEvalTaskRequest on closed DB should error")
	}
	if err := UpdateEvalTaskHoldout(db, 1, []byte("x")); err == nil {
		t.Error("UpdateEvalTaskHoldout on closed DB should error")
	}
	if err := ApplyEvalJudgeVerdict(db, 1, 1, `{"x":1}`); err == nil {
		t.Error("ApplyEvalJudgeVerdict on closed DB should error")
	}

	rows, err := FailedUnjudgedEvalResults(db, 0)
	if err == nil {
		t.Error("FailedUnjudgedEvalResults on closed DB should error")
	}
	if rows != nil {
		t.Errorf("FailedUnjudgedEvalResults returned non-nil slice on error: %v", rows)
	}
}
