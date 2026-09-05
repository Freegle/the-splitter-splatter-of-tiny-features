package store

import (
	"database/sql"
	"testing"
)

func TestUpdateEvalTaskAgenticReady(t *testing.T) {
	db, _ := openTestDB(t)

	taskID, inserted, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "agentic task", Origin: "history", RequestZstd: []byte("x"),
	})
	if err != nil || !inserted {
		t.Fatalf("InsertEvalTask: id=%d inserted=%v err=%v", taskID, inserted, err)
	}

	// Verify initial state: agentic_ready is NULL
	var readyVal sql.NullInt64
	if err := db.QueryRow(`SELECT agentic_ready FROM eval_tasks WHERE id = ?`, taskID).Scan(&readyVal); err != nil {
		t.Fatalf("querying agentic_ready: %v", err)
	}
	if readyVal.Valid {
		t.Errorf("initial agentic_ready.Valid = true, want false (NULL)")
	}

	// Set to true
	if err := UpdateEvalTaskAgenticReady(db, taskID, true); err != nil {
		t.Fatalf("UpdateEvalTaskAgenticReady(true): %v", err)
	}
	if err := db.QueryRow(`SELECT agentic_ready FROM eval_tasks WHERE id = ?`, taskID).Scan(&readyVal); err != nil {
		t.Fatalf("querying after true: %v", err)
	}
	if !readyVal.Valid || readyVal.Int64 != 1 {
		t.Errorf("agentic_ready after true = %+v, want Valid=true Int64=1", readyVal)
	}

	// Set to false
	if err := UpdateEvalTaskAgenticReady(db, taskID, false); err != nil {
		t.Fatalf("UpdateEvalTaskAgenticReady(false): %v", err)
	}
	if err := db.QueryRow(`SELECT agentic_ready FROM eval_tasks WHERE id = ?`, taskID).Scan(&readyVal); err != nil {
		t.Fatalf("querying after false: %v", err)
	}
	if !readyVal.Valid || readyVal.Int64 != 0 {
		t.Errorf("agentic_ready after false = %+v, want Valid=true Int64=0", readyVal)
	}

	// Re-running overwrites previous outcome
	if err := UpdateEvalTaskAgenticReady(db, taskID, true); err != nil {
		t.Fatalf("UpdateEvalTaskAgenticReady(true) again: %v", err)
	}
	if err := db.QueryRow(`SELECT agentic_ready FROM eval_tasks WHERE id = ?`, taskID).Scan(&readyVal); err != nil {
		t.Fatalf("querying after re-run: %v", err)
	}
	if !readyVal.Valid || readyVal.Int64 != 1 {
		t.Errorf("agentic_ready after re-run = %+v, want Valid=true Int64=1", readyVal)
	}

	// Non-existent task should still succeed (no rows affected is fine for this API)
	if err := UpdateEvalTaskAgenticReady(db, -99999, true); err != nil {
		t.Errorf("UpdateEvalTaskAgenticReady on non-existent id: %v", err)
	}
}

func TestAgenticGradableEvalTasks(t *testing.T) {
	db, _ := openTestDB(t)

	// Task A: holdout payload, active (will be in result)
	idA, insertedA, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:01Z", Brief: "with holdout A", Origin: "history", RequestZstd: []byte("x"),
		HoldoutTestsZstd: []byte("holdout payload A"),
	})
	if err != nil || !insertedA {
		t.Fatalf("InsertEvalTask (task A): id=%d inserted=%v err=%v", idA, insertedA, err)
	}

	// Task B: holdout payload, active (will be in result)
	idB, insertedB, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:02Z", Brief: "with holdout B", Origin: "history", RequestZstd: []byte("x"),
		HoldoutTestsZstd: []byte("holdout payload B"),
	})
	if err != nil || !insertedB {
		t.Fatalf("InsertEvalTask (task B): id=%d inserted=%v err=%v", idB, insertedB, err)
	}

	// Task C: holdout payload, then deactivated (will be excluded)
	idC, insertedC, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:03Z", Brief: "with holdout C", Origin: "history", RequestZstd: []byte("x"),
		HoldoutTestsZstd: []byte("holdout payload C"),
	})
	if err != nil || !insertedC {
		t.Fatalf("InsertEvalTask (task C): id=%d inserted=%v err=%v", idC, insertedC, err)
	}
	if _, err := db.Exec(`UPDATE eval_tasks SET active = 0 WHERE id = ?`, idC); err != nil {
		t.Fatalf("deactivating task %d: %v", idC, err)
	}

	// Task D: no holdout payload, active (will be excluded)
	idD, insertedD, err := InsertEvalTask(db, EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:04Z", Brief: "no holdout", Origin: "history", RequestZstd: []byte("x"),
	})
	if err != nil || !insertedD {
		t.Fatalf("InsertEvalTask (task D): id=%d inserted=%v err=%v", idD, insertedD, err)
	}

	// Query agentic gradable tasks
	tasks, err := AgenticGradableEvalTasks(db)
	if err != nil {
		t.Fatalf("AgenticGradableEvalTasks: %v", err)
	}

	// Should return only the active tasks with holdout tests (idA and idB), ordered by id ascending.
	if len(tasks) != 2 {
		t.Errorf("AgenticGradableEvalTasks returned %d rows, want 2", len(tasks))
	} else {
		if tasks[0].ID != idA || tasks[1].ID != idB {
			t.Errorf("tasks = %+v, want [id=%d, id=%d]", tasks, idA, idB)
		}
		if tasks[0].ID >= tasks[1].ID {
			t.Errorf("ordering violated: tasks[0].ID=%d should be < tasks[1].ID=%d", tasks[0].ID, tasks[1].ID)
		}
		for _, task := range tasks {
			if task.HoldoutTestsZstd == nil {
				t.Errorf("task %d has nil HoldoutTestsZstd, expected non-nil", task.ID)
			}
			if !task.Active {
				t.Errorf("task %d is not active, expected active=true", task.ID)
			}
		}
	}

	// Test empty result when no tasks match
	if _, err := db.Exec(`DELETE FROM eval_tasks WHERE id IN (?, ?, ?, ?)`, idA, idB, idC, idD); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	tasks, err = AgenticGradableEvalTasks(db)
	if err != nil {
		t.Fatalf("AgenticGradableEvalTasks after cleanup: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("AgenticGradableEvalTasks after cleanup = %+v, want empty", tasks)
	}
}
