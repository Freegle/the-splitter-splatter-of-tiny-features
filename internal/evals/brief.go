package evals

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/store"
)

// callFallbackBriefChars bounds the fallback brief taken from the task
// call's own last user text when no session chain exists.
const callFallbackBriefChars = 120

// DeriveBrief derives a live task's brief, per DESIGN.md "Brief
// derivation": walk back to the session's earliest call and take its last
// plain-text user block (the human's initiating instruction), falling back
// to the first callFallbackBriefChars characters of the task call's own
// last plain-text user block when sessionID is empty, no earlier call can
// be found or decoded, or the earliest call carries no plain-text user
// block at all. Any failure along the session path (a missing session, a
// decompression or decode error) falls through to the fallback rather than
// failing the whole harvest: brief derivation is best-effort.
func DeriveBrief(db *sql.DB, sessionID string, ownRequestJSON []byte) (brief, source string, err error) {
	if sessionID != "" {
		if text, ok := sessionInitiatingText(db, sessionID); ok {
			return text, BriefSourceSession, nil
		}
	}

	var req anthropic.MessagesRequest
	if err := json.Unmarshal(ownRequestJSON, &req); err != nil {
		return "", "", fmt.Errorf("decoding call's own request for brief fallback: %w", err)
	}
	text, _ := lastPlainTextUserBlock(req)
	text = strings.TrimSpace(text)
	if text == "" {
		text = "(no plain-text user message found in this call)"
	}
	if r := []rune(text); len(r) > callFallbackBriefChars {
		text = string(r[:callFallbackBriefChars])
	}
	return text, BriefSourceCall, nil
}

// sessionInitiatingText attempts the session walk-back: the earliest call
// in sessionID, decompressed and decoded, with its last plain-text user
// block returned. ok is false on any failure (no such session, decompress
// or decode error, no plain-text user block), never an error: the caller
// treats this purely as "did the session path work".
func sessionInitiatingText(db *sql.DB, sessionID string) (string, bool) {
	earliest, err := store.EarliestCallInSession(db, sessionID)
	if err != nil {
		return "", false
	}
	reqJSON, err := store.Decompress(earliest.RequestZstd)
	if err != nil {
		return "", false
	}
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return "", false
	}
	text, ok := lastPlainTextUserBlock(req)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// lastPlainTextUserBlock returns the text of the last message in
// req.Messages that has role "user" and is not entirely tool_result blocks
// (a plain-text user turn, as opposed to a tool result being handed back).
func lastPlainTextUserBlock(req anthropic.MessagesRequest) (string, bool) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" || messageIsToolResultOnly(m) {
			continue
		}
		if text := concatTextBlocks(m); text != "" {
			return text, true
		}
	}
	return "", false
}

// messageIsToolResultOnly reports whether m has at least one content block
// and every block is a tool_result: a pure tool-result turn carries no
// human-authored text.
func messageIsToolResultOnly(m anthropic.Message) bool {
	if len(m.Content) == 0 {
		return false
	}
	for _, b := range m.Content {
		if b.Type != anthropic.BlockToolResult {
			return false
		}
	}
	return true
}

// concatTextBlocks joins m's text blocks with newlines, skipping any other
// block type.
func concatTextBlocks(m anthropic.Message) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == anthropic.BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
