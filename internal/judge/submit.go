package judge

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/freegle/splitter/internal/store"
)

// SubmitResult summarises one Submit call.
type SubmitResult struct {
	ItemCount       int
	BatchID         string
	JudgeBatchRowID int64
}

// Submit gathers every queued judge_items row, builds one Message Batches
// API request covering all of them, and records the result: a new
// judge_batches row, and every submitted judge_items row flipped to
// status=submitted and linked to it. When there are no queued items it
// makes no HTTP call and returns a zero SubmitResult.
func Submit(ctx context.Context, db *sql.DB, cfg Config) (SubmitResult, error) {
	queued, err := store.QueuedJudgeItems(db)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("loading queued judge items: %w", err)
	}
	if len(queued) == 0 {
		return SubmitResult{}, nil
	}

	items := make([]PromptItem, 0, len(queued))
	ids := make([]int64, 0, len(queued))
	for _, q := range queued {
		prompt, err := buildQueuedPrompt(q, cfg.MaxContextChars)
		if err != nil {
			return SubmitResult{}, fmt.Errorf("building judge prompt for judge item %d: %w", q.JudgeItemID, err)
		}
		items = append(items, PromptItem{CustomID: fmt.Sprintf("ji-%d", q.JudgeItemID), Prompt: prompt})
		ids = append(ids, q.JudgeItemID)
	}

	client := NewClient(cfg)
	batchID, err := client.SubmitBatch(ctx, items)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("submitting judge batch: %w", err)
	}

	rowID, err := store.InsertJudgeBatch(db, batchID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return SubmitResult{}, fmt.Errorf("recording judge batch %s: %w", batchID, err)
	}

	if err := store.MarkJudgeItemsSubmitted(db, ids, rowID); err != nil {
		return SubmitResult{}, fmt.Errorf("flipping judge items to submitted for batch %s: %w", batchID, err)
	}

	return SubmitResult{ItemCount: len(items), BatchID: batchID, JudgeBatchRowID: rowID}, nil
}

// buildQueuedPrompt decompresses q's frontier request/response and local
// response and renders the judge prompt for it.
func buildQueuedPrompt(q store.QueuedJudgeItem, maxContextChars int) (string, error) {
	requestJSON, err := store.Decompress(q.RequestZstd)
	if err != nil {
		return "", fmt.Errorf("decompressing frontier request: %w", err)
	}
	frontierRespJSON, err := store.Decompress(q.FrontierResponseZstd)
	if err != nil {
		return "", fmt.Errorf("decompressing frontier response: %w", err)
	}
	localRespJSON, err := store.Decompress(q.LocalResponseZstd)
	if err != nil {
		return "", fmt.Errorf("decompressing local response: %w", err)
	}

	frontierText := ExtractResponseText(frontierRespJSON)
	localText := ExtractResponseText(localRespJSON)
	return BuildPrompt(string(requestJSON), frontierText, localText, maxContextChars), nil
}
