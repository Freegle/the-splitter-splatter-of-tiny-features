package store

import (
	"database/sql"
	"testing"
)

func mustInsertEvalTask(t *testing.T, db *sql.DB, row EvalTaskRow) int64 {
	t.Helper()
	id, _, err := InsertEvalTask(db, row)
	if err != nil {
		t.Fatalf("InsertEvalTask(%q): %v", row.Brief, err)
	}
	return id
}

func TestDecidedVerificationsForRouter(t *testing.T) {
	db, _ := openTestDB(t)

	t.Run("empty_database", func(t *testing.T) {
		rows, err := DecidedVerificationsForRouter(db)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("expected empty slice, got %d rows", len(rows))
		}
	})

	t.Run("decided_vs_undecided", func(t *testing.T) {
		tru := true
		fal := false

		insertJudgeFixture(t, db, &tru)  // decided: agree=true
		insertJudgeFixture(t, db, &fal)  // decided: agree=false
		insertJudgeFixture(t, db, nil)   // undecided: agree=NULL

		rows, err := DecidedVerificationsForRouter(db)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rows) != 2 {
			t.Errorf("expected 2 rows, got %d", len(rows))
		}

		var foundAgree, foundDisagree bool
		for _, r := range rows {
			if r.TurnType != "single_file_edit" || r.Subsystem != "internal" ||
				r.FrontierModel != "claude-sonnet-4-6" || r.LocalModel != "qwen2.5-coder:7b" {
				t.Errorf("unexpected row: %+v", r)
			}
			if r.Agree {
				foundAgree = true
			} else {
				foundDisagree = true
			}
		}
		if !foundAgree || !foundDisagree {
			t.Errorf("expected one agree=true and one agree=false row, got agree=%v disagree=%v", foundAgree, foundDisagree)
		}
	})
}

