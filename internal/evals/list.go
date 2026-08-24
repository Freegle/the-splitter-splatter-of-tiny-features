package evals

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/freegle/splitter/internal/store"
)

// shortSHALen bounds the displayed commit sha length.
const shortSHALen = 8

// PassRate is one (task, model) pair's pass rate across every eval run
// that has scored it so far.
type PassRate struct {
	Passed int
	Total  int
}

// ListRow is one eval_tasks row summarised for `eval list`.
type ListRow struct {
	ID              int64
	Origin          string
	ShortSHA        string
	Brief           string
	Characteristics string
	PassRates       map[string]PassRate // model -> pass rate
}

// List returns every eval task (active or not) with a short commit
// reference, a one-line characteristics summary, and its per-model pass
// rate across every eval run that has scored it so far, per DESIGN.md
// "eval list": "id, origin, repo_head short sha, brief, pass rate per
// model so far."
func List(db *sql.DB) ([]ListRow, error) {
	tasks, err := store.AllEvalTasks(db)
	if err != nil {
		return nil, fmt.Errorf("loading eval tasks: %w", err)
	}
	results, err := store.AllEvalResultsWithTask(db)
	if err != nil {
		return nil, fmt.Errorf("loading eval results: %w", err)
	}

	byTask := make(map[int64]map[string]PassRate, len(tasks))
	for _, r := range results {
		if !r.Passed.Valid {
			continue // ladder_skipped or not-yet-decided: does not count toward pass rate
		}
		m := byTask[r.EvalTaskID]
		if m == nil {
			m = map[string]PassRate{}
			byTask[r.EvalTaskID] = m
		}
		pr := m[r.Model]
		pr.Total++
		if r.Passed.Int64 == 1 {
			pr.Passed++
		}
		m[r.Model] = pr
	}

	out := make([]ListRow, 0, len(tasks))
	for _, t := range tasks {
		c := ParseCharacteristics(t.Characteristics.String)
		out = append(out, ListRow{
			ID:              t.ID,
			Origin:          t.Origin,
			ShortSHA:        shortSHA(t, c),
			Brief:           t.Brief,
			Characteristics: summarizeCharacteristics(t),
			PassRates:       byTask[t.ID],
		})
	}
	return out, nil
}

// shortSHA prefers the task's own commit sha (characteristics.commit_sha,
// set by seed-history) for display, since that is the actual commit a
// history-origin task's brief and diff describe; repo_head for a
// history-origin task is its PARENT commit (the worktree checkout target),
// which would be a confusing "git commit number" to show next to the
// brief. Every other origin has no commit_sha and falls back to repo_head
// (the call-time HEAD, the relevant commit there).
func shortSHA(t store.EvalTaskRow, c Characteristics) string {
	sha := c.CommitSHA
	if sha == "" {
		sha = t.RepoHead.String
	}
	if len(sha) > shortSHALen {
		return sha[:shortSHALen]
	}
	return sha
}

// summarizeCharacteristics renders a compact "language/layer/nature/
// difficulty" summary from an eval_tasks row's own columns, "-" for a
// dimension that was never derived, per this task's "characteristics
// summary" deliverable (DESIGN.md's own eval list bullet does not name
// this column, but the profile is otherwise invisible in a one-line list).
func summarizeCharacteristics(t store.EvalTaskRow) string {
	dim := func(s sql.NullString) string {
		if s.Valid && s.String != "" {
			return s.String
		}
		return "-"
	}
	return strings.Join([]string{dim(t.Language), dim(t.Layer), dim(t.Nature), dim(t.Difficulty)}, "/")
}
