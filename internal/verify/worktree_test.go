package verify

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweep_AgeFiltering(t *testing.T) {
	origTemp := os.Getenv("TMPDIR")
	tempRoot := t.TempDir()
	if err := os.Setenv("TMPDIR", tempRoot); err != nil {
		t.Fatalf("setting TMPDIR: %v", err)
	}
	t.Cleanup(func() { os.Setenv("TMPDIR", origTemp) })

	staleDir := filepath.Join(os.TempDir(), verifyTempPrefix+"stale")
	freshDir := filepath.Join(os.TempDir(), verifyTempPrefix+"fresh")
	unrelatedDir := filepath.Join(os.TempDir(), "not-a-verify-dir")

	for _, d := range []string{staleDir, freshDir, unrelatedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}

	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleDir, oldTime, oldTime); err != nil {
		t.Fatalf("backdating %s: %v", staleDir, err)
	}

	removed, err := Sweep("", time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(removed) != 1 || removed[0] != staleDir {
		t.Errorf("removed = %v, want only %v", removed, staleDir)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("stale dir still exists after sweep")
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("fresh dir should not have been removed: %v", err)
	}
	if _, err := os.Stat(unrelatedDir); err != nil {
		t.Errorf("unrelated dir should not have been removed: %v", err)
	}
}

func TestSweep_EmptyTempDirIsFine(t *testing.T) {
	origTemp := os.Getenv("TMPDIR")
	tempRoot := t.TempDir()
	if err := os.Setenv("TMPDIR", tempRoot); err != nil {
		t.Fatalf("setting TMPDIR: %v", err)
	}
	t.Cleanup(func() { os.Setenv("TMPDIR", origTemp) })

	removed, err := Sweep("", time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
}
