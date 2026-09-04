package store

import (
	"database/sql"
	"testing"
)

func TestHarvestDisagreements(t *testing.T) {
	db, _ := openTestDB(t)

	agreeFalse := false
	callID1, _, _, _ := insertJudgeFixture(t, db, &agreeFalse)

	agreeTrue := true
	insertJudgeFixture(t, db, &agreeTrue)

	rows, err := HarvestDisagreements(db)
	if err != nil {
		t.Fatalf("HarvestDisagreements: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].CallID != callID1 {
		t.Errorf("CallID = %d, want %d", rows[0].CallID, callID1)
	}
}

func TestHarvestEscalations(t *testing.T) {
	db, _ := openTestDB(t)

	callID, err := InsertCall(db, CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-1", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  []byte("req"),
		ResponseZstd: []byte("resp"),
		Source:       "proxy",
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	if err := UpsertFeature(db, FeatureRow{
		CallID:    callID,
		TurnType:  "single_file_edit",
		Subsystem: sql.NullString{String: "internal", Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}

	if _, err := InsertRouterDecision(db, RouterDecisionRow{
		TS:       "2026-08-24T12:01:00Z",
		CallID:   sql.NullInt64{Int64: callID, Valid: true},
		Decision: "escalated",
	}); err != nil {
		t.Fatalf("InsertRouterDecision: %v", err)
	}

	rows, err := HarvestEscalations(db)
	if err != nil {
		t.Fatalf("HarvestEscalations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].CallID != callID {
		t.Errorf("CallID = %d, want %d", rows[0].CallID, callID)
	}
}

func TestHarvestErrorFollowups(t *testing.T) {
	db, _ := openTestDB(t)

	callID, err := InsertCall(db, CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-1", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  []byte("req"),
		ResponseZstd: []byte("resp"),
		Source:       "proxy",
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	if err := UpsertFeature(db, FeatureRow{
		CallID:           callID,
		TurnType:         "single_file_edit",
		Subsystem:        sql.NullString{String: "internal", Valid: true},
		HadErrorFollowup: sql.NullInt64{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}

	rows, err := HarvestErrorFollowups(db)
	if err != nil {
		t.Fatalf("HarvestErrorFollowups: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].CallID != callID {
		t.Errorf("CallID = %d, want %d", rows[0].CallID, callID)
	}
}

func TestHarvestCleanCandidates(t *testing.T) {
	db, _ := openTestDB(t)

	// Fixture 1: callNoReplays - InsertCall directly, no replay/verification, feature with HadErrorFollowup=0.
	reqZstd, err := Compress([]byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"fix the bug"}]}`))
	if err != nil {
		t.Fatalf("Compress request: %v", err)
	}
	frontierRespZstd, err := Compress([]byte(`{"content":[{"type":"text","text":"frontier answer"}]}`))
	if err != nil {
		t.Fatalf("Compress frontier response: %v", err)
	}

	callNoReplays, err := InsertCall(db, CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-no-replay", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  reqZstd,
		ResponseZstd: frontierRespZstd,
		Source:       "proxy",
	})
	if err != nil {
		t.Fatalf("InsertCall (callNoReplays): %v", err)
	}

	if err := UpsertFeature(db, FeatureRow{
		CallID:           callNoReplays,
		TurnType:         "single_file_edit",
		Subsystem:        sql.NullString{String: "internal", Valid: true},
		HadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature (callNoReplays): %v", err)
	}

	// Fixture 2: callAgreed - insertJudgeFixture with agree=true.
	agreeTrue := true
	callAgreed, _, _, _ := insertJudgeFixture(t, db, &agreeTrue)

	if err := UpsertFeature(db, FeatureRow{
		CallID:           callAgreed,
		TurnType:         "single_file_edit",
		Subsystem:        sql.NullString{String: "internal", Valid: true},
		HadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature (callAgreed): %v", err)
	}

	// Fixture 3: callDisagreed - insertJudgeFixture with agree=false.
	agreeFalse := false
	callDisagreed, _, _, _ := insertJudgeFixture(t, db, &agreeFalse)

	if err := UpsertFeature(db, FeatureRow{
		CallID:           callDisagreed,
		TurnType:         "single_file_edit",
		Subsystem:        sql.NullString{String: "internal", Valid: true},
		HadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature (callDisagreed): %v", err)
	}

	// Fixture 4: callErrorFollowup - InsertCall directly, feature with HadErrorFollowup=1.
	callErrorFollowup, err := InsertCall(db, CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-error-followup", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  reqZstd,
		ResponseZstd: frontierRespZstd,
		Source:       "proxy",
	})
	if err != nil {
		t.Fatalf("InsertCall (callErrorFollowup): %v", err)
	}

	if err := UpsertFeature(db, FeatureRow{
		CallID:           callErrorFollowup,
		TurnType:         "single_file_edit",
		Subsystem:        sql.NullString{String: "internal", Valid: true},
		HadErrorFollowup: sql.NullInt64{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature (callErrorFollowup): %v", err)
	}

	// Fixture 5: callWrongTurnType - InsertCall directly, feature with TurnType "multi_file_edit".
	callWrongTurnType, err := InsertCall(db, CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-wrong-turn", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  reqZstd,
		ResponseZstd: frontierRespZstd,
		Source:       "proxy",
	})
	if err != nil {
		t.Fatalf("InsertCall (callWrongTurnType): %v", err)
	}

	if err := UpsertFeature(db, FeatureRow{
		CallID:           callWrongTurnType,
		TurnType:         "multi_file_edit",
		Subsystem:        sql.NullString{String: "internal", Valid: true},
		HadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature (callWrongTurnType): %v", err)
	}

	// Assert HarvestCleanCandidates(db, 10) returns exactly 2 rows: callNoReplays then callAgreed.
	rows, err := HarvestCleanCandidates(db, 10)
	if err != nil {
		t.Fatalf("HarvestCleanCandidates(limit=10): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].CallID != callNoReplays || rows[1].CallID != callAgreed {
		t.Errorf("rows = %+v, want [callNoReplays=%d, callAgreed=%d]", rows, callNoReplays, callAgreed)
	}

	// Assert HarvestCleanCandidates(db, 1) returns exactly 1 row: callNoReplays.
	rows, err = HarvestCleanCandidates(db, 1)
	if err != nil {
		t.Fatalf("HarvestCleanCandidates(limit=1): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].CallID != callNoReplays {
		t.Errorf("rows[0].CallID = %d, want %d", rows[0].CallID, callNoReplays)
	}
}

func TestHarvestQueryErrors(t *testing.T) {
	db, _ := openTestDB(t)
	db.Close()

	tests := []struct {
		name string
		call func() ([]HarvestSourceRow, error)
	}{
		{
			name: "Disagreements",
			call: func() ([]HarvestSourceRow, error) { return HarvestDisagreements(db) },
		},
		{
			name: "Escalations",
			call: func() ([]HarvestSourceRow, error) { return HarvestEscalations(db) },
		},
		{
			name: "ErrorFollowups",
			call: func() ([]HarvestSourceRow, error) { return HarvestErrorFollowups(db) },
		},
		{
			name: "CleanCandidates",
			call: func() ([]HarvestSourceRow, error) { return HarvestCleanCandidates(db, 10) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := tt.call()
			if err == nil {
				t.Fatal("expected error from closed db")
			}
			if rows != nil {
				t.Errorf("returned non-nil slice on error: %v", rows)
			}
		})
	}
}
