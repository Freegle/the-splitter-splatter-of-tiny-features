package evals

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/store"
)

// Add inserts one manually-entered eval task (origin='manual'):
// `eval add -commit <sha> -brief "..." -request <file.json> [-reference
// <file.json>]`. requestJSON must decode as an Anthropic MessagesRequest;
// referenceJSON, when non-empty, must decode as a complete Anthropic
// message (only its content array is read). Characteristics are derived
// from referenceJSON's touched files exactly as harvest does, with
// localization=given (the operator supplies the request directly) and
// brief_source=manual.
func Add(db *sql.DB, cfg *config.Config, commit, brief string, requestJSON, referenceJSON []byte) (int64, error) {
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return 0, fmt.Errorf("decoding -request JSON: %w", err)
	}

	var respBlocks []anthropic.ContentBlock
	if len(referenceJSON) > 0 {
		var msg struct {
			Content []anthropic.ContentBlock `json:"content"`
		}
		if err := json.Unmarshal(referenceJSON, &msg); err != nil {
			return 0, fmt.Errorf("decoding -reference JSON: %w", err)
		}
		respBlocks = msg.Content
	}

	filesTouched := feature.FilesTouched(respBlocks, cfg.RepoPath)
	turnType := ""
	switch {
	case len(filesTouched) >= 2:
		turnType = feature.TurnMultiFileEdit
	case len(filesTouched) == 1:
		turnType = feature.TurnSingleFileEdit
	}
	subsystem := feature.Subsystem(filesTouched)
	language := Language(filesTouched)
	layer := Layer(filesTouched, cfg.Layers)
	nature, natureEvidence := Nature(brief, filesTouched, cfg.Layers)
	specClarity, specEvidence := SpecClarity(brief, filesTouched)
	framework := Framework(filesTouched, language, "")

	characteristics := Characteristics{
		Framework:    framework,
		SpecClarity:  specClarity,
		Size:         Size{Files: len(filesTouched), ContextBytes: len(requestJSON)},
		TaskDate:     time.Now().UTC().Format(time.RFC3339),
		Localization: LocalizationGiven,
		BriefSource:  BriefSourceManual,
		Evidence:     map[string]string{"spec_clarity": specEvidence},
	}
	if natureEvidence != "" {
		characteristics.Evidence["nature"] = natureEvidence
	}

	reqCompressed, err := store.Compress(requestJSON)
	if err != nil {
		return 0, fmt.Errorf("compressing request: %w", err)
	}
	var refCompressed []byte
	if len(referenceJSON) > 0 {
		refCompressed, err = store.Compress(referenceJSON)
		if err != nil {
			return 0, fmt.Errorf("compressing reference: %w", err)
		}
	}

	row := store.EvalTaskRow{
		CreatedTS:             time.Now().UTC().Format(time.RFC3339),
		RepoHead:              sql.NullString{String: commit, Valid: commit != ""},
		Brief:                 brief,
		TurnType:              sql.NullString{String: turnType, Valid: turnType != ""},
		Subsystem:             sql.NullString{String: subsystem, Valid: subsystem != ""},
		RequestZstd:           reqCompressed,
		ReferenceResponseZstd: refCompressed,
		Origin:                OriginManual,
		Language:              sql.NullString{String: language, Valid: language != ""},
		Layer:                 sql.NullString{String: layer, Valid: layer != ""},
		Nature:                sql.NullString{String: nature, Valid: nature != ""},
		Characteristics:       sql.NullString{String: characteristics.JSON(), Valid: true},
	}

	id, _, err := store.InsertEvalTask(db, row)
	if err != nil {
		return 0, fmt.Errorf("inserting manual eval task: %w", err)
	}
	if id == 0 {
		// A manual task's call_id is always NULL, so InsertEvalTask's
		// ON CONFLICT(call_id, origin) DO NOTHING never fires for it
		// (SQLite treats every NULL as distinct); id == 0 here would only
		// mean the insert itself silently affected no row, which
		// InsertEvalTask's own RowsAffected check already turns into a
		// non-nil error above. Defensive only.
		return 0, fmt.Errorf("inserting manual eval task: no row was created")
	}
	return id, nil
}
