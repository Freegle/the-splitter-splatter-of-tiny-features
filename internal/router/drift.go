package router

import (
	"encoding/json"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// jaccardAgreeThreshold is the token-set similarity above which
// roughAgree treats two shadow-compared responses as agreeing. This is a
// cheap, self-contained drift signal for the live proxy path, not the
// Phase 3 verification cascade (internal/verify): that cascade needs
// ephemeral worktrees, lint and tests, far too heavy to run synchronously
// inside a request-serving goroutine even in the background, and its
// exactness is not needed here, only a rough "did the two answers land in
// the same place" signal for the weekly drift report.
const jaccardAgreeThreshold = 0.6

// roughAgree reports a cheap best-effort agreement signal between two
// complete Anthropic message JSON payloads: exact match on normalised
// (whitespace-collapsed) concatenated text once decoded, else a token-set
// Jaccard similarity at or above jaccardAgreeThreshold. Undecodable input
// on either side never agrees.
func roughAgree(a, b []byte) bool {
	ta, errA := messageText(a)
	tb, errB := messageText(b)
	if errA != nil || errB != nil {
		return false
	}
	if ta == tb {
		return true
	}
	return jaccardSimilarity(ta, tb) >= jaccardAgreeThreshold
}

// messageText decodes a complete Anthropic message JSON's content blocks
// and returns their concatenated text (text blocks verbatim, tool_use
// blocks as "name input-json"), whitespace-normalised.
func messageText(msgJSON []byte) (string, error) {
	var msg struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(msgJSON, &msg); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range msg.Content {
		switch b.Type {
		case anthropic.BlockText:
			sb.WriteString(b.Text)
			sb.WriteString(" ")
		case anthropic.BlockToolUse:
			sb.WriteString(b.Name)
			sb.WriteString(" ")
			sb.Write(b.Input)
			sb.WriteString(" ")
		}
	}
	return strings.Join(strings.Fields(sb.String()), " "), nil
}

// jaccardSimilarity returns the Jaccard similarity (intersection over
// union) of a and b's whitespace-separated token sets. Two empty token
// sets are treated as identical (similarity 1).
func jaccardSimilarity(a, b string) float64 {
	setA := tokenSet(a)
	setB := tokenSet(b)
	if len(setA) == 0 && len(setB) == 0 {
		return 1
	}

	intersection := 0
	for tok := range setA {
		if setB[tok] {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 1
	}
	return float64(intersection) / float64(union)
}

func tokenSet(s string) map[string]bool {
	fields := strings.Fields(s)
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	return set
}
