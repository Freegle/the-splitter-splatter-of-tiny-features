// Package proxy implements Phase 1 of the splitter pipeline: a pass-through
// HTTP proxy that forwards Claude Code's traffic to the Anthropic API
// verbatim, including SSE streaming, and asynchronously logs POST
// /v1/messages calls to the store. Every other path is forwarded unlogged.
// Fail-open is the governing property throughout: any internal error in the
// capture or logging path degrades to a plain forward or a dropped log
// record, and never fails, slows, or truncates a forwarded request.
//
// Adapted from seifghazi/claude-code-proxy (MIT), commit 02c9c766.
package proxy

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	// maxCaptureBytes bounds how much of a response body is buffered for
	// logging. Beyond this the capture is marked truncated and stops
	// growing, but forwarding to the client continues unaffected.
	maxCaptureBytes = 32 * 1024 * 1024

	// chunkSize is the read buffer size used when relaying a response body,
	// matching DESIGN.md's streaming requirement of <=32KB chunks written
	// and flushed immediately.
	chunkSize = 32 * 1024

	// dialTimeout bounds establishing the TCP/TLS connection to upstream.
	// It does not bound the overall request, so a long SSE stream is never
	// cut off once the connection is open.
	dialTimeout = 30 * time.Second

	// loggerBufSize is the capacity of the async logger's channel.
	loggerBufSize = 64
)

// Config configures a Server.
type Config struct {
	// Upstream is the base URL every request is forwarded to, e.g.
	// "https://api.anthropic.com".
	Upstream string
	// DB is the store database captured calls are logged to. A nil DB
	// disables logging entirely; requests are still forwarded normally.
	DB *sql.DB
	// RepoPath is the target repository whose HEAD is recorded against
	// each captured call. Empty disables HEAD capture.
	RepoPath string
}

// Server is a pass-through logging proxy. It implements http.Handler.
type Server struct {
	upstream *url.URL
	client   *http.Client
	logger   *callLogger
}

// New builds a Server from cfg and starts its async logger goroutine.
func New(cfg Config) (*Server, error) {
	u, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("parsing upstream url %q: %w", cfg.Upstream, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream url %q must be absolute", cfg.Upstream)
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}

	s := &Server{
		upstream: u,
		client:   &http.Client{Transport: transport},
		logger:   newCallLogger(cfg.DB, cfg.RepoPath, loggerBufSize),
	}
	s.logger.start()
	return s, nil
}

// Dropped returns the number of capture records dropped because the
// logger's channel was full. A nonzero count is a fail-open overload
// signal, never a request failure.
func (s *Server) Dropped() int64 {
	return s.logger.dropped.Load()
}

// Close stops accepting new capture records and waits for the logger to
// drain the ones already queued, bounded by ctx. Callers must ensure no
// handler goroutine is still running when Close is called (an http.Server's
// Shutdown, which waits for every ServeHTTP call to return, guarantees
// this), since a request completing enqueue concurrently with the channel
// close is a data race.
func (s *Server) Close(ctx context.Context) error {
	return s.logger.close(ctx)
}

// trackingWriter wraps an http.ResponseWriter to record whether any bytes
// have been written to the client yet, so panic recovery knows whether a
// plain-forward fallback is still safe to attempt.
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wroteHeader = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	t.wroteHeader = true
	return t.ResponseWriter.Write(p)
}

func (t *trackingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ServeHTTP forwards every request to the configured upstream. Only
// POST /v1/messages responses are captured for logging; everything else is
// forwarded unlogged. An internal panic anywhere in request handling is
// recovered and falls back to a plain forward with no capture, provided no
// response bytes have reached the client yet; if the panic happens after
// bytes were already written, the partial response is left as is (the
// underlying connection is normally already broken at that point).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var bodyBytes []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, "splitter proxy: error reading request body", http.StatusBadRequest)
			return
		}
		bodyBytes = b
	}

	tw := &trackingWriter{ResponseWriter: w}
	captured := r.Method == http.MethodPost && r.URL.Path == "/v1/messages"

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("splitter: proxy: recovered from panic handling %s %s: %v", r.Method, r.URL.Path, rec)
				if !tw.wroteHeader {
					s.plainForward(tw, r, bodyBytes)
				}
			}
		}()

		if captured {
			s.handleCaptured(tw, r, bodyBytes)
		} else {
			s.plainForward(tw, r, bodyBytes)
		}
	}()
}
