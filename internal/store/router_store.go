package store

import (
	"database/sql"
	"fmt"
)

// VerificationForRouter is one decided verification's category inputs
// (turn_type, subsystem), the exact frontier and local model ids that
// produced it, and whether it agreed: the raw input to `splitter router
// update`'s family-scoped, per-exact-version aggregation.
type VerificationForRouter struct {
	TurnType      string
	Subsystem     string
	FrontierModel string
	LocalModel    string
	Agree         bool
}

// DecidedVerificationsForRouter returns every verification with a decided
// outcome (agree IS NOT NULL), joined to its call's turn_type, subsystem
// and frontier model, and its replay's exact local model.
func DecidedVerificationsForRouter(db *sql.DB) ([]VerificationForRouter, error) {
	rows, err := db.Query(`
SELECT f.turn_type, COALESCE(f.subsystem, ''), COALESCE(c.model, ''), r.model, v.agree
FROM verifications v
JOIN replays r ON r.id = v.replay_id
JOIN calls c ON c.id = r.call_id
JOIN features f ON f.call_id = c.id
WHERE v.agree IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("querying decided verifications for router: %w", err)
	}
	defer rows.Close()

	var out []VerificationForRouter
	for rows.Next() {
		var v VerificationForRouter
		var agree int
		if err := rows.Scan(&v.TurnType, &v.Subsystem, &v.FrontierModel, &v.LocalModel, &agree); err != nil {
			return nil, fmt.Errorf("scanning decided verification for router: %w", err)
		}
		v.Agree = agree != 0
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating decided verifications for router: %w", err)
	}
	return out, nil
}

// RouterStateRow mirrors one row of the router_state table.
type RouterStateRow struct {
	Category       string
	Families       string
	N              int
	Agreed         int
	WilsonLB       float64
	Routable       bool
	DisabledReason string
	UpdatedTS      string
}

// UpsertRouterState inserts row, or updates the existing row for
// (row.Category, row.Families) when one already exists: `splitter router
// update`'s idempotency mechanism, matching UNIQUE(category, families).
func UpsertRouterState(db *sql.DB, row RouterStateRow) error {
	_, err := db.Exec(`
INSERT INTO router_state (category, families, n, agreed, wilson_lb, routable, disabled_reason, updated_ts)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(category, families) DO UPDATE SET
  n = excluded.n,
  agreed = excluded.agreed,
  wilson_lb = excluded.wilson_lb,
  routable = excluded.routable,
  disabled_reason = excluded.disabled_reason,
  updated_ts = excluded.updated_ts`,
		row.Category, row.Families, row.N, row.Agreed, row.WilsonLB,
		boolToInt(row.Routable), nullIfEmpty(row.DisabledReason), row.UpdatedTS,
	)
	if err != nil {
		return fmt.Errorf("upserting router state for %s/%s: %w", row.Category, row.Families, err)
	}
	return nil
}

// DisableRouterState marks the router_state row for (category, families)
// disabled: disabled_reason set and routable forced to 0. A no-op (0 rows
// affected, no error) when no such row exists yet.
func DisableRouterState(db *sql.DB, category, families, reason, updatedTS string) error {
	_, err := db.Exec(`
UPDATE router_state SET disabled_reason = ?, routable = 0, updated_ts = ?
WHERE category = ? AND families = ?`, reason, updatedTS, category, families)
	if err != nil {
		return fmt.Errorf("disabling router state for %s/%s: %w", category, families, err)
	}
	return nil
}

// AllRouterState returns every router_state row, the input to the live
// router's in-memory snapshot refresh.
func AllRouterState(db *sql.DB) ([]RouterStateRow, error) {
	rows, err := db.Query(`
SELECT category, families, n, agreed, wilson_lb, routable, COALESCE(disabled_reason, '')
FROM router_state`)
	if err != nil {
		return nil, fmt.Errorf("querying all router state: %w", err)
	}
	defer rows.Close()
	return scanRouterStateRows(rows)
}

// RouterStateDivergences returns every router_state row whose
// disabled_reason marks it as recomputed from a diverged exact model
// version (see internal/router.Update), the input to the weekly report's
// per-version divergence flags.
func RouterStateDivergences(db *sql.DB) ([]RouterStateRow, error) {
	rows, err := db.Query(`
SELECT category, families, n, agreed, wilson_lb, routable, COALESCE(disabled_reason, '')
FROM router_state
WHERE disabled_reason LIKE 'divergent_version:%'`)
	if err != nil {
		return nil, fmt.Errorf("querying router state divergences: %w", err)
	}
	defer rows.Close()
	return scanRouterStateRows(rows)
}

func scanRouterStateRows(rows *sql.Rows) ([]RouterStateRow, error) {
	var out []RouterStateRow
	for rows.Next() {
		var r RouterStateRow
		var routable int
		if err := rows.Scan(&r.Category, &r.Families, &r.N, &r.Agreed, &r.WilsonLB, &routable, &r.DisabledReason); err != nil {
			return nil, fmt.Errorf("scanning router state row: %w", err)
		}
		r.Routable = routable != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating router state rows: %w", err)
	}
	return out, nil
}

// RouterDecisionRow mirrors one row of the router_decisions table.
type RouterDecisionRow struct {
	ID        int64
	TS        string
	SessionID sql.NullString
	CallID    sql.NullInt64
	Category  sql.NullString
	Decision  string
	Stats     sql.NullString
}

// InsertRouterDecision inserts row into router_decisions and returns the
// new row's id.
func InsertRouterDecision(db *sql.DB, row RouterDecisionRow) (int64, error) {
	res, err := db.Exec(`
INSERT INTO router_decisions (ts, session_id, call_id, category, decision, stats)
VALUES (?, ?, ?, ?, ?, ?)`,
		row.TS, row.SessionID, row.CallID, row.Category, row.Decision, row.Stats,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting router decision: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted router decision id: %w", err)
	}
	return id, nil
}

// UpdateRouterDecisionStats overwrites decisionID's stats column, used once
// an async shadow comparison completes and the outcome needs recording
// against the decision row logged when the shadow dispatch was chosen.
func UpdateRouterDecisionStats(db *sql.DB, decisionID int64, statsJSON string) error {
	if _, err := db.Exec(`UPDATE router_decisions SET stats = ? WHERE id = ?`, statsJSON, decisionID); err != nil {
		return fmt.Errorf("updating router decision %d stats: %w", decisionID, err)
	}
	return nil
}

// RouterDecisionsSince returns every router_decisions row with ts >= since
// (RFC3339 UTC), the input to `splitter report weekly`.
func RouterDecisionsSince(db *sql.DB, since string) ([]RouterDecisionRow, error) {
	rows, err := db.Query(`
SELECT id, ts, session_id, call_id, category, decision, stats
FROM router_decisions WHERE ts >= ? ORDER BY id`, since)
	if err != nil {
		return nil, fmt.Errorf("querying router decisions since %s: %w", since, err)
	}
	defer rows.Close()

	var out []RouterDecisionRow
	for rows.Next() {
		var r RouterDecisionRow
		if err := rows.Scan(&r.ID, &r.TS, &r.SessionID, &r.CallID, &r.Category, &r.Decision, &r.Stats); err != nil {
			return nil, fmt.Errorf("scanning router decision row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating router decision rows: %w", err)
	}
	return out, nil
}

// TrustedEvalResultsForRouter returns every trusted scored eval result as
// router evidence, in the same shape as decided verifications: passed is
// the agreement signal, the task's frontier model (or "human" for
// history-seeded tasks, whose reference answer is the real committed fix)
// is the frontier side, and the run's model is the cheaper side. Trusted
// means scored without error and without any cheat flag; ladder-skipped
// rows (passed IS NULL) are excluded by the same predicate.
func TrustedEvalResultsForRouter(db *sql.DB) ([]VerificationForRouter, error) {
	rows, err := db.Query(`
SELECT COALESCE(NULLIF(t.turn_type, ''), 'other'),
       COALESCE(t.subsystem, ''),
       COALESCE(NULLIF(t.frontier_model, ''), 'human'),
       run.model,
       er.passed
FROM eval_results er
JOIN eval_runs run ON run.id = er.eval_run_id
JOIN eval_tasks t ON t.id = er.eval_task_id
WHERE er.passed IS NOT NULL
  AND (er.error IS NULL OR er.error = '')
  AND (er.cheat_flags IS NULL OR er.cheat_flags = '' OR er.cheat_flags = '[]')`)
	if err != nil {
		return nil, fmt.Errorf("querying trusted eval results for router: %w", err)
	}
	defer rows.Close()

	var out []VerificationForRouter
	for rows.Next() {
		var v VerificationForRouter
		var agree int
		if err := rows.Scan(&v.TurnType, &v.Subsystem, &v.FrontierModel, &v.LocalModel, &agree); err != nil {
			return nil, fmt.Errorf("scanning trusted eval result for router: %w", err)
		}
		v.Agree = agree != 0
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating trusted eval results for router: %w", err)
	}
	return out, nil
}