func TestTrustedEvalResultsForRouter(t *testing.T) {
	db, _ := openTestDB(t)

	t.Run("empty_database", func(t *testing.T) {
		rows, err := TrustedEvalResultsForRouter(db)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("expected empty slice, got %d rows", len(rows))
		}
	})

	t.Run("defaults_and_filtering", func(t *testing.T) {
		runID, err := InsertEvalRun(db, "2026-08-24T12:00:00Z", "ollama", "m1")
		if err != nil {
			t.Fatalf("InsertEvalRun: %v", err)
		}

		taskIDDefault := mustInsertEvalTask(t, db, EvalTaskRow{
			CreatedTS:     "2026-08-24T00:00:00Z", Brief: "task with empty fields", Origin: "manual",
			TurnType:      sql.NullString{}, // empty -> "other"
			FrontierModel: sql.NullString{}, // empty -> "human"
			RequestZstd:   []byte("x"),
		})

		taskIDReal := mustInsertEvalTask(t, db, EvalTaskRow{
			CreatedTS:     "2026-08-24T00:00:00Z", Brief: "task with real fields", Origin: "manual",
			TurnType:      sql.NullString{String: "multi_file_edit", Valid: true},
			FrontierModel: sql.NullString{String: "gemini-2.5-pro", Valid: true},
			RequestZstd:   []byte("x"),
		})

		taskIDError := mustInsertEvalTask(t, db, EvalTaskRow{
			CreatedTS:     "2026-08-24T00:00:00Z", Brief: "task with error", Origin: "manual",
			TurnType:      sql.NullString{String: "single_file_edit", Valid: true},
			FrontierModel: sql.NullString{String: "claude-3.5", Valid: true},
			RequestZstd:   []byte("x"),
		})

		taskIDCheat := mustInsertEvalTask(t, db, EvalTaskRow{
			CreatedTS:     "2026-08-24T00:00:00Z", Brief: "task with cheat flags", Origin: "manual",
			TurnType:      sql.NullString{String: "single_file_edit", Valid: true},
			FrontierModel: sql.NullString{String: "claude-3.5", Valid: true},
			RequestZstd:   []byte("x"),
		})

		taskIDPassedNull := mustInsertEvalTask(t, db, EvalTaskRow{
			CreatedTS:     "2026-08-24T00:00:00Z", Brief: "task with passed null", Origin: "manual",
			TurnType:      sql.NullString{String: "single_file_edit", Valid: true},
			FrontierModel: sql.NullString{String: "claude-3.5", Valid: true},
			RequestZstd:   []byte("x"),
		})

		taskIDToolUse := mustInsertEvalTask(t, db, EvalTaskRow{
			CreatedTS:     "2026-08-24T00:00:00Z", Brief: "task with empty cheat flags string", Origin: "manual",
			TurnType:      sql.NullString{String: "tool_use", Valid: true},
			FrontierModel: sql.NullString{String: "llama-3.1", Valid: true},
			RequestZstd:   []byte("x"),
		})

		// Qualifying: empty turn_type/frontier_model, passed=1, no error, cheat_flags='[]'
		_, err = InsertEvalResult(db, EvalResultRow{
			EvalRunID:    runID, EvalTaskID: taskIDDefault,
			Passed:       sql.NullInt64{Int64: 1, Valid: true},
			Stage:        sql.NullString{String: "unit", Valid: true},
			Error:        sql.NullString{},
			CheatFlags:   sql.NullString{String: "[]", Valid: true},
			ResponseZstd: []byte("resp"),
		})
		if err != nil {
			t.Fatalf("InsertEvalResult (default): %v", err)
		}

		// Qualifying: real values, passed=1, no error, no cheat_flags
		_, err = InsertEvalResult(db, EvalResultRow{
			EvalRunID:    runID, EvalTaskID: taskIDReal,
			Passed:       sql.NullInt64{Int64: 1, Valid: true},
			Stage:        sql.NullString{String: "unit", Valid: true},
			Error:        sql.NullString{},
			CheatFlags:   sql.NullString{},
			ResponseZstd: []byte("resp"),
		})
		if err != nil {
			t.Fatalf("InsertEvalResult (real): %v", err)
		}

		// Excluded: error set to non-empty string
		_, err = InsertEvalResult(db, EvalResultRow{
			EvalRunID:    runID, EvalTaskID: taskIDError,
			Passed:       sql.NullInt64{Int64: 1, Valid: true},
			Stage:        sql.NullString{String: "unit", Valid: true},
			Error:        sql.NullString{String: "boom", Valid: true},
			CheatFlags:   sql.NullString{},
			ResponseZstd: []byte("resp"),
		})
		if err != nil {
			t.Fatalf("InsertEvalResult (error): %v", err)
		}

		// Excluded: cheat_flags = '[cheat]' (non-empty, not '[]')
		_, err = InsertEvalResult(db, EvalResultRow{
			EvalRunID:    runID, EvalTaskID: taskIDCheat,
			Passed:       sql.NullInt64{Int64: 1, Valid: true},
			Stage:        sql.NullString{String: "unit", Valid: true},
			Error:        sql.NullString{},
			CheatFlags:   sql.NullString{String: "[cheat]", Valid: true},
			ResponseZstd: []byte("resp"),
		})
		if err != nil {
			t.Fatalf("InsertEvalResult (cheat): %v", err)
		}

		// Excluded: passed IS NULL
		_, err = InsertEvalResult(db, EvalResultRow{
			EvalRunID:    runID, EvalTaskID: taskIDPassedNull,
			Passed:       sql.NullInt64{}, // NULL
			Stage:        sql.NullString{String: "unit", Valid: true},
			Error:        sql.NullString{},
			CheatFlags:   sql.NullString{},
			ResponseZstd: []byte("resp"),
		})
		if err != nil {
			t.Fatalf("InsertEvalResult (passed null): %v", err)
		}

		// Qualifying: empty string cheat_flags, passed=1, no error
		_, err = InsertEvalResult(db, EvalResultRow{
			EvalRunID:    runID, EvalTaskID: taskIDToolUse,
			Passed:       sql.NullInt64{Int64: 1, Valid: true},
			Stage:        sql.NullString{String: "unit", Valid: true},
			Error:        sql.NullString{},
			CheatFlags:   sql.NullString{String: "", Valid: true},
			ResponseZstd: []byte("resp"),
		})
		if err != nil {
			t.Fatalf("InsertEvalResult (tool_use): %v", err)
		}

		rows, err := TrustedEvalResultsForRouter(db)
		if err != nil {
			t.Fatalf("TrustedEvalResultsForRouter: %v", err)
		}
		if len(rows) != 3 {
			t.Errorf("expected 3 rows, got %d", len(rows))
		}

		var foundDefault, foundReal, foundToolUse bool
		for _, r := range rows {
			switch r.TurnType {
			case "other":
				foundDefault = true
				if r.FrontierModel != "human" {
					t.Errorf("expected FrontierModel='human' for empty frontier_model, got %q", r.FrontierModel)
				}
			case "multi_file_edit":
				foundReal = true
				if r.FrontierModel != "gemini-2.5-pro" {
					t.Errorf("expected FrontierModel='gemini-2.5-pro', got %q", r.FrontierModel)
				}
			case "tool_use":
				foundToolUse = true
				if r.FrontierModel != "llama-3.1" {
					t.Errorf("expected FrontierModel='llama-3.1' for tool_use, got %q", r.FrontierModel)
				}
			default:
				t.Errorf("unexpected TurnType=%q in row %+v", r.TurnType, r)
			}
		}
		if !foundDefault {
			t.Error("expected a row with TurnType='other' (default for empty turn_type)")
		}
		if !foundReal {
			t.Error("expected a row with TurnType='multi_file_edit'")
		}
		if !foundToolUse {
			t.Error("expected a row with TurnType='tool_use' (empty cheat_flags string)")
		}
	})
}

