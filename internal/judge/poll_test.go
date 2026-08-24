package judge

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/freegle/splitter/internal/store"
)

// insertVerificationFixture inserts a call/features/replay/verification
// chain with the given lint/test JSON columns (empty string means NULL,
// matching store.InsertVerification's nullIfEmpty convention) and a
// queued judge_items row, returning the judge item id and verification id.
func insertVerificationFixture(t *testing.T, db *sql.DB, frontierLint, localLint, frontierTests, localTests string) (judgeItemID, verificationID int64) {
	t.Helper()

	reqZstd, err := store.Compress([]byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"fix it"}]}`))
	if err != nil {
		t.Fatalf("Compress request: %v", err)
	}
	frontierZstd, err := store.Compress([]byte(`{"content":[{"type":"text","text":"frontier answer"}]}`))
	if err != nil {
		t.Fatalf("Compress frontier response: %v", err)
	}
	callID, err := store.InsertCall(db, store.CallRow{
		TS:           "2026-08-24T12:00:00Z",
		SessionID:    sql.NullString{String: "sess-1", Valid: true},
		Model:        sql.NullString{String: "claude-sonnet-4-6", Valid: true},
		RequestZstd:  reqZstd,
		ResponseZstd: frontierZstd,
		Source:       "proxy",
	})
	if err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	if err := store.UpsertFeature(db, store.FeatureRow{
		CallID:    callID,
		TurnType:  "single_file_edit",
		Subsystem: sql.NullString{String: "internal", Valid: true},
	}); err != nil {
		t.Fatalf("UpsertFeature: %v", err)
	}

	localZstd, err := store.Compress([]byte(`{"content":[{"type":"text","text":"local answer"}]}`))
	if err != nil {
		t.Fatalf("Compress local response: %v", err)
	}
	replayID, err := store.InsertReplay(db, store.ReplayRow{
		CallID:       callID,
		Backend:      "ollama",
		Model:        "qwen2.5-coder:7b",
		ResponseZstd: localZstd,
		CreatedTS:    "2026-08-24T12:01:00Z",
	})
	if err != nil {
		t.Fatalf("InsertReplay: %v", err)
	}

	verificationID, err = store.InsertVerification(db, store.VerificationRow{
		ReplayID:      replayID,
		Stage:         "ast",
		Similarity:    0.7,
		FrontierLint:  frontierLint,
		LocalLint:     localLint,
		FrontierTests: frontierTests,
		LocalTests:    localTests,
	})
	if err != nil {
		t.Fatalf("InsertVerification: %v", err)
	}

	judgeItemID, err = store.InsertJudgeItem(db, verificationID, "2026-08-24T12:02:00Z")
	if err != nil {
		t.Fatalf("InsertJudgeItem: %v", err)
	}
	return judgeItemID, verificationID
}

// submitAndAssignBatch marks itemID submitted and linked to a fresh
// judge_batches row, returning the batch's DB row id and its (fake)
// upstream batch_id.
func submitAndAssignBatch(t *testing.T, db *sql.DB, upstreamBatchID string, itemIDs ...int64) int64 {
	t.Helper()
	rowID, err := store.InsertJudgeBatch(db, upstreamBatchID, "2026-08-24T12:03:00Z")
	if err != nil {
		t.Fatalf("InsertJudgeBatch: %v", err)
	}
	if err := store.MarkJudgeItemsSubmitted(db, itemIDs, rowID); err != nil {
		t.Fatalf("MarkJudgeItemsSubmitted: %v", err)
	}
	return rowID
}

func TestPoll_NoPendingBatches(t *testing.T) {
	db := openTestDB(t)
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	result, err := Poll(context.Background(), db, Config{Upstream: server.URL})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if result.BatchesChecked != 0 {
		t.Errorf("BatchesChecked = %d, want 0", result.BatchesChecked)
	}
	if called {
		t.Error("Poll made an HTTP call despite there being no pending batches")
	}
}

func TestPoll_StaysPendingUntilEnded(t *testing.T) {
	db := openTestDB(t)
	itemID, verificationID := insertVerificationFixture(t, db, "", "", "", "")
	submitAndAssignBatch(t, db, "msgbatch_pending", itemID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msgbatch_pending","processing_status":"in_progress"}`))
	}))
	defer server.Close()

	result, err := Poll(context.Background(), db, Config{Upstream: server.URL, Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if result.BatchesChecked != 1 || result.BatchesEnded != 0 {
		t.Errorf("result = %+v, want BatchesChecked=1 BatchesEnded=0", result)
	}

	decision, err := store.GetVerificationDecision(db, verificationID)
	if err != nil {
		t.Fatalf("GetVerificationDecision: %v", err)
	}
	if decision.Agree.Valid {
		t.Errorf("verification decided (%+v) while its batch is still in_progress", decision)
	}
	if decision.Stage != "ast" {
		t.Errorf("stage = %q, want ast (unchanged, judge has not decided yet)", decision.Stage)
	}

	pending, err := store.PendingJudgeBatches(db)
	if err != nil {
		t.Fatalf("PendingJudgeBatches: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("PendingJudgeBatches = %+v, want the batch still pending", pending)
	}
}

// TestPoll_ConflictShuffledSpend covers, in one end-to-end Poll call:
// results arriving in an order that does not match submission order, a
// batch-level errored result, a verdict wrapped in prose that only a
// lenient parse recovers, and the tests-win-over-judge conflict rule in
// both directions (tests failing overrides an agreeing judge, and tests
// passing overrides a disagreeing judge), plus usage accumulation.
func TestPoll_ConflictShuffledSpend(t *testing.T) {
	db := openTestDB(t)

	// A: both sides clean per lint/tests, but the judge (lenient-parsed from
	// a prose-wrapped reply) says NOT equivalent. Tests win: agree=true,
	// conflict=1.
	itemA, verificationA := insertVerificationFixture(t, db,
		`{"tool":"gofmt","ok":true}`, `{"tool":"gofmt","ok":true}`, "", "")

	// B: the local side's tests failed, but the judge says equivalent with
	// high confidence. Tests win: agree=false, conflict=1. This is
	// DESIGN.md's example conflict.
	itemB, verificationB := insertVerificationFixture(t, db,
		`{"tool":"go test","ok":true}`, `{"tool":"go test","ok":false}`, "", "")

	// C: no lint/test information recorded at all, so the judge's own
	// verdict decides outright: agree=true, conflict=0.
	itemC, verificationC := insertVerificationFixture(t, db, "", "", "", "")

	// D: the batch itself reports this item as errored; it must not touch
	// its verification at all.
	itemD, verificationD := insertVerificationFixture(t, db, "", "", "", "")

	batchRowID := submitAndAssignBatch(t, db, "msgbatch_mixed", itemA, itemB, itemC, itemD)

	customA := "ji-" + strconv.FormatInt(itemA, 10)
	customB := "ji-" + strconv.FormatInt(itemB, 10)
	customC := "ji-" + strconv.FormatInt(itemC, 10)
	customD := "ji-" + strconv.FormatInt(itemD, 10)

	// Deliberately shuffled: D, B, A, C (not submission order A,B,C,D).
	jsonl := `{"custom_id":"` + customD + `","result":{"type":"errored","error":{"type":"invalid_request","message":"content policy violation"}}}
{"custom_id":"` + customB + `","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"{\"equivalent\": true, \"confidence\": 0.95, \"reason\": \"functionally identical\"}"}],"usage":{"input_tokens":100,"output_tokens":30}}}}
{"custom_id":"` + customA + `","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"Looking closely, the outputs differ.\n\n{\"equivalent\": false, \"confidence\": 0.8, \"reason\": \"local model missed the edge case\"}\n\nLet me know if you need more.\n"}],"usage":{"input_tokens":110,"output_tokens":35}}}}
{"custom_id":"` + customC + `","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"{\"equivalent\": true, \"confidence\": 0.9, \"reason\": \"same behaviour\"}"}],"usage":{"input_tokens":90,"output_tokens":20}}}}
`

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages/batches/msgbatch_mixed":
			_, _ = w.Write([]byte(`{"id":"msgbatch_mixed","processing_status":"ended","results_url":"` + server.URL + `/results"}`))
		case "/results":
			w.Header().Set("Content-Type", "application/x-jsonl")
			_, _ = w.Write([]byte(jsonl))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer server.Close()

	result, err := Poll(context.Background(), db, Config{Upstream: server.URL, Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if result.BatchesChecked != 1 || result.BatchesEnded != 1 {
		t.Errorf("result = %+v, want BatchesChecked=1 BatchesEnded=1", result)
	}
	if result.ItemsSucceeded != 3 {
		t.Errorf("ItemsSucceeded = %d, want 3", result.ItemsSucceeded)
	}
	if result.ItemsErrored != 1 {
		t.Errorf("ItemsErrored = %d, want 1", result.ItemsErrored)
	}
	wantInputTokens := int64(100 + 110 + 90)
	wantOutputTokens := int64(30 + 35 + 20)
	if result.InputTokens != wantInputTokens || result.OutputTokens != wantOutputTokens {
		t.Errorf("tokens = %d/%d, want %d/%d", result.InputTokens, result.OutputTokens, wantInputTokens, wantOutputTokens)
	}

	decisionA, err := store.GetVerificationDecision(db, verificationA)
	if err != nil {
		t.Fatalf("GetVerificationDecision A: %v", err)
	}
	if decisionA.Stage != "judge" || !decisionA.Agree.Valid || decisionA.Agree.Int64 != 1 || !decisionA.Conflict {
		t.Errorf("A decision = %+v, want stage=judge agree=1 conflict=true (tests clean overrides judge's disagreement)", decisionA)
	}

	decisionB, err := store.GetVerificationDecision(db, verificationB)
	if err != nil {
		t.Fatalf("GetVerificationDecision B: %v", err)
	}
	if decisionB.Stage != "judge" || !decisionB.Agree.Valid || decisionB.Agree.Int64 != 0 || !decisionB.Conflict {
		t.Errorf("B decision = %+v, want stage=judge agree=0 conflict=true (tests failing overrides judge's agreement)", decisionB)
	}

	decisionC, err := store.GetVerificationDecision(db, verificationC)
	if err != nil {
		t.Fatalf("GetVerificationDecision C: %v", err)
	}
	if decisionC.Stage != "judge" || !decisionC.Agree.Valid || decisionC.Agree.Int64 != 1 || decisionC.Conflict {
		t.Errorf("C decision = %+v, want stage=judge agree=1 conflict=false (no tests info, judge decides)", decisionC)
	}

	decisionD, err := store.GetVerificationDecision(db, verificationD)
	if err != nil {
		t.Fatalf("GetVerificationDecision D: %v", err)
	}
	if decisionD.Stage != "ast" || decisionD.Agree.Valid {
		t.Errorf("D decision = %+v, want stage=ast agree=NULL (errored batch item must not touch its verification)", decisionD)
	}

	refs, err := store.JudgeItemsForBatch(db, batchRowID)
	if err != nil {
		t.Fatalf("JudgeItemsForBatch: %v", err)
	}
	if len(refs) != 4 {
		t.Fatalf("JudgeItemsForBatch = %+v, want 4 entries", refs)
	}

	pendingAfter, err := store.PendingJudgeBatches(db)
	if err != nil {
		t.Fatalf("PendingJudgeBatches: %v", err)
	}
	if len(pendingAfter) != 0 {
		t.Errorf("PendingJudgeBatches after completion = %+v, want empty", pendingAfter)
	}

	spentIn, spentOut, err := store.JudgeSpendTotals(db)
	if err != nil {
		t.Fatalf("JudgeSpendTotals: %v", err)
	}
	if spentIn != wantInputTokens || spentOut != wantOutputTokens {
		t.Errorf("JudgeSpendTotals = %d/%d, want %d/%d", spentIn, spentOut, wantInputTokens, wantOutputTokens)
	}
}
