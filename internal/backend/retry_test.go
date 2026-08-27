package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
)

func TestAnthropicClientWaitsOutRateLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if got["model"] != "m" {
			t.Errorf("retry lost the body: %v", got)
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	c := &AnthropicClient{BaseURL: srv.URL, Model: "m"}
	start := time.Now()
	out, err := c.Complete(context.Background(), anthropic.MessagesRequest{Model: "m", MaxTokens: 16})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected one retry, got %d calls", calls)
	}
	if time.Since(start) < time.Second {
		t.Fatal("Retry-After was not honoured")
	}
	if !strings.Contains(string(out), `"ok"`) {
		t.Fatalf("unexpected body: %s", out)
	}
}

func TestRetryAfterDelayFallsBackToBackoff(t *testing.T) {
	if d := retryAfterDelay("", 0, 0); d != 15*time.Second {
		t.Fatalf("first backoff %v", d)
	}
	if d := retryAfterDelay("", 20, 0); d != 5*time.Minute {
		t.Fatalf("backoff not capped: %v", d)
	}
	if d := retryAfterDelay("3", 0, 0); d != 3*time.Second {
		t.Fatalf("header ignored: %v", d)
	}
}

func TestOpenAICompatibleClientWaitsOutRateLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"Rate limit reached for requests"}}`))
			return
		}
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if got["model"] != "glm-5.3" {
			t.Errorf("retry lost or mangled the body: %v", got)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "glm-5.3"}
	resp, err := c.Complete(context.Background(), &ChatRequest{Model: "ignored"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected one retry, got %d calls", calls)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestBalanceExhaustionFailsFast(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "glm-5.3"}
	start := time.Now()
	_, err := c.Complete(context.Background(), &ChatRequest{})
	if err == nil {
		t.Fatal("expected an error when the account has no balance")
	}
	if calls != 1 {
		t.Fatalf("an empty wallet must not be retried: %d calls", calls)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("balance exhaustion should fail fast, not wait out a backoff ladder")
	}
	if !strings.Contains(err.Error(), "Insufficient balance") {
		t.Fatalf("error should carry the provider's message: %v", err)
	}
}

func TestIsBalanceExhausted(t *testing.T) {
	cases := map[string]bool{
		`{"error":{"code":"1113","message":"Insufficient balance"}}`: true,
		`{"error":{"message":"no resource package"}}`:                true,
		`{"error":{"message":"Rate limit reached for requests"}}`:    false,
		`{"error":{"message":"too many requests, slow down"}}`:       false,
	}
	for body, want := range cases {
		if got := isBalanceExhausted([]byte(body)); got != want {
			t.Errorf("isBalanceExhausted(%s) = %v, want %v", body, got, want)
		}
	}
}
