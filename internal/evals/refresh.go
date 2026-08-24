package evals

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/store"
)

// RefreshRequestsSummary reports what one `eval refresh-requests` run did.
type RefreshRequestsSummary struct {
	Considered int
	Refreshed  int
	Skipped    int
}

// RefreshRequests rebuilds every history-origin task's synthesized request
// from the repo's parent-state content, the task's CURRENT brief and the
// CURRENT seed system prompt, leaving everything else (brief, reference,
// characteristics, results) untouched. Exists so a prompt-template change
// (like the stated test requirement the Sonnet 5 control run exposed as
// missing) reaches already-seeded tasks without reseeding, which would
// discard their reverse-engineered briefs.
func RefreshRequests(db *sql.DB, cfg *config.Config) (*RefreshRequestsSummary, error) {
	tasks, err := store.EvalTasksByOrigin(db, OriginHistory)
	if err != nil {
		return nil, fmt.Errorf("loading history tasks: %w", err)
	}

	summary := &RefreshRequestsSummary{}
	floor := cfg.Evals.MaxAnswerTokens
	for _, t := range tasks {
		summary.Considered++
		c := ParseCharacteristics(t.Characteristics.String)
		sha := c.CommitSHA
		if sha == "" {
			summary.Skipped++
			continue
		}
		meta, ok, err := loadCommitMeta(cfg.RepoPath, sha)
		if err != nil || !ok {
			summary.Skipped++
			continue
		}
		files, err := changedFiles(cfg.RepoPath, meta.Parent, sha)
		if err != nil || len(files) == 0 {
			summary.Skipped++
			continue
		}
		touched, err := buildSeedTouchedFiles(cfg.RepoPath, meta.Parent, sha, files)
		if err != nil {
			summary.Skipped++
			continue
		}
		req, err := buildSeedRequest(t.Brief, touched, floor)
		if err != nil {
			summary.Skipped++
			continue
		}
		reqJSON, err := json.Marshal(req)
		if err != nil {
			summary.Skipped++
			continue
		}
		compressed, err := store.Compress(reqJSON)
		if err != nil {
			return nil, fmt.Errorf("compressing refreshed request for task %d: %w", t.ID, err)
		}
		if err := store.UpdateEvalTaskRequest(db, t.ID, compressed); err != nil {
			return nil, fmt.Errorf("updating request for task %d: %w", t.ID, err)
		}

		// Holdouts are rebuilt with the same touched-file data so tasks
		// seeded before a lane's command derivation existed become
		// agentic-gradable without reseeding.
		var paths []string
		for _, tf := range touched {
			if !tf.ContextOnly {
				paths = append(paths, tf.Path)
			}
		}
		subsystem := feature.Subsystem(paths)
		if holdout, hasHoldout := buildHoldoutPayload(touched, cfg.Layers, subsystem); hasHoldout {
			holdoutJSON, herr := json.Marshal(holdout)
			if herr != nil {
				return nil, fmt.Errorf("marshaling holdout for task %d: %w", t.ID, herr)
			}
			holdoutZstd, herr := store.Compress(holdoutJSON)
			if herr != nil {
				return nil, fmt.Errorf("compressing holdout for task %d: %w", t.ID, herr)
			}
			if err := store.UpdateEvalTaskHoldout(db, t.ID, holdoutZstd); err != nil {
				return nil, err
			}
		}
		summary.Refreshed++
	}
	return summary, nil
}
