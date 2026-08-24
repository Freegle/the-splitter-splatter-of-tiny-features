package store

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func openTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "splitter.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db, path
}

func TestOpen_CreatesParentDirsAndFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file permissions not applicable on windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "splitter.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	parentInfo, err := os.Stat(filepath.Join(dir, "a", "b"))
	if err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
	if mode := parentInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("parent dir mode = %o, want 0700", mode)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db file: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("db file mode = %o, want 0600", mode)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db, _ := openTestDB(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate call: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("third Migrate call: %v", err)
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}

	tables := []string{
		"calls", "features", "replays", "verifications",
		"judge_batches", "judge_items", "router_state", "router_decisions",
		"eval_tasks", "eval_runs", "eval_results",
	}
	for _, tbl := range tables {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after migrate: %v", tbl, err)
		}
	}
}

func TestMigrate_RouterStateUniqueConstraint(t *testing.T) {
	db, _ := openTestDB(t)

	insert := `INSERT INTO router_state (category, families) VALUES (?, ?)`
	if _, err := db.Exec(insert, "single_file_edit|internal", "claude-sonnet>qwen-coder"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := db.Exec(insert, "single_file_edit|internal", "claude-sonnet>qwen-coder"); err == nil {
		t.Error("expected UNIQUE(category, families) violation on duplicate insert")
	}
	// A different family pair for the same category is allowed.
	if _, err := db.Exec(insert, "single_file_edit|internal", "claude-opus>qwen-coder"); err != nil {
		t.Errorf("insert with different families should succeed: %v", err)
	}
}

func TestMigrate_EvalTasksUniqueConstraint(t *testing.T) {
	db, _ := openTestDB(t)

	if _, err := db.Exec(`INSERT INTO calls (ts) VALUES (?)`, "2026-08-24T00:00:00Z"); err != nil {
		t.Fatalf("insert calls row: %v", err)
	}

	insert := `INSERT INTO eval_tasks (created_ts, brief, origin, request_zstd) VALUES (?, ?, ?, ?)`
	if _, err := db.Exec(insert, "2026-08-24T00:00:00Z", "fix the thing", "clean", []byte("x")); err != nil {
		t.Fatalf("first insert (call_id NULL): %v", err)
	}
	// call_id is nullable and UNIQUE(call_id, origin) does not constrain
	// multiple NULLs (SQLite treats NULLs as distinct in a UNIQUE index).
	if _, err := db.Exec(insert, "2026-08-24T00:00:01Z", "fix another thing", "clean", []byte("y")); err != nil {
		t.Fatalf("second insert (call_id NULL) should not violate UNIQUE: %v", err)
	}

	insertWithCall := `INSERT INTO eval_tasks (created_ts, call_id, brief, origin, request_zstd) VALUES (?, ?, ?, ?, ?)`
	if _, err := db.Exec(insertWithCall, "2026-08-24T00:00:02Z", 1, "brief a", "disagreement", []byte("z")); err != nil {
		t.Fatalf("first insert with call_id: %v", err)
	}
	if _, err := db.Exec(insertWithCall, "2026-08-24T00:00:03Z", 1, "brief a again", "disagreement", []byte("z")); err == nil {
		t.Error("expected UNIQUE(call_id, origin) violation on duplicate insert")
	}
	// A different origin for the same call_id is allowed.
	if _, err := db.Exec(insertWithCall, "2026-08-24T00:00:04Z", 1, "brief a, escalated", "escalated", []byte("z")); err != nil {
		t.Errorf("insert with different origin should succeed: %v", err)
	}
}

func TestMigrate_EvalResultsUniqueConstraint(t *testing.T) {
	db, _ := openTestDB(t)

	if _, err := db.Exec(`INSERT INTO eval_tasks (created_ts, brief, origin, request_zstd) VALUES (?, ?, ?, ?)`,
		"2026-08-24T00:00:00Z", "a task", "manual", []byte("x")); err != nil {
		t.Fatalf("insert eval_task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO eval_runs (ts, backend, model) VALUES (?, ?, ?)`,
		"2026-08-24T00:00:00Z", "ollama", "qwen2.5-coder:7b"); err != nil {
		t.Fatalf("insert eval_run: %v", err)
	}

	insert := `INSERT INTO eval_results (eval_run_id, eval_task_id, passed) VALUES (1, 1, ?)`
	if _, err := db.Exec(insert, 1); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := db.Exec(insert, 0); err == nil {
		t.Error("expected UNIQUE(eval_run_id, eval_task_id) violation on duplicate insert")
	}
}

func TestMigrate_CallsSourceDefault(t *testing.T) {
	db, _ := openTestDB(t)

	res, err := db.Exec(`INSERT INTO calls (ts) VALUES ('2026-08-24T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert without source: %v", err)
	}
	id, _ := res.LastInsertId()

	var source string
	if err := db.QueryRow("SELECT source FROM calls WHERE id = ?", id).Scan(&source); err != nil {
		t.Fatalf("select source: %v", err)
	}
	if source != "proxy" {
		t.Errorf("source default = %q, want proxy", source)
	}
}

func TestCompressDecompress_RoundTrip(t *testing.T) {
	cases := [][]byte{
		[]byte(""),
		[]byte("hello world"),
		bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 1000),
	}
	for _, original := range cases {
		compressed, err := Compress(original)
		if err != nil {
			t.Fatalf("Compress: %v", err)
		}
		decompressed, err := Decompress(compressed)
		if err != nil {
			t.Fatalf("Decompress: %v", err)
		}
		if !bytes.Equal(original, decompressed) {
			t.Errorf("round trip mismatch: got %q, want %q", decompressed, original)
		}
	}
}

func TestCompress_ActuallyCompressesRepetitiveData(t *testing.T) {
	original := bytes.Repeat([]byte("a"), 100000)
	compressed, err := Compress(original)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if len(compressed) >= len(original) {
		t.Errorf("compressed size %d should be smaller than original %d", len(compressed), len(original))
	}
}
