package evals

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/store"
)

// HarvestSummary reports what one eval harvest run did.
type HarvestSummary struct {
	Disagreements  int
	Escalations    int
	ErrorFollowups int
	CleanSampled   int
	Inserted       int
	Deduped        int
}

// harvestSource pairs one origin with the store query that fetches its
// candidate calls.
type harvestSource struct {
	origin string
	fetch  func(db *sql.DB) ([]store.HarvestSourceRow, error)
}

// harvestSources are the three signal-based origins DESIGN.md always
// harvests. -include-clean's sampling is handled separately since it takes
// a limit argument the others do not.
var harvestSources = []harvestSource{
	{OriginDisagreement, func(db *sql.DB) ([]store.HarvestSourceRow, error) { return store.HarvestDisagreements(db) }},
	{OriginEscalation, func(db *sql.DB) ([]store.HarvestSourceRow, error) { return store.HarvestEscalations(db) }},
	{OriginErrorFollowup, func(db *sql.DB) ([]store.HarvestSourceRow, error) { return store.HarvestErrorFollowups(db) }},
}

// Harvest creates eval_tasks from live capture: disagreements, escalations
// and frontier error-followups always, plus up to includeClean sampled
// clean (origin='clean') tasks when includeClean > 0. Every task gets a
// full characteristics profile and an auto-generated brief. Re-running
// Harvest is idempotent: a call already harvested under a given origin is
// silently skipped (DESIGN.md's (call_id, origin) dedup key), counted as
// Deduped rather than Inserted.
func Harvest(db *sql.DB, cfg *config.Config, includeClean int) (*HarvestSummary, error) {
	summary := &HarvestSummary{}

	for _, src := range harvestSources {
		rows, err := src.fetch(db)
		if err != nil {
			return nil, fmt.Errorf("fetching %s harvest candidates: %w", src.origin, err)
		}
		for _, r := range rows {
			inserted, err := harvestOneTask(db, cfg, r, src.origin)
			if err != nil {
				return nil, fmt.Errorf("harvesting call %d as %s: %w", r.CallID, src.origin, err)
			}
			switch src.origin {
			case OriginDisagreement:
				summary.Disagreements++
			case OriginEscalation:
				summary.Escalations++
			case OriginErrorFollowup:
				summary.ErrorFollowups++
			}
			tally(summary, inserted)
		}
	}

	if includeClean > 0 {
		rows, err := store.HarvestCleanCandidates(db, includeClean)
		if err != nil {
			return nil, fmt.Errorf("fetching clean harvest candidates: %w", err)
		}
		for _, r := range rows {
			inserted, err := harvestOneTask(db, cfg, r, OriginClean)
			if err != nil {
				return nil, fmt.Errorf("harvesting call %d as clean: %w", r.CallID, err)
			}
			if inserted {
				summary.CleanSampled++
			}
			tally(summary, inserted)
		}
	}

	return summary, nil
}

func tally(s *HarvestSummary, inserted bool) {
	if inserted {
		s.Inserted++
	} else {
		s.Deduped++
	}
}

