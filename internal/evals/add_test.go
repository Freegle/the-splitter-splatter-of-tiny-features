package evals

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

// manualTaskByID returns the manual eval task with the given id, failing the
// test if it is not there.
func manualTaskByID(t *testing.T, db *sql.DB, id int64) *store.EvalTaskRow {
	t.Helper()
	rows, err := store.EvalTasksByOrigin(db, OriginManual)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	t.Fatalf("no manual eval task with id %d; got %d task(s)", id, len(rows))
	return nil
}

func TestAdd_InvalidRequestJSON(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := &config.Config{RepoPath: t.TempDir()}

	requestJSON := []byte("not valid json")
	_, err := Add(db, cfg, "abc123", "fix a bug", requestJSON, nil)
	if err == nil {
		t.Fatal("expected error for invalid request JSON")
	}
	if !strings.Contains(err.Error(), "-request") {
		t.Errorf("error message %q should mention -request", err.Error())
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM eval_tasks`).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no rows inserted, got %d", count)
	}
}

func TestAdd_InvalidReferenceJSON(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := &config.Config{RepoPath: t.TempDir()}

	requestJSON, err := json.Marshal(userTextRequest("fix a bug"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	referenceJSON := []byte("not valid json")

	_, err = Add(db, cfg, "abc123", "fix a bug", requestJSON, referenceJSON)
	if err == nil {
		t.Fatal("expected error for invalid reference JSON")
	}
	if !strings.Contains(err.Error(), "-reference") {
		t.Errorf("error message %q should mention -reference", err.Error())
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM eval_tasks`).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no rows inserted, got %d", count)
	}
}

