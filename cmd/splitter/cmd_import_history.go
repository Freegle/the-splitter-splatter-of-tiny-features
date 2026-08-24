package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

// importHistoryBanner is printed at the start of every run and repeated in
// -h/--help output. This command is a one-off bootstrap only: it is never
// invoked by the live pipeline (proxy, featurise, replay, judge and router
// never read ~/.claude), because Claude Code's transcript format is
// internal and can change between releases without notice.
const importHistoryBanner = `splitter import-history: ONE-OFF BEST-EFFORT BOOTSTRAP.

Reads Claude Code's own session transcript files to reconstruct approximate
calls rows (source='import') for bootstrap data only. The transcript format
is Claude Code's internal implementation detail, not a documented contract,
so reconstruction is best-effort: unparseable or unusable lines are skipped
and counted rather than aborting the run, and a reconstructed "request" is
only an approximation (it carries the prior turns of the session as
messages; it never had the original system prompt or tool definitions,
which transcripts do not record). This command is NEVER used by the live
pipeline: proxy, featurise, replay, judge and router do not read
~/.claude, so a future Claude Code transcript format change cannot break
anything but a repeat run of this command.`

// maxHistoryMessages bounds how many prior messages of a session are
// carried into a reconstructed request, keeping reconstructed request
// bodies bounded on long-running sessions. The turn_type classifier and
// files_touched extraction only need the response plus the last user
// message, so a generous recent window is enough context without
// reconstructing an entire multi-hour session verbatim.
const maxHistoryMessages = 40

func init() {
	register("import-history", runImportHistory)
}

// importSummary counts what happened during one import-history run, across
// however many transcript files were scanned.
type importSummary struct {
	FilesScanned       int
	FilesErrored       int
	LinesTotal         int
	AssistantTurns     int
	Imported           int
	SkippedSidechain   int
	SkippedUnparseable int
}

// add accumulates o's counts into s.
func (s *importSummary) add(o importSummary) {
	s.LinesTotal += o.LinesTotal
	s.AssistantTurns += o.AssistantTurns
	s.Imported += o.Imported
	s.SkippedSidechain += o.SkippedSidechain
	s.SkippedUnparseable += o.SkippedUnparseable
}

// runImportHistory runs the one-off best-effort transcript bootstrap: every
// ~/.claude/projects/*/*.jsonl file (or -dir override) is scanned, each
// top-level assistant turn is reconstructed into a calls row with
// source='import', and a summary of what was imported versus skipped is
// printed. It is idempotent only in the sense that running it again over
// the same files inserts the same rows again (calls has no natural key to
// conflict on); this is a one-off command, not a resumable sync.
func runImportHistory(args []string) error {
	fs := flag.NewFlagSet("import-history", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, importHistoryBanner)
		fmt.Fprintln(os.Stderr, "\nusage: splitter import-history [-dir path] [-config path]")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	dir := fs.String("dir", "", "directory to glob */*.jsonl transcripts from (default ~/.claude/projects)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	transcriptDir, err := resolveTranscriptDir(*dir)
	if err != nil {
		return fmt.Errorf("resolving transcript directory: %w", err)
	}

	files, err := filepath.Glob(filepath.Join(transcriptDir, "*", "*.jsonl"))
	if err != nil {
		return fmt.Errorf("globbing transcripts under %s: %w", transcriptDir, err)
	}
	sort.Strings(files)

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening store at %s: %w", cfg.DBPath, err)
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		return fmt.Errorf("migrating store: %w", err)
	}

	fmt.Println(importHistoryBanner)
	fmt.Printf("\nscanning %d transcript file(s) under %s\n", len(files), transcriptDir)

	var total importSummary
	total.FilesScanned = len(files)
	for _, f := range files {
		sum, ferr := importTranscriptFile(db, f)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "splitter import-history: %s: %v\n", f, ferr)
			total.FilesErrored++
			continue
		}
		total.add(sum)
	}

	printImportSummary(total)
	return nil
}

