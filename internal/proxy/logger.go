// Adapted from seifghazi/claude-code-proxy (MIT), commit 02c9c766.
package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/store"
)

// callLogger is the async logger goroutine: it owns the buffered channel
// capture records are enqueued to, and does every piece of work kept off
// the request path (SSE assembly, compression, repo HEAD read, DB insert).
type callLogger struct {
	db       *sql.DB
	repoPath string
	ch       chan *captureRecord
	dropped  atomic.Int64
	done     chan struct{}

	// testDelay, when nonzero, is slept at the start of process(). It
	// exists only so tests can force a slow drain to exercise close's
	// context deadline; production code never sets it.
	testDelay time.Duration

	// testPanicOnce, when true, makes process() panic on the first record
	// it handles and then clears itself. It exists only so tests can prove
	// the logger goroutine survives a panic and keeps consuming; production
	// code never sets it.
	testPanicOnce bool
}

func newCallLogger(db *sql.DB, repoPath string, bufSize int) *callLogger {
	return &callLogger{
		db:       db,
		repoPath: repoPath,
		ch:       make(chan *captureRecord, bufSize),
		done:     make(chan struct{}),
	}
}

func (l *callLogger) start() {
	go l.run()
}

func (l *callLogger) run() {
	defer close(l.done)
	for rec := range l.ch {
		l.process(rec)
	}
}

// enqueue hands rec to the logger goroutine without ever blocking the
// request path. When the buffered channel is full the record is dropped
// and counted: logging must never slow down or fail a forwarded request.
func (l *callLogger) enqueue(rec *captureRecord) {
	select {
	case l.ch <- rec:
	default:
		l.dropped.Add(1)
		log.Printf("splitter: proxy: logger channel full, dropping capture record for session %s", rec.sessionID)
	}
}

// close stops accepting new records and waits for the goroutine to finish
// draining whatever was already queued, bounded by ctx. Callers must not
// have any in-flight enqueue call racing with close; see Server.Close.
func (l *callLogger) close(ctx context.Context) error {
	close(l.ch)
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("logger drain did not finish: %w", ctx.Err())
	}
}

// process turns one capture record into a calls row. Every failure mode
// here (SSE assembly error, compression error, DB error) is logged to
// stderr and the record is dropped; nothing here is allowed to propagate
// back into a request, and a panic is recovered so a single bad record can
// never take down the logger goroutine.
func (l *callLogger) process(rec *captureRecord) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("splitter: proxy: logger: recovered from panic processing capture record: %v", r)
		}
	}()

	if l.testDelay > 0 {
		time.Sleep(l.testDelay)
	}
	if l.testPanicOnce {
		l.testPanicOnce = false
		panic("splitter: proxy: injected test panic")
	}

	responseJSON := rec.responseBody
	var usage anthropic.Usage
	errText := rec.procError

	if rec.isSSE {
		assembled, u, _, err := anthropic.AssembleSSE(rec.responseBody)
		if len(assembled) > 0 {
			responseJSON = assembled
		}
		usage = u
		if err != nil {
			errText = appendErr(errText, err)
		}
	} else {
		usage = extractUsage(rec.responseBody)
	}

	reqZstd, err := store.Compress(rec.requestBody)
	if err != nil {
		log.Printf("splitter: proxy: logger: compressing request: %v", err)
		return
	}
	respZstd, err := store.Compress(responseJSON)
	if err != nil {
		log.Printf("splitter: proxy: logger: compressing response: %v", err)
		return
	}

	row := store.CallRow{
		TS:           rec.ts.Format(time.RFC3339),
		SessionID:    optionalString(rec.sessionID),
		Model:        optionalString(rec.model),
		Stream:       rec.stream,
		RequestZstd:  reqZstd,
		ResponseZstd: respZstd,
		LatencyMs:    sql.NullInt64{Int64: rec.latencyMs, Valid: true},
		RepoHead:     optionalString(readRepoHead(l.repoPath)),
		Status:       sql.NullInt64{Int64: int64(rec.status), Valid: true},
		Error:        optionalString(errText),
	}
	if rec.status == http.StatusOK {
		row.InputTokens = sql.NullInt64{Int64: int64(usage.InputTokens), Valid: true}
		row.OutputTokens = sql.NullInt64{Int64: int64(usage.OutputTokens), Valid: true}
	}

	if l.db == nil {
		return
	}
	if _, err := store.InsertCall(l.db, row); err != nil {
		// Fail-open: the DB may have been deleted or become unwritable
		// mid-run. The request this record came from has already
		// completed successfully; losing the log entry is the accepted
		// cost, never a request failure.
		log.Printf("splitter: proxy: logger: inserting call: %v", err)
	}
}

func appendErr(existing string, err error) string {
	if existing == "" {
		return err.Error()
	}
	return existing + "; " + err.Error()
}

func optionalString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// extractUsage decodes the top-level "usage" object of a non-streaming
// Messages API response body. Any decode failure yields a zero Usage.
func extractUsage(body []byte) anthropic.Usage {
	var wrapper struct {
		Usage anthropic.Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return anthropic.Usage{}
	}
	return wrapper.Usage
}
