// Adapted from seifghazi/claude-code-proxy (MIT), commit 02c9c766.
package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
)

// captureRecord holds everything the async logger needs to assemble,
// compress and insert one calls row. It is built entirely from bytes
// already read off the wire; the logger does no further network I/O.
type captureRecord struct {
	ts           time.Time
	sessionID    string
	model        string
	stream       bool
	requestBody  []byte
	responseBody []byte
	isSSE        bool
	latencyMs    int64
	status       int
	procError    string
}

// capBuffer accumulates response bytes for capture up to a fixed cap. Once
// the cap is reached, further writes are dropped and truncated is set, but
// the caller keeps forwarding the untouched bytes to the client regardless.
type capBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newCapBuffer(max int) *capBuffer {
	return &capBuffer{max: max}
}

func (c *capBuffer) Write(p []byte) {
	if c.truncated {
		return
	}
	remaining := c.max - c.buf.Len()
	if len(p) > remaining {
		if remaining > 0 {
			c.buf.Write(p[:remaining])
		}
		c.truncated = true
		return
	}
	c.buf.Write(p)
}

func (c *capBuffer) Bytes() []byte {
	return c.buf.Bytes()
}

// handleCaptured forwards a POST /v1/messages request to upstream and, once
// the response has been fully relayed to the client, hands a capture
// record to the async logger. sessionIDFunc is called before the upstream
// round trip so a fault injected into it (used by panic-recovery tests)
// never causes upstream to be contacted twice.
func (s *Server) handleCaptured(w http.ResponseWriter, r *http.Request, body []byte) {
	start := time.Now()

	var parsedReq anthropic.MessagesRequest
	_ = json.Unmarshal(body, &parsedReq) // best-effort; zero value on failure, never blocks forwarding

	sessionID := sessionIDFunc(r.Header.Get("User-Agent"), &parsedReq)

	upReq, err := s.buildUpstreamRequest(r.Context(), r, body)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	resp, err := s.client.Do(upReq)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	captured := newCapBuffer(maxCaptureBytes)
	relayBody(w, resp.Body, captured)

	procError := ""
	if captured.truncated {
		procError = "response capture truncated at 32MB cap"
	}

	rec := &captureRecord{
		ts:           start.UTC(),
		sessionID:    sessionID,
		model:        parsedReq.Model,
		stream:       parsedReq.Stream,
		requestBody:  body,
		responseBody: captured.Bytes(),
		isSSE:        isSSE,
		latencyMs:    time.Since(start).Milliseconds(),
		status:       resp.StatusCode,
		procError:    procError,
	}
	s.logger.enqueue(rec)
}
