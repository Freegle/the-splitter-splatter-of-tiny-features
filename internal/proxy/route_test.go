package proxy

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/router"
	"github.com/freegle/splitter/internal/store"
)

// routeTestConfig returns a config wired to backendURL as the default
// routing backend ("testbackend"), with the router thresholds relaxed so
// a single seeded router_state row is easy to control from the test.
func routeTestConfig(backendURL string) *config.Config {
	cfg := config.Default()
	cfg.Replay.Backend = "testbackend"
	cfg.Backends = map[string]config.BackendConfig{
		"testbackend": {BaseURL: backendURL, Model: "local-model"},
	}
	cfg.Families = map[string]string{}
	cfg.Router.MinN = 1
	cfg.Router.MinWilsonLB = 0
	cfg.Router.DualDispatchPct = 0
	return cfg
}

// seedRoutableCategory upserts a routable router_state row and refreshes
// lr's snapshot so Lookup sees it immediately.
func seedRoutableCategory(t *testing.T, db *sql.DB, lr *router.LiveRouter, category, families string) {
	t.Helper()
	if err := store.UpsertRouterState(db, store.RouterStateRow{
		Category: category, Families: families,
		N: 50, Agreed: 49, WilsonLB: 0.95, Routable: true,
		UpdatedTS: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seeding router_state: %v", err)
	}
	if err := lr.RefreshSnapshot(); err != nil {
		t.Fatalf("RefreshSnapshot: %v", err)
	}
}

// questionAnswerRequest is a plain request that internal/feature.RequestOnly
// classifies as turn_type "question_answer", subsystem "" (no assistant
// history), matching category "question_answer|".
func questionAnswerRequest(stream bool) []byte {
	req := map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 32,
		"stream":     stream,
		"messages": []map[string]any{
			{"role": "user", "content": "what does this do?"},
		},
	}
	b, _ := json.Marshal(req)
	return b
}

// countingBackend counts requests and answers every call with a fixed
// plain-text OpenAI chat completion response.
func countingBackend(t *testing.T, text string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	return srv, &calls
}

func countingUpstream(t *testing.T, respJSON string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(respJSON))
	}))
	return srv, &calls
}