// harvestOneTask builds and inserts one eval_tasks row from a harvest
// candidate call, returning whether a new row was actually inserted (false
// means the (call_id, origin) dedup key already existed).
func harvestOneTask(db *sql.DB, cfg *config.Config, r store.HarvestSourceRow, origin string) (bool, error) {
	reqJSON, err := store.Decompress(r.RequestZstd)
	if err != nil {
		return false, fmt.Errorf("decompressing request for call %d: %w", r.CallID, err)
	}
	respJSON, err := store.Decompress(r.ResponseZstd)
	if err != nil {
		return false, fmt.Errorf("decompressing response for call %d: %w", r.CallID, err)
	}

	var respMsg struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(respJSON, &respMsg); err != nil {
		return false, fmt.Errorf("decoding response for call %d: %w", r.CallID, err)
	}

	filesTouched := feature.FilesTouched(respMsg.Content, cfg.RepoPath)

	brief, briefSource, err := DeriveBrief(db, r.SessionID.String, reqJSON)
	if err != nil {
		return false, fmt.Errorf("deriving brief for call %d: %w", r.CallID, err)
	}

	language := Language(filesTouched)
	layer := Layer(filesTouched, cfg.Layers)
	// Live captured calls have no commit subject; the derived brief is the
	// closest mechanical stand-in for "what this task is about" (see
	// DECISIONS.md).
	nature, natureEvidence := Nature(brief, filesTouched, cfg.Layers)
	difficulty, difficultyEvidence := harvestDifficulty(origin, r.HadErrorFollowup)
	specClarity, specEvidence := SpecClarity(brief, filesTouched)
	framework := Framework(filesTouched, language, "")

	characteristics := Characteristics{
		Framework:    framework,
		SpecClarity:  specClarity,
		Size:         Size{Files: len(filesTouched), DiffLines: estimateDiffLines(respMsg.Content), ContextBytes: len(reqJSON)},
		TaskDate:     r.TS,
		Localization: LocalizationDiscovered,
		BriefSource:  briefSource,
		Evidence:     map[string]string{"spec_clarity": specEvidence},
	}
	if natureEvidence != "" {
		characteristics.Evidence["nature"] = natureEvidence
	}
	if difficultyEvidence != "" {
		characteristics.Evidence["difficulty"] = difficultyEvidence
	}

	row := store.EvalTaskRow{
		CreatedTS:             time.Now().UTC().Format(time.RFC3339),
		CallID:                sql.NullInt64{Int64: r.CallID, Valid: true},
		RepoHead:              r.RepoHead,
		Brief:                 brief,
		TurnType:              r.TurnType,
		Subsystem:             r.Subsystem,
		FrontierModel:         r.FrontierModel,
		RequestZstd:           r.RequestZstd,
		ReferenceResponseZstd: r.ResponseZstd,
		Origin:                origin,
		Language:              sql.NullString{String: language, Valid: language != ""},
		Layer:                 sql.NullString{String: layer, Valid: layer != ""},
		Nature:                sql.NullString{String: nature, Valid: nature != ""},
		Difficulty:            sql.NullString{String: difficulty, Valid: difficulty != ""},
		Characteristics:       sql.NullString{String: characteristics.JSON(), Valid: true},
	}

	_, inserted, err := store.InsertEvalTask(db, row)
	if err != nil {
		return false, fmt.Errorf("inserting eval task for call %d: %w", r.CallID, err)
	}
	return inserted, nil
}

// harvestDifficulty derives a harvested task's difficulty label from its
// origin, per DESIGN.md's eval harvest rules.
func harvestDifficulty(origin string, hadErrorFollowup sql.NullInt64) (difficulty, evidence string) {
	switch origin {
	case OriginDisagreement:
		if hadErrorFollowup.Valid && hadErrorFollowup.Int64 == 1 {
			return DifficultyChallenging, "local model disagreed with the frontier, and the frontier's own response had an error followup"
		}
		return "", ""
	case OriginEscalation:
		return DifficultyChallenging, "router escalated this session away from local routing after this call"
	case OriginErrorFollowup:
		return DifficultyChallenging, "the frontier's own response had an error followup in the next call"
	case OriginClean:
		return DifficultySimple, "single_file_edit with no error followup and no disagreeing verification"
	default:
		return "", ""
	}
}

// estimateDiffLines gives a coarse line-count size signal for a harvested
// (live) task, which has no real git diff to measure: the sum of old/new
// line counts across every edit-family tool_use block in the frontier's
// response. Used only for the characteristics.size bucket, never for
// difficulty (DESIGN.md: size is non-monotonic and must never drive it).
func estimateDiffLines(blocks []anthropic.ContentBlock) int {
	total := 0
	for _, b := range blocks {
		if b.Type != anthropic.BlockToolUse {
			continue
		}
		switch b.Name {
		case "Edit":
			var in struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}
			if json.Unmarshal(b.Input, &in) == nil {
				total += countLines(in.OldString) + countLines(in.NewString)
			}
		case "MultiEdit":
			var in struct {
				Edits []struct {
					OldString string `json:"old_string"`
					NewString string `json:"new_string"`
				} `json:"edits"`
			}
			if json.Unmarshal(b.Input, &in) == nil {
				for _, e := range in.Edits {
					total += countLines(e.OldString) + countLines(e.NewString)
				}
			}
		case "Write":
			var in struct {
				Content string `json:"content"`
			}
			if json.Unmarshal(b.Input, &in) == nil {
				total += countLines(in.Content)
			}
		}
	}
	return total
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
