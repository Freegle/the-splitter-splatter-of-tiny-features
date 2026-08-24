package evals

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/backend"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/judge"
	"github.com/freegle/splitter/internal/store"
)

// judgeFailsInstruction grades a candidate change against the shipped one.
// Omitted tests count as failure (house rule: code changes ship with
// tests); different wording of the same substance does not.
const judgeFailsInstruction = `You are grading a candidate code change against the change that actually shipped, given the request both were answering. They are EQUIVALENT when the candidate accomplishes the same behavioural change to the production code AND provides comparable test coverage wherever the shipped change added or updated tests: a candidate that skipped the tests the shipped change made is NOT equivalent (this project requires tests with code changes). Differences of wording, naming, comment text, formatting or ordering that do not change behaviour or coverage do NOT make them inequivalent; for documentation changes, equivalent substance in different words is equivalent. Answer ONLY JSON {"equivalent": bool, "confidence": 0-1, "reason": "one line"}.`

// JudgeFailsSummary reports what one `eval judge-fails` run did.
type JudgeFailsSummary struct {
	Considered    int
	Judged        int
	FlippedToPass int
	FlippedToFail int
	Errored       int
}

// JudgeFails grades every scored, unjudged, non-exact eval result by
// asking a judge model whether the stored candidate response is
// equivalent to the task's reference response. The judge verdict is the
// DECIDING grade (Edward, after the Sonnet 5 control run: mechanical
// similarity is "likely useless", decide on the judge); similarity stays
// recorded as a diagnostic. Omitted tests count as failure per the house
// rule baked into the instruction. Every verdict is stored in
// judge_verdict and the stage becomes "judge". Calls are synchronous
// single messages via the anthropic-native client, so a subscription
// token works.
func JudgeFails(ctx context.Context, db *sql.DB, cfg *config.Config, model string) (*JudgeFailsSummary, error) {
	rows, err := store.FailedUnjudgedEvalResults(db, 0)
	if err != nil {
		return nil, fmt.Errorf("loading failed results: %w", err)
	}

	client := &backend.AnthropicClient{
		BaseURL:   cfg.Upstream,
		APIKeyEnv: cfg.Judge.APIKeyEnv,
		Model:     model,
	}

	summary := &JudgeFailsSummary{}
	for _, r := range rows {
		summary.Considered++
		task, err := store.GetEvalTask(db, r.EvalTaskID)
		if err != nil {
			return nil, fmt.Errorf("loading task %d: %w", r.EvalTaskID, err)
		}
		refJSON, err := store.Decompress(task.ReferenceResponseZstd)
		if err != nil || len(refJSON) == 0 {
			summary.Errored++
			continue
		}
		candJSON, err := store.Decompress(r.ResponseZstd)
		if err != nil || len(candJSON) == 0 {
			summary.Errored++
			continue
		}

		prompt := fmt.Sprintf("The request both changes answered:\n%s\n\nThe change that shipped:\n%s\n\nThe candidate change:\n%s\n\n%s",
			task.Brief, truncate(string(refJSON), 12000), truncate(string(candJSON), 12000), judgeFailsInstruction)

		req := anthropic.MessagesRequest{
			Model:     model,
			MaxTokens: 512,
			Messages: []anthropic.Message{
				{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: prompt}}},
			},
		}
		respJSON, err := client.Complete(ctx, req)
		if err != nil {
			summary.Errored++
			continue
		}
		text := extractResponseText(respJSON)
		verdict, err := judge.ParseVerdict(text)
		if err != nil {
			summary.Errored++
			continue
		}
		verdictJSON, err := json.Marshal(verdict)
		if err != nil {
			summary.Errored++
			continue
		}
		passed := 0
		if verdict.Agree() {
			passed = 1
		}
		if passed == 1 && r.Passed == 0 {
			summary.FlippedToPass++
		}
		if passed == 0 && r.Passed == 1 {
			summary.FlippedToFail++
		}
		if err := store.ApplyEvalJudgeVerdict(db, r.ID, passed, string(verdictJSON)); err != nil {
			return nil, fmt.Errorf("applying verdict to result %d: %w", r.ID, err)
		}
		summary.Judged++
	}
	return summary, nil
}

// extractResponseText concatenates the text blocks of an Anthropic message
// JSON body.
func extractResponseText(msg []byte) string {
	var m struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return ""
	}
	out := ""
	for _, b := range m.Content {
		if b.Type == anthropic.BlockText {
			out += b.Text
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[truncated]"
}
