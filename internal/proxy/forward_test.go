package proxy

import (
	"net/http"
	"testing"
)

func TestCleanForwardHeaders_RemovesHopByHop(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "Keep-Alive, X-Custom")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("X-Custom", "should-be-removed-via-connection-header")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Upgrade", "websocket")
	h.Set("Proxy-Authorization", "Basic xyz")
	h.Set("TE", "trailers")
	h.Set("Trailers", "X-Foo")
	h.Set("x-api-key", "sk-keep-me")
	h.Set("Content-Type", "application/json")

	cleanForwardHeaders(h, false)

	for _, name := range []string{"Connection", "Keep-Alive", "X-Custom", "Transfer-Encoding", "Upgrade", "Proxy-Authorization", "TE", "Trailers"} {
		if h.Get(name) != "" {
			t.Errorf("header %s = %q, want removed", name, h.Get(name))
		}
	}
	if h.Get("x-api-key") != "sk-keep-me" {
		t.Errorf("x-api-key = %q, want preserved sk-keep-me", h.Get("x-api-key"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want preserved", h.Get("Content-Type"))
	}
}

func TestCleanForwardHeaders_StripsAcceptEncodingOnlyWhenAsked(t *testing.T) {
	h := http.Header{}
	h.Set("Accept-Encoding", "gzip")

	cleanForwardHeaders(h, false)
	if h.Get("Accept-Encoding") != "gzip" {
		t.Errorf("Accept-Encoding stripped when stripAcceptEncoding=false")
	}

	cleanForwardHeaders(h, true)
	if h.Get("Accept-Encoding") != "" {
		t.Errorf("Accept-Encoding = %q, want stripped", h.Get("Accept-Encoding"))
	}
}

func TestCleanForwardHeaders_RemovesProxyPrefixedHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Proxy-Foo", "bar")
	h.Set("X-Kept", "1")

	cleanForwardHeaders(h, false)

	if h.Get("Proxy-Foo") != "" {
		t.Errorf("Proxy-Foo = %q, want removed", h.Get("Proxy-Foo"))
	}
	if h.Get("X-Kept") != "1" {
		t.Errorf("X-Kept = %q, want preserved", h.Get("X-Kept"))
	}
}

func TestJoinURLPath(t *testing.T) {
	tests := []struct {
		base, reqPath, want string
	}{
		{"", "/v1/messages", "/v1/messages"},
		{"/", "/v1/messages", "/v1/messages"},
		{"/api", "/v1/messages", "/api/v1/messages"},
		{"/api/", "/v1/messages", "/api/v1/messages"},
	}
	for _, tt := range tests {
		got := joinURLPath(tt.base, tt.reqPath)
		if got != tt.want {
			t.Errorf("joinURLPath(%q, %q) = %q, want %q", tt.base, tt.reqPath, got, tt.want)
		}
	}
}
