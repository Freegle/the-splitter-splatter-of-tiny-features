package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

func TestRouteEnabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"on", true},
		{"off", false},
		{"", false},
		{"ON", false},
		{"true", false},
	}
	for _, tt := range tests {
		t.Run("SPLITTER_ROUTE="+tt.value, func(t *testing.T) {
			t.Setenv("SPLITTER_ROUTE", tt.value)
			if got := RouteEnabled(); got != tt.want {
				t.Errorf("RouteEnabled() with SPLITTER_ROUTE=%q = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestRouteEnabled_Unset(t *testing.T) {
	os.Unsetenv("SPLITTER_ROUTE")
	if RouteEnabled() {
		t.Error("RouteEnabled() with SPLITTER_ROUTE unset = true, want false")
	}
}

func TestLiveRouter_RefreshSnapshotAndLookup(t *testing.T) {
	db := openTestDB(t)
	lr := NewLiveRouter(db, testUpdateConfig())

	if _, ok := lr.Lookup("single_file_edit|iznik-server-go", "claude-sonnet>qwen-coder:7b"); ok {
		t.Fatal("Lookup found an entry before any refresh")
	}

	if err := store.UpsertRouterState(db, store.RouterStateRow{
		Category: "single_file_edit|iznik-server-go",
		Families: "claude-sonnet>qwen-coder:7b",
		N:        50, Agreed: 48, WilsonLB: 0.91, Routable: true,
		UpdatedTS: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertRouterState: %v", err)
	}

	if err := lr.RefreshSnapshot(); err != nil {
		t.Fatalf("RefreshSnapshot: %v", err)
	}

	entry, ok := lr.Lookup("single_file_edit|iznik-server-go", "claude-sonnet>qwen-coder:7b")
	if !ok {
		t.Fatal("Lookup did not find the entry after RefreshSnapshot")
	}
	if !entry.Routable || entry.N != 50 || entry.Agreed != 48 {
		t.Errorf("entry = %+v, want Routable=true N=50 Agreed=48", entry)
	}
}

func TestLiveRouter_DisableCategory_PersistsAndUpdatesSnapshotImmediately(t *testing.T) {
	db := openTestDB(t)
	lr := NewLiveRouter(db, testUpdateConfig())

	if err := store.UpsertRouterState(db, store.RouterStateRow{
		Category: "tool_result_summary|iznik-server-go",
		Families: "claude-sonnet>qwen-coder:7b",
		N:        50, Agreed: 48, WilsonLB: 0.91, Routable: true,
		UpdatedTS: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertRouterState: %v", err)
	}
	if err := lr.RefreshSnapshot(); err != nil {
		t.Fatalf("RefreshSnapshot: %v", err)
	}

	if err := lr.DisableCategory("tool_result_summary|iznik-server-go", "claude-sonnet>qwen-coder:7b", "escalation"); err != nil {
		t.Fatalf("DisableCategory: %v", err)
	}

	// Visible immediately, with no further RefreshSnapshot call.
	entry, ok := lr.Lookup("tool_result_summary|iznik-server-go", "claude-sonnet>qwen-coder:7b")
	if !ok {
		t.Fatal("entry missing from snapshot after DisableCategory")
	}
	if entry.Routable {
		t.Error("Routable = true after DisableCategory, want false")
	}
	if entry.DisabledReason != "escalation" {
		t.Errorf("DisabledReason = %q, want escalation", entry.DisabledReason)
	}

	// And persisted to the store.
	persisted, err := store.AllRouterState(db)
	if err != nil {
		t.Fatalf("AllRouterState: %v", err)
	}
	if len(persisted) != 1 || persisted[0].Routable || persisted[0].DisabledReason != "escalation" {
		t.Errorf("persisted state = %+v, want disabled", persisted)
	}
}

func TestLiveRouter_SessionBreaker(t *testing.T) {
	lr := NewLiveRouter(openTestDB(t), testUpdateConfig())

	if lr.SessionBroken("s1") {
		t.Fatal("SessionBroken(s1) = true before TripBreaker")
	}
	lr.TripBreaker("s1")
	if !lr.SessionBroken("s1") {
		t.Error("SessionBroken(s1) = false after TripBreaker")
	}
	if lr.SessionBroken("s2") {
		t.Error("SessionBroken(s2) = true, want false (breaker is per session)")
	}
}

func TestLiveRouter_PendingServe_TakeOnce(t *testing.T) {
	lr := NewLiveRouter(openTestDB(t), testUpdateConfig())

	if _, ok := lr.TakePending("s1"); ok {
		t.Fatal("TakePending found a pending entry before RecordServedLocally")
	}

	lr.RecordServedLocally("s1", "single_file_edit|iznik-server-go", "claude-sonnet>qwen-coder:7b", []string{"iznik-server-go/foo.go"})

	p, ok := lr.TakePending("s1")
	if !ok {
		t.Fatal("TakePending did not find the pending entry")
	}
	if p.Category != "single_file_edit|iznik-server-go" || len(p.FilesTouched) != 1 {
		t.Errorf("pending = %+v, unexpected", p)
	}

	if _, ok := lr.TakePending("s1"); ok {
		t.Error("TakePending returned an entry a second time, want it cleared after the first take")
	}
}

func TestLiveRouter_NextOrdinal_Increments(t *testing.T) {
	lr := NewLiveRouter(openTestDB(t), testUpdateConfig())
	first := lr.NextOrdinal()
	second := lr.NextOrdinal()
	third := lr.NextOrdinal()
	if !(first == 0 && second == 1 && third == 2) {
		t.Errorf("ordinals = %d, %d, %d, want 0, 1, 2", first, second, third)
	}
}

func TestLiveRouter_LogDecisionAndUpdateStats(t *testing.T) {
	db := openTestDB(t)
	lr := NewLiveRouter(db, testUpdateConfig())

	id, err := lr.LogDecision("sess-1", nil, "single_file_edit|iznik-server-go", DecisionLocal, map[string]any{
		"n": 50, "agreed": 48, "wilson_lb": 0.91,
	})
	if err != nil {
		t.Fatalf("LogDecision: %v", err)
	}

	rows, err := store.RouterDecisionsSince(db, "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("RouterDecisionsSince: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Decision != DecisionLocal || rows[0].SessionID.String != "sess-1" {
		t.Errorf("row = %+v, unexpected", rows[0])
	}
	var stats map[string]any
	if err := json.Unmarshal([]byte(rows[0].Stats.String), &stats); err != nil {
		t.Fatalf("stats not valid JSON: %v", err)
	}
	if stats["n"].(float64) != 50 {
		t.Errorf("stats[n] = %v, want 50", stats["n"])
	}

	if err := lr.UpdateDecisionStats(id, map[string]any{"n": 50, "shadow_agree": true}); err != nil {
		t.Fatalf("UpdateDecisionStats: %v", err)
	}
	rows, err = store.RouterDecisionsSince(db, "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("RouterDecisionsSince: %v", err)
	}
	if err := json.Unmarshal([]byte(rows[0].Stats.String), &stats); err != nil {
		t.Fatalf("updated stats not valid JSON: %v", err)
	}
	if stats["shadow_agree"] != true {
		t.Errorf("stats[shadow_agree] = %v, want true", stats["shadow_agree"])
	}
}

// fakeOpenAIBackend returns an httptest server speaking the OpenAI
// chat-completions shape internal/backend.Client expects, replying with a
// fixed assistant text message.
func fakeOpenAIBackend(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
}

func liveRouterConfigForBackend(url string) *config.Config {
	cfg := testUpdateConfig()
	cfg.Replay.Backend = "testbackend"
	cfg.Backends = map[string]config.BackendConfig{
		"testbackend": {BaseURL: url, Model: "test-model"},
	}
	return cfg
}

func TestLiveRouter_ServeLocal(t *testing.T) {
	upstream := fakeOpenAIBackend(t, "hello from local")
	defer upstream.Close()

	db := openTestDB(t)
	lr := NewLiveRouter(db, liveRouterConfigForBackend(upstream.URL))

	req := anthropic.MessagesRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 16,
		Messages:  []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hi"}}}},
	}
	msgJSON, blocks, inTok, outTok, err := lr.ServeLocal(context.Background(), req)
	if err != nil {
		t.Fatalf("ServeLocal: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != anthropic.BlockText || blocks[0].Text != "hello from local" {
		t.Errorf("blocks = %+v, want a single text block \"hello from local\"", blocks)
	}
	if inTok != 7 || outTok != 3 {
		t.Errorf("tokens = %d/%d, want 7/3", inTok, outTok)
	}
	if !json.Valid(msgJSON) {
		t.Errorf("msgJSON not valid JSON: %s", msgJSON)
	}
}

func TestLiveRouter_DispatchShadow_AgreeAndDisagree(t *testing.T) {
	tests := []struct {
		name        string
		localText   string
		frontierMsg []byte
		wantAgree   bool
	}{
		{"agree", "hello world", []byte(`{"content":[{"type":"text","text":"hello world"}]}`), true},
		{"disagree", "totally unrelated garbage output", []byte(`{"content":[{"type":"text","text":"hello world"}]}`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := fakeOpenAIBackend(t, tt.localText)
			defer upstream.Close()

			db := openTestDB(t)
			lr := NewLiveRouter(db, liveRouterConfigForBackend(upstream.URL))
			lr.ShadowDone = make(chan struct{}, 1)

			id, err := lr.LogDecision("sess-1", nil, "question_answer|", DecisionShadow, map[string]any{"n": 10})
			if err != nil {
				t.Fatalf("LogDecision: %v", err)
			}

			req := anthropic.MessagesRequest{
				Model: "claude-sonnet-4-6", MaxTokens: 16,
				Messages: []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hi"}}}},
			}
			lr.DispatchShadow(id, req, tt.frontierMsg, map[string]any{"n": 10})

			select {
			case <-lr.ShadowDone:
			case <-time.After(5 * time.Second):
				t.Fatal("DispatchShadow did not finish within 5s")
			}

			rows, err := store.RouterDecisionsSince(db, "2000-01-01T00:00:00Z")
			if err != nil {
				t.Fatalf("RouterDecisionsSince: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("len(rows) = %d, want 1", len(rows))
			}
			var stats map[string]any
			if err := json.Unmarshal([]byte(rows[0].Stats.String), &stats); err != nil {
				t.Fatalf("stats not valid JSON: %v", err)
			}
			if stats["n"].(float64) != 10 {
				t.Errorf("original stats field n = %v not preserved, want 10", stats["n"])
			}
			got, ok := stats["shadow_agree"].(bool)
			if !ok {
				t.Fatalf("stats[shadow_agree] missing or not bool: %+v", stats)
			}
			if got != tt.wantAgree {
				t.Errorf("shadow_agree = %v, want %v", got, tt.wantAgree)
			}
		})
	}
}
