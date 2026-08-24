package judge

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/freegle/splitter/internal/store"
)

// PollResult summarises one Poll call.
type PollResult struct {
	BatchesChecked int
	BatchesEnded   int
	ItemsSucceeded int
	ItemsErrored   int
	InputTokens    int64
	OutputTokens   int64
}

// Poll checks every judge_batches row not yet completed, one GET per
// batch: no internal loop or wait, a batch still processing is left alone
// and picked up again on the next invocation, which cron drives. A batch
// that has ended has its JSONL results streamed and keyed by custom_id
// (never by arrival order); each succeeded item's verdict is applied to
// its verification per the tests-win conflict rule, and the batch's usage
// tokens are accumulated into judge_batches.
func Poll(ctx context.Context, db *sql.DB, cfg Config) (PollResult, error) {
	pending, err := store.PendingJudgeBatches(db)
	if err != nil {
		return PollResult{}, fmt.Errorf("loading pending judge batches: %w", err)
	}

	client := NewClient(cfg)
	var result PollResult
	for _, batch := range pending {
		result.BatchesChecked++

		status, err := client.PollBatch(ctx, batch.BatchID)
		if err != nil {
			return result, fmt.Errorf("polling judge batch %s: %w", batch.BatchID, err)
		}
		if !status.Ended() {
			continue
		}
		result.BatchesEnded++

		if err := processEndedBatch(ctx, db, client, batch, status, &result); err != nil {
			return result, err
		}
	}

	return result, nil
}

// processEndedBatch fetches and applies one ended batch's results, and
// marks it completed with its accumulated usage tokens.
func processEndedBatch(ctx context.Context, db *sql.DB, client *Client, batch store.PendingJudgeBatch, status BatchStatus, result *PollResult) error {
	lines, err := client.FetchResults(ctx, status.ResultsURL)
	if err != nil {
		return fmt.Errorf("fetching results for judge batch %s: %w", batch.BatchID, err)
	}

	items, err := store.JudgeItemsForBatch(db, batch.ID)
	if err != nil {
		return fmt.Errorf("loading judge items for batch %s: %w", batch.BatchID, err)
	}

	var inputTokens, outputTokens int64
	for _, line := range lines {
		ref, ok := items[line.CustomID]
		if !ok {
			continue
		}
		inputTokens += int64(line.InputTokens)
		outputTokens += int64(line.OutputTokens)

		if err := applyResultLine(db, ref, line, result); err != nil {
			return err
		}
	}

	if err := store.CompleteJudgeBatch(db, batch.ID, inputTokens, outputTokens, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("completing judge batch %s: %w", batch.BatchID, err)
	}
	result.InputTokens += inputTokens
	result.OutputTokens += outputTokens
	return nil
}

// applyResultLine updates one judge_items row (and, on success, its
// verification) for one JSONL result line.
func applyResultLine(db *sql.DB, ref store.JudgeItemRef, line ResultLine, result *PollResult) error {
	if !line.Succeeded {
		result.ItemsErrored++
		if err := store.UpdateJudgeItemResult(db, ref.ID, "errored", line.ErrorMessage); err != nil {
			return fmt.Errorf("recording errored judge item %d: %w", ref.ID, err)
		}
		return nil
	}

	verdict, verr := ParseVerdict(line.Text)
	if verr != nil {
		result.ItemsErrored++
		if err := store.UpdateJudgeItemResult(db, ref.ID, "errored", line.Text); err != nil {
			return fmt.Errorf("recording unparseable judge item %d: %w", ref.ID, err)
		}
		return nil
	}

	verdictJSON, err := json.Marshal(verdict)
	if err != nil {
		return fmt.Errorf("encoding parsed verdict for judge item %d: %w", ref.ID, err)
	}
	if err := store.UpdateJudgeItemResult(db, ref.ID, "done", string(verdictJSON)); err != nil {
		return fmt.Errorf("recording done judge item %d: %w", ref.ID, err)
	}
	if err := applyVerdict(db, ref.VerificationID, verdict, string(verdictJSON)); err != nil {
		return fmt.Errorf("applying verdict to verification %d: %w", ref.VerificationID, err)
	}
	result.ItemsSucceeded++
	return nil
}

// lintOutcome is the {tool, ok, output} shape internal/verify records per
// side in verifications.frontier_lint/local_lint/frontier_tests/local_tests.
type lintOutcome struct {
	OK bool `json:"ok"`
}

// testsVerdict inspects a verification's four lint/test JSON columns and
// returns the tests-based agree signal. known is false when none of the
// four columns carry a value that parses (nothing to arbitrate with).
// When known, agree is true only when every recorded column parses with
// ok=true: a single recorded failure makes the tests side disagree, and
// both sides linting/testing clean makes it agree.
func testsVerdict(fields store.VerificationLint) (agree bool, known bool) {
	agree = true
	for _, raw := range []sql.NullString{fields.FrontierLint, fields.LocalLint, fields.FrontierTests, fields.LocalTests} {
		if !raw.Valid || raw.String == "" {
			continue
		}
		var o lintOutcome
		if err := json.Unmarshal([]byte(raw.String), &o); err != nil {
			continue
		}
		known = true
		if !o.OK {
			agree = false
		}
	}
	return agree, known
}

// applyVerdict resolves a verification's judge stage decision. When tests
// information exists it wins over the judge: agree follows the tests
// signal, and a mismatch between the two is counted as
// tests_judge_conflict. With no tests information (a non-edit turn, or an
// edit turn where no lint/test tool ran), the judge's own verdict decides
// and no conflict is recorded.
func applyVerdict(db *sql.DB, verificationID int64, verdict Verdict, verdictJSON string) error {
	fields, err := store.GetVerificationLint(db, verificationID)
	if err != nil {
		return fmt.Errorf("loading lint/test fields: %w", err)
	}

	judgeAgree := verdict.Agree()
	finalAgree := judgeAgree
	conflict := false

	if testsAgree, known := testsVerdict(fields); known {
		finalAgree = testsAgree
		conflict = testsAgree != judgeAgree
	}

	return store.UpdateVerificationJudgeDecision(
		db, verificationID, verdictJSON, finalAgree, conflict,
		time.Now().UTC().Format(time.RFC3339),
	)
}
