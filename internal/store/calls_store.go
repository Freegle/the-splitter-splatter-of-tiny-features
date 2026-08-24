package store

import (
	"database/sql"
	"fmt"
)

// CallRow mirrors one row of the calls table.
type CallRow struct {
	ID           int64
	TS           string
	SessionID    sql.NullString
	Model        sql.NullString
	Stream       bool
	RequestZstd  []byte
	ResponseZstd []byte
	InputTokens  sql.NullInt64
	OutputTokens sql.NullInt64
	LatencyMs    sql.NullInt64
	RepoHead     sql.NullString
	Status       sql.NullInt64
	Error        sql.NullString
	Source       string
}

// InsertCall inserts row into calls and returns the new row's id. Source
// defaults to "proxy" when empty.
func InsertCall(db *sql.DB, row CallRow) (int64, error) {
	source := row.Source
	if source == "" {
		source = "proxy"
	}

	res, err := db.Exec(`
INSERT INTO calls (
  ts, session_id, model, stream, request_zstd, response_zstd,
  input_tokens, output_tokens, latency_ms, repo_head, status, error, source
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.TS, row.SessionID, row.Model, boolToInt(row.Stream), row.RequestZstd, row.ResponseZstd,
		row.InputTokens, row.OutputTokens, row.LatencyMs, row.RepoHead, row.Status, row.Error, source,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting call: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted call id: %w", err)
	}
	return id, nil
}

// GetCall fetches the call row with the given id. It returns
// sql.ErrNoRows (wrapped) when no such row exists.
func GetCall(db *sql.DB, id int64) (*CallRow, error) {
	row := db.QueryRow(`
SELECT id, ts, session_id, model, stream, request_zstd, response_zstd,
       input_tokens, output_tokens, latency_ms, repo_head, status, error, source
FROM calls WHERE id = ?`, id)

	var c CallRow
	var stream int
	if err := row.Scan(
		&c.ID, &c.TS, &c.SessionID, &c.Model, &stream, &c.RequestZstd, &c.ResponseZstd,
		&c.InputTokens, &c.OutputTokens, &c.LatencyMs, &c.RepoHead, &c.Status, &c.Error, &c.Source,
	); err != nil {
		return nil, fmt.Errorf("getting call %d: %w", id, err)
	}
	c.Stream = stream != 0
	return &c, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
