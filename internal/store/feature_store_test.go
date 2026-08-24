package store

import (
	"database/sql"
	"errors"
	"testing"
)

func insertPlainCall(t *testing.T, db *sql.DB, sessionID string, withResponse bool) int64 {
	t.Helper()
	row := CallRow{TS: "2026-08-24T12:00:00Z"}
	if sessionID != "" {
		row.SessionID = sql.NullString{String: sessionID, Valid: true}
	}
	if withResponse {
		row.ResponseZstd = []byte("x")
	}
	id, err := InsertCall(db, row)
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	return id
}

func TestUpsertFeature_InsertThenUpdate(t *testing.T) {
	db, _ := openTestDB(t)
	callID := insertPlainCall(t, db, "", true)

	if err := UpsertFeature(db, FeatureRow{
		CallID:       callID,
		TurnType:     "single_file_edit",
		FilesTouched: `["a.go"]`,
		Subsystem:    sql.NullString{String: "internal", Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature (insert): %v", err)
	}

	f, err := GetFeature(db, callID)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	if f.TurnType != "single_file_edit" {
		t.Errorf("TurnType = %q, want single_file_edit", f.TurnType)
	}

	if err := UpsertFeature(db, FeatureRow{
		CallID:           callID,
		TurnType:         "multi_file_edit",
		FilesTouched:     `["a.go","b.go"]`,
		Subsystem:        sql.NullString{String: "internal", Valid: true},
		HadErrorFollowup: sql.NullInt64{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature (update): %v", err)
	}

	f, err = GetFeature(db, callID)
	if err != nil {
		t.Fatalf("GetFeature after update: %v", err)
	}
	if f.TurnType != "multi_file_edit" {
		t.Errorf("TurnType after update = %q, want multi_file_edit", f.TurnType)
	}
	if f.FilesTouched != `["a.go","b.go"]` {
		t.Errorf("FilesTouched after update = %q", f.FilesTouched)
	}
	if !f.HadErrorFollowup.Valid || f.HadErrorFollowup.Int64 != 1 {
		t.Errorf("HadErrorFollowup after update = %+v, want 1", f.HadErrorFollowup)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM features WHERE call_id = ?`, callID).Scan(&count); err != nil {
		t.Fatalf("counting features rows: %v", err)
	}
	if count != 1 {
		t.Errorf("features row count = %d, want 1 (upsert, not duplicate insert)", count)
	}
}

func TestGetFeature_NotFound(t *testing.T) {
	db, _ := openTestDB(t)
	if _, err := GetFeature(db, 999); err == nil {
		t.Fatal("expected error for missing features row")
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected wrapped sql.ErrNoRows, got %v", err)
	}
}

func TestCallIDsMissingFeatures(t *testing.T) {
	db, _ := openTestDB(t)
	withFeatures := insertPlainCall(t, db, "", true)
	missing1 := insertPlainCall(t, db, "", true)
	missing2 := insertPlainCall(t, db, "", true)
	insertPlainCall(t, db, "", false) // no response captured: excluded

	if err := UpsertFeature(db, FeatureRow{CallID: withFeatures, TurnType: "other"}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}

	got, err := CallIDsMissingFeatures(db)
	if err != nil {
		t.Fatalf("CallIDsMissingFeatures: %v", err)
	}
	want := []int64{missing1, missing2}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("CallIDsMissingFeatures() = %v, want %v", got, want)
	}
}

func TestCallIDsWithNullFollowup(t *testing.T) {
	db, _ := openTestDB(t)
	resolvedCall := insertPlainCall(t, db, "", true)
	pendingCall := insertPlainCall(t, db, "", true)

	if err := UpsertFeature(db, FeatureRow{
		CallID: resolvedCall, TurnType: "other",
		HadErrorFollowup: sql.NullInt64{Int64: 0, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature resolved: %v", err)
	}
	if err := UpsertFeature(db, FeatureRow{CallID: pendingCall, TurnType: "other"}); err != nil {
		t.Fatalf("UpsertFeature pending: %v", err)
	}

	got, err := CallIDsWithNullFollowup(db)
	if err != nil {
		t.Fatalf("CallIDsWithNullFollowup: %v", err)
	}
	if len(got) != 1 || got[0] != pendingCall {
		t.Errorf("CallIDsWithNullFollowup() = %v, want [%d]", got, pendingCall)
	}
}

func TestAllCallIDsWithResponse(t *testing.T) {
	db, _ := openTestDB(t)
	withResp1 := insertPlainCall(t, db, "", true)
	withResp2 := insertPlainCall(t, db, "", true)
	insertPlainCall(t, db, "", false)

	got, err := AllCallIDsWithResponse(db)
	if err != nil {
		t.Fatalf("AllCallIDsWithResponse: %v", err)
	}
	want := []int64{withResp1, withResp2}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("AllCallIDsWithResponse() = %v, want %v", got, want)
	}
}

func TestNextCallInSession(t *testing.T) {
	db, _ := openTestDB(t)
	first := insertPlainCall(t, db, "sess-1", true)
	second := insertPlainCall(t, db, "sess-1", true)
	insertPlainCall(t, db, "sess-2", true) // different session, must not match

	next, err := NextCallInSession(db, "sess-1", first)
	if err != nil {
		t.Fatalf("NextCallInSession: %v", err)
	}
	if next.ID != second {
		t.Errorf("NextCallInSession() id = %d, want %d", next.ID, second)
	}

	_, err = NextCallInSession(db, "sess-1", second)
	if err == nil {
		t.Fatal("expected error when no next call exists")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected wrapped sql.ErrNoRows, got %v", err)
	}
}

func TestSpendByTurnType(t *testing.T) {
	db, _ := openTestDB(t)
	callID, err := InsertCall(db, CallRow{
		TS:           "2026-08-24T12:00:00Z",
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		ResponseZstd: []byte("x"),
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	if err := UpsertFeature(db, FeatureRow{
		CallID:        callID,
		TurnType:      "single_file_edit",
		ContextTokens: sql.NullInt64{Int64: 1000, Valid: true},
		OutputTokens:  sql.NullInt64{Int64: 200, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}

	rows, err := SpendByTurnType(db)
	if err != nil {
		t.Fatalf("SpendByTurnType: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].TurnType != "single_file_edit" || rows[0].Model != "claude-sonnet-4-6" {
		t.Errorf("row = %+v", rows[0])
	}
	if rows[0].ContextTokens != 1000 || rows[0].OutputTokens != 200 {
		t.Errorf("token counts = %+v, want 1000/200", rows[0])
	}
}
