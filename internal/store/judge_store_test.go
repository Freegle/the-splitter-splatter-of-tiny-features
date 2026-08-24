package store

import (
	"database/sql"
	"strconv"
	"testing"
)

// insertJudgeFixture inserts a full chain (call, replay, verification,
// queued judge_items row) and returns their ids, mirroring what
// internal/replay and internal/verify write in production.
func insertJudgeFixture(t *testing.T, db *sql.DB, agree *bool) (callID, replayID, verificationID, judgeItemID int64) {
	t.Helper()

	reqZstd, err := Compress([]byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"fix the bug"}]}`))
	if err != nil {
		t.Fatalf("Compress request: %v", err)
	}
	frontierRespZstd, err := Compress([]byte(`{"content":[{"type":"text","text":"frontier answer"}]}`))
	if err != nil {
		t.Fatalf("Compress frontier response: %v", err)
	}

	callID, err = InsertCall(db, CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-1", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  reqZstd,
		ResponseZstd: frontierRespZstd,
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

	localRespZstd, err := Compress([]byte(`{"content":[{"type":"text","text":"local answer"}]}`))
	if err != nil {
		t.Fatalf("Compress local response: %v", err)
	}
	replayID, err = InsertReplay(db, ReplayRow{
		CallID:       callID,
		Backend:      "ollama",
		Model:        "qwen2.5-coder:7b",
		ResponseZstd: localRespZstd,
		CreatedTS:    "2026-08-24T12:01:00Z",
	})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}

	verificationID, err = InsertVerification(db, VerificationRow{
		ReplayID:   replayID,
		Stage:      "ast",
		Similarity: 0.7,
		Agree:      agree,
		DecidedTS:  "2026-08-24T12:02:00Z",
	})
	if err != nil {
		t.Fatalf("InsertVerification: %v", err)
	}

	judgeItemID, err = InsertJudgeItem(db, verificationID, "2026-08-24T12:02:00Z")
	if err != nil {
		t.Fatalf("InsertJudgeItem: %v", err)
	}

	return callID, replayID, verificationID, judgeItemID
}

func TestQueuedJudgeItems(t *testing.T) {
	db, _ := openTestDB(t)

	_, _, verificationID, judgeItemID := insertJudgeFixture(t, db, nil)

	items, err := QueuedJudgeItems(db)
	if err != nil {
		t.Fatalf("QueuedJudgeItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	got := items[0]
	if got.JudgeItemID != judgeItemID {
		t.Errorf("JudgeItemID = %d, want %d", got.JudgeItemID, judgeItemID)
	}
	if got.VerificationID != verificationID {
		t.Errorf("VerificationID = %d, want %d", got.VerificationID, verificationID)
	}
	if len(got.RequestZstd) == 0 || len(got.FrontierResponseZstd) == 0 || len(got.LocalResponseZstd) == 0 {
		t.Errorf("QueuedJudgeItem missing compressed payloads: %+v", got)
	}

	reqJSON, err := Decompress(got.RequestZstd)
	if err != nil {
		t.Fatalf("Decompress request: %v", err)
	}
	if string(reqJSON) != `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"fix the bug"}]}` {
		t.Errorf("decompressed request = %s", reqJSON)
	}
}

func TestQueuedJudgeItems_ExcludesNonQueued(t *testing.T) {
	db, _ := openTestDB(t)
	_, _, _, judgeItemID := insertJudgeFixture(t, db, nil)

	if err := MarkJudgeItemsSubmitted(db, []int64{judgeItemID}, mustInsertJudgeBatch(t, db)); err != nil {
		t.Fatalf("MarkJudgeItemsSubmitted: %v", err)
	}

	items, err := QueuedJudgeItems(db)
	if err != nil {
		t.Fatalf("QueuedJudgeItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("len(items) = %d, want 0 once the only item is submitted", len(items))
	}
}

func mustInsertJudgeBatch(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id, err := InsertJudgeBatch(db, "msgbatch_1", "2026-08-24T12:03:00Z")
	if err != nil {
		t.Fatalf("InsertJudgeBatch: %v", err)
	}
	return id
}

func TestSubmitAndPollBatchLifecycle(t *testing.T) {
	db, _ := openTestDB(t)
	_, _, verificationID, judgeItemID := insertJudgeFixture(t, db, nil)

	batchRowID := mustInsertJudgeBatch(t, db)
	if err := MarkJudgeItemsSubmitted(db, []int64{judgeItemID}, batchRowID); err != nil {
		t.Fatalf("MarkJudgeItemsSubmitted: %v", err)
	}

	pending, err := PendingJudgeBatches(db)
	if err != nil {
		t.Fatalf("PendingJudgeBatches: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != batchRowID || pending[0].BatchID != "msgbatch_1" {
		t.Fatalf("PendingJudgeBatches = %+v", pending)
	}

	refs, err := JudgeItemsForBatch(db, batchRowID)
	if err != nil {
		t.Fatalf("JudgeItemsForBatch: %v", err)
	}
	ref, ok := refs["ji-"+strconv.FormatInt(judgeItemID, 10)]
	if !ok {
		t.Fatalf("JudgeItemsForBatch missing ji-%d in %+v", judgeItemID, refs)
	}
	if ref.ID != judgeItemID || ref.VerificationID != verificationID {
		t.Errorf("ref = %+v, want ID=%d VerificationID=%d", ref, judgeItemID, verificationID)
	}

	if err := UpdateJudgeItemResult(db, judgeItemID, "done", `{"equivalent":true,"confidence":0.9,"reason":"same"}`); err != nil {
		t.Fatalf("UpdateJudgeItemResult: %v", err)
	}
	if err := UpdateVerificationJudgeDecision(db, verificationID, `{"equivalent":true,"confidence":0.9,"reason":"same"}`, true, false, "2026-08-24T12:05:00Z"); err != nil {
		t.Fatalf("UpdateVerificationJudgeDecision: %v", err)
	}
	if err := CompleteJudgeBatch(db, batchRowID, 150, 40, "2026-08-24T12:05:00Z"); err != nil {
		t.Fatalf("CompleteJudgeBatch: %v", err)
	}

	pendingAfter, err := PendingJudgeBatches(db)
	if err != nil {
		t.Fatalf("PendingJudgeBatches (after complete): %v", err)
	}
	if len(pendingAfter) != 0 {
		t.Fatalf("PendingJudgeBatches after completion = %+v, want empty", pendingAfter)
	}

	inputTokens, outputTokens, err := JudgeSpendTotals(db)
	if err != nil {
		t.Fatalf("JudgeSpendTotals: %v", err)
	}
	if inputTokens != 150 || outputTokens != 40 {
		t.Errorf("JudgeSpendTotals = %d/%d, want 150/40", inputTokens, outputTokens)
	}
}

func TestGetVerificationLint(t *testing.T) {
	db, _ := openTestDB(t)
	_, _, verificationID, _ := insertJudgeFixture(t, db, nil)

	// insertJudgeFixture leaves all four lint/test columns NULL.
	lint, err := GetVerificationLint(db, verificationID)
	if err != nil {
		t.Fatalf("GetVerificationLint: %v", err)
	}
	if lint.FrontierLint.Valid || lint.LocalLint.Valid || lint.FrontierTests.Valid || lint.LocalTests.Valid {
		t.Errorf("GetVerificationLint = %+v, want all four columns NULL", lint)
	}
}

func TestAgreementByCategory(t *testing.T) {
	db, _ := openTestDB(t)

	agreeTrue := true
	agreeFalse := false
	insertJudgeFixture(t, db, &agreeTrue)
	insertJudgeFixture(t, db, &agreeFalse)
	insertJudgeFixture(t, db, nil) // still queued, must not count

	rows, err := AgreementByCategory(db)
	if err != nil {
		t.Fatalf("AgreementByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 category (single_file_edit|internal): %+v", len(rows), rows)
	}
	got := rows[0]
	if got.TurnType != "single_file_edit" || got.Subsystem != "internal" {
		t.Errorf("category = %s|%s, want single_file_edit|internal", got.TurnType, got.Subsystem)
	}
	if got.N != 2 {
		t.Errorf("N = %d, want 2 (the still-queued row must be excluded)", got.N)
	}
	if got.Agreed != 1 {
		t.Errorf("Agreed = %d, want 1", got.Agreed)
	}
}

func TestDisagreementRows(t *testing.T) {
	db, _ := openTestDB(t)

	agreeFalse := false
	agreeTrue := true
	insertJudgeFixture(t, db, &agreeFalse)
	insertJudgeFixture(t, db, &agreeTrue) // agreeing row must be excluded

	rows, err := DisagreementRows(db)
	if err != nil {
		t.Fatalf("DisagreementRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].TurnType != "single_file_edit" || rows[0].Subsystem != "internal" {
		t.Errorf("row = %+v", rows[0])
	}
}

func TestEditTurnJudgeCounts(t *testing.T) {
	db, _ := openTestDB(t)

	agreeTrue := true
	_, _, verificationID, _ := insertJudgeFixture(t, db, &agreeTrue)
	insertJudgeFixture(t, db, nil) // still ast stage, not judge

	total, judged, err := EditTurnJudgeCounts(db)
	if err != nil {
		t.Fatalf("EditTurnJudgeCounts: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if judged != 0 {
		t.Errorf("judged = %d, want 0 (neither row has stage='judge' yet)", judged)
	}

	if err := UpdateVerificationJudgeDecision(db, verificationID, `{"equivalent":true,"confidence":0.9,"reason":"x"}`, true, false, "2026-08-24T12:05:00Z"); err != nil {
		t.Fatalf("UpdateVerificationJudgeDecision: %v", err)
	}

	total, judged, err = EditTurnJudgeCounts(db)
	if err != nil {
		t.Fatalf("EditTurnJudgeCounts (after decision): %v", err)
	}
	if total != 2 || judged != 1 {
		t.Errorf("total/judged = %d/%d, want 2/1", total, judged)
	}
}

func TestJudgeSpendTotals_NoBatchesYet(t *testing.T) {
	db, _ := openTestDB(t)
	inputTokens, outputTokens, err := JudgeSpendTotals(db)
	if err != nil {
		t.Fatalf("JudgeSpendTotals: %v", err)
	}
	if inputTokens != 0 || outputTokens != 0 {
		t.Errorf("JudgeSpendTotals = %d/%d, want 0/0", inputTokens, outputTokens)
	}
}

func TestTotalReplayCount(t *testing.T) {
	db, _ := openTestDB(t)
	insertJudgeFixture(t, db, nil)
	insertJudgeFixture(t, db, nil)

	n, err := TotalReplayCount(db)
	if err != nil {
		t.Fatalf("TotalReplayCount: %v", err)
	}
	if n != 2 {
		t.Errorf("TotalReplayCount = %d, want 2", n)
	}
}
