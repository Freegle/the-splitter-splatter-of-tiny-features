package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

func TestAnthropicClient_Complete_Success(t *testing.T) {
	var gotAPIKey, gotVersion string
	var gotBody map[string]any

	const rawResponse = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if r.URL.Path != "/v1/messages" {
			t.Errorf("request path = %q, want /v1/messages", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("server: decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rawResponse))
	}))
	defer server.Close()

	t.Setenv("TEST_ANTHROPIC_KEY", "ant-secret")
	c := &AnthropicClient{BaseURL: server.URL, APIKeyEnv: "TEST_ANTHROPIC_KEY", Model: "claude-haiku-4-5"}

	got, err := c.Complete(context.Background(), anthropic.MessagesRequest{
		Messages:  []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: anthropic.BlockText, Text: "hello"}}}},
		Stream:    true,
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotAPIKey != "ant-secret" {
		t.Errorf("x-api-key header = %q, want ant-secret", gotAPIKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version header = %q, want 2023-06-01", gotVersion)
	}
	if gotBody["model"] != "claude-haiku-4-5" {
		t.Errorf("request body model = %v, want claude-haiku-4-5", gotBody["model"])
	}
	if gotBody["stream"] != false && gotBody["stream"] != nil {
		t.Errorf("request body stream = %v, want false (Complete must force non-streaming)", gotBody["stream"])
	}
	if string(got) != rawResponse {
		t.Errorf("Complete returned %s, want the raw upstream body %s unmodified", got, rawResponse)
	}
}

func TestAnthropicClient_Complete_NoAPIKeyEnvConfigured_NoAuthHeader(t *testing.T) {
	var sawKeyHeader bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKeyHeader = r.Header.Get("x-api-key") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_01","type":"message","role":"assistant","content":[],"usage":{}}`))
	}))
	defer server.Close()

	c := &AnthropicClient{BaseURL: server.URL, Model: "m"}
	if _, err := c.Complete(context.Background(), anthropic.MessagesRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sawKeyHeader {
		t.Error("x-api-key header sent even though APIKeyEnv is empty")
	}
}

func TestAnthropicClient_Complete_ErrorBodySurfaced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited, slow down"}}`))
	}))
	defer server.Close()

	// MaxRetries -1: this test asserts the error body surfaces, not the
	// retry ladder (429 is retried in production, see retry_test.go).
	c := &AnthropicClient{BaseURL: server.URL, Model: "m", MaxRetries: -1}
	_, err := c.Complete(context.Background(), anthropic.MessagesRequest{})
	if err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
	if !strings.Contains(err.Error(), "rate limited, slow down") {
		t.Errorf("err = %v, want it to surface the JSON error body's message", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v, want it to mention the status code", err)
	}
}

func TestAnthropicClientOAuthTokenAuth(t *testing.T) {
	var gotAuth, gotBeta, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotKey = r.Header.Get("x-api-key")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	t.Setenv("SPLITTER_TEST_OAT", "sk-ant-oat01-abcdef")
	c := &AnthropicClient{BaseURL: srv.URL, APIKeyEnv: "SPLITTER_TEST_OAT", Model: "m"}
	if _, err := c.Complete(context.Background(), anthropic.MessagesRequest{Model: "m", MaxTokens: 16}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer sk-ant-oat01-abcdef" || gotBeta != "oauth-2025-04-20" || gotKey != "" {
		t.Fatalf("oauth token sent wrongly: auth=%q beta=%q x-api-key=%q", gotAuth, gotBeta, gotKey)
	}
}

func TestAnthropicClientOAuthAddsClaudeCodeShape(t *testing.T) {
	var gotUA string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	t.Setenv("SPLITTER_TEST_OAT2", "sk-ant-oat01-xyz")
	c := &AnthropicClient{BaseURL: srv.URL, APIKeyEnv: "SPLITTER_TEST_OAT2", Model: "m"}
	req := anthropic.MessagesRequest{Model: "m", MaxTokens: 16, System: json.RawMessage(`"original system"`)}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotUA != claudeCodeUserAgent {
		t.Fatalf("user-agent %q, want %q", gotUA, claudeCodeUserAgent)
	}
	sys, ok := gotBody["system"].([]any)
	if !ok || len(sys) != 2 {
		t.Fatalf("system not a 2-block array: %v", gotBody["system"])
	}
	first := sys[0].(map[string]any)["text"].(string)
	second := sys[1].(map[string]any)["text"].(string)
	if first != claudeCodeIdentity || second != "original system" {
		t.Fatalf("system blocks wrong: %q then %q", first, second)
	}
}
