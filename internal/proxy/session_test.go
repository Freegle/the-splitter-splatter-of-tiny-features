package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

func TestDeriveSessionID_ExtractsSessionPatternFromUserID(t *testing.T) {
	req := &anthropic.MessagesRequest{
		Metadata: json.RawMessage(`{"user_id":"user_abc123_account_xyz_session_a1b2c3d4-e5f6-7890"}`),
	}
	got := deriveSessionID("some-agent/1.0", req)
	want := "session_a1b2c3d4-e5f6-7890"
	if got != want {
		t.Errorf("deriveSessionID() = %q, want %q", got, want)
	}
}

func TestDeriveSessionID_FallsBackToWholeUserID(t *testing.T) {
	req := &anthropic.MessagesRequest{
		Metadata: json.RawMessage(`{"user_id":"opaque-id-with-no-session-marker"}`),
	}
	got := deriveSessionID("some-agent/1.0", req)
	if got != "opaque-id-with-no-session-marker" {
		t.Errorf("deriveSessionID() = %q, want the whole user_id", got)
	}
}

func TestDeriveSessionID_FallsBackToHashWhenNoMetadata(t *testing.T) {
	req := &anthropic.MessagesRequest{
		System: json.RawMessage(`"You are a coding assistant."`),
	}
	got := deriveSessionID("claude-code/1.2.3", req)

	sum := sha256.Sum256([]byte("claude-code/1.2.3" + "You are a coding assistant."))
	want := hex.EncodeToString(sum[:])[:16]

	if got != want {
		t.Errorf("deriveSessionID() = %q, want %q", got, want)
	}
	if len(got) != 16 {
		t.Errorf("len(deriveSessionID()) = %d, want 16", len(got))
	}
}

func TestDeriveSessionID_HashFallbackIsStablePerInput(t *testing.T) {
	req := &anthropic.MessagesRequest{}
	a := deriveSessionID("agent-a", req)
	b := deriveSessionID("agent-a", req)
	c := deriveSessionID("agent-b", req)

	if a != b {
		t.Errorf("hash fallback not stable for identical input: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("hash fallback identical for different User-Agents: %q", a)
	}
}

func TestDeriveSessionID_NilRequest(t *testing.T) {
	got := deriveSessionID("agent", nil)
	if len(got) != 16 {
		t.Errorf("deriveSessionID(nil req) = %q, want a 16 char hash", got)
	}
}

func TestFirstSystemBlockText_StringShape(t *testing.T) {
	got := firstSystemBlockText(json.RawMessage(`"plain string system prompt"`))
	if got != "plain string system prompt" {
		t.Errorf("firstSystemBlockText() = %q", got)
	}
}

func TestFirstSystemBlockText_BlockArrayShape(t *testing.T) {
	got := firstSystemBlockText(json.RawMessage(`[{"type":"text","text":"first block"},{"type":"text","text":"second"}]`))
	if got != "first block" {
		t.Errorf("firstSystemBlockText() = %q, want %q", got, "first block")
	}
}

func TestFirstSystemBlockText_EmptyOrMalformed(t *testing.T) {
	if got := firstSystemBlockText(nil); got != "" {
		t.Errorf("firstSystemBlockText(nil) = %q, want empty", got)
	}
	if got := firstSystemBlockText(json.RawMessage(`[]`)); got != "" {
		t.Errorf("firstSystemBlockText(empty array) = %q, want empty", got)
	}
	if got := firstSystemBlockText(json.RawMessage(`not json`)); got != "" {
		t.Errorf("firstSystemBlockText(malformed) = %q, want empty", got)
	}
}
