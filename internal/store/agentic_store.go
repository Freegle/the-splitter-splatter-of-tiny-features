package store

import (
	"database/sql"
	"fmt"
)

// UpdateEvalTaskAgenticReady records the outcome of a sandbox dependency
// prep attempt for an agentic eval task (internal/agentic): ready=true when
// `go mod download` / `npm ci` (whichever lockfiles are present) succeeded,
// false when prep failed. Re-running eval-agentic overwrites the previous
// outcome, so this always reflects the most recent attempt.
func UpdateEvalTaskAgenticReady(db *sql.DB, id int64, ready bool) error {
	val := 0
	if ready {
		val = 1
	}
	if _, err := db.Exec(`UPDATE eval_tasks SET agentic_ready = ? WHERE id = ?`, val, id); err != nil {
		return fmt.Errorf("updating eval task %d agentic_ready: %w", id, err)
	}
	return nil
}

// AgenticGradableEvalTasks returns every active eval_tasks row that carries
// a held-out test payload (origin='history' tasks split by
// internal/evals.SplitTestFiles), oldest first: one half of `eval-agentic`'s
// candidate set. The other half, harvested live tasks whose subsystem has a
// configured [tests] command, is not a store-level concept (subsystem to
// command mapping lives in config), so the caller filters ActiveEvalTasks
// for those itself.
func AgenticGradableEvalTasks(db *sql.DB) ([]EvalTaskRow, error) {
	rows, err := db.Query(`SELECT ` + evalTaskColumns + ` FROM eval_tasks WHERE active = 1 AND holdout_tests_zstd IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("querying agentic-gradable eval tasks: %w", err)
	}
	defer rows.Close()
	return scanEvalTaskRows(rows)
}
