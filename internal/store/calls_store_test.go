package store

import (
	"database/sql"
	"testing"
)

func TestInsertGetCall_RoundTrip(t *testing.T) {
	db, _ := openTestDB(t)

	reqJSON := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	respJSON := []byte(`{"content":[{"type":"text","text":"hello"}]}`)

	reqZstd, err := Compress(reqJSON)
	if err != nil {
		t.Fatalf("Compress request: %v", err)
	}
	respZstd, err := Compress(respJSON)
	if err != nil {
		t.Fatalf("Compress response: %v", err)
	}

	row := CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-123", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		Stream:       true,
		RequestZstd:  reqZstd,
		ResponseZstd: respZstd,
		InputTokens:  sql.NullInt64{Int64: 42, Valid: true},
		OutputTokens: sql.NullInt64{Int64: 7, Valid: true},
		LatencyMs:    sql.NullInt64{Int64: 1234, Valid: true},
		RepoHead:     sql.NullString{String: "abc123", Valid: true},
		Status:       sql.NullInt64{Int64: 200, Valid: true},
		Error:        sql.NullString{},
		Source:       "proxy",
	}

	id, err := InsertCall(db, row)
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	if id == 0 {
		t.Fatal("InsertCall returned id 0")
	}

	got, err := GetCall(db, id)
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}

	if got.TS != row.TS {
		t.Errorf("TS = %q, want %q", got.TS, row.TS)
	}
	if got.SessionID != row.SessionID {
		t.Errorf("SessionID = %+v, want %+v", got.SessionID, row.SessionID)
	}
	if got.Model != row.Model {
		t.Errorf("Model = %+v, want %+v", got.Model, row.Model)
	}
	if got.Stream != true {
		t.Errorf("Stream = %v, want true", got.Stream)
	}
	if got.InputTokens != row.InputTokens {
		t.Errorf("InputTokens = %+v, want %+v", got.InputTokens, row.InputTokens)
	}
	if got.OutputTokens != row.OutputTokens {
		t.Errorf("OutputTokens = %+v, want %+v", got.OutputTokens, row.OutputTokens)
	}
	if got.LatencyMs != row.LatencyMs {
		t.Errorf("LatencyMs = %+v, want %+v", got.LatencyMs, row.LatencyMs)
	}
	if got.RepoHead != row.RepoHead {
		t.Errorf("RepoHead = %+v, want %+v", got.RepoHead, row.RepoHead)
	}
	if got.Status != row.Status {
		t.Errorf("Status = %+v, want %+v", got.Status, row.Status)
	}
	if got.Source != "proxy" {
		t.Errorf("Source = %q, want proxy", got.Source)
	}

	decompressedReq, err := Decompress(got.RequestZstd)
	if err != nil {
		t.Fatalf("Decompress request: %v", err)
	}
	if string(decompressedReq) != string(reqJSON) {
		t.Errorf("decompressed request = %q, want %q", decompressedReq, reqJSON)
	}

	decompressedResp, err := Decompress(got.ResponseZstd)
	if err != nil {
		t.Fatalf("Decompress response: %v", err)
	}
	if string(decompressedResp) != string(respJSON) {
		t.Errorf("decompressed response = %q, want %q", decompressedResp, respJSON)
	}
}

func TestInsertCall_DefaultsSourceToProxy(t *testing.T) {
	db, _ := openTestDB(t)

	id, err := InsertCall(db, CallRow{TS: "2026-08-24T12:00:00Z"})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	got, err := GetCall(db, id)
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if got.Source != "proxy" {
		t.Errorf("Source = %q, want proxy", got.Source)
	}
}

func TestInsertCall_ImportSource(t *testing.T) {
	db, _ := openTestDB(t)

	id, err := InsertCall(db, CallRow{TS: "2026-08-24T12:00:00Z", Source: "import"})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}

	got, err := GetCall(db, id)
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if got.Source != "import" {
		t.Errorf("Source = %q, want import", got.Source)
	}
}

func TestGetCall_NotFound(t *testing.T) {
	db, _ := openTestDB(t)

	_, err := GetCall(db, 999999)
	if err == nil {
		t.Fatal("expected error for missing call id")
	}
}