func TestGetVerificationDecision(t *testing.T) {
	db, _ := openTestDB(t)

	t.Run("happy_path", func(t *testing.T) {
		tru := true
		_, _, verificationID, _ := insertJudgeFixture(t, db, &tru)

		d, err := GetVerificationDecision(db, verificationID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.Stage != "ast" {
			t.Errorf("Stage = %q, want ast", d.Stage)
		}
		if !d.Agree.Valid || d.Agree.Int64 == 0 {
			t.Error("Agree should be true")
		}
		if d.Conflict {
			t.Error("Conflict should be false for this fixture")
		}
	})

	t.Run("not_found", func(t *testing.T) {
		_, err := GetVerificationDecision(db, 99999)
		if err == nil {
			t.Error("expected error for non-existent verification ID")
		}
	})

	t.Run("after_judge_verdict", func(t *testing.T) {
		fal := false
		_, _, verificationID, _ := insertJudgeFixture(t, db, &fal)

		const verdictJSON = `{"equivalent":false,"confidence":0.8,"reason":"different"}`
		if err := UpdateVerificationJudgeDecision(db, verificationID, verdictJSON, false, true, "2026-08-24T12:05:00Z"); err != nil {
			t.Fatalf("UpdateVerificationJudgeDecision: %v", err)
		}

		d, err := GetVerificationDecision(db, verificationID)
		if err != nil {
			t.Fatalf("GetVerificationDecision: %v", err)
		}
		if d.Stage != "judge" {
			t.Errorf("Stage = %q, want judge", d.Stage)
		}
		if !d.Agree.Valid || d.Agree.Int64 != 0 {
			t.Errorf("Agree should be Valid with Int64=0, got Valid=%v Int64=%d", d.Agree.Valid, d.Agree.Int64)
		}
		if !d.Conflict {
			t.Error("Conflict should be true")
		}
		if !d.JudgeVerdict.Valid || d.JudgeVerdict.String != verdictJSON {
			t.Errorf("JudgeVerdict = %+v, want Valid=true String=%q", d.JudgeVerdict, verdictJSON)
		}
	})
}

func TestRouterFeedQueryErrors(t *testing.T) {
	db, _ := openTestDB(t)
	db.Close()

	if _, err := DecidedVerificationsForRouter(db); err == nil {
		t.Error("DecidedVerificationsForRouter on closed DB should error")
	}

	if _, err := TrustedEvalResultsForRouter(db); err == nil {
		t.Error("TrustedEvalResultsForRouter on closed DB should error")
	}
}
