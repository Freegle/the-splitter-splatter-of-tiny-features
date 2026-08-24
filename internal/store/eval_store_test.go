package store

import (
	"database/sql"
	"testing"
)

func TestInsertEvalTask_DedupOnCallOrigin(t *testing.T) {
	db, _ := openTestDB(t)
	callID := insertPlainCall(t, db, "", false)

	row := EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", CallID: sql.NullInt64{Int64: callID, Valid: true},
		Brief: "fix the thing", Origin: "disagreement", RequestZstd: []byte("x"),
	}
	id1, inserted1, err := InsertEvalTask(db, row)
	if err != nil {
		t.Fatalf("first InsertEvalTask: %v", err)
	}
	if !inserted1 || id1 == 0 {
		t.Fatalf("first insert: inserted=%v id=%d, want true and non-zero", inserted1, id1)
	}

	id2, inserted2, err := InsertEvalTask(db, row)
	if err != nil {
		t.Fatalf("second InsertEvalTask: %v", err)
	}
	if inserted2 || id2 != 0 {
		t.Errorf("second insert (same call_id, origin): inserted=%v id=%d, want false and 0", inserted2, id2)
	}

	// A different origin for the same call_id is a distinct row.
	row.Origin = "escalation"
	id3, inserted3, err := InsertEvalTask(db, row)
	if err != nil {
		t.Fatalf("third InsertEvalTask: %v", err)
	}
	if !inserted3 || id3 == id1 {
		t.Errorf("third insert (different origin): inserted=%v id=%d, want true and != %d", inserted3, id3, id1)
	}

	// A manual task (call_id NULL) never dedupes against another NULL.
	manualRow := EvalTaskRow{CreatedTS: "2026-08-24T00:00:00Z", Brief: "manual one", Origin: "manual", RequestZstd: []byte("y")}
	m1, mi1, err := InsertEvalTask(db, manualRow)
	if err != nil || !mi1 || m1 == 0 {
		t.Fatalf("first manual insert: id=%d inserted=%v err=%v", m1, mi1, err)
	}
	m2, mi2, err := InsertEvalTask(db, manualRow)
	if err != nil || !mi2 || m2 == m1 {
		t.Errorf("second manual insert should also succeed (NULL call_id never dedupes): id=%d inserted=%v err=%v", m2, mi2, err)
	}
}

