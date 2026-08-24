// Package store opens and migrates splitter's SQLite database and provides
// compression helpers and query functions used by every component. New
// per-component queries belong in their own <component>_store.go file in
// this package rather than in this one.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

// schemaVersion is the current PRAGMA user_version. There is one migration:
// v1 creates the full schema described in DESIGN.md.
const schemaVersion = 1

// Open opens (creating if needed) the SQLite database at path, with parent
// directories created 0700 and the database file chmod 0600, WAL mode,
// busy_timeout=5000ms, and foreign_keys on. Call Migrate on the result
// before using it.
func Open(path string) (*sql.DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating db directory %s: %w", dir, err)
	}

	// Ensure the file exists so the chmod below applies to it, and so a
	// database created with a restrictive umask still ends up 0600.
	if _, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600); err != nil {
		return nil, fmt.Errorf("creating db file %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod db file %s: %w", path, err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening db %s: %w", path, err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting WAL mode on %s: %w", path, err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting busy_timeout on %s: %w", path, err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enabling foreign_keys on %s: %w", path, err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("chmod db file %s: %w", path, err)
	}

	return db, nil
}

// Migrate brings the database schema up to schemaVersion. It is idempotent:
// calling it again on an already migrated database is a no-op.
func Migrate(db *sql.DB) error {
	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}

	if current >= schemaVersion {
		return nil
	}

	if current < 1 {
		if err := migrateV1(db); err != nil {
			return fmt.Errorf("migrating to v1: %w", err)
		}
	}

	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("setting user_version: %w", err)
	}
	return nil
}

func migrateV1(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS calls (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  session_id TEXT,
  model TEXT,
  stream INTEGER NOT NULL DEFAULT 0,
  request_zstd BLOB,
  response_zstd BLOB,
  input_tokens INTEGER,
  output_tokens INTEGER,
  latency_ms INTEGER,
  repo_head TEXT,
  status INTEGER,
  error TEXT,
  source TEXT NOT NULL DEFAULT 'proxy'
);
CREATE INDEX IF NOT EXISTS idx_calls_session ON calls(session_id, id);

CREATE TABLE IF NOT EXISTS features (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  call_id INTEGER NOT NULL UNIQUE REFERENCES calls(id),
  turn_type TEXT NOT NULL,
  files_touched TEXT NOT NULL DEFAULT '[]',
  subsystem TEXT,
  context_tokens INTEGER,
  output_tokens INTEGER,
  had_error_followup INTEGER
);

CREATE TABLE IF NOT EXISTS replays (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  call_id INTEGER NOT NULL REFERENCES calls(id),
  backend TEXT NOT NULL,
  model TEXT NOT NULL,
  response_zstd BLOB,
  latency_ms INTEGER,
  error TEXT,
  created_ts TEXT NOT NULL,
  UNIQUE(call_id, backend, model)
);

CREATE TABLE IF NOT EXISTS verifications (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  replay_id INTEGER NOT NULL UNIQUE REFERENCES replays(id),
  stage TEXT NOT NULL,
  similarity REAL,
  frontier_lint TEXT,
  local_lint TEXT,
  frontier_tests TEXT,
  local_tests TEXT,
  judge_verdict TEXT,
  tests_judge_conflict INTEGER NOT NULL DEFAULT 0,
  agree INTEGER,
  decided_ts TEXT
);

CREATE TABLE IF NOT EXISTS judge_batches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  batch_id TEXT NOT NULL,
  submitted_ts TEXT NOT NULL,
  completed_ts TEXT,
  input_tokens INTEGER,
  output_tokens INTEGER,
  status TEXT NOT NULL DEFAULT 'submitted'
);

CREATE TABLE IF NOT EXISTS judge_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  judge_batch_id INTEGER REFERENCES judge_batches(id),
  verification_id INTEGER NOT NULL UNIQUE REFERENCES verifications(id),
  custom_id TEXT NOT NULL,
  verdict TEXT,
  status TEXT NOT NULL DEFAULT 'queued',
  created_ts TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS router_state (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  category TEXT NOT NULL,
  families TEXT NOT NULL,
  n INTEGER NOT NULL DEFAULT 0,
  agreed INTEGER NOT NULL DEFAULT 0,
  wilson_lb REAL,
  routable INTEGER NOT NULL DEFAULT 0,
  disabled_reason TEXT,
  updated_ts TEXT,
  UNIQUE(category, families)
);

CREATE TABLE IF NOT EXISTS router_decisions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  session_id TEXT,
  call_id INTEGER,
  category TEXT,
  decision TEXT NOT NULL,
  stats TEXT
);

CREATE TABLE IF NOT EXISTS eval_tasks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_ts TEXT NOT NULL,
  call_id INTEGER REFERENCES calls(id),
  repo_head TEXT,
  brief TEXT NOT NULL,
  turn_type TEXT,
  subsystem TEXT,
  frontier_model TEXT,
  request_zstd BLOB NOT NULL,
  reference_response_zstd BLOB,
  origin TEXT NOT NULL,
  language TEXT,
  layer TEXT,
  nature TEXT,
  difficulty TEXT,
  characteristics TEXT,
  active INTEGER NOT NULL DEFAULT 1,
  UNIQUE(call_id, origin)
);

CREATE TABLE IF NOT EXISTS eval_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  backend TEXT NOT NULL,
  model TEXT NOT NULL,
  tasks_total INTEGER,
  tasks_passed INTEGER
);

CREATE TABLE IF NOT EXISTS eval_results (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  eval_run_id INTEGER NOT NULL REFERENCES eval_runs(id),
  eval_task_id INTEGER NOT NULL REFERENCES eval_tasks(id),
  passed INTEGER,
  stage TEXT,
  similarity REAL,
  response_zstd BLOB,
  error TEXT,
  UNIQUE(eval_run_id, eval_task_id)
);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("executing v1 schema: %w", err)
	}
	return nil
}

// Compress zstd-compresses data.
func Compress(data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("creating zstd encoder: %w", err)
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
}

// Decompress reverses Compress.
func Decompress(data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("creating zstd decoder: %w", err)
	}
	defer dec.Close()
	out, err := dec.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}
	return out, nil
}
