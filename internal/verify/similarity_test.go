package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTokenSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want float64
	}{
		{"both empty", "", "", 1},
		{"identical", "a b c", "a b c", 1},
		{"one substitution of three", "a b c", "a b d", 2.0 / 3.0},
		{"completely different, equal length", "a b", "c d", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenSimilarity(tc.a, tc.b)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("tokenSimilarity(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestFileSimilarity_LevenshteinFallback(t *testing.T) {
	// Force the fallback path even though difft may be installed on the
	// machine running this test.
	original := difftAvailable
	difftAvailable = func() bool { return false }
	t.Cleanup(func() { difftAvailable = original })

	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(pathA, []byte("alpha beta gamma delta"), 0o644); err != nil {
		t.Fatalf("writing a.txt: %v", err)
	}
	if err := os.WriteFile(pathB, []byte("alpha beta gamma epsilon"), 0o644); err != nil {
		t.Fatalf("writing b.txt: %v", err)
	}

	got := fileSimilarity(context.Background(), pathA, pathB)
	want := 0.75 // one substitution among four tokens
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("fileSimilarity = %v, want %v", got, want)
	}
}

func TestFileSimilarity_LevenshteinFallback_MissingFileScoresZero(t *testing.T) {
	original := difftAvailable
	difftAvailable = func() bool { return false }
	t.Cleanup(func() { difftAvailable = original })

	dir := t.TempDir()
	pathA := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(pathA, []byte("content"), 0o644); err != nil {
		t.Fatalf("writing exists.txt: %v", err)
	}
	pathB := filepath.Join(dir, "missing.txt")

	got := fileSimilarity(context.Background(), pathA, pathB)
	if got != 0 {
		t.Errorf("fileSimilarity with a missing file = %v, want 0", got)
	}
}

func TestUnionPaths(t *testing.T) {
	got := unionPaths([]string{"a", "b"}, []string{"b", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("unionPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unionPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMeanSimilarity_FileOnOneSideOnlyScoresZero(t *testing.T) {
	dir := t.TempDir()
	frontierDir := filepath.Join(dir, "frontier")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(frontierDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontierDir, "only_frontier.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := meanSimilarity(context.Background(), frontierDir, localDir, []string{"only_frontier.txt"}, nil)
	if got != 0 {
		t.Errorf("meanSimilarity = %v, want 0 (file touched on one side only)", got)
	}
}