func TestGetEvalTask_And_ActiveEvalTasks(t *testing.T) {
	db, _ := openTestDB(t)

	id, inserted, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "a task", Origin: "manual",
		RequestZstd: []byte("x"), Language: sql.NullString{String: "go", Valid: true},
	})
	if err != nil || !inserted {
		t.Fatalf("InsertEvalTask: id=%d inserted=%v err=%v", id, inserted, err)
	}

	got, err := GetEvalTask(db, id)
	if err != nil {
		t.Fatalf("GetEvalTask: %v", err)
	}
	if got.Brief != "a task" || got.Language.String != "go" || !got.Active {
		t.Errorf("GetEvalTask = %+v, want Brief=\"a task\" Language=go Active=true", got)
	}

	active, err := ActiveEvalTasks(db)
	if err != nil {
		t.Fatalf("ActiveEvalTasks: %v", err)
	}
	if len(active) != 1 || active[0].ID != id {
		t.Errorf("ActiveEvalTasks = %+v, want exactly the inserted task", active)
	}

	if _, err := db.Exec(`UPDATE eval_tasks SET active = 0 WHERE id = ?`, id); err != nil {
		t.Fatalf("deactivating task: %v", err)
	}
	active, err = ActiveEvalTasks(db)
	if err != nil {
		t.Fatalf("ActiveEvalTasks after deactivate: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("ActiveEvalTasks after deactivate = %+v, want empty", active)
	}

	all, err := AllEvalTasks(db)
	if err != nil {
		t.Fatalf("AllEvalTasks: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("AllEvalTasks = %+v, want the deactivated task still listed", all)
	}
}

func TestEvalTasksByOrigin(t *testing.T) {
	db, _ := openTestDB(t)

	if _, _, err := InsertEvalTask(db, EvalTaskRow{CreatedTS: "t", Brief: "h1", Origin: "history", RequestZstd: []byte("x")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := InsertEvalTask(db, EvalTaskRow{CreatedTS: "t", Brief: "m1", Origin: "manual", RequestZstd: []byte("x")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := InsertEvalTask(db, EvalTaskRow{CreatedTS: "t", Brief: "h2", Origin: "history", RequestZstd: []byte("x")}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	history, err := EvalTasksByOrigin(db, "history")
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("EvalTasksByOrigin(history) = %d rows, want 2", len(history))
	}
	for _, task := range history {
		if task.Origin != "history" {
			t.Errorf("task.Origin = %q, want history", task.Origin)
		}
	}
}

func TestUpdateEvalTaskBrief(t *testing.T) {
	db, _ := openTestDB(t)
	id, _, err := InsertEvalTask(db, EvalTaskRow{CreatedTS: "t", Brief: "old brief", Origin: "history", RequestZstd: []byte("x")})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := UpdateEvalTaskBrief(db, id, "new brief", `{"brief_source":"reverse_engineered"}`); err != nil {
		t.Fatalf("UpdateEvalTaskBrief: %v", err)
	}

	got, err := GetEvalTask(db, id)
	if err != nil {
		t.Fatalf("GetEvalTask: %v", err)
	}
	if got.Brief != "new brief" {
		t.Errorf("Brief = %q, want %q", got.Brief, "new brief")
	}
	if got.Characteristics.String != `{"brief_source":"reverse_engineered"}` {
		t.Errorf("Characteristics = %q", got.Characteristics.String)
	}
}

func TestEvalRun_InsertAndUpdateSummary(t *testing.T) {
	db, _ := openTestDB(t)

	id, err := InsertEvalRun(db, "2026-08-24T00:00:00Z", "ollama", "qwen2.5-coder:7b")
	if err != nil {
		t.Fatalf("InsertEvalRun: %v", err)
	}
	if err := UpdateEvalRunSummary(db, id, 10, 7, `{"go":{"stop_rung":0,"rungs":{}}}`, 1000, 200); err != nil {
		t.Fatalf("UpdateEvalRunSummary: %v", err)
	}

	var tasksTotal, tasksPassed int
	var ladder string
	var tokensIn, tokensOut int64
	if err := db.QueryRow(`SELECT tasks_total, tasks_passed, ladder, tokens_in, tokens_out FROM eval_runs WHERE id = ?`, id).
		Scan(&tasksTotal, &tasksPassed, &ladder, &tokensIn, &tokensOut); err != nil {
		t.Fatalf("querying eval_runs: %v", err)
	}
	if tasksTotal != 10 || tasksPassed != 7 || tokensIn != 1000 || tokensOut != 200 {
		t.Errorf("summary = total=%d passed=%d in=%d out=%d, want 10 7 1000 200", tasksTotal, tasksPassed, tokensIn, tokensOut)
	}
	if ladder != `{"go":{"stop_rung":0,"rungs":{}}}` {
		t.Errorf("ladder = %q", ladder)
	}
}

func TestMostRecentPriorRunOtherModel(t *testing.T) {
	db, _ := openTestDB(t)

	runA, err := InsertEvalRun(db, "2026-08-20T00:00:00Z", "ollama", "model-a")
	if err != nil {
		t.Fatalf("InsertEvalRun A: %v", err)
	}
	runB, err := InsertEvalRun(db, "2026-08-21T00:00:00Z", "ollama", "model-a")
	if err != nil {
		t.Fatalf("InsertEvalRun B: %v", err)
	}
	runC, err := InsertEvalRun(db, "2026-08-22T00:00:00Z", "ollama", "model-b")
	if err != nil {
		t.Fatalf("InsertEvalRun C: %v", err)
	}

	// From runC's perspective, the most recent prior run of a DIFFERENT
	// model is runB (model-a), not runA (older) and not runC itself.
	prior, err := MostRecentPriorRunOtherModel(db, runC, "model-b")
	if err != nil {
		t.Fatalf("MostRecentPriorRunOtherModel: %v", err)
	}
	if prior == nil || prior.ID != runB {
		t.Fatalf("prior = %+v, want run id %d", prior, runB)
	}

	// No prior run of a different model exists before runA.
	none, err := MostRecentPriorRunOtherModel(db, runA, "model-a")
	if err != nil {
		t.Fatalf("MostRecentPriorRunOtherModel (none expected): %v", err)
	}
	if none != nil {
		t.Errorf("expected no prior run before the first one, got %+v", none)
	}
}

func TestEvalResult_InsertAndQuery(t *testing.T) {
	db, _ := openTestDB(t)

	taskID, _, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "t", Brief: "b", Origin: "manual", RequestZstd: []byte("x"),
		Language: sql.NullString{String: "go", Valid: true},
		TurnType: sql.NullString{String: "single_file_edit", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}
	runID, err := InsertEvalRun(db, "t", "ollama", "m1")
	if err != nil {
		t.Fatalf("InsertEvalRun: %v", err)
	}

	if _, err := InsertEvalResult(db, EvalResultRow{
		EvalRunID: runID, EvalTaskID: taskID,
		Passed: sql.NullInt64{Int64: 1, Valid: true}, Stage: sql.NullString{String: "exact", Valid: true},
		Similarity: sql.NullFloat64{Float64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("InsertEvalResult: %v", err)
	}

	results, err := EvalResultsForRun(db, runID)
	if err != nil {
		t.Fatalf("EvalResultsForRun: %v", err)
	}
	if len(results) != 1 || !results[0].Passed.Valid || results[0].Passed.Int64 != 1 {
		t.Errorf("EvalResultsForRun = %+v", results)
	}

	withTask, err := EvalResultsWithTaskForRun(db, runID)
	if err != nil {
		t.Fatalf("EvalResultsWithTaskForRun: %v", err)
	}
	if len(withTask) != 1 || withTask[0].Model != "m1" || withTask[0].Language.String != "go" {
		t.Errorf("EvalResultsWithTaskForRun = %+v", withTask)
	}

	all, err := AllEvalResultsWithTask(db)
	if err != nil {
		t.Fatalf("AllEvalResultsWithTask: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("AllEvalResultsWithTask = %+v, want 1 row", all)
	}
}

func TestEarliestCallInSession(t *testing.T) {
	db, _ := openTestDB(t)

	_, err := EarliestCallInSession(db, "")
	if err == nil {
		t.Error("EarliestCallInSession(\"\") should error")
	}
	_, err = EarliestCallInSession(db, "no-such-session")
	if err == nil {
		t.Error("EarliestCallInSession for an unknown session should error (wrapped sql.ErrNoRows)")
	}

	first, err := InsertCall(db, CallRow{TS: "2026-08-24T09:00:00Z", SessionID: sql.NullString{String: "sess-x", Valid: true}})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	if _, err := InsertCall(db, CallRow{TS: "2026-08-24T09:05:00Z", SessionID: sql.NullString{String: "sess-x", Valid: true}}); err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	earliest, err := EarliestCallInSession(db, "sess-x")
	if err != nil {
		t.Fatalf("EarliestCallInSession: %v", err)
	}
	if earliest.ID != first {
		t.Errorf("EarliestCallInSession id = %d, want %d", earliest.ID, first)
	}
}
