package proxy

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/store"
)

// waitForCall polls store.GetCall for id until it succeeds or a short
// deadline passes, since the logger inserts asynchronously off the request
// path.
func waitForCall(t *testing.T, db *sql.DB, id int64) *store.CallRow {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		row, err := store.GetCall(db, id)
		if err == nil {
			return row
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("call id %d was not logged within the deadline", id)
	return nil
}

func TestProxy_NonStreamingRoundTrip_CapturesUsageAndBody(t *testing.T) {
	upstreamBody := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":3}}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream got path %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(upstreamBody)
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	reqBody := []byte(`{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	clientBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading client response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, clientBody)
	}
	if !bytes.Equal(clientBody, upstreamBody) {
		t.Errorf("client body = %s, want verbatim upstream body %s", clientBody, upstreamBody)
	}

	row := waitForCall(t, db, 1)

	reqJSON, err := store.Decompress(row.RequestZstd)
	if err != nil {
		t.Fatalf("decompressing request: %v", err)
	}
	if !json.Valid(reqJSON) {
		t.Errorf("decompressed request is not valid JSON: %s", reqJSON)
	}
	if !bytes.Equal(reqJSON, reqBody) {
		t.Errorf("decompressed request = %s, want %s", reqJSON, reqBody)
	}

	respJSON, err := store.Decompress(row.ResponseZstd)
	if err != nil {
		t.Fatalf("decompressing response: %v", err)
	}
	if !json.Valid(respJSON) {
		t.Errorf("decompressed response is not valid JSON: %s", respJSON)
	}

	if row.InputTokens.Int64 != 11 {
		t.Errorf("InputTokens = %d, want 11", row.InputTokens.Int64)
	}
	if row.OutputTokens.Int64 != 3 {
		t.Errorf("OutputTokens = %d, want 3", row.OutputTokens.Int64)
	}
	if row.Model.String != "claude-x" {
		t.Errorf("Model = %q, want claude-x", row.Model.String)
	}
	if row.Status.Int64 != 200 {
		t.Errorf("Status = %d, want 200", row.Status.Int64)
	}
}

func TestProxy_SSE_StreamsUnbuffered_AndAssembles(t *testing.T) {
	const firstEvent = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-x\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n"
	const restEvents = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	release := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		io.WriteString(w, firstEvent)
		flusher.Flush()

		<-release // block until the test proves the client already has firstEvent

		io.WriteString(w, restEvents)
		flusher.Flush()
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	reqBody := []byte(`{"model":"claude-x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// Read exactly len(firstEvent) bytes: proving the client has received
	// them, before upstream has been allowed to send anything more, is the
	// deterministic proof that streaming is unbuffered end to end.
	first := make([]byte, len(firstEvent))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("reading first SSE event from client: %v", err)
	}
	if string(first) != firstEvent {
		t.Fatalf("first event received by client = %q, want %q", first, firstEvent)
	}

	close(release) // only now does upstream send the rest

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading remaining SSE events: %v", err)
	}
	if string(rest) != restEvents {
		t.Errorf("remaining events received by client = %q, want %q", rest, restEvents)
	}

	row := waitForCall(t, db, 1)
	if !row.Stream {
		t.Error("Stream = false, want true (request had stream:true)")
	}

	respJSON, err := store.Decompress(row.ResponseZstd)
	if err != nil {
		t.Fatalf("decompressing response: %v", err)
	}

	var assembled struct {
		Content []anthropic.ContentBlock `json:"content"`
		Usage   anthropic.Usage          `json:"usage"`
	}
	if err := json.Unmarshal(respJSON, &assembled); err != nil {
		t.Fatalf("logged response is not the assembled message JSON: %v\n%s", err, respJSON)
	}
	if len(assembled.Content) != 1 || assembled.Content[0].Text != "hi" {
		t.Errorf("assembled content = %+v, want a single text block \"hi\"", assembled.Content)
	}
	if row.InputTokens.Int64 != 5 {
		t.Errorf("InputTokens = %d, want 5", row.InputTokens.Int64)
	}
	if row.OutputTokens.Int64 != 2 {
		t.Errorf("OutputTokens = %d, want 2", row.OutputTokens.Int64)
	}
}

func TestProxy_DBDeletedMidRun_RequestsStillSucceed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "splitter.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	reqBody := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)

	resp1, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("first POST: %v", err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", resp1.StatusCode)
	}
	waitForCall(t, db, 1)

	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("removing db file mid-run: %v", err)
	}

	for i := 0; i < 5; i++ {
		resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("request %d after db deletion: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d after db deletion: status = %d, want 200; body=%s", i, resp.StatusCode, body)
		}
	}

	// Give the async logger a moment to attempt, and fail open past, the
	// now-broken inserts. Nothing here should be able to fail the test:
	// the requests above already proved fail-open by succeeding.
	time.Sleep(100 * time.Millisecond)
}

