package store

import (
	"database/sql"
	"fmt"
)

// EvalTaskRow mirrors one row of the eval_tasks table.
type EvalTaskRow struct {
	ID                    int64
	CreatedTS             string
	CallID                sql.NullInt64
	RepoHead              sql.NullString
	Brief                 string
	TurnType              sql.NullString
	Subsystem             sql.NullString
	FrontierModel         sql.NullString
	RequestZstd           []byte
	ReferenceResponseZstd []byte
	Origin                string
	Language              sql.NullString
	Layer                 sql.NullString
	Nature                sql.NullString
	Difficulty            sql.NullString
	Characteristics       sql.NullString
	Active                bool
	// HoldoutTestsZstd is agentic eval mode's held-out test-file payload
	// (see internal/evals.SplitTestFiles), nil for a task with none.
	HoldoutTestsZstd []byte
	// AgenticReady is agentic eval mode's last sandbox dependency prep
	// outcome: NULL (Valid false) when never attempted.
	AgenticReady sql.NullInt64
}

const evalTaskColumns = `id, created_ts, call_id, repo_head, brief, turn_type, subsystem, frontier_model,
       request_zstd, reference_response_zstd, origin, language, layer, nature, difficulty, characteristics, active,
       holdout_tests_zstd, agentic_ready`

