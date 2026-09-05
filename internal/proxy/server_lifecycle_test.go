package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServer_Dropped_ZeroOnHealthyServer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	reqBody := []byte(`{"model":"m","max_tokens":1,"messages":[]}`)
	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if srv.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0 on healthy server", srv.Dropped())
	}
}

func TestServer_Dropped_OverloadDropsRecordsButNeverFailsRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	// Set testDelay before any requests so the logger goroutine reads a stable value.
	srv.logger.testDelay = 10 * time.Millisecond

	reqBody := []byte(`{"model":"m","max_tokens":1,"messages":[]}`)
	for i := 0; i < 200; i++ {
		resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, resp.StatusCode)
		}
	}

	// Close the proxy server to ensure all handler goroutines have returned before we close the logger.
	proxyTS.Close()

	dropped := srv.Dropped()
	if dropped <= 0 {
		t.Errorf("Dropped = %d, want > 0 after overload; some records must have been dropped", dropped)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// This test deliberately leaves a backlog, so a full drain inside any fixed budget is
	// not guaranteed; either outcome is correct. Anything else is a real failure.
	if err := srv.Close(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close: unexpected error: %v", err)
	}
}

func TestServer_Close_DrainsQueuedRecordsBeforeReturning(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	reqBody := []byte(`{"model":"m","max_tokens":1,"messages":[]}`)
	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	// Close the proxy server to ensure all handler goroutines have returned before we close the logger.
	proxyTS.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM calls").Scan(&count); err != nil {
		t.Fatalf("counting calls: %v", err)
	}
	if count != 1 {
		t.Errorf("calls row count = %d, want 1 immediately after Close returns", count)
	}
}

func TestServer_Close_ReturnsErrorWhenDrainExceedsContext(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	// Set testDelay before any requests so the logger goroutine reads a stable value.
	srv.logger.testDelay = 200 * time.Millisecond

	reqBody := []byte(`{"model":"m","max_tokens":1,"messages":[]}`)
	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	// httptest.Server.Close blocks until every in-flight ServeHTTP has returned, which is
	// Server.Close's documented precondition: enqueue must not race the channel close.
	proxyTS.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err = srv.Close(ctx)
	if err == nil {
		t.Fatal("Close returned nil, want error when drain exceeds context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close error = %v, want context.DeadlineExceeded", err)
	}
}

func TestServer_Close_NilDB(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	srv, err := New(Config{Upstream: upstream.URL, DB: nil})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	reqBody := []byte(`{"model":"m","max_tokens":1,"messages":[]}`)
	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Close the proxy server to ensure all handler goroutines have returned before we close the logger.
	proxyTS.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