func TestProxy_HeadersPassThrough(t *testing.T) {
	var gotAPIKey, gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("X-Custom-Response", "yes")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[],"usage":{"input_tokens":0,"output_tokens":0}}`))
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	req, err := http.NewRequest(http.MethodPost, proxyTS.URL+"/v1/messages", bytes.NewReader([]byte(`{"model":"m","max_tokens":1,"messages":[]}`)))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("x-api-key", "sk-test-123")
	req.Header.Set("anthropic-beta", "beta-flag")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if gotAPIKey != "sk-test-123" {
		t.Errorf("upstream saw x-api-key = %q, want sk-test-123", gotAPIKey)
	}
	if gotBeta != "beta-flag" {
		t.Errorf("upstream saw anthropic-beta = %q, want beta-flag", gotBeta)
	}
	if resp.Header.Get("X-Custom-Response") != "yes" {
		t.Errorf("client saw X-Custom-Response = %q, want yes", resp.Header.Get("X-Custom-Response"))
	}
}

func TestProxy_NonMessagesPath_NotCaptured(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	requests := []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/v1/messages/count_tokens"},
		{http.MethodPost, "/v1/messages/batches"},
		{http.MethodGet, "/v1/messages"}, // right path, wrong method: not captured
	}
	for _, rr := range requests {
		req, err := http.NewRequest(rr.method, proxyTS.URL+rr.path, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatalf("building %s %s: %v", rr.method, rr.path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rr.method, rr.path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: status = %d, want 200", rr.method, rr.path, resp.StatusCode)
		}
	}

	time.Sleep(50 * time.Millisecond)
	if _, err := store.GetCall(db, 1); err == nil {
		t.Error("a non-captured request produced a calls row")
	}
}

func TestProxy_UpstreamUnreachable_Returns502(t *testing.T) {
	db, _ := openTestDB(t)
	// Port 1 is a privileged port nothing listens on; the connection is
	// refused immediately rather than needing the dial timeout to expire.
	srv, err := New(Config{Upstream: "http://127.0.0.1:1", DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader([]byte(`{"model":"m","max_tokens":1,"messages":[]}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if len(body) == 0 {
		t.Error("expected upstream error text in the 502 body, got empty body")
	}
}

func TestProxy_PanicRecovery_FallsBackToPlainForward(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	original := sessionIDFunc
	sessionIDFunc = func(userAgent string, req *anthropic.MessagesRequest) string {
		panic("injected fault for panic recovery test")
	}
	defer func() { sessionIDFunc = original }()

	resp, err := http.Post(proxyTS.URL+"/v1/messages", "application/json", bytes.NewReader([]byte(`{"model":"m","max_tokens":1,"messages":[]}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (plain-forward fallback); body=%s", resp.StatusCode, body)
	}
	if !json.Valid(body) {
		t.Errorf("plain-forward body not valid JSON: %s", body)
	}
	if upstreamCalls != 1 {
		t.Errorf("upstream called %d times, want exactly 1", upstreamCalls)
	}

	// No capture record can have been produced: handleCaptured panicked
	// before ever reaching the upstream round trip.
	time.Sleep(50 * time.Millisecond)
	if _, err := store.GetCall(db, 1); err == nil {
		t.Error("a request whose capture path panicked still produced a calls row")
	}
}

func TestProxy_Overhead_P50BelowBudget(t *testing.T) {
	respBody := []byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(respBody)
	}))
	defer upstream.Close()

	db, _ := openTestDB(t)
	srv, err := New(Config{Upstream: upstream.URL, DB: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyTS := httptest.NewServer(srv)
	defer proxyTS.Close()

	const n = 200
	reqBody := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)

	baseline := measureP50(t, upstream.URL+"/v1/messages", reqBody, n)
	proxied := measureP50(t, proxyTS.URL+"/v1/messages", reqBody, n)

	delta := proxied - baseline
	t.Logf("baseline p50 = %v, proxied p50 = %v, delta = %v", baseline, proxied, delta)
	if delta > 5*time.Millisecond {
		t.Errorf("proxy overhead p50 delta = %v, want < 5ms", delta)
	}
}

func measureP50(t *testing.T, url string, body []byte, n int) time.Duration {
	t.Helper()
	client := &http.Client{}
	durations := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request %d to %s: %v", i, url, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations[len(durations)/2]
}
