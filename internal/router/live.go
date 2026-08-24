package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/backend"
	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

// Decision values, stored verbatim in router_decisions.decision.
const (
	DecisionLocal      = "local"
	DecisionFrontier   = "frontier"
	DecisionShadow     = "shadow"
	DecisionEscalated  = "escalated"
	DecisionKillswitch = "killswitch"
)

// snapshotRefreshInterval is how often Start's background goroutine
// refreshes the in-memory router_state snapshot (DESIGN.md: "refreshed
// every 60s from router_state").
const snapshotRefreshInterval = 60 * time.Second

// RouteEnabled reports whether live routing is on: SPLITTER_ROUTE=on
// enables it, any other value (including "off", unset, or a typo) is pure
// pass-through. "off" is the documented kill switch; it is simply one of
// the many values this treats identically to "not on".
func RouteEnabled() bool {
	return os.Getenv("SPLITTER_ROUTE") == "on"
}

// StateEntry is the live snapshot's view of one router_state row: enough
// to decide routability and to build the stats JSON a decision is logged
// with.
type StateEntry struct {
	N              int
	Agreed         int
	WilsonLB       float64
	Routable       bool
	DisabledReason string
}

// PendingServe records one session's most recently locally-served turn,
// kept only until the next request in that session is checked for the
// escalation signal (see LiveRouter.TakePending).
type PendingServe struct {
	Category     string
	Families     string
	FilesTouched []string
}

// LiveRouter holds Phase 4's live routing state: a periodically refreshed
// in-memory snapshot of router_state, the per-session escalation circuit
// breaker, each session's pending locally-served turn awaiting an
// escalation check, and the dual-dispatch ordinal counter. The proxy is
// the only caller: it owns request/response mechanics (translation,
// streaming, HTTP), LiveRouter owns the decision and its audit trail.
type LiveRouter struct {
	db  *sql.DB
	cfg *config.Config

	snapshot atomic.Pointer[map[string]StateEntry]

	mu      sync.Mutex
	broken  map[string]bool
	pending map[string]PendingServe

	ordinal atomic.Int64

	stopCh chan struct{}

	// ShadowDone, when non-nil, is sent to once each time DispatchShadow's
	// background goroutine finishes. Production code leaves it nil; tests
	// set a buffered channel to synchronize with the goroutine instead of
	// sleeping.
	ShadowDone chan struct{}
}

// NewLiveRouter builds a LiveRouter with an empty snapshot; call
// RefreshSnapshot (directly, for tests) or Start (for the running proxy)
// before relying on Lookup.
func NewLiveRouter(db *sql.DB, cfg *config.Config) *LiveRouter {
	lr := &LiveRouter{
		db:      db,
		cfg:     cfg,
		broken:  map[string]bool{},
		pending: map[string]PendingServe{},
		stopCh:  make(chan struct{}),
	}
	empty := map[string]StateEntry{}
	lr.snapshot.Store(&empty)
	return lr
}

// stateKey builds the snapshot's internal lookup key for a (category,
// families) pair. NUL cannot appear in either input (they are built from
// turn_type/subsystem/model-family text), so this is collision-free.
func stateKey(category, families string) string {
	return category + "\x00" + families
}

// RefreshSnapshot reloads every router_state row from the store and
// replaces the in-memory snapshot atomically. Safe to call concurrently
// with Lookup from request-handling goroutines.
func (lr *LiveRouter) RefreshSnapshot() error {
	rows, err := store.AllRouterState(lr.db)
	if err != nil {
		return fmt.Errorf("refreshing router snapshot: %w", err)
	}
	next := make(map[string]StateEntry, len(rows))
	for _, r := range rows {
		next[stateKey(r.Category, r.Families)] = StateEntry{
			N: r.N, Agreed: r.Agreed, WilsonLB: r.WilsonLB,
			Routable: r.Routable, DisabledReason: r.DisabledReason,
		}
	}
	lr.snapshot.Store(&next)
	return nil
}

// Start launches a background goroutine that calls RefreshSnapshot every
// snapshotRefreshInterval until Stop is called. It does not perform the
// initial load itself; call RefreshSnapshot once synchronously first so
// Lookup has data immediately.
func (lr *LiveRouter) Start() {
	go func() {
		ticker := time.NewTicker(snapshotRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				lr.RefreshSnapshot()
			case <-lr.stopCh:
				return
			}
		}
	}()
}