// InsertEvalTask inserts row into eval_tasks. When a row with the same
// (call_id, origin) already exists (the harvester's dedup key), no row is
// inserted and inserted is false; this is not an error, since re-running
// harvest over already-seen calls is expected. row.Active is ignored on
// insert: the table's own DEFAULT 1 applies, so a caller that forgets to
// set it never accidentally inserts an inactive task. row.AgenticReady is
// also ignored on insert: it is only ever set later, by a sandbox
// dependency prep attempt (UpdateEvalTaskAgenticReady).
func InsertEvalTask(db *sql.DB, row EvalTaskRow) (id int64, inserted bool, err error) {
	res, err := db.Exec(`
INSERT INTO eval_tasks (
  created_ts, call_id, repo_head, brief, turn_type, subsystem, frontier_model,
  request_zstd, reference_response_zstd, origin, language, layer, nature, difficulty, characteristics,
  holdout_tests_zstd
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(call_id, origin) DO NOTHING`,
		row.CreatedTS, row.CallID, row.RepoHead, row.Brief, row.TurnType, row.Subsystem, row.FrontierModel,
		row.RequestZstd, nilIfEmptyBytes(row.ReferenceResponseZstd), row.Origin,
		row.Language, row.Layer, row.Nature, row.Difficulty, row.Characteristics,
		nilIfEmptyBytes(row.HoldoutTestsZstd),
	)
	if err != nil {
		return 0, false, fmt.Errorf("inserting eval task (origin %s): %w", row.Origin, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("reading rows affected inserting eval task: %w", err)
	}
	if affected == 0 {
		return 0, false, nil
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("reading inserted eval task id: %w", err)
	}
	return newID, true, nil
}

// GetEvalTask fetches the eval_tasks row with the given id. It returns
// sql.ErrNoRows (wrapped) when no such row exists.
func GetEvalTask(db *sql.DB, id int64) (*EvalTaskRow, error) {
	row := db.QueryRow(`SELECT `+evalTaskColumns+` FROM eval_tasks WHERE id = ?`, id)
	t, err := scanEvalTaskRow(row)
	if err != nil {
		return nil, fmt.Errorf("getting eval task %d: %w", id, err)
	}
	return t, nil
}

// ActiveEvalTasks returns every eval_tasks row with active = 1, oldest
// first: the input to `eval run`.
func ActiveEvalTasks(db *sql.DB) ([]EvalTaskRow, error) {
	rows, err := db.Query(`SELECT ` + evalTaskColumns + ` FROM eval_tasks WHERE active = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("querying active eval tasks: %w", err)
	}
	defer rows.Close()
	return scanEvalTaskRows(rows)
}

// AllEvalTasks returns every eval_tasks row, active or not, oldest first:
// the input to `eval list`.
func AllEvalTasks(db *sql.DB) ([]EvalTaskRow, error) {
	rows, err := db.Query(`SELECT ` + evalTaskColumns + ` FROM eval_tasks ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("querying all eval tasks: %w", err)
	}
	defer rows.Close()
	return scanEvalTaskRows(rows)
}

// EvalTasksByOrigin returns every eval_tasks row with the given origin,
// oldest first. Used by seed-history to load existing history-origin tasks
// for its commit-sha dedup check (done in Go over the characteristics JSON,
// not in SQL, matching this package's existing JSON-in-Go convention), and
// by reverse-briefs to find commit_subject-sourced tasks.
func EvalTasksByOrigin(db *sql.DB, origin string) ([]EvalTaskRow, error) {
	rows, err := db.Query(`SELECT `+evalTaskColumns+` FROM eval_tasks WHERE origin = ? ORDER BY id`, origin)
	if err != nil {
		return nil, fmt.Errorf("querying eval tasks by origin %s: %w", origin, err)
	}
	defer rows.Close()
	return scanEvalTaskRows(rows)
}

// UpdateEvalTaskBrief overwrites an eval_tasks row's brief and
// characteristics columns: the mutation `eval reverse-briefs` performs once
// a batched rewrite comes back (brief replaced, characteristics'
// brief_source and reverse_brief state updated).
func UpdateEvalTaskBrief(db *sql.DB, id int64, brief, characteristicsJSON string) error {
	if _, err := db.Exec(`UPDATE eval_tasks SET brief = ?, characteristics = ? WHERE id = ?`, brief, characteristicsJSON, id); err != nil {
		return fmt.Errorf("updating eval task %d brief: %w", id, err)
	}
	return nil
}

func scanEvalTaskRow(row *sql.Row) (*EvalTaskRow, error) {
	var t EvalTaskRow
	var active int
	if err := row.Scan(
		&t.ID, &t.CreatedTS, &t.CallID, &t.RepoHead, &t.Brief, &t.TurnType, &t.Subsystem, &t.FrontierModel,
		&t.RequestZstd, &t.ReferenceResponseZstd, &t.Origin, &t.Language, &t.Layer, &t.Nature, &t.Difficulty,
		&t.Characteristics, &active, &t.HoldoutTestsZstd, &t.AgenticReady,
	); err != nil {
		return nil, err
	}
	t.Active = active != 0
	return &t, nil
}

func scanEvalTaskRows(rows *sql.Rows) ([]EvalTaskRow, error) {
	var out []EvalTaskRow
	for rows.Next() {
		var t EvalTaskRow
		var active int
		if err := rows.Scan(
			&t.ID, &t.CreatedTS, &t.CallID, &t.RepoHead, &t.Brief, &t.TurnType, &t.Subsystem, &t.FrontierModel,
			&t.RequestZstd, &t.ReferenceResponseZstd, &t.Origin, &t.Language, &t.Layer, &t.Nature, &t.Difficulty,
			&t.Characteristics, &active, &t.HoldoutTestsZstd, &t.AgenticReady,
		); err != nil {
			return nil, fmt.Errorf("scanning eval task row: %w", err)
		}
		t.Active = active != 0
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating eval task rows: %w", err)
	}
	return out, nil
}

// EvalRunRow mirrors one row of the eval_runs table.
type EvalRunRow struct {
	ID          int64
	TS          string
	Backend     string
	Model       string
	TasksTotal  sql.NullInt64
	TasksPassed sql.NullInt64
	Ladder      sql.NullString
	TokensIn    sql.NullInt64
	TokensOut   sql.NullInt64
}

// InsertEvalRun inserts a new eval_runs row (backend/model/ts only; totals
// are filled in later via UpdateEvalRunSummary once every task has been
// scored) and returns its id, needed up front so eval_results rows can
// reference it as they are written.
func InsertEvalRun(db *sql.DB, ts, backend, model string) (int64, error) {
	res, err := db.Exec(`INSERT INTO eval_runs (ts, backend, model) VALUES (?, ?, ?)`, ts, backend, model)
	if err != nil {
		return 0, fmt.Errorf("inserting eval run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted eval run id: %w", err)
	}
	return id, nil
}

// UpdateEvalRunSummary records an eval run's final totals: how many tasks
// were scored and how many passed, the per-track ladder JSON, and the
// accumulated token counts from every backend response in the run.
func UpdateEvalRunSummary(db *sql.DB, id int64, tasksTotal, tasksPassed int, ladderJSON string, tokensIn, tokensOut int64) error {
	_, err := db.Exec(`
UPDATE eval_runs SET tasks_total = ?, tasks_passed = ?, ladder = ?, tokens_in = ?, tokens_out = ?
WHERE id = ?`, tasksTotal, tasksPassed, nullIfEmpty(ladderJSON), tokensIn, tokensOut, id)
	if err != nil {
		return fmt.Errorf("updating eval run %d summary: %w", id, err)
	}
	return nil
}

// MostRecentPriorRunOtherModel returns the most recent eval_runs row (by
// id, so strictly earlier than beforeRunID) whose model differs from
// model, the comparison basis for eval run's per-task regression listing.
// It returns (nil, nil) when no such run exists yet.
func MostRecentPriorRunOtherModel(db *sql.DB, beforeRunID int64, model string) (*EvalRunRow, error) {
	row := db.QueryRow(`
SELECT id, ts, backend, model, tasks_total, tasks_passed, ladder, tokens_in, tokens_out
FROM eval_runs WHERE model != ? AND id < ? ORDER BY id DESC LIMIT 1`, model, beforeRunID)

	var r EvalRunRow
	err := row.Scan(&r.ID, &r.TS, &r.Backend, &r.Model, &r.TasksTotal, &r.TasksPassed, &r.Ladder, &r.TokensIn, &r.TokensOut)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying most recent prior run for a different model than %s: %w", model, err)
	}
	return &r, nil
}