func TestProxy_Routed_ServesLocally_NonStreaming(t *testing.T) {
	t.Setenv("SPLITTER_ROUTE", "on")

	localBackend, localCalls := countingBackend(t, "local answer")
	defer localBackend.Close()
	upstream, upstreamCalls := countingUpstream(t, `{"content":[{"type":"text","text":"should never be seen"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	defer upstream.Close()

	db, _ := openTestDB(t)
	cfg := routeTestConfig(localBackend.URL)
	lr := router.NewLiveRouter(db, cfg)

	families := router.FamilyPair("claude-sonnet-4-6", "local-model", cfg.Families)
	seedRoutableCategory(t, db, lr, "question_answer|", families)

	srv, err := New(Config{Upstream: upstream.URL, DB: db, Router: lr, FamilyOverrides: cfg.Families})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(questionAnswerRequest(false)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var decoded struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decoding response: %v; body=%s", err, body)
	}
	if len(decoded.Content) != 1 || decoded.Content[0].Text != "local answer" {
		t.Errorf("content = %+v, want a single text block \"local answer\"", decoded.Content)
	}

	if localCalls.Load() != 1 {
		t.Errorf("local backend called %d times, want 1", localCalls.Load())
	}
	if upstreamCalls.Load() != 0 {
		t.Errorf("upstream called %d times, want 0 (routed locally, must never touch frontier)", upstreamCalls.Load())
	}

	time.Sleep(50 * time.Millisecond)
	if _, err := store.GetCall(db, 1); err == nil {
		t.Error("a locally-served request produced a calls row, want none (see DECISIONS.md)")
	}

	decisions, err := store.RouterDecisionsSince(db, "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("RouterDecisionsSince: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Decision != router.DecisionLocal {
		t.Fatalf("decisions = %+v, want exactly one local decision", decisions)
	}
	if !decisions[0].Stats.Valid || decisions[0].Stats.String == "" {
		t.Error("local decision logged with no stats JSON")
	}
}

func TestProxy_Routed_ServesLocally_Streaming_SynthesizesSSE(t *testing.T) {
	t.Setenv("SPLITTER_ROUTE", "on")

	localBackend, _ := countingBackend(t, "streamed local answer")
	defer localBackend.Close()
	upstream, upstreamCalls := countingUpstream(t, `{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	defer upstream.Close()

	db, _ := openTestDB(t)
	cfg := routeTestConfig(localBackend.URL)
	lr := router.NewLiveRouter(db, cfg)
	families := router.FamilyPair("claude-sonnet-4-6", "local-model", cfg.Families)
	seedRoutableCategory(t, db, lr, "question_answer|", families)

	srv, err := New(Config{Upstream: upstream.URL, DB: db, Router: lr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(questionAnswerRequest(true)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	assembled, _, _, err := anthropic.AssembleSSE(body)
	if err != nil {
		t.Fatalf("AssembleSSE on synthesized stream: %v; body=%s", err, body)
	}
	var decoded struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(assembled, &decoded); err != nil {
		t.Fatalf("decoding assembled message: %v", err)
	}
	if len(decoded.Content) != 1 || decoded.Content[0].Text != "streamed local answer" {
		t.Errorf("content = %+v, want a single text block \"streamed local answer\"", decoded.Content)
	}
	if upstreamCalls.Load() != 0 {
		t.Errorf("upstream called %d times, want 0", upstreamCalls.Load())
	}
}

func TestProxy_KillSwitch_ForcesPassThrough(t *testing.T) {
	tests := []string{"off", "", "banana"}
	for _, val := range tests {
		t.Run("SPLITTER_ROUTE="+val, func(t *testing.T) {
			t.Setenv("SPLITTER_ROUTE", val)

			localBackend, localCalls := countingBackend(t, "local answer")
			defer localBackend.Close()
			upstream, upstreamCalls := countingUpstream(t, `{"content":[{"type":"text","text":"frontier answer"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			defer upstream.Close()

			db, _ := openTestDB(t)
			cfg := routeTestConfig(localBackend.URL)
			lr := router.NewLiveRouter(db, cfg)
			families := router.FamilyPair("claude-sonnet-4-6", "local-model", cfg.Families)
			seedRoutableCategory(t, db, lr, "question_answer|", families)

			srv, err := New(Config{Upstream: upstream.URL, DB: db, Router: lr})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			proxyTS := httptest.NewServer(srv)
			defer proxyTS.Close()

			resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(questionAnswerRequest(false)))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			if upstreamCalls.Load() != 1 {
				t.Errorf("upstream called %d times, want 1 (kill switch: pure pass-through)", upstreamCalls.Load())
			}
			if localCalls.Load() != 0 {
				t.Errorf("local backend called %d times, want 0", localCalls.Load())
			}
		})
	}
}

// editToolCallBackend answers with an OpenAI tool_calls response invoking
// Edit on filePath: internal/backend.FromOpenAI translates it into a
// tool_use content block, exactly the shape a genuinely broken local
// answer would have (a plausible-looking edit that turns out to be
// garbage once the client tries to apply it).
func editToolCallBackend(t *testing.T, filePath string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args, _ := json.Marshal(map[string]string{
			"file_path": filePath, "old_string": "a", "new_string": "b syntax error (((",
		})
		resp := map[string]any{
			"id": "chatcmpl-1",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "tc_1", "type": "function",
						"function": map[string]any{"name": "Edit", "arguments": string(args)},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 4},
		}
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func requestWithSession(sessionID string, messages []map[string]any) []byte {
	req := map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 32,
		"metadata":   map[string]any{"user_id": sessionID},
		"messages":   messages,
	}
	b, _ := json.Marshal(req)
	return b
}

func TestProxy_InjectedFault_EscalatesAndTripsSessionBreaker(t *testing.T) {
	t.Setenv("SPLITTER_ROUTE", "on")
	const sessionID = "session_deadbeef01"
	const touchedFile = "iznik-server-go/foo.go"

	localBackend := editToolCallBackend(t, touchedFile)
	defer localBackend.Close()
	upstream, upstreamCalls := countingUpstream(t, `{"content":[{"type":"text","text":"frontier fallback"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	defer upstream.Close()

	db, _ := openTestDB(t)
	cfg := routeTestConfig(localBackend.URL)
	lr := router.NewLiveRouter(db, cfg)
	families := router.FamilyPair("claude-sonnet-4-6", "local-model", cfg.Families)
	const category = "question_answer|"
	seedRoutableCategory(t, db, lr, category, families)

	srv, err := New(Config{Upstream: upstream.URL, DB: db, Router: lr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	// Turn 1: routable category, served locally, "garbage" edit touching
	// touchedFile.
	firstReq := requestWithSession(sessionID, []map[string]any{
		{"role": "user", "content": "please fix the bug"},
	})
	resp1, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(firstReq))
	if err != nil {
		t.Fatalf("first POST: %v", err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first response status = %d, want 200", resp1.StatusCode)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream called after turn 1, want 0 (should have been served locally)")
	}

	// Turn 2: the followup, reporting the edit failed (is_error tool_result).
	secondReq := requestWithSession(sessionID, []map[string]any{
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "tc_1", "is_error": true, "content": "apply failed: syntax error"},
		}},
	})
	resp2, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(secondReq))
	if err != nil {
		t.Fatalf("second POST: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second response status = %d, want 200", resp2.StatusCode)
	}
	if upstreamCalls.Load() != 1 {
		t.Errorf("upstream called %d times after turn 2, want 1 (escalated turn goes to frontier)", upstreamCalls.Load())
	}

	// Turn 3: same session, must also hit upstream (breaker stays tripped
	// until process restart).
	thirdReq := requestWithSession(sessionID, []map[string]any{
		{"role": "user", "content": "anything else"},
	})
	resp3, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(thirdReq))
	if err != nil {
		t.Fatalf("third POST: %v", err)
	}
	io.Copy(io.Discard, resp3.Body)
	resp3.Body.Close()
	if upstreamCalls.Load() != 2 {
		t.Errorf("upstream called %d times after turn 3, want 2", upstreamCalls.Load())
	}

	// router_state disabled.
	states, err := store.AllRouterState(db)
	if err != nil {
		t.Fatalf("AllRouterState: %v", err)
	}
	var found bool
	for _, s := range states {
		if s.Category == category && s.Families == families {
			found = true
			if s.Routable {
				t.Error("router_state.routable = true after escalation, want false")
			}
			if s.DisabledReason != "escalation" {
				t.Errorf("router_state.disabled_reason = %q, want escalation", s.DisabledReason)
			}
		}
	}
	if !found {
		t.Fatal("no router_state row found for the escalated category/families")
	}

	// router_decisions: exactly one local, one escalated (turns 2 and 3
	// that fell through to pass-through log nothing new, matching the
	// "pure pass-through logs nothing" design).
	decisions, err := store.RouterDecisionsSince(db, "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("RouterDecisionsSince: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("len(decisions) = %d, want 2: %+v", len(decisions), decisions)
	}
	var sawLocal, sawEscalated bool
	for _, d := range decisions {
		switch d.Decision {
		case router.DecisionLocal:
			sawLocal = true
		case router.DecisionEscalated:
			sawEscalated = true
			var stats map[string]any
			if err := json.Unmarshal([]byte(d.Stats.String), &stats); err != nil {
				t.Fatalf("escalated decision stats not valid JSON: %v", err)
			}
			touched, _ := stats["files_touched"].([]any)
			if len(touched) != 1 || touched[0] != touchedFile {
				t.Errorf("escalated stats files_touched = %+v, want [%q]", stats["files_touched"], touchedFile)
			}
		}
	}
	if !sawLocal || !sawEscalated {
		t.Errorf("decisions = %+v, want one local and one escalated", decisions)
	}
}

func TestProxy_ShadowDispatch_ForceOrdinal(t *testing.T) {
	t.Setenv("SPLITTER_ROUTE", "on")

	localBackend, localCalls := countingBackend(t, "frontier answer")
	defer localBackend.Close()
	upstream, upstreamCalls := countingUpstream(t, `{"content":[{"type":"text","text":"frontier answer"}],"usage":{"input_tokens":4,"output_tokens":2}}`)
	defer upstream.Close()

	db, _ := openTestDB(t)
	cfg := routeTestConfig(localBackend.URL)
	cfg.Router.DualDispatchPct = 100 // force every routable decision to shadow
	lr := router.NewLiveRouter(db, cfg)
	lr.ShadowDone = make(chan struct{}, 1)

	families := router.FamilyPair("claude-sonnet-4-6", "local-model", cfg.Families)
	seedRoutableCategory(t, db, lr, "question_answer|", families)

	srv, err := New(Config{Upstream: upstream.URL, DB: db, Router: lr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(questionAnswerRequest(false)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var decoded struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(decoded.Content) != 1 || decoded.Content[0].Text != "frontier answer" {
		t.Errorf("client response content = %+v, want the FRONTIER answer (shadow serves frontier, not local)", decoded.Content)
	}
	if upstreamCalls.Load() != 1 {
		t.Errorf("upstream called %d times, want 1 (shadow still serves frontier)", upstreamCalls.Load())
	}

	select {
	case <-lr.ShadowDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shadow dispatch did not finish within 5s")
	}
	if localCalls.Load() != 1 {
		t.Errorf("local backend called %d times, want 1 (async shadow replay)", localCalls.Load())
	}

	// The normal capture pathway still ran (shadow proceeds through the
	// usual pass-through/capture flow).
	waitForCall(t, db, 1)

	decisions, err := store.RouterDecisionsSince(db, "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("RouterDecisionsSince: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Decision != router.DecisionShadow {
		t.Fatalf("decisions = %+v, want exactly one shadow decision", decisions)
	}
	var stats map[string]any
	if err := json.Unmarshal([]byte(decisions[0].Stats.String), &stats); err != nil {
		t.Fatalf("shadow decision stats not valid JSON: %v", err)
	}
	agree, ok := stats["shadow_agree"].(bool)
	if !ok {
		t.Fatalf("stats missing shadow_agree: %+v", stats)
	}
	if !agree {
		t.Errorf("shadow_agree = false, want true (local and frontier answers matched)")
	}
}
