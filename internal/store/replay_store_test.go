package store

import (
	"database/sql"
	"fmt"
	"testing"
)

func insertCallWithFeature(t *testing.T, db *sql.DB, ts, turnType string) int64 {
	t.Helper()
	id, err := InsertCall(db, CallRow{TS: ts})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	if turnType != "" {
		if err := UpsertFeature(db, FeatureRow{CallID: id, TurnType: turnType, FilesTouched: "[]"}); err != nil {
			t.Fatalf("UpsertFeature: %v", err)
		}
	}
	return id
}

func TestSelectReplayCandidates_FiltersAndOrders(t *testing.T) {
	db, _ := openTestDB(t)

	other := insertCallWithFeature(t, db, "2026-08-24T10:00:00Z", "other")
	eligible1 := insertCallWithFeature(t, db, "2026-08-24T10:01:00Z", "question_answer")
	alreadyReplayed := insertCallWithFeature(t, db, "2026-08-24T10:02:00Z", "question_answer")
	eligible2 := insertCallWithFeature(t, db, "2026-08-24T10:03:00Z", "single_file_edit")
	noFeature, err := InsertCall(db, CallRow{TS: "2026-08-24T10:04:00Z"})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	if _, err := InsertReplay(db, ReplayRow{CallID: alreadyReplayed, Backend: "ollama", Model: "m1", CreatedTS: "2026-08-24T10:05:00Z"}); err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}
	// A replay for a DIFFERENT backend/model must not exclude eligible2.
	if _, err := InsertReplay(db, ReplayRow{CallID: eligible2, Backend: "other-backend", Model: "m1", CreatedTS: "2026-08-24T10:05:00Z"}); err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}

	got, err := SelectReplayCandidates(db, "ollama", "m1", 10)
	if err != nil {
		t.Fatalf("SelectReplayCandidates: %v", err)
	}

	var gotIDs []int64
	for _, c := range got {
		gotIDs = append(gotIDs, c.CallID)
	}
	want := []int64{eligible1, eligible2}
	if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Errorf("candidate ids = %v, want %v (excludes other=%d, alreadyReplayed=%d, noFeature=%d)", gotIDs, want, other, alreadyReplayed, noFeature)
	}

	if got[0].TurnType != "question_answer" || got[1].TurnType != "single_file_edit" {
		t.Errorf("turn types = %q, %q", got[0].TurnType, got[1].TurnType)
	}
}

func TestSelectReplayCandidates_RespectsLimit(t *testing.T) {
	db, _ := openTestDB(t)
	for i := 0; i < 5; i++ {
		insertCallWithFeature(t, db, fmt.Sprintf("2026-08-24T10:0%d:00Z", i), "question_answer")
	}

	got, err := SelectReplayCandidates(db, "ollama", "m1", 2)
	if err != nil {
		t.Fatalf("SelectReplayCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}
}

func TestInsertReplay_ErrorRowHasNoResponse(t *testing.T) {
	db, _ := openTestDB(t)
	callID := insertCallWithFeature(t, db, "2026-08-24T10:00:00Z", "question_answer")

	id, err := InsertReplay(db, ReplayRow{
		CallID: callID, Backend: "ollama", Model: "m1", Error: "boom", CreatedTS: "2026-08-24T10:01:00Z",
	})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}

	var respZstd []byte
	var errCol sql.NullString
	if err := db.QueryRow(`SELECT response_zstd, error FROM replays WHERE id = ?`, id).Scan(&respZstd, &errCol); err != nil {
		t.Fatalf("querying replay: %v", err)
	}
	if respZstd != nil {
		t.Errorf("response_zstd = %v, want NULL", respZstd)
	}
	if !errCol.Valid || errCol.String != "boom" {
		t.Errorf("error = %+v, want boom", errCol)
	}
}

func TestInsertVerification_DecidedRowVsBandRow(t *testing.T) {
	db, _ := openTestDB(t)
	callID := insertCallWithFeature(t, db, "2026-08-24T10:00:00Z", "question_answer")
	replayID, err := InsertReplay(db, ReplayRow{CallID: callID, Backend: "ollama", Model: "m1", CreatedTS: "2026-08-24T10:01:00Z"})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}

	agree := true
	id, err := InsertVerification(db, VerificationRow{
		ReplayID: replayID, Stage: "exact", Similarity: 1, Agree: &agree, DecidedTS: "2026-08-24T10:02:00Z",
	})
	if err != nil {
		t.Fatalf("InsertVerification: %v", err)
	}
	var gotAgree sql.NullInt64
	var gotDecided sql.NullString
	if err := db.QueryRow(`SELECT agree, decided_ts FROM verifications WHERE id = ?`, id).Scan(&gotAgree, &gotDecided); err != nil {
		t.Fatalf("querying verification: %v", err)
	}
	if !gotAgree.Valid || gotAgree.Int64 != 1 {
		t.Errorf("agree = %+v, want 1", gotAgree)
	}
	if !gotDecided.Valid || gotDecided.String != "2026-08-24T10:02:00Z" {
		t.Errorf("decided_ts = %+v, want 2026-08-24T10:02:00Z", gotDecided)
	}

	replayID2, err := InsertReplay(db, ReplayRow{CallID: callID, Backend: "ollama2", Model: "m1", CreatedTS: "2026-08-24T10:01:00Z"})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}
	bandID, err := InsertVerification(db, VerificationRow{ReplayID: replayID2, Stage: "ast", Similarity: 0.7})
	if err != nil {
		t.Fatalf("InsertVerification (band): %v", err)
	}
	var bandAgree sql.NullInt64
	var bandDecided sql.NullString
	if err := db.QueryRow(`SELECT agree, decided_ts FROM verifications WHERE id = ?`, bandID).Scan(&bandAgree, &bandDecided); err != nil {
		t.Fatalf("querying band verification: %v", err)
	}
	if bandAgree.Valid {
		t.Errorf("agree = %v, want NULL for a middle-band row", bandAgree.Int64)
	}
	if bandDecided.Valid {
		t.Errorf("decided_ts = %q, want NULL for a middle-band row", bandDecided.String)
	}
}

