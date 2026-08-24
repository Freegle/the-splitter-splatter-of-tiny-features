package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient_Complete_Success(t *testing.T) {
	var gotAuth string
	var gotModel string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("server: decoding request body: %v", err)
		}
		gotModel = req.Model

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatResponse{
			ID:    "chatcmpl-1",
			Model: req.Model,
			Choices: []ChatChoice{
				{Index: 0, Message: ChatMessage{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
			},
			Usage: ChatUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		})
	}))
	defer server.Close()

	t.Setenv("TEST_BACKEND_KEY", "secret-key")
	c := &Client{BaseURL: server.URL, APIKeyEnv: "TEST_BACKEND_KEY", Model: "some-model"}

	resp, err := c.Complete(context.Background(), &ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-key")
	}
	if gotModel != "some-model" {
		t.Errorf("request model = %q, want client's configured model %q (Complete must override req.Model)", gotModel, "some-model")
	}
	if resp.Choices[0].Message.Content != "hello" {
		t.Errorf("response content = %q, want hello", resp.Choices[0].Message.Content)
	}
	if resp.Usage.TotalTokens != 12 {
		t.Errorf("usage.TotalTokens = %d, want 12", resp.Usage.TotalTokens)
	}
}

func TestClient_Complete_NoAPIKeyEnvConfigured_NoAuthHeader(t *testing.T) {
	var sawAuthHeader bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthHeader = r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "ok"}}}})
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL, Model: "m"}
	if _, err := c.Complete(context.Background(), &ChatRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sawAuthHeader {
		t.Error("Authorization header sent even though APIKeyEnv is empty (this is the ollama configuration)")
	}
}

func TestClient_Complete_APIKeyEnvSetButEmpty_NoAuthHeader(t *testing.T) {
	var sawAuthHeader bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthHeader = r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "ok"}}}})
	}))
	defer server.Close()

	t.Setenv("TEST_BACKEND_EMPTY_KEY", "")
	c := &Client{BaseURL: server.URL, APIKeyEnv: "TEST_BACKEND_EMPTY_KEY", Model: "m"}
	if _, err := c.Complete(context.Background(), &ChatRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sawAuthHeader {
		t.Error("Authorization header sent even though the named env var is set but empty")
	}
}

func TestClient_Complete_ErrorBodySurfaced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"missing required field: messages","type":"invalid_request_error"}}`))
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL, Model: "m"}
	_, err := c.Complete(context.Background(), &ChatRequest{})
	if err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
	if !strings.Contains(err.Error(), "missing required field: messages") {
		t.Errorf("err = %v, want it to surface the JSON error body's message", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want it to mention the status code", err)
	}
}

func TestClient_Complete_ErrorBodyNonJSON_RawBodySurfaced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL, Model: "m"}
	_, err := c.Complete(context.Background(), &ChatRequest{})
	if err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Errorf("err = %v, want it to include the raw body when it is not JSON-shaped", err)
	}
}

func TestClient_Complete_BaseURLTrailingSlashTolerated(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant"}}}})
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL + "/", Model: "m"}
	if _, err := c.Complete(context.Background(), &ChatRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("request path = %q, want /chat/completions", gotPath)
	}
}

func TestClient_Complete_ContextCanceledPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatResponse{})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	c := &Client{BaseURL: server.URL, Model: "m"}
	_, err := c.Complete(ctx, &ChatRequest{})
	if err == nil {
		t.Fatal("expected an error when the caller's context deadline is exceeded")
	}
}

func TestClient_Complete_TemperatureAndModelSentEvenWhenRequestOmitsThem(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant"}}}})
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL, Model: "target-model"}
	if _, err := c.Complete(context.Background(), &ChatRequest{Model: "ignored-model"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotBody["model"] != "target-model" {
		t.Errorf("body[model] = %v, want target-model (client-configured model must win)", gotBody["model"])
	}
	if gotBody["temperature"] != float64(0) {
		t.Errorf("body[temperature] = %v, want 0", gotBody["temperature"])
	}
}
