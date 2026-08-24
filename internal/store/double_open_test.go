package store

import (
	"path/filepath"
	"testing"
)

// TestSecondOpenSamePathSameProcess reproduces the bootstrap failure: a
// second store.Open on the same path while the first connection is still
// open must work (bootstrap holds one connection for reverse-briefs while
// its registry-invoked steps open their own).
func TestSecondOpenSamePathSameProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "splitter.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer db1.Close()
	if err := Migrate(db1); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db1.Exec(`INSERT INTO calls (ts) VALUES ('2026-08-24T00:00:00Z')`); err != nil {
		t.Fatalf("insert on first connection: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open while first still open: %v", err)
	}
	defer db2.Close()
	var n int
	if err := db2.QueryRow(`SELECT count(*) FROM calls`).Scan(&n); err != nil {
		t.Fatalf("query on second connection: %v", err)
	}
	if n != 1 {
		t.Fatalf("second connection sees %d calls, want 1", n)
	}
}