// Stop ends Start's background refresh goroutine.
func (lr *LiveRouter) Stop() {
	close(lr.stopCh)
}

// Lookup returns the current snapshot's entry for (category, families).
func (lr *LiveRouter) Lookup(category, families string) (StateEntry, bool) {
	snap := *lr.snapshot.Load()
	e, ok := snap[stateKey(category, families)]
	return e, ok
}

// DisableCategory persists disabled_reason and routable=0 for (category,
// families) to router_state, and updates the in-memory snapshot in place
// so the change is visible to the very next Lookup, without waiting for
// the next periodic refresh.
func (lr *LiveRouter) DisableCategory(category, families, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if err := store.DisableRouterState(lr.db, category, families, reason, now); err != nil {
		return err
	}

	old := *lr.snapshot.Load()
	next := make(map[string]StateEntry, len(old))
	for k, v := range old {
		next[k] = v
	}
	key := stateKey(category, families)
	entry := next[key]
	entry.Routable = false
	entry.DisabledReason = reason
	next[key] = entry
	lr.snapshot.Store(&next)
	return nil
}

// SessionBroken reports whether sessionID's circuit breaker has tripped:
// once tripped, a session stays on frontier for the rest of this process's
// lifetime (DESIGN.md: "those sessions go pure frontier until proxy
// restart").
func (lr *LiveRouter) SessionBroken(sessionID string) bool {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.broken[sessionID]
}

// TripBreaker marks sessionID's circuit breaker tripped.
func (lr *LiveRouter) TripBreaker(sessionID string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.broken[sessionID] = true
}

// RecordServedLocally remembers sessionID's just-served category, families
// and touched files, for TakePending to check on the next request in that
// session.
func (lr *LiveRouter) RecordServedLocally(sessionID, category, families string, filesTouched []string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.pending[sessionID] = PendingServe{Category: category, Families: families, FilesTouched: filesTouched}
}

// TakePending returns and clears sessionID's pending locally-served turn,
// if any. It is checked (and cleared) at most once per locally-served
// turn, matching Phase 2's had_error_followup semantics, which looks only
// at "the next call" in the session.
func (lr *LiveRouter) TakePending(sessionID string) (PendingServe, bool) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	p, ok := lr.pending[sessionID]
	if ok {
		delete(lr.pending, sessionID)
	}
	return p, ok
}

// NextOrdinal returns the next dual-dispatch ordinal for this process: an
// atomically incrementing counter starting at 0, shared across all
// sessions and categories (DESIGN.md: "5% of routable turns").
func (lr *LiveRouter) NextOrdinal() int64 {
	return lr.ordinal.Add(1) - 1
}

// ShouldShadow reports whether the next routable decision should be
// dual-dispatched (frontier served, local shadowed) rather than served
// locally, per [router].dual_dispatch_pct.
func (lr *LiveRouter) ShouldShadow() bool {
	return IsDualDispatchOrdinal(lr.NextOrdinal(), lr.cfg.Router.DualDispatchPct)
}

// LogDecision inserts a router_decisions row and returns its id.
// callID is nil when no calls row exists for this request (true for every
// locally-served or escalated decision: DESIGN.md's live routing path
// never writes to calls, see DECISIONS.md).
func (lr *LiveRouter) LogDecision(sessionID string, callID *int64, category, decision string, stats map[string]any) (int64, error) {
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return 0, fmt.Errorf("marshaling router decision stats: %w", err)
	}
	row := store.RouterDecisionRow{
		TS:       time.Now().UTC().Format(time.RFC3339),
		Decision: decision,
	}
	if sessionID != "" {
		row.SessionID = sql.NullString{String: sessionID, Valid: true}
	}
	if callID != nil {
		row.CallID = sql.NullInt64{Int64: *callID, Valid: true}
	}
	if category != "" {
		row.Category = sql.NullString{String: category, Valid: true}
	}
	row.Stats = sql.NullString{String: string(statsJSON), Valid: true}

	id, err := store.InsertRouterDecision(lr.db, row)
	if err != nil {
		return 0, fmt.Errorf("logging router decision: %w", err)
	}
	return id, nil
}

