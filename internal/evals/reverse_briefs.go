package evals

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/freegle/splitter/internal/judge"
	"github.com/freegle/splitter/internal/store"
)

// reverseBriefCustomIDPrefix names reverse-briefs' batch items, distinct
// from judge's own "ji-<id>" items sharing the same Batches API.
const reverseBriefCustomIDPrefix = "rb-"

// reverseBriefInstruction is appended to every reverse-briefs prompt.
const reverseBriefInstruction = `Write a SHORT problem statement or feature request, in the voice of the person who would have asked for this change BEFORE it existed. Describe only the observed problem or desired behaviour. Do NOT name any function, variable, file, or the specific approach the actual fix used. Do NOT explain WHY the problem happens or what the root cause is: the requester only sees symptoms. Do NOT describe the corrected behaviour, the new value, or what the code does "now": at the time of asking, the change does not exist. Do NOT quote or paraphrase the commit message's own wording. Answer with ONLY the rewritten request text: no preamble, no quotes, no markdown.`

// ReverseBriefsSubmitSummary reports what one `eval reverse-briefs submit`
// call did.
type ReverseBriefsSubmitSummary struct {
	Eligible  int
	ItemCount int
	BatchID   string
}

// ReverseBriefsSubmit finds every history-origin task whose brief is still
// the mechanical commit-subject one and has never been submitted for
// reversal, and submits them as one Message Batches API request via
// internal/judge's exported batch client (DESIGN.md: "reusing the exported
// batch client from internal/judge, judge model, cheap"). Each task's
// characteristics.reverse_brief is set to status=submitted with the batch
// and item ids, so a later ReverseBriefsPoll call can find it; the task's
// brief itself is left unchanged until poll applies the rewritten text.
func ReverseBriefsSubmit(ctx context.Context, db *sql.DB, jcfg judge.Config) (ReverseBriefsSubmitSummary, error) {
	tasks, err := store.EvalTasksByOrigin(db, OriginHistory)
	if err != nil {
		return ReverseBriefsSubmitSummary{}, fmt.Errorf("loading history-origin tasks: %w", err)
	}

	type pending struct {
		id    int64
		brief string
		c     Characteristics
	}
	var eligible []pending
	for _, t := range tasks {
		c := ParseCharacteristics(t.Characteristics.String)
		if c.BriefSource != BriefSourceCommitSubject || c.ReverseBrief != nil {
			continue
		}
		eligible = append(eligible, pending{id: t.ID, brief: t.Brief, c: c})
	}

	summary := ReverseBriefsSubmitSummary{Eligible: len(eligible)}
	if len(eligible) == 0 {
		return summary, nil
	}

	items := make([]judge.PromptItem, 0, len(eligible))
	for _, p := range eligible {
		task, err := store.GetEvalTask(db, p.id)
		if err != nil {
			return summary, fmt.Errorf("reloading eval task %d for reverse-briefs prompt: %w", p.id, err)
		}
		diffText, err := reverseBriefDiffText(task)
		if err != nil {
			return summary, fmt.Errorf("building reverse-briefs diff text for task %d: %w", p.id, err)
		}
		customID := reverseBriefCustomIDPrefix + strconv.FormatInt(p.id, 10)
		items = append(items, judge.PromptItem{
			CustomID: customID,
			Prompt:   buildReverseBriefPrompt(p.brief, diffText),
		})
	}

	client := judge.NewClient(jcfg)
	batchID, err := client.SubmitBatch(ctx, items)
	if err != nil {
		return summary, fmt.Errorf("submitting reverse-briefs batch: %w", err)
	}
	summary.ItemCount = len(items)
	summary.BatchID = batchID

	for i, p := range eligible {
		c := p.c
		c.ReverseBrief = &ReverseBriefState{Status: ReverseBriefSubmitted, BatchID: batchID, CustomID: items[i].CustomID}
		if err := store.UpdateEvalTaskBrief(db, p.id, p.brief, c.JSON()); err != nil {
			return summary, fmt.Errorf("recording reverse-briefs submission for task %d: %w", p.id, err)
		}
	}

	return summary, nil
}

// ReverseBriefsPollSummary reports what one `eval reverse-briefs -poll`
// call did.
type ReverseBriefsPollSummary struct {
	BatchesChecked int
	BatchesEnded   int
	Rewritten      int
	Errored        int
}

