package verify

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelativize(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		repoPath string
		wantRel  string
		wantOK   bool
	}{
		{"absolute under repo", "/repo/a/b.go", "/repo", "a/b.go", true},
		{"absolute equal to repo root file", "/repo/b.go", "/repo", "b.go", true},
		{"absolute outside repo", "/elsewhere/b.go", "/repo", "", false},
		{"already relative", "a/b.go", "/repo", "a/b.go", true},
		{"empty path", "", "/repo", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel, ok := relativize(tc.path, tc.repoPath)
			if ok != tc.wantOK || rel != tc.wantRel {
				t.Errorf("relativize(%q, %q) = (%q, %v), want (%q, %v)", tc.path, tc.repoPath, rel, ok, tc.wantRel, tc.wantOK)
			}
		})
	}
}

func TestApplyTextEdits_Basic(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applyTextEdits(full, []textEdit{{Old: "world", New: "there"}}); err != nil {
		t.Fatalf("applyTextEdits: %v", err)
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello there" {
		t.Errorf("content = %q, want %q", got, "hello there")
	}
}

func TestApplyTextEdits_OldStringNotFoundFails(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := applyTextEdits(full, []textEdit{{Old: "not present", New: "x"}})
	if err == nil {
		t.Fatal("expected an error when old_string is not found")
	}
	got, readErr := os.ReadFile(full)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "hello world" {
		t.Errorf("file was modified despite a failed edit: %q", got)
	}
}

func TestApplyTextEdits_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, []byte("a a a"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applyTextEdits(full, []textEdit{{Old: "a", New: "b", ReplaceAll: true}}); err != nil {
		t.Fatalf("applyTextEdits: %v", err)
	}
	got, _ := os.ReadFile(full)
	if string(got) != "b b b" {
		t.Errorf("content = %q, want %q", got, "b b b")
	}
}

func TestApplyTextEdits_ReplaceFirstOnly(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, []byte("a a a"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applyTextEdits(full, []textEdit{{Old: "a", New: "b", ReplaceAll: false}}); err != nil {
		t.Fatalf("applyTextEdits: %v", err)
	}
	got, _ := os.ReadFile(full)
	if string(got) != "b a a" {
		t.Errorf("content = %q, want %q", got, "b a a")
	}
}

func TestApplyTextEdits_MultiEditAppliesInOrder(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, []byte("one two three"), 0o644); err != nil {
		t.Fatal(err)
	}

	edits := []textEdit{
		{Old: "one", New: "1"},
		{Old: "three", New: "3"},
	}
	if err := applyTextEdits(full, edits); err != nil {
		t.Fatalf("applyTextEdits: %v", err)
	}
	got, _ := os.ReadFile(full)
	if string(got) != "1 two 3" {
		t.Errorf("content = %q, want %q", got, "1 two 3")
	}
}

func TestApplyFileEdits_Write(t *testing.T) {
	repoPath := t.TempDir()
	worktreeDir := t.TempDir()

	edits := []fileEdit{
		{Tool: toolWrite, FilePath: filepath.Join(repoPath, "new/nested/file.txt"), Content: "hello"},
	}
	touched, failures := applyFileEdits(worktreeDir, edits, repoPath)
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if len(touched) != 1 || touched[0] != filepath.Join("new", "nested", "file.txt") {
		t.Errorf("touched = %v", touched)
	}
	got, err := os.ReadFile(filepath.Join(worktreeDir, "new", "nested", "file.txt"))
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}

func TestApplyFileEdits_PathOutsideRepoRecordsFailure(t *testing.T) {
	repoPath := t.TempDir()
	worktreeDir := t.TempDir()

	edits := []fileEdit{
		{Tool: toolWrite, FilePath: "/somewhere/else/file.txt", Content: "hello"},
	}
	touched, failures := applyFileEdits(worktreeDir, edits, repoPath)
	if len(touched) != 0 {
		t.Errorf("touched = %v, want none", touched)
	}
	if len(failures) != 1 || failures[0].Tool != "apply" || failures[0].OK {
		t.Errorf("failures = %v, want one apply failure", failures)
	}
}

func TestApplyFileEdits_NotebookEditUnsupported(t *testing.T) {
	repoPath := t.TempDir()
	worktreeDir := t.TempDir()

	edits := []fileEdit{
		{Tool: toolNotebookEdit, FilePath: filepath.Join(repoPath, "nb.ipynb")},
	}
	touched, failures := applyFileEdits(worktreeDir, edits, repoPath)
	if len(touched) != 1 {
		t.Errorf("touched = %v, want the notebook path recorded even though application failed", touched)
	}
	if len(failures) != 1 || failures[0].OK {
		t.Errorf("failures = %v, want one apply failure", failures)
	}
}

func TestExtractFileEdits(t *testing.T) {
	msg := mustMarshalMessage([]any{
		toolUseBlock("Edit", map[string]any{"file_path": "/repo/a.go", "old_string": "x", "new_string": "y"}),
		toolUseBlock("MultiEdit", map[string]any{
			"file_path": "/repo/b.go",
			"edits": []map[string]any{
				{"old_string": "p", "new_string": "q"},
				{"old_string": "r", "new_string": "s", "replace_all": true},
			},
		}),
		toolUseBlock("Write", map[string]any{"file_path": "/repo/c.go", "content": "package c\n"}),
		toolUseBlock("SomeOtherTool", map[string]any{"whatever": "value"}),
	})

	edits, err := extractFileEdits(msg)
	if err != nil {
		t.Fatalf("extractFileEdits: %v", err)
	}
	if len(edits) != 3 {
		t.Fatalf("len(edits) = %d, want 3 (SomeOtherTool ignored)", len(edits))
	}
	if edits[0].Tool != toolEdit || edits[0].FilePath != "/repo/a.go" {
		t.Errorf("edits[0] = %+v", edits[0])
	}
	if edits[1].Tool != toolMultiEdit || len(edits[1].Edits) != 2 || !edits[1].Edits[1].ReplaceAll {
		t.Errorf("edits[1] = %+v", edits[1])
	}
	if edits[2].Tool != toolWrite || edits[2].Content != "package c\n" {
		t.Errorf("edits[2] = %+v", edits[2])
	}
}

func TestLanguageForPath(t *testing.T) {
	cases := map[string]string{
		"a/b.go":  "go",
		"a/b.php": "php",
		"a/b.vue": "vue",
		"a/b.ts":  "ts",
		"a/b":     "",
		"":        "",
	}
	for path, want := range cases {
		if got := languageForPath(path); got != want {
			t.Errorf("languageForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSubsystemOf(t *testing.T) {
	if got := subsystemOf("iznik-server-go/handler/x.go"); got != "iznik-server-go" {
		t.Errorf("subsystemOf = %q, want %q", got, "iznik-server-go")
	}
	if got := subsystemOf("README.md"); got != "" {
		t.Errorf("subsystemOf = %q, want empty", got)
	}
}
