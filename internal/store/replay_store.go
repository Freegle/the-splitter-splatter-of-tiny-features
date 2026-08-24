package store

import (
	"database/sql"
	"fmt"
	"time"
)

// NewestProxyCallTS returns the timestamp of the most recently logged
// live-proxy call (source = 'proxy'; imported history never counts as
// traffic for the replay worker's idle gate). ok is false when there are
// no proxy calls yet, in which case the idle gate always passes.
func NewestProxyCallTS(db *sql.DB) (ts time.Time, ok bool, err error) {
	var s sql.NullString
	if err := db.QueryRow(`SELECT MAX(ts) FROM calls WHERE source = 'proxy'`).Scan(&s); err != nil {
		return time.Time{}, false, fmt.Errorf("reading newest proxy call timestamp: %w", err)
	}
	if !s.Valid || s.String == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339, s.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parsing newest call timestamp %q: %w", s.String, err)
	}
	return parsed, true, nil
}

// ReplayCandidate is one call selected for replay: enough of calls and
// features to translate the request and drive the verification cascade.
type ReplayCandidate struct {
	CallID               int64
	RequestZstd          []byte
	FrontierResponseZstd []byte
	FrontierModel        string
	RepoHead             string
	TurnType             string
}

// SelectReplayCandidates returns up to limit calls eligible for replay
// against (backend, model): calls with a features row whose turn_type is
// not "other", that do not already have a replays row for this exact
// (backend, model) pair, oldest call id first.
func SelectReplayCandidates(db *sql.DB, backend, model string, limit int) ([]ReplayCandidate, error) {
	rows, err := db.Query(`
SELECT c.id, c.request_zstd, c.response_zstd, c.model, c.repo_head, f.turn_type
FROM calls c
JOIN features f ON f.call_id = c.id
WHERE f.turn_type != 'other'
  AND NOT EXISTS (
    SELECT 1 FROM replays r WHERE r.call_id = c.id AND r.backend = ? AND r.model = ?
  )
ORDER BY c.id ASC
LIMIT ?`, backend, model, limit)
	if err != nil {
		return nil, fmt.Errorf("selecting replay candidates: %w", err)
	}
	defer rows.Close()

	var out []ReplayCandidate
	for rows.Next() {
		var c ReplayCandidate
		var frontierModel, repoHead sql.NullString
		if err := rows.Scan(&c.CallID, &c.RequestZstd, &c.FrontierResponseZstd, &frontierModel, &repoHead, &c.TurnType); err != nil {
			return nil, fmt.Errorf("scanning replay candidate: %w", err)
		}
		c.FrontierModel = frontierModel.String
		c.RepoHead = repoHead.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating replay candidates: %w", err)
	}
	return out, nil
}

// ReplayRow mirrors the columns of one replays row that internal/replay
// writes. Error set (non-empty) means the backend call or translation
// failed for this call; ResponseZstd is nil in that case.
type ReplayRow struct {
	CallID       int64
	Backend      string
	Model        string
	ResponseZstd []byte
	LatencyMs    int64
	Error        string
	CreatedTS    string
}

// InsertReplay inserts row into replays and returns the new row's id.
func InsertReplay(db *sql.DB, row ReplayRow) (int64, error) {
	var responseZstd any
	if len(row.ResponseZstd) > 0 {
		responseZstd = row.ResponseZstd
	}

	res, err := db.Exec(`
INSERT INTO replays (call_id, backend, model, response_zstd, latency_ms, error, created_ts)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		row.CallID, row.Backend, row.Model, responseZstd, row.LatencyMs, nullIfEmpty(row.Error), row.CreatedTS,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting replay for call %d: %w", row.CallID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted replay id: %w", err)
	}
	return id, nil
}

// VerificationRow mirrors the columns of one verifications row that
// internal/replay writes when the cascade completes. The judge/conflict
// columns are left at their SQL defaults here: only `splitter judge poll`
// (a separate command) fills them in, once arbitration for a middle-band
// row (Agree == nil) resolves.
type VerificationRow struct {
	ReplayID      int64
	Stage         string
	Similarity    float64
	FrontierLint  string
	LocalLint     string
	FrontierTests string
	LocalTests    string
	// Agree is nil for the middle band (queued for judge arbitration,
	// decided_ts left NULL), else the cascade's outright decision.
	Agree *bool
	// DecidedTS is required whenever Agree is non-nil, ignored otherwise.
	DecidedTS string
}

// InsertVerification inserts row into verifications and returns the new
// row's id.
func InsertVerification(db *sql.DB, row VerificationRow) (int64, error) {
	var agree any
	var decidedTS any
	if row.Agree != nil {
		agree = boolToInt(*row.Agree)
		decidedTS = row.DecidedTS
	}

	res, err := db.Exec(`
INSERT INTO verifications (
  replay_id, stage, similarity, frontier_lint, local_lint,
  frontier_tests, local_tests, agree, decided_ts
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ReplayID, row.Stage, row.Similarity,
		nullIfEmpty(row.FrontierLint), nullIfEmpty(row.LocalLint),
		nullIfEmpty(row.FrontierTests), nullIfEmpty(row.LocalTests),
		agree, decidedTS,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting verification for replay %d: %w", row.ReplayID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted verification id: %w", err)
	}
	return id, nil
}

// InsertJudgeItem queues verificationID for judge arbitration: it inserts
// a judge_items row with status 'queued' and sets its custom_id to
// "ji-<id>" from the row's own id, the format `splitter judge submit`
// sends to the Anthropic Batches API.
func InsertJudgeItem(db *sql.DB, verificationID int64, createdTS string) (int64, error) {
	res, err := db.Exec(`
INSERT INTO judge_items (verification_id, custom_id, status, created_ts)
VALUES (?, '', 'queued', ?)`, verificationID, createdTS)
	if err != nil {
		return 0, fmt.Errorf("inserting judge item for verification %d: %w", verificationID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted judge item id: %w", err)
	}
	if _, err := db.Exec(`UPDATE judge_items SET custom_id = ? WHERE id = ?`, fmt.Sprintf("ji-%d", id), id); err != nil {
		return 0, fmt.Errorf("setting judge item custom_id for verification %d: %w", verificationID, err)
	}
	return id, nil
}

// nullIfEmpty returns nil for an empty string, else s, so an optional TEXT
// column is stored as SQL NULL rather than an empty string.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