// EvalResultRow mirrors one row of the eval_results table. Mode, Turns,
// TestsRan, TestsPassed, Regressions, TranscriptZstd and CheatFlags are
// agentic eval mode fields (internal/agentic); a single-turn caller that
// leaves Mode empty gets the table's 'single' default and every other
// agentic field stored NULL.
type EvalResultRow struct {
	ID             int64
	EvalRunID      int64
	EvalTaskID     int64
	Passed         sql.NullInt64
	Stage          sql.NullString
	Similarity     sql.NullFloat64
	ResponseZstd   []byte
	Error          sql.NullString
	Mode           string
	Turns          sql.NullInt64
	TestsRan       sql.NullInt64
	TestsPassed    sql.NullInt64
	Regressions    sql.NullInt64
	TranscriptZstd []byte
	CheatFlags     sql.NullString
	JudgeVerdict   sql.NullString
}

// InsertEvalResult inserts row into eval_results and returns the new row's
// id. Passed is nil for a ladder_skipped row (never scored). An empty
// row.Mode is stored as 'single' (the table's own DEFAULT does not apply
// once the column is named in the INSERT list).
func InsertEvalResult(db *sql.DB, row EvalResultRow) (int64, error) {
	mode := row.Mode
	if mode == "" {
		mode = "single"
	}
	res, err := db.Exec(`
INSERT INTO eval_results (
  eval_run_id, eval_task_id, passed, stage, similarity, response_zstd, error,
  mode, turns, tests_ran, tests_passed, regressions, transcript_zstd, cheat_flags,
  judge_verdict
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.EvalRunID, row.EvalTaskID, row.Passed, row.Stage, row.Similarity,
		nilIfEmptyBytes(row.ResponseZstd), row.Error,
		mode, row.Turns, row.TestsRan, row.TestsPassed, row.Regressions,
		nilIfEmptyBytes(row.TranscriptZstd), row.CheatFlags, row.JudgeVerdict,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting eval result for task %d: %w", row.EvalTaskID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted eval result id: %w", err)
	}
	return id, nil
}

const evalResultColumns = `id, eval_run_id, eval_task_id, passed, stage, similarity, response_zstd, error,
       mode, turns, tests_ran, tests_passed, regressions, transcript_zstd, cheat_flags`

// EvalResultsForRun returns every eval_results row for runID.
func EvalResultsForRun(db *sql.DB, runID int64) ([]EvalResultRow, error) {
	rows, err := db.Query(`SELECT `+evalResultColumns+` FROM eval_results WHERE eval_run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("querying eval results for run %d: %w", runID, err)
	}
	defer rows.Close()
	return scanEvalResultRows(rows)
}