func TestAdd_NoReference(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := &config.Config{RepoPath: t.TempDir()}

	requestJSON, err := json.Marshal(userTextRequest("fix a bug"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	brief := "Fix the login bug"

	id, err := Add(db, cfg, "abc123", brief, requestJSON, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	task := manualTaskByID(t, db, id)

	if task.Origin != OriginManual {
		t.Errorf("origin = %q, want %q", task.Origin, OriginManual)
	}
	if task.Brief != brief {
		t.Errorf("brief = %q, want %q", task.Brief, brief)
	}
	if task.TurnType.Valid {
		t.Errorf("TurnType.Valid should be false when no files touched")
	}
	if task.Language.Valid {
		t.Errorf("Language.Valid should be false when no files touched")
	}
	if len(task.ReferenceResponseZstd) > 0 {
		t.Errorf("ReferenceResponseZstd should be empty, got %d bytes", len(task.ReferenceResponseZstd))
	}

	decompressed, err := store.Decompress(task.RequestZstd)
	if err != nil {
		t.Fatalf("decompress request: %v", err)
	}
	if string(decompressed) != string(requestJSON) {
		t.Errorf("stored request mismatch")
	}
}

func TestAdd_SingleFileReferenceSetsTurnTypeAndLanguage(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := &config.Config{RepoPath: t.TempDir()}

	requestJSON, err := json.Marshal(userTextRequest("fix a bug"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	refPath := filepath.Join(cfg.RepoPath, "internal/foo.go")
	referenceJSON := editResponseJSON(t, refPath, "old", "new")

	id, err := Add(db, cfg, "abc123", "fix a bug", requestJSON, referenceJSON)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	task := manualTaskByID(t, db, id)

	if !task.TurnType.Valid || task.TurnType.String != "single_file_edit" {
		t.Errorf("TurnType = %q (valid=%v), want 'single_file_edit' (valid=true)", task.TurnType.String, task.TurnType.Valid)
	}
	if !task.Language.Valid || task.Language.String != "go" {
		t.Errorf("Language = %q (valid=%v), want 'go' (valid=true)", task.Language.String, task.Language.Valid)
	}
	if len(task.ReferenceResponseZstd) == 0 {
		t.Errorf("ReferenceResponseZstd should be non-empty")
	}
}

func TestAdd_MultiFileReferenceIsMixedLanguage(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := &config.Config{RepoPath: t.TempDir()}

	requestJSON, err := json.Marshal(userTextRequest("fix a bug"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	goPath := filepath.Join(cfg.RepoPath, "internal/foo.go")
	phpPath := filepath.Join(cfg.RepoPath, "src/bar.php")

	editGoInput, err := json.Marshal(struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}{goPath, "old", "new"})
	if err != nil {
		t.Fatalf("marshal go edit input: %v", err)
	}

	editPhpInput, err := json.Marshal(struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}{phpPath, "old", "new"})
	if err != nil {
		t.Fatalf("marshal php edit input: %v", err)
	}

	msg := struct {
		Content []anthropic.ContentBlock `json:"content"`
	}{
		Content: []anthropic.ContentBlock{
			{Type: anthropic.BlockToolUse, ID: "tu1", Name: "Edit", Input: editGoInput},
			{Type: anthropic.BlockToolUse, ID: "tu2", Name: "Edit", Input: editPhpInput},
		},
	}
	referenceJSON, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal reference: %v", err)
	}

	id, err := Add(db, cfg, "abc123", "fix a bug", requestJSON, referenceJSON)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	task := manualTaskByID(t, db, id)

	if !task.TurnType.Valid || task.TurnType.String != "multi_file_edit" {
		t.Errorf("TurnType = %q (valid=%v), want 'multi_file_edit' (valid=true)", task.TurnType.String, task.TurnType.Valid)
	}
	if !task.Language.Valid || task.Language.String != "mixed" {
		t.Errorf("Language = %q (valid=%v), want 'mixed' (valid=true)", task.Language.String, task.Language.Valid)
	}
}

func TestAdd_EmptyCommitLeavesRepoHeadNull(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := &config.Config{RepoPath: t.TempDir()}

	requestJSON, err := json.Marshal(userTextRequest("fix a bug"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	tests := []struct {
		name       string
		commit     string
		wantValid  bool
		wantString string
	}{
		{"empty commit", "", false, ""},
		{"non-empty commit", "abc123def456", true, "abc123def456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Add(db, cfg, tt.commit, "fix a bug", requestJSON, nil)
			if err != nil {
				t.Fatalf("Add: %v", err)
			}

			task := manualTaskByID(t, db, id)

			if task.RepoHead.Valid != tt.wantValid {
				t.Errorf("RepoHead.Valid = %v, want %v", task.RepoHead.Valid, tt.wantValid)
			}
			if task.RepoHead.String != tt.wantString {
				t.Errorf("RepoHead.String = %q, want %q", task.RepoHead.String, tt.wantString)
			}
		})
	}
}

func TestAdd_CharacteristicsRecordManualProvenance(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := &config.Config{RepoPath: t.TempDir()}

	requestJSON, err := json.Marshal(userTextRequest("fix a bug"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	refPath := filepath.Join(cfg.RepoPath, "internal/foo.go")
	referenceJSON := editResponseJSON(t, refPath, "old", "new")

	id, err := Add(db, cfg, "abc123", "fix a bug", requestJSON, referenceJSON)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	task := manualTaskByID(t, db, id)

	c := ParseCharacteristics(task.Characteristics.String)
	if c.Localization != LocalizationGiven {
		t.Errorf("Localization = %q, want %q", c.Localization, LocalizationGiven)
	}
	if c.BriefSource != BriefSourceManual {
		t.Errorf("BriefSource = %q, want %q", c.BriefSource, BriefSourceManual)
	}
}

func TestAdd_InsertFailureIsReported(t *testing.T) {
	db := openHarvestTestDB(t)
	cfg := &config.Config{RepoPath: t.TempDir()}

	requestJSON, err := json.Marshal(userTextRequest("fix a bug"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	refPath := filepath.Join(cfg.RepoPath, "internal/foo.go")
	referenceJSON := editResponseJSON(t, refPath, "old", "new")

	db.Close()

	id, err := Add(db, cfg, "abc123", "fix a bug", requestJSON, referenceJSON)
	if err == nil {
		t.Fatal("expected error for closed database")
	}
	if id != 0 {
		t.Errorf("id = %d, want 0", id)
	}
	if !strings.Contains(err.Error(), "inserting manual eval task") {
		t.Errorf("error message %q should mention 'inserting manual eval task'", err.Error())
	}
}
