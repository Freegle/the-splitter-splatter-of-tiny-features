package store

import (
	"database/sql"
	"fmt"
)

// QueuedJudgeItem carries everything judge submit needs to build one batch
// request entry for a queued judge_items row: the frontier request and
// response of the call the local model was replayed against, and the
// local model's own response, all still zstd-compressed (callers
// decompress).
type QueuedJudgeItem struct {
	JudgeItemID          int64
	VerificationID       int64
	CallID               int64
	RequestZstd          []byte
	FrontierResponseZstd []byte
	LocalResponseZstd    []byte
}

// QueuedJudgeItems returns every judge_items row with status='queued',
// oldest first, joined to the verification's replay and call for the
// context judge submit needs.
func QueuedJudgeItems(db *sql.DB) ([]QueuedJudgeItem, error) {
	rows, err := db.Query(`
SELECT ji.id, v.id, c.id, c.request_zstd, c.response_zstd, r.response_zstd
FROM judge_items ji
JOIN verifications v ON v.id = ji.verification_id
JOIN replays r ON r.id = v.replay_id
JOIN calls c ON c.id = r.call_id
WHERE ji.status = 'queued'
ORDER BY ji.id`)
	if err != nil {
		return nil, fmt.Errorf("querying queued judge items: %w", err)
	}
	defer rows.Close()

	var out []QueuedJudgeItem
	for rows.Next() {
		var q QueuedJudgeItem
		if err := rows.Scan(&q.JudgeItemID, &q.VerificationID, &q.CallID, &q.RequestZstd, &q.FrontierResponseZstd, &q.LocalResponseZstd); err != nil {
			return nil, fmt.Errorf("scanning queued judge item: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating queued judge items: %w", err)
	}
	return out, nil
}

// InsertJudgeBatch inserts a new judge_batches row with status='submitted'
// and returns its id.
func InsertJudgeBatch(db *sql.DB, batchID, submittedTS string) (int64, error) {
	res, err := db.Exec(`
INSERT INTO judge_batches (batch_id, submitted_ts, status) VALUES (?, ?, 'submitted')`,
		batchID, submittedTS)
	if err != nil {
		return 0, fmt.Errorf("inserting judge batch %s: %w", batchID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted judge batch id: %w", err)
	}
	return id, nil
}

// MarkJudgeItemsSubmitted links itemIDs to judgeBatchRowID and flips their
// status to 'submitted'.
func MarkJudgeItemsSubmitted(db *sql.DB, itemIDs []int64, judgeBatchRowID int64) error {
	stmt, err := db.Prepare(`UPDATE judge_items SET judge_batch_id = ?, status = 'submitted' WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("preparing judge item submit update: %w", err)
	}
	defer stmt.Close()

	for _, id := range itemIDs {
		if _, err := stmt.Exec(judgeBatchRowID, id); err != nil {
			return fmt.Errorf("marking judge item %d submitted: %w", id, err)
		}
	}
	return nil
}

// PendingJudgeBatch is one judge_batches row not yet marked completed.
type PendingJudgeBatch struct {
	ID      int64
	BatchID string
}

// PendingJudgeBatches returns every judge_batches row with status
// 'submitted', oldest first: batches judge poll has not yet seen end.
func PendingJudgeBatches(db *sql.DB) ([]PendingJudgeBatch, error) {
	rows, err := db.Query(`SELECT id, batch_id FROM judge_batches WHERE status = 'submitted' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("querying pending judge batches: %w", err)
	}
	defer rows.Close()

	var out []PendingJudgeBatch
	for rows.Next() {
		var b PendingJudgeBatch
		if err := rows.Scan(&b.ID, &b.BatchID); err != nil {
			return nil, fmt.Errorf("scanning pending judge batch: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pending judge batches: %w", err)
	}
	return out, nil
}

// JudgeItemRef locates the verification one judge_items row decides.
type JudgeItemRef struct {
	ID             int64
	VerificationID int64
}

// JudgeItemsForBatch returns every judge_items row belonging to
// judgeBatchRowID, keyed by custom_id, for poll to resolve a batch's JSONL
// results against (results arrive keyed by custom_id, never by order).
func JudgeItemsForBatch(db *sql.DB, judgeBatchRowID int64) (map[string]JudgeItemRef, error) {
	rows, err := db.Query(`SELECT id, verification_id, custom_id FROM judge_items WHERE judge_batch_id = ?`, judgeBatchRowID)
	if err != nil {
		return nil, fmt.Errorf("querying judge items for batch %d: %w", judgeBatchRowID, err)
	}
	defer rows.Close()

	out := make(map[string]JudgeItemRef)
	for rows.Next() {
		var ref JudgeItemRef
		var customID string
		if err := rows.Scan(&ref.ID, &ref.VerificationID, &customID); err != nil {
			return nil, fmt.Errorf("scanning judge item for batch %d: %w", judgeBatchRowID, err)
		}
		out[customID] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating judge items for batch %d: %w", judgeBatchRowID, err)
	}
	return out, nil
}

// UpdateJudgeItemResult sets a judge_items row's status and verdict text:
// the marshaled Verdict JSON when status is 'done', or a diagnostic string
// when 'errored'.
func UpdateJudgeItemResult(db *sql.DB, id int64, status, verdict string) error {
	if _, err := db.Exec(`UPDATE judge_items SET status = ?, verdict = ? WHERE id = ?`, status, verdict, id); err != nil {
		return fmt.Errorf("updating judge item %d result: %w", id, err)
	}
	return nil
}

// CompleteJudgeBatch marks a judge_batches row completed and records its
// accumulated usage tokens.
func CompleteJudgeBatch(db *sql.DB, id int64, inputTokens, outputTokens int64, completedTS string) error {
	_, err := db.Exec(`
UPDATE judge_batches SET status = 'completed', completed_ts = ?, input_tokens = ?, output_tokens = ?
WHERE id = ?`, completedTS, inputTokens, outputTokens, id)
	if err != nil {
		return fmt.Errorf("completing judge batch %d: %w", id, err)
	}
	return nil
}

// VerificationLint holds the four lint/test JSON columns of one
// verification, used to arbitrate the tests-vs-judge conflict rule.
type VerificationLint struct {
	FrontierLint  sql.NullString
	LocalLint     sql.NullString
	FrontierTests sql.NullString
	LocalTests    sql.NullString
}

// GetVerificationLint fetches verificationID's four lint/test columns.
func GetVerificationLint(db *sql.DB, verificationID int64) (VerificationLint, error) {
	var v VerificationLint
	row := db.QueryRow(`SELECT frontier_lint, local_lint, frontier_tests, local_tests FROM verifications WHERE id = ?`, verificationID)
	if err := row.Scan(&v.FrontierLint, &v.LocalLint, &v.FrontierTests, &v.LocalTests); err != nil {
		return VerificationLint{}, fmt.Errorf("getting lint/test fields for verification %d: %w", verificationID, err)
	}
	return v, nil
}

// UpdateVerificationJudgeDecision records the judge stage's decision for a
// verification: its judge_verdict JSON, the final agree value (after the
// tests-win conflict rule), whether that rule found a conflict, and marks
// stage='judge' (the judge is what decided this verification, whatever
// placeholder stage it carried while queued).
func UpdateVerificationJudgeDecision(db *sql.DB, verificationID int64, verdictJSON string, agree, conflict bool, decidedTS string) error {
	_, err := db.Exec(`
UPDATE verifications
SET stage = 'judge', judge_verdict = ?, agree = ?, tests_judge_conflict = ?, decided_ts = ?
WHERE id = ?`, verdictJSON, boolToInt(agree), boolToInt(conflict), decidedTS, verificationID)
	if err != nil {
		return fmt.Errorf("updating verification %d judge decision: %w", verificationID, err)
	}
	return nil
}

// CategoryAgreement is one turn_type x subsystem row of decided
// verifications (agree IS NOT NULL): the sample size and how many agreed.
type CategoryAgreement struct {
	TurnType  string
	Subsystem string
	N         int
	Agreed    int
}

// AgreementByCategory groups every decided verification by its call's
// turn_type and subsystem (features.subsystem, empty string when none).
func AgreementByCategory(db *sql.DB) ([]CategoryAgreement, error) {
	rows, err := db.Query(`
SELECT f.turn_type, COALESCE(f.subsystem, ''), COUNT(*), COALESCE(SUM(v.agree), 0)
FROM verifications v
JOIN replays r ON r.id = v.replay_id
JOIN calls c ON c.id = r.call_id
JOIN features f ON f.call_id = c.id
WHERE v.agree IS NOT NULL
GROUP BY f.turn_type, COALESCE(f.subsystem, '')
ORDER BY f.turn_type, COALESCE(f.subsystem, '')`)
	if err != nil {
		return nil, fmt.Errorf("querying agreement by category: %w", err)
	}
	defer rows.Close()

	var out []CategoryAgreement
	for rows.Next() {
		var c CategoryAgreement
		if err := rows.Scan(&c.TurnType, &c.Subsystem, &c.N, &c.Agreed); err != nil {
			return nil, fmt.Errorf("scanning agreement category row: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating agreement category rows: %w", err)
	}
	return out, nil
}

// DisagreementReason is one disagreeing (agree=0) verification's category
// and raw judge_verdict JSON (invalid/empty when it was decided by a stage
// other than judge, so has no reason to offer).
type DisagreementReason struct {
	TurnType     string
	Subsystem    string
	JudgeVerdict sql.NullString
}

// DisagreementRows returns every disagreeing verification's category and
// judge_verdict, the input to the report's top-N-reasons-per-category
// aggregation (done in Go, not SQL, so it does not depend on SQLite's
// JSON1 extension being compiled into the driver).
func DisagreementRows(db *sql.DB) ([]DisagreementReason, error) {
	rows, err := db.Query(`
SELECT f.turn_type, COALESCE(f.subsystem, ''), v.judge_verdict
FROM verifications v
JOIN replays r ON r.id = v.replay_id
JOIN calls c ON c.id = r.call_id
JOIN features f ON f.call_id = c.id
WHERE v.agree = 0`)
	if err != nil {
		return nil, fmt.Errorf("querying disagreement rows: %w", err)
	}
	defer rows.Close()

	var out []DisagreementReason
	for rows.Next() {
		var d DisagreementReason
		if err := rows.Scan(&d.TurnType, &d.Subsystem, &d.JudgeVerdict); err != nil {
			return nil, fmt.Errorf("scanning disagreement row: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating disagreement rows: %w", err)
	}
	return out, nil
}

// EditTurnJudgeCounts returns the number of verified edit turns
// (single_file_edit or multi_file_edit) and how many of those were decided
// by the judge stage: the inputs to the judge-share-of-edit-turns report
// line and its <= 30% acceptance check.
func EditTurnJudgeCounts(db *sql.DB) (total, judged int, err error) {
	row := db.QueryRow(`
SELECT COUNT(*), COALESCE(SUM(CASE WHEN v.stage = 'judge' THEN 1 ELSE 0 END), 0)
FROM verifications v
JOIN replays r ON r.id = v.replay_id
JOIN calls c ON c.id = r.call_id
JOIN features f ON f.call_id = c.id
WHERE f.turn_type IN ('single_file_edit', 'multi_file_edit')`)
	if scanErr := row.Scan(&total, &judged); scanErr != nil {
		return 0, 0, fmt.Errorf("querying edit turn judge counts: %w", scanErr)
	}
	return total, judged, nil
}

// JudgeSpendTotals returns the total input and output tokens recorded
// across every judge_batches row (a batch not yet completed contributes 0
// since its token columns are still NULL at that point).
func JudgeSpendTotals(db *sql.DB) (inputTokens, outputTokens int64, err error) {
	row := db.QueryRow(`SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0) FROM judge_batches`)
	if scanErr := row.Scan(&inputTokens, &outputTokens); scanErr != nil {
		return 0, 0, fmt.Errorf("querying judge spend totals: %w", scanErr)
	}
	return inputTokens, outputTokens, nil
}

// TotalReplayCount returns the total number of rows in replays, the
// denominator for the judge-spend-per-100-replays report line.
func TotalReplayCount(db *sql.DB) (int64, error) {
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM replays`).Scan(&n); err != nil {
		return 0, fmt.Errorf("querying total replay count: %w", err)
	}
	return n, nil
}
