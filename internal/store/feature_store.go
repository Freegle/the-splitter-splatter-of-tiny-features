package store

import (
	"database/sql"
	"fmt"
)

// FeatureRow mirrors one row of the features table.
type FeatureRow struct {
	ID               int64
	CallID           int64
	TurnType         string
	FilesTouched     string
	Subsystem        sql.NullString
	ContextTokens    sql.NullInt64
	OutputTokens     sql.NullInt64
	HadErrorFollowup sql.NullInt64
}

// UpsertFeature inserts row's features, or updates the existing row for
// row.CallID when one already exists. This is the featuriser's idempotency
// mechanism: running it twice over the same call produces one row, not two.
func UpsertFeature(db *sql.DB, row FeatureRow) error {
	_, err := db.Exec(`
INSERT INTO features (
  call_id, turn_type, files_touched, subsystem, context_tokens, output_tokens, had_error_followup
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(call_id) DO UPDATE SET
  turn_type = excluded.turn_type,
  files_touched = excluded.files_touched,
  subsystem = excluded.subsystem,
  context_tokens = excluded.context_tokens,
  output_tokens = excluded.output_tokens,
  had_error_followup = excluded.had_error_followup`,
		row.CallID, row.TurnType, row.FilesTouched, row.Subsystem,
		row.ContextTokens, row.OutputTokens, row.HadErrorFollowup,
	)
	if err != nil {
		return fmt.Errorf("upserting features for call %d: %w", row.CallID, err)
	}
	return nil
}

// GetFeature fetches the features row for callID. It returns sql.ErrNoRows
// (wrapped) when no such row exists.
func GetFeature(db *sql.DB, callID int64) (*FeatureRow, error) {
	row := db.QueryRow(`
SELECT id, call_id, turn_type, files_touched, subsystem, context_tokens, output_tokens, had_error_followup
FROM features WHERE call_id = ?`, callID)

	var f FeatureRow
	if err := row.Scan(
		&f.ID, &f.CallID, &f.TurnType, &f.FilesTouched, &f.Subsystem,
		&f.ContextTokens, &f.OutputTokens, &f.HadErrorFollowup,
	); err != nil {
		return nil, fmt.Errorf("getting features for call %d: %w", callID, err)
	}
	return &f, nil
}

// CallIDsMissingFeatures returns, oldest first, the ids of calls that have a
// captured response but no matching row in features yet.
func CallIDsMissingFeatures(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`
SELECT c.id FROM calls c
LEFT JOIN features f ON f.call_id = c.id
WHERE f.id IS NULL AND c.response_zstd IS NOT NULL
ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("querying calls missing features: %w", err)
	}
	defer rows.Close()
	return scanInt64Column(rows)
}

// CallIDsWithNullFollowup returns, oldest first, the call ids of existing
// features rows whose had_error_followup has not been determined yet: the
// next call in the session did not exist the last time featurise ran.
func CallIDsWithNullFollowup(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`SELECT call_id FROM features WHERE had_error_followup IS NULL ORDER BY call_id`)
	if err != nil {
		return nil, fmt.Errorf("querying calls with unresolved error followup: %w", err)
	}
	defer rows.Close()
	return scanInt64Column(rows)
}

// AllCallIDsWithResponse returns every call id that has a captured
// response, oldest first. Used by featurise --refresh to reprocess every
// call regardless of its current features row.
func AllCallIDsWithResponse(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`SELECT id FROM calls WHERE response_zstd IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("querying all calls with a response: %w", err)
	}
	defer rows.Close()
	return scanInt64Column(rows)
}

// NextCallInSession returns the earliest call after afterCallID in the same
// session, the "next call" the had_error_followup heuristic inspects. It
// returns sql.ErrNoRows (wrapped) when no later call exists in the session
// yet.
func NextCallInSession(db *sql.DB, sessionID string, afterCallID int64) (*CallRow, error) {
	row := db.QueryRow(`
SELECT id, ts, session_id, model, stream, request_zstd, response_zstd,
       input_tokens, output_tokens, latency_ms, repo_head, status, error, source
FROM calls WHERE session_id = ? AND id > ? ORDER BY id ASC LIMIT 1`, sessionID, afterCallID)

	var c CallRow
	var stream int
	if err := row.Scan(
		&c.ID, &c.TS, &c.SessionID, &c.Model, &stream, &c.RequestZstd, &c.ResponseZstd,
		&c.InputTokens, &c.OutputTokens, &c.LatencyMs, &c.RepoHead, &c.Status, &c.Error, &c.Source,
	); err != nil {
		return nil, fmt.Errorf("getting next call after %d in session %s: %w", afterCallID, sessionID, err)
	}
	c.Stream = stream != 0
	return &c, nil
}

// SpendRow is one featurised call's turn_type, frontier model and token
// counts, the input to splitter report spend's aggregation.
type SpendRow struct {
	TurnType      string
	Model         string
	ContextTokens int64
	OutputTokens  int64
}

// SpendByTurnType returns one SpendRow per featurised call, joining
// features to its calls row for the model that priced it.
func SpendByTurnType(db *sql.DB) ([]SpendRow, error) {
	rows, err := db.Query(`
SELECT f.turn_type, COALESCE(c.model, ''), COALESCE(f.context_tokens, 0), COALESCE(f.output_tokens, 0)
FROM features f
JOIN calls c ON c.id = f.call_id
ORDER BY f.turn_type`)
	if err != nil {
		return nil, fmt.Errorf("querying spend by turn_type: %w", err)
	}
	defer rows.Close()

	var out []SpendRow
	for rows.Next() {
		var r SpendRow
		if err := rows.Scan(&r.TurnType, &r.Model, &r.ContextTokens, &r.OutputTokens); err != nil {
			return nil, fmt.Errorf("scanning spend row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating spend rows: %w", err)
	}
	return out, nil
}

// scanInt64Column drains rows into a slice of the first int64 column, then
// closes it via the caller's own defer (rows is left positioned at EOF).
func scanInt64Column(rows *sql.Rows) ([]int64, error) {
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning id column: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating id rows: %w", err)
	}
	return out, nil
}