// UpdateDecisionStats overwrites a previously logged decision row's stats,
// used once DispatchShadow's async comparison completes.
func (lr *LiveRouter) UpdateDecisionStats(decisionID int64, stats map[string]any) error {
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshaling updated router decision stats: %w", err)
	}
	return store.UpdateRouterDecisionStats(lr.db, decisionID, string(statsJSON))
}

// defaultBackend returns the configured default routing backend's name and
// config (DESIGN.md: "Routable request -> serve from default backend",
// reusing [replay].backend, the same backend Phase 3 replays against by
// default).
func (lr *LiveRouter) defaultBackend() (name string, bcfg config.BackendConfig, ok bool) {
	name = lr.cfg.Replay.Backend
	bcfg, ok = lr.cfg.Backends[name]
	return name, bcfg, ok
}

// BackendModel returns the exact model string of the configured default
// routing backend, and whether one is configured, for callers that need to
// compute a family pair without going through ServeLocal.
func (lr *LiveRouter) BackendModel() (name, model string, ok bool) {
	backendName, bcfg, ok := lr.defaultBackend()
	return backendName, bcfg.Model, ok
}

// ServeLocal translates req to the configured default backend's format,
// calls it, and translates the result back to a complete Anthropic message
// JSON. blocks is that message's own content blocks (for the caller's
// escalation file-tracking); inputTokens/outputTokens come from the
// backend's own usage accounting.
func (lr *LiveRouter) ServeLocal(ctx context.Context, req anthropic.MessagesRequest) (msgJSON []byte, blocks []anthropic.ContentBlock, inputTokens, outputTokens int, err error) {
	_, bcfg, ok := lr.defaultBackend()
	if !ok {
		return nil, nil, 0, 0, fmt.Errorf("no backend configured for %q", lr.cfg.Replay.Backend)
	}

	client := &backend.Client{BaseURL: bcfg.BaseURL, APIKeyEnv: bcfg.APIKeyEnv, Model: bcfg.Model}
	openaiReq := backend.ToOpenAI(req, bcfg.Model)
	resp, err := client.Complete(ctx, &openaiReq)
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("calling local backend: %w", err)
	}

	msgJSON, err = backend.FromOpenAI(*resp)
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("translating local backend response: %w", err)
	}

	var decoded struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(msgJSON, &decoded); err != nil {
		return nil, nil, 0, 0, fmt.Errorf("decoding translated local response: %w", err)
	}

	return msgJSON, decoded.Content, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, nil
}

// DispatchShadow fires an async local replay of req against the configured
// default backend, compares its answer to frontierMsgJSON (the response
// actually served to the client for this dual-dispatched turn), and
// updates decisionID's stats with the comparison outcome merged onto
// baseStats (the stats the decision was originally logged with, so the
// audit trail keeps its justification once the shadow outcome is added,
// rather than the update overwriting it away). It returns immediately; the
// work happens in a background goroutine. When lr.ShadowDone is non-nil it
// receives a value once that goroutine finishes (test synchronization
// only, production leaves it nil and never blocks on it).
func (lr *LiveRouter) DispatchShadow(decisionID int64, req anthropic.MessagesRequest, frontierMsgJSON []byte, baseStats map[string]any) {
	go func() {
		defer func() {
			if lr.ShadowDone != nil {
				lr.ShadowDone <- struct{}{}
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), shadowDispatchTimeout)
		defer cancel()

		localMsgJSON, _, _, _, err := lr.ServeLocal(ctx, req)
		stats := cloneStats(baseStats)
		if err != nil {
			stats["shadow_error"] = err.Error()
		} else {
			stats["shadow_agree"] = roughAgree(frontierMsgJSON, localMsgJSON)
		}
		if updErr := lr.UpdateDecisionStats(decisionID, stats); updErr != nil {
			// Fail-open: a shadow comparison is an offline drift signal,
			// never allowed to affect the request that already completed.
			fmt.Fprintf(os.Stderr, "splitter: router: recording shadow outcome for decision %d: %v\n", decisionID, updErr)
		}
	}()
}

// cloneStats returns a shallow copy of stats, so DispatchShadow's goroutine
// never mutates a map its caller might still be holding a reference to.
func cloneStats(stats map[string]any) map[string]any {
	out := make(map[string]any, len(stats)+1)
	for k, v := range stats {
		out[k] = v
	}
	return out
}

// shadowDispatchTimeout bounds one async shadow replay call.
const shadowDispatchTimeout = 5 * time.Minute