// ReverseBriefsPoll checks every distinct batch id referenced by a
// submitted (not yet done) reverse_brief, one GET per batch (no internal
// loop, matching internal/judge.Poll's convention), and applies a
// succeeded batch's rewritten text as each task's new brief, flipping
// brief_source to reverse_engineered.
func ReverseBriefsPoll(ctx context.Context, db *sql.DB, jcfg judge.Config) (ReverseBriefsPollSummary, error) {
	tasks, err := store.EvalTasksByOrigin(db, OriginHistory)
	if err != nil {
		return ReverseBriefsPollSummary{}, fmt.Errorf("loading history-origin tasks: %w", err)
	}

	byBatch := map[string][]store.EvalTaskRow{}
	for _, t := range tasks {
		c := ParseCharacteristics(t.Characteristics.String)
		if c.ReverseBrief != nil && c.ReverseBrief.Status == ReverseBriefSubmitted {
			byBatch[c.ReverseBrief.BatchID] = append(byBatch[c.ReverseBrief.BatchID], t)
		}
	}

	var summary ReverseBriefsPollSummary
	client := judge.NewClient(jcfg)
	for batchID, batchTasks := range byBatch {
		summary.BatchesChecked++
		status, err := client.PollBatch(ctx, batchID)
		if err != nil {
			return summary, fmt.Errorf("polling reverse-briefs batch %s: %w", batchID, err)
		}
		if !status.Ended() {
			continue
		}
		summary.BatchesEnded++

		lines, err := client.FetchResults(ctx, status.ResultsURL)
		if err != nil {
			return summary, fmt.Errorf("fetching reverse-briefs results for batch %s: %w", batchID, err)
		}
		byCustomID := make(map[string]judge.ResultLine, len(lines))
		for _, l := range lines {
			byCustomID[l.CustomID] = l
		}

		for _, t := range batchTasks {
			c := ParseCharacteristics(t.Characteristics.String)
			line, ok := byCustomID[c.ReverseBrief.CustomID]
			newText := ""
			if ok && line.Succeeded {
				newText = strings.TrimSpace(strings.Trim(line.Text, "\"'"))
			}
			if newText == "" {
				c.ReverseBrief.Status = ReverseBriefErrored
				if err := store.UpdateEvalTaskBrief(db, t.ID, t.Brief, c.JSON()); err != nil {
					return summary, fmt.Errorf("recording errored reverse-brief for task %d: %w", t.ID, err)
				}
				summary.Errored++
				continue
			}

			c.BriefSource = BriefSourceReverseEngineered
			c.ReverseBrief.Status = ReverseBriefDone
			if err := store.UpdateEvalTaskBrief(db, t.ID, newText, c.JSON()); err != nil {
				return summary, fmt.Errorf("applying rewritten brief for task %d: %w", t.ID, err)
			}
			summary.Rewritten++
		}
	}

	return summary, nil
}

// buildReverseBriefPrompt assembles one reverse-briefs prompt: the commit
// message (the mechanical brief) and the reconstructed diff, then the
// rewrite instruction.
func buildReverseBriefPrompt(commitMessage, diffText string) string {
	var b strings.Builder
	b.WriteString("Commit message:\n")
	b.WriteString(commitMessage)
	b.WriteString("\n\nDiff (for context only, do not describe it literally):\n")
	b.WriteString(diffText)
	b.WriteString("\n\n")
	b.WriteString(reverseBriefInstruction)
	return b.String()
}

// reverseBriefDiffText renders task's reference response (the
// reconstructed Edit/MultiEdit/Write blocks) as plain text for the
// reverse-briefs prompt, reusing internal/judge's own response-to-text
// renderer (the same "tool_use name(input json)" rendering the judge
// prompt uses) rather than duplicating it.
func reverseBriefDiffText(task *store.EvalTaskRow) (string, error) {
	if len(task.ReferenceResponseZstd) == 0 {
		return "", nil
	}
	refJSON, err := store.Decompress(task.ReferenceResponseZstd)
	if err != nil {
		return "", fmt.Errorf("decompressing reference response: %w", err)
	}
	return judge.ExtractResponseText(refJSON), nil
}