func TestInsertJudgeItem_SetsCustomIDFromOwnID(t *testing.T) {
	db, _ := openTestDB(t)
	callID := insertCallWithFeature(t, db, "2026-08-24T10:00:00Z", "question_answer")
	replayID, err := InsertReplay(db, ReplayRow{CallID: callID, Backend: "ollama", Model: "m1", CreatedTS: "2026-08-24T10:01:00Z"})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}
	verificationID, err := InsertVerification(db, VerificationRow{ReplayID: replayID, Stage: "ast", Similarity: 0.7})
	if err != nil {
		t.Fatalf("InsertVerification: %v", err)
	}

	itemID, err := InsertJudgeItem(db, verificationID, "2026-08-24T10:03:00Z")
	if err != nil {
		t.Fatalf("InsertJudgeItem: %v", err)
	}

	var customID, status string
	if err := db.QueryRow(`SELECT custom_id, status FROM judge_items WHERE id = ?`, itemID).Scan(&customID, &status); err != nil {
		t.Fatalf("querying judge_items: %v", err)
	}
	wantCustomID := fmt.Sprintf("ji-%d", itemID)
	if customID != wantCustomID {
		t.Errorf("custom_id = %q, want %q", customID, wantCustomID)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
}

func TestNewestProxyCallTS_IgnoresImportedAndEmpty(t *testing.T) {
	db, _ := openTestDB(t)

	_, ok, err := NewestProxyCallTS(db)
	if err != nil {
		t.Fatalf("NewestProxyCallTS: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false with no calls at all")
	}

	if _, err := InsertCall(db, CallRow{TS: "2026-08-24T12:00:00Z", Source: "import"}); err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	_, ok, err = NewestProxyCallTS(db)
	if err != nil {
		t.Fatalf("NewestProxyCallTS: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false with only an imported call")
	}

	if _, err := InsertCall(db, CallRow{TS: "2026-08-24T11:00:00Z", Source: "proxy"}); err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	if _, err := InsertCall(db, CallRow{TS: "2026-08-24T13:00:00Z", Source: "proxy"}); err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	ts, ok, err := NewestProxyCallTS(db)
	if err != nil {
		t.Fatalf("NewestProxyCallTS: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with proxy calls present")
	}
	if got := ts.Format("2006-01-02T15:04:05Z"); got != "2026-08-24T13:00:00Z" {
		t.Errorf("newest ts = %s, want 2026-08-24T13:00:00Z", got)
	}
}
