// Adapted from seifghazi/claude-code-proxy (MIT), commit 02c9c766.
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"

	"github.com/freegle/splitter/internal/anthropic"
)

// sessionIDPattern extracts a session-shaped substring from an opaque
// metadata.user_id value, e.g. "session_a1b2c3d4-...".
var sessionIDPattern = regexp.MustCompile(`session[_-][0-9a-f-]{8,}`)

// sessionIDFunc computes the best-effort session id for a captured request.
// It is a package variable, not a plain function call, so tests can
// substitute a faulty implementation to exercise the proxy's panic
// recovery path; production code never reassigns it.
var sessionIDFunc = deriveSessionID

// deriveSessionID derives a best-effort session id for req, in order:
// request metadata.user_id (Claude Code sends one); a "session[_-]<hex>"
// substring extracted from it when present, else the whole user_id; else
// SHA256 of (User-Agent + first system block text) truncated to 16 hex
// characters. This is a heuristic, not a guaranteed-stable identifier:
// Claude Code's exact metadata shape is not a documented contract.
func deriveSessionID(userAgent string, req *anthropic.MessagesRequest) string {
	if req != nil && len(req.Metadata) > 0 {
		var meta struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(req.Metadata, &meta); err == nil && meta.UserID != "" {
			if m := sessionIDPattern.FindString(meta.UserID); m != "" {
				return m
			}
			return meta.UserID
		}
	}

	text := ""
	if req != nil {
		text = firstSystemBlockText(req.System)
	}
	sum := sha256.Sum256([]byte(userAgent + text))
	return hex.EncodeToString(sum[:])[:16]
}

// firstSystemBlockText extracts the text of the first system block from a
// MessagesRequest.System value, which is either a bare string or an array
// of content blocks on the wire. Returns "" for any other or empty shape.
func firstSystemBlockText(system json.RawMessage) string {
	if len(system) == 0 {
		return ""
	}
	if system[0] == '"' {
		var s string
		if err := json.Unmarshal(system, &s); err == nil {
			return s
		}
		return ""
	}
	var blocks []anthropic.ContentBlock
	if err := json.Unmarshal(system, &blocks); err == nil && len(blocks) > 0 {
		return blocks[0].Text
	}
	return ""
}
