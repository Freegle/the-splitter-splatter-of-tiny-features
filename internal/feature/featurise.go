package feature

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/store"
)

// responseMessage is the subset of a captured response's JSON this package
// reads: the content blocks that drive turn_type classification and
// files_touched extraction. Both a non-streaming response body and an
// AssembleSSE result share this shape.
type responseMessage struct {
	Content []anthropic.ContentBlock `json:"content"`
}

// Run processes calls needing features into the features table: calls with
// no features row yet, plus existing rows whose had_error_followup is still
// unknown because the next call in their session had not arrived the last
// time featurise ran. When refresh is true, every call with a captured
// response is reprocessed instead. Each call is written via
// store.UpsertFeature (INSERT ... ON CONFLICT(call_id) DO UPDATE), so
// running Run again over the same calls updates rows rather than
// duplicating them. It returns the number of features rows written; a
// per-call error is logged and skipped rather than aborting the whole run.
func Run(db *sql.DB, repoPath string, refresh bool) (int, error) {
	callIDs, err := callIDsToProcess(db, refresh)
	if err != nil {
		return 0, err
	}

	processed := 0
	for _, id := range callIDs {
		ok, err := processCall(db, repoPath, id)
		if err != nil {
			log.Printf("splitter: feature: call %d: %v", id, err)
			continue
		}
		if ok {
			processed++
		}
	}
	return processed, nil
}

// callIDsToProcess returns the call ids Run should featurise, oldest first
// and with no duplicates.
func callIDsToProcess(db *sql.DB, refresh bool) ([]int64, error) {
	if refresh {
		ids, err := store.AllCallIDsWithResponse(db)
		if err != nil {
			return nil, fmt.Errorf("listing all calls: %w", err)
		}
		return ids, nil
	}

	missing, err := store.CallIDsMissingFeatures(db)
	if err != nil {
		return nil, fmt.Errorf("listing calls missing features: %w", err)
	}
	pending, err := store.CallIDsWithNullFollowup(db)
	if err != nil {
		return nil, fmt.Errorf("listing calls with unresolved error followup: %w", err)
	}
	return mergeSortedUnique(missing, pending), nil
}

// mergeSortedUnique merges two ascending, duplicate-free int64 slices into
// one ascending, duplicate-free slice.
func mergeSortedUnique(a, b []int64) []int64 {
	out := make([]int64, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		default:
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// processCall featurises one call and upserts its features row. It returns
// false without error for a call with no captured response yet: there is
// nothing to featurise.
func processCall(db *sql.DB, repoPath string, callID int64) (bool, error) {
	call, err := store.GetCall(db, callID)
	if err != nil {
		return false, fmt.Errorf("getting call: %w", err)
	}
	if len(call.ResponseZstd) == 0 {
		return false, nil
	}

	req, err := decodeRequest(call.RequestZstd)
	if err != nil {
		return false, fmt.Errorf("decoding request: %w", err)
	}
	respBlocks, err := decodeResponseBlocks(call.ResponseZstd)
	if err != nil {
		return false, fmt.Errorf("decoding response: %w", err)
	}

	turnType := ClassifyTurnType(req, respBlocks)
	filesTouched := FilesTouched(respBlocks, repoPath)
	subsystem := Subsystem(filesTouched)

	filesJSON, err := json.Marshal(filesTouched)
	if err != nil {
		return false, fmt.Errorf("marshaling files_touched: %w", err)
	}
	if filesTouched == nil {
		filesJSON = []byte("[]")
	}

	hadErrorFollowup, err := lookupErrorFollowup(db, call, filesTouched, repoPath)
	if err != nil {
		return false, fmt.Errorf("determining error followup: %w", err)
	}

	row := store.FeatureRow{
		CallID:           call.ID,
		TurnType:         turnType,
		FilesTouched:     string(filesJSON),
		Subsystem:        sql.NullString{String: subsystem, Valid: true},
		ContextTokens:    call.InputTokens,
		OutputTokens:     call.OutputTokens,
		HadErrorFollowup: hadErrorFollowup,
	}
	if err := store.UpsertFeature(db, row); err != nil {
		return false, fmt.Errorf("upserting features: %w", err)
	}
	return true, nil
}

// decodeRequest decompresses and decodes a call's request, returning a zero
// value when compressed is empty rather than treating it as an error.
func decodeRequest(compressed []byte) (anthropic.MessagesRequest, error) {
	var req anthropic.MessagesRequest
	if len(compressed) == 0 {
		return req, nil
	}
	raw, err := store.Decompress(compressed)
	if err != nil {
		return req, fmt.Errorf("decompressing: %w", err)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("unmarshaling: %w", err)
	}
	return req, nil
}

// decodeResponseBlocks decompresses and decodes a call's response into its
// content blocks.
func decodeResponseBlocks(compressed []byte) ([]anthropic.ContentBlock, error) {
	raw, err := store.Decompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}
	var msg responseMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("unmarshaling: %w", err)
	}
	return msg.Content, nil
}

// lookupErrorFollowup determines had_error_followup for call: an invalid
// (NULL) sql.NullInt64 when call has no session id or no next call exists
// in that session yet, else 1 or 0 from HasErrorFollowup applied to that
// next call's request.
func lookupErrorFollowup(db *sql.DB, call *store.CallRow, filesTouched []string, repoPath string) (sql.NullInt64, error) {
	if !call.SessionID.Valid || call.SessionID.String == "" {
		return sql.NullInt64{}, nil
	}

	next, err := store.NextCallInSession(db, call.SessionID.String, call.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.NullInt64{}, nil
		}
		return sql.NullInt64{}, fmt.Errorf("getting next call in session: %w", err)
	}

	nextReq, err := decodeRequest(next.RequestZstd)
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("decoding next call's request: %w", err)
	}

	val := int64(0)
	if HasErrorFollowup(filesTouched, nextReq, repoPath) {
		val = 1
	}
	return sql.NullInt64{Int64: val, Valid: true}, nil
}