// resolveTranscriptDir returns override (tilde-expanded) when set, else
// ~/.claude/projects.
func resolveTranscriptDir(override string) (string, error) {
	if override != "" {
		return expandHomeDir(override), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// expandHomeDir replaces a leading "~" or "~/" with the user's home
// directory. A path not starting with "~" is returned unchanged.
func expandHomeDir(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func printImportSummary(s importSummary) {
	fmt.Println()
	fmt.Println("splitter import-history summary:")
	fmt.Printf("  files scanned:          %d\n", s.FilesScanned)
	if s.FilesErrored > 0 {
		fmt.Printf("  files errored:          %d\n", s.FilesErrored)
	}
	fmt.Printf("  lines scanned:          %d\n", s.LinesTotal)
	fmt.Printf("  assistant turns seen:   %d\n", s.AssistantTurns)
	fmt.Printf("  calls imported:         %d\n", s.Imported)
	fmt.Printf("  skipped (subagent):     %d\n", s.SkippedSidechain)
	fmt.Printf("  skipped (unparseable):  %d\n", s.SkippedUnparseable)
	fmt.Println("BEST-EFFORT bootstrap data only: imported rows are approximate reconstructions, never used by the live pipeline.")
}

// transcriptLine is the subset of one Claude Code transcript JSONL line
// this command reads. Every line carries a "type"; only "user" and
// "assistant" lines matter here. Every other type (attachment, mode,
// permission-mode, bridge-session, ai-title, last-prompt, summary, and any
// future type) carries no API turn and is silently ignored, not counted as
// skipped: skip counts are reserved for lines that look like they should
// have yielded a call but could not be reconstructed.
type transcriptLine struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"sessionId"`
	IsSidechain bool            `json:"isSidechain"`
	Timestamp   string          `json:"timestamp"`
	Message     json.RawMessage `json:"message"`
}

// transcriptAssistantMessage is the shape of an assistant transcript
// line's "message" object. It overlaps with, but is not identical to, the
// Anthropic Messages API response: transcripts add "model" and "id"
// directly on the message and nest only input/output token counts of
// Usage that calls rows actually store.
type transcriptAssistantMessage struct {
	ID         string                   `json:"id"`
	Model      string                   `json:"model"`
	Content    []anthropic.ContentBlock `json:"content"`
	StopReason string                   `json:"stop_reason"`
	Usage      *transcriptUsage         `json:"usage"`
}

// transcriptUsage is the subset of an assistant message's usage block that
// calls.input_tokens/output_tokens store. Pointer fields distinguish
// "usage present but token count omitted" from "no usage block at all".
type transcriptUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

// importedResponse is the shape this command writes to a call's
// response_zstd: a minimal Anthropic-message-like envelope. Only Content
// is read by the rest of splitter (internal/feature, internal/verify), but
// the other fields are carried through for completeness and for anyone
// inspecting the row directly.
type importedResponse struct {
	ID         string                   `json:"id,omitempty"`
	Type       string                   `json:"type"`
	Role       string                   `json:"role"`
	Content    []anthropic.ContentBlock `json:"content"`
	StopReason string                   `json:"stop_reason,omitempty"`
}

// importTranscriptFile reads one transcript file and inserts a calls row
// for every top-level (non-subagent) assistant turn it can reconstruct.
// Session id is taken from each line's own "sessionId" field, falling back
// to the file's basename (transcript files are named
// "<sessionId>.jsonl") when a line omits it. History accumulates per
// session id within the file so that a reconstructed request's messages
// reflect the turns that actually preceded it.
func importTranscriptFile(db *sql.DB, path string) (importSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return importSummary{}, fmt.Errorf("reading: %w", err)
	}
	fileSessionID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

	var sum importSummary
	history := map[string][]anthropic.Message{}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sum.LinesTotal++

		var tl transcriptLine
		if err := json.Unmarshal([]byte(line), &tl); err != nil {
			sum.SkippedUnparseable++
			continue
		}
		sessionID := tl.SessionID
		if sessionID == "" {
			sessionID = fileSessionID
		}

		switch tl.Type {
		case "user":
			if msg, ok := decodeUserMessage(tl); ok {
				history[sessionID] = appendBounded(history[sessionID], msg)
			}

		case "assistant":
			if tl.IsSidechain {
				sum.SkippedSidechain++
				continue
			}
			sum.AssistantTurns++

			row, asMsg, err := buildImportedCall(tl, sessionID, history[sessionID])
			if err != nil {
				sum.SkippedUnparseable++
				continue
			}
			if _, err := store.InsertCall(db, row); err != nil {
				return sum, fmt.Errorf("inserting call for %s: %w", tl.Timestamp, err)
			}
			sum.Imported++
			history[sessionID] = appendBounded(history[sessionID], asMsg)
		}
	}

	return sum, nil
}

// decodeUserMessage decodes a "user" transcript line's message into an
// anthropic.Message usable as request history, returning ok=false when the
// line has no usable message (no message field, undecodable, empty role,
// or empty content, e.g. a bare tool-permission line). A sidechain
// (subagent-internal) user turn is excluded too: it belongs to a nested
// conversation the main session never saw directly.
func decodeUserMessage(tl transcriptLine) (anthropic.Message, bool) {
	if tl.IsSidechain || len(tl.Message) == 0 {
		return anthropic.Message{}, false
	}
	var msg anthropic.Message
	if err := json.Unmarshal(tl.Message, &msg); err != nil {
		return anthropic.Message{}, false
	}
	if msg.Role == "" || len(msg.Content) == 0 {
		return anthropic.Message{}, false
	}
	return msg, true
}

// buildImportedCall reconstructs one calls row and the anthropic.Message
// form of its response (for appending to the session's rolling history)
// from an assistant transcript line. It fails (counted as skipped by the
// caller) when the line lacks a timestamp, a message, a model-shaped
// message, or any content blocks: there is nothing usable to import.
func buildImportedCall(tl transcriptLine, sessionID string, priorMessages []anthropic.Message) (store.CallRow, anthropic.Message, error) {
	if tl.Timestamp == "" {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("no timestamp")
	}
	if _, ok := parseTranscriptTimestamp(tl.Timestamp); !ok {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("unparseable timestamp %q", tl.Timestamp)
	}
	if len(tl.Message) == 0 {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("no message field")
	}

	var am transcriptAssistantMessage
	if err := json.Unmarshal(tl.Message, &am); err != nil {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("decoding assistant message: %w", err)
	}
	if len(am.Content) == 0 {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("assistant message has no content blocks")
	}

	req := anthropic.MessagesRequest{
		Model:    am.Model,
		Messages: append([]anthropic.Message(nil), priorMessages...),
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("marshaling reconstructed request: %w", err)
	}

	resp := importedResponse{
		ID:         am.ID,
		Type:       "message",
		Role:       "assistant",
		Content:    am.Content,
		StopReason: am.StopReason,
	}
	respJSON, err := json.Marshal(resp)
	if err != nil {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("marshaling reconstructed response: %w", err)
	}

	reqZstd, err := store.Compress(reqJSON)
	if err != nil {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("compressing request: %w", err)
	}
	respZstd, err := store.Compress(respJSON)
	if err != nil {
		return store.CallRow{}, anthropic.Message{}, fmt.Errorf("compressing response: %w", err)
	}

	row := store.CallRow{
		TS:           tl.Timestamp,
		SessionID:    sql.NullString{String: sessionID, Valid: sessionID != ""},
		Model:        sql.NullString{String: am.Model, Valid: am.Model != ""},
		Stream:       false,
		RequestZstd:  reqZstd,
		ResponseZstd: respZstd,
		Source:       "import",
	}
	if am.Usage != nil {
		if am.Usage.InputTokens != nil {
			row.InputTokens = sql.NullInt64{Int64: *am.Usage.InputTokens, Valid: true}
		}
		if am.Usage.OutputTokens != nil {
			row.OutputTokens = sql.NullInt64{Int64: *am.Usage.OutputTokens, Valid: true}
		}
	}

	asMsg := anthropic.Message{Role: "assistant", Content: am.Content}
	return row, asMsg, nil
}

// parseTranscriptTimestamp parses a transcript "timestamp" value, which is
// RFC3339 with millisecond fractional seconds in practice (e.g.
// "2026-08-20T09:00:00.000Z"). RFC3339Nano is tried first since it accepts
// the fractional form directly, falling back to plain RFC3339 for a
// timestamp with no fractional part.
func parseTranscriptTimestamp(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// appendBounded appends msg to history, dropping the oldest entries beyond
// maxHistoryMessages so a reconstructed request's message list stays
// bounded on long-running sessions.
func appendBounded(history []anthropic.Message, msg anthropic.Message) []anthropic.Message {
	history = append(history, msg)
	if len(history) > maxHistoryMessages {
		history = history[len(history)-maxHistoryMessages:]
	}
	return history
}