func scanEvalResultRows(rows *sql.Rows) ([]EvalResultRow, error) {
	var out []EvalResultRow
	for rows.Next() {
		var r EvalResultRow
		if err := rows.Scan(
			&r.ID, &r.EvalRunID, &r.EvalTaskID, &r.Passed, &r.Stage, &r.Similarity, &r.ResponseZstd, &r.Error,
			&r.Mode, &r.Turns, &r.TestsRan, &r.TestsPassed, &r.Regressions, &r.TranscriptZstd, &r.CheatFlags,
		); err != nil {
			return nil, fmt.Errorf("scanning eval result row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating eval result rows: %w", err)
	}
	return out, nil
}

// EvalResultWithTask is one eval_results row joined to the eval_tasks and
// eval_runs rows it belongs to: everything a scorecard or `eval list` needs
// to group and label a result without a second round trip per row.
type EvalResultWithTask struct {
	EvalResultID    int64
	EvalRunID       int64
	EvalTaskID      int64
	Backend         string
	Model           string
	Passed          sql.NullInt64
	Stage           sql.NullString
	Error           sql.NullString
	Brief           string
	RepoHead        sql.NullString
	Origin          string
	TurnType        sql.NullString
	Subsystem       sql.NullString
	Language        sql.NullString
	Layer           sql.NullString
	Nature          sql.NullString
	Difficulty      sql.NullString
	Characteristics sql.NullString
}

const evalResultWithTaskColumns = `
res.id, res.eval_run_id, res.eval_task_id, er.backend, er.model, res.passed, res.stage, res.error,
et.brief, et.repo_head, et.origin, et.turn_type, et.subsystem, et.language, et.layer, et.nature, et.difficulty, et.characteristics`

// EvalResultsWithTaskForRun returns every eval_results row for runID joined
// to its task's characteristics, the input to eval run's per-dimension
// scorecard.
func EvalResultsWithTaskForRun(db *sql.DB, runID int64) ([]EvalResultWithTask, error) {
	rows, err := db.Query(`
SELECT `+evalResultWithTaskColumns+`
FROM eval_results res
JOIN eval_runs er ON er.id = res.eval_run_id
JOIN eval_tasks et ON et.id = res.eval_task_id
WHERE res.eval_run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("querying eval results with task for run %d: %w", runID, err)
	}
	defer rows.Close()
	return scanEvalResultsWithTask(rows)
}

// AllEvalResultsWithTask returns every eval_results row across every run,
// joined to its task's characteristics: the input to `eval list`'s
// per-model pass rate aggregation.
func AllEvalResultsWithTask(db *sql.DB) ([]EvalResultWithTask, error) {
	rows, err := db.Query(`
SELECT ` + evalResultWithTaskColumns + `
FROM eval_results res
JOIN eval_runs er ON er.id = res.eval_run_id
JOIN eval_tasks et ON et.id = res.eval_task_id`)
	if err != nil {
		return nil, fmt.Errorf("querying all eval results with task: %w", err)
	}
	defer rows.Close()
	return scanEvalResultsWithTask(rows)
}

func scanEvalResultsWithTask(rows *sql.Rows) ([]EvalResultWithTask, error) {
	var out []EvalResultWithTask
	for rows.Next() {
		var r EvalResultWithTask
		if err := rows.Scan(
			&r.EvalResultID, &r.EvalRunID, &r.EvalTaskID, &r.Backend, &r.Model, &r.Passed, &r.Stage, &r.Error,
			&r.Brief, &r.RepoHead, &r.Origin, &r.TurnType, &r.Subsystem, &r.Language, &r.Layer, &r.Nature,
			&r.Difficulty, &r.Characteristics,
		); err != nil {
			return nil, fmt.Errorf("scanning eval result with task row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating eval result with task rows: %w", err)
	}
	return out, nil
}

// EarliestCallInSession returns the oldest call (lowest id) with the given
// session_id: the brief derivation's session walk-back target, the human's
// initiating instruction for that session. It returns sql.ErrNoRows
// (wrapped) when sessionID is empty or no call carries it.
func EarliestCallInSession(db *sql.DB, sessionID string) (*CallRow, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("getting earliest call in session: %w", sql.ErrNoRows)
	}
	row := db.QueryRow(`
SELECT id, ts, session_id, model, stream, request_zstd, response_zstd,
       input_tokens, output_tokens, latency_ms, repo_head, status, error, source
FROM calls WHERE session_id = ? ORDER BY id ASC LIMIT 1`, sessionID)

	var c CallRow
	var stream int
	if err := row.Scan(
		&c.ID, &c.TS, &c.SessionID, &c.Model, &stream, &c.RequestZstd, &c.ResponseZstd,
		&c.InputTokens, &c.OutputTokens, &c.LatencyMs, &c.RepoHead, &c.Status, &c.Error, &c.Source,
	); err != nil {
		return nil, fmt.Errorf("getting earliest call in session %s: %w", sessionID, err)
	}
	c.Stream = stream != 0
	return &c, nil
}

// HarvestSourceRow is one call eligible to become a harvested eval task:
// enough of calls and features to derive its characteristics and freeze a
// request/reference copy. Origin is set by the caller (each harvest query
// covers exactly one origin).
type HarvestSourceRow struct {
	CallID           int64
	SessionID        sql.NullString
	RepoHead         sql.NullString
	TurnType         sql.NullString
	Subsystem        sql.NullString
	FrontierModel    sql.NullString
	RequestZstd      []byte
	ResponseZstd     []byte
	HadErrorFollowup sql.NullInt64
	TS               string
}

// HarvestDisagreements returns one row per verification with agree = 0
// (the local model tripped up), joined back to the call it replayed.
func HarvestDisagreements(db *sql.DB) ([]HarvestSourceRow, error) {
	rows, err := db.Query(`
SELECT c.id, c.session_id, c.repo_head, f.turn_type, f.subsystem, c.model, c.request_zstd, c.response_zstd,
       f.had_error_followup, c.ts
FROM verifications v
JOIN replays r ON r.id = v.replay_id
JOIN calls c ON c.id = r.call_id
JOIN features f ON f.call_id = c.id
WHERE v.agree = 0
ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("querying disagreement harvest candidates: %w", err)
	}
	defer rows.Close()
	return scanHarvestSourceRows(rows)
}

// HarvestEscalations returns one row per router_decisions row with
// decision = 'escalated', joined to the call that was served locally.
func HarvestEscalations(db *sql.DB) ([]HarvestSourceRow, error) {
	rows, err := db.Query(`
SELECT c.id, c.session_id, c.repo_head, f.turn_type, f.subsystem, c.model, c.request_zstd, c.response_zstd,
       f.had_error_followup, c.ts
FROM router_decisions rd
JOIN calls c ON c.id = rd.call_id
JOIN features f ON f.call_id = c.id
WHERE rd.decision = 'escalated'
ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("querying escalation harvest candidates: %w", err)
	}
	defer rows.Close()
	return scanHarvestSourceRows(rows)
}

// HarvestErrorFollowups returns one row per features row with
// had_error_followup = 1: the frontier itself struggled on this call.
func HarvestErrorFollowups(db *sql.DB) ([]HarvestSourceRow, error) {
	rows, err := db.Query(`
SELECT c.id, c.session_id, c.repo_head, f.turn_type, f.subsystem, c.model, c.request_zstd, c.response_zstd,
       f.had_error_followup, c.ts
FROM features f
JOIN calls c ON c.id = f.call_id
WHERE f.had_error_followup = 1
ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("querying error-followup harvest candidates: %w", err)
	}
	defer rows.Close()
	return scanHarvestSourceRows(rows)
}

// HarvestCleanCandidates returns up to limit single_file_edit calls with
// had_error_followup = 0 whose replays, if any exist, all agreed (never
// disagreed and never sat in the judge middle band), oldest first: the
// -include-clean sample of tasks that gave the local model no trouble.
func HarvestCleanCandidates(db *sql.DB, limit int) ([]HarvestSourceRow, error) {
	rows, err := db.Query(`
SELECT c.id, c.session_id, c.repo_head, f.turn_type, f.subsystem, c.model, c.request_zstd, c.response_zstd,
       f.had_error_followup, c.ts
FROM features f
JOIN calls c ON c.id = f.call_id
WHERE f.had_error_followup = 0
  AND f.turn_type = 'single_file_edit'
  AND NOT EXISTS (
    SELECT 1 FROM replays r JOIN verifications v ON v.replay_id = r.id
    WHERE r.call_id = c.id AND (v.agree = 0 OR v.agree IS NULL)
  )
ORDER BY c.id
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("querying clean harvest candidates: %w", err)
	}
	defer rows.Close()
	return scanHarvestSourceRows(rows)
}

func scanHarvestSourceRows(rows *sql.Rows) ([]HarvestSourceRow, error) {
	var out []HarvestSourceRow
	for rows.Next() {
		var h HarvestSourceRow
		if err := rows.Scan(
			&h.CallID, &h.SessionID, &h.RepoHead, &h.TurnType, &h.Subsystem, &h.FrontierModel,
			&h.RequestZstd, &h.ResponseZstd, &h.HadErrorFollowup, &h.TS,
		); err != nil {
			return nil, fmt.Errorf("scanning harvest source row: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating harvest source rows: %w", err)
	}
	return out, nil
}

// nilIfEmptyBytes returns nil for an empty byte slice, else b unchanged, so
// an optional BLOB column is stored as SQL NULL rather than a zero-length
// blob.
func nilIfEmptyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// UpdateEvalTaskRequest replaces one task's frozen synthesized request
// (eval refresh-requests: prompt-template changes reach seeded tasks
// without discarding their briefs).
func UpdateEvalTaskRequest(db *sql.DB, id int64, requestZstd []byte) error {
	if _, err := db.Exec(`UPDATE eval_tasks SET request_zstd = ? WHERE id = ?`, requestZstd, id); err != nil {
		return fmt.Errorf("updating eval task %d request: %w", id, err)
	}
	return nil
}

// FailedUnjudgedEvalResultRow is one failed eval result awaiting judge
// re-grading (eval judge-fails).
type FailedUnjudgedEvalResultRow struct {
	ID           int64
	EvalRunID    int64
	EvalTaskID   int64
	Passed       int
	ResponseZstd []byte
}

// FailedUnjudgedEvalResults returns scored, error-free eval results that
// have no judge verdict yet, for runID, or for every run when runID is 0.
// Mechanical exact matches (stage='exact') are excluded: byte-equality
// needs no judge. Everything else, passes included, goes to the judge:
// the judge verdict is the deciding grade (mechanical similarity is
// recorded as a diagnostic only).
func FailedUnjudgedEvalResults(db *sql.DB, runID int64) ([]FailedUnjudgedEvalResultRow, error) {
	q := `SELECT id, eval_run_id, eval_task_id, passed, response_zstd FROM eval_results
WHERE passed IS NOT NULL AND (error IS NULL OR error = '')
  AND COALESCE(stage, '') != 'exact'
  AND (judge_verdict IS NULL OR judge_verdict = '')
  AND response_zstd IS NOT NULL`
	args := []any{}
	if runID != 0 {
		q += ` AND eval_run_id = ?`
		args = append(args, runID)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying failed unjudged eval results: %w", err)
	}
	defer rows.Close()
	var out []FailedUnjudgedEvalResultRow
	for rows.Next() {
		var r FailedUnjudgedEvalResultRow
		if err := rows.Scan(&r.ID, &r.EvalRunID, &r.EvalTaskID, &r.Passed, &r.ResponseZstd); err != nil {
			return nil, fmt.Errorf("scanning failed unjudged eval result: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApplyEvalJudgeVerdict records a judge verdict on one eval result,
// updating passed accordingly and marking the deciding stage "judge".
func ApplyEvalJudgeVerdict(db *sql.DB, id int64, passed int, verdictJSON string) error {
	if _, err := db.Exec(`UPDATE eval_results SET passed = ?, judge_verdict = ?, stage = 'judge' WHERE id = ?`, passed, verdictJSON, id); err != nil {
		return fmt.Errorf("applying judge verdict to eval result %d: %w", id, err)
	}
	return nil
}

// UpdateEvalTaskHoldout replaces one task's held-out tests payload
// (eval refresh-requests backfill: tasks seeded before holdout derivation
// existed, or before a lane's command derivation was added).
func UpdateEvalTaskHoldout(db *sql.DB, id int64, holdoutZstd []byte) error {
	if _, err := db.Exec(`UPDATE eval_tasks SET holdout_tests_zstd = ? WHERE id = ?`, holdoutZstd, id); err != nil {
		return fmt.Errorf("updating eval task %d holdout: %w", id, err)
	}
	return nil
}
