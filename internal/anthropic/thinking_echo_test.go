package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// display:omitted thinking blocks arrive with an empty thinking string; the
// echoed turn must still carry the "thinking" key or the API rejects it.
func TestThinkingBlockEchoKeepsEmptyThinkingKey(t *testing.T) {
	var b ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"thinking","thinking":"","signature":"c2ln"}`), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"thinking":""`) {
		t.Fatalf("empty thinking key dropped: %s", out)
	}
	if !strings.Contains(string(out), `"signature":"c2ln"`) {
		t.Fatalf("signature lost: %s", out)
	}

	var b2 ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"thinking","thinking":"real reasoning","signature":"x"}`), &b2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out2, _ := json.Marshal(b2)
	if !strings.Contains(string(out2), `"thinking":"real reasoning"`) {
		t.Fatalf("non-empty thinking mangled: %s", out2)
	}
}
