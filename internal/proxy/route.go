package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/router"
)

// routeOutcome is what routeDecision decided for one captured request,
// before any upstream call has been made.
type routeOutcome struct {
	// servedLocally is true when routeDecision has already written the
	// complete client response itself; the caller must not continue to
	// the normal upstream forwarding path.
	servedLocally bool
	// shadow is true when the caller should proceed with the normal
	// upstream pass-through as usual, then hand the resulting frontier
	// response to finishShadow.
	shadow bool
	// decisionID is the router_decisions row id logged for a shadow
	// outcome (0 for every other outcome, decisionID 0 is never a valid
	// row id since SQLite AUTOINCREMENT starts at 1).
	decisionID int64
	// stats is the justification snapshot logged with the shadow decision,
	// carried forward so finishShadow can merge the eventual shadow_agree
	// outcome onto it rather than losing it.
	stats map[string]any
}

// routeDecision applies Phase 4 live routing ahead of the normal
// pass-through path. It always runs the escalation check for the
// session's previous locally-served turn (if any), independent of how
// this request itself is routed. When live routing is disabled (nil
// Router, or SPLITTER_ROUTE not "on"), or the session's circuit breaker
// has tripped, or no routable category matches this request, it returns
// the zero routeOutcome: the caller proceeds exactly as Phase 1 always
// has. Otherwise it either serves the response locally itself (writing to
// w, returning servedLocally=true) or, for the 5% dual-dispatch pick,
// logs a shadow decision and asks the caller to proceed with the normal
// frontier pass-through.
func (s *Server) routeDecision(ctx context.Context, w http.ResponseWriter, req anthropic.MessagesRequest, sessionID string) routeOutcome {
	if s.router == nil || !router.RouteEnabled() {
		return routeOutcome{}
	}

	s.checkEscalation(sessionID, req)
	if s.router.SessionBroken(sessionID) {
		return routeOutcome{}
	}

	turnType, subsystem := feature.RequestOnly(req, s.repoPath)
	category := router.Category(turnType, subsystem)

	_, backendModel, ok := s.router.BackendModel()
	if !ok {
		return routeOutcome{}
	}
	families := router.FamilyPair(req.Model, backendModel, s.familyOverrides)

	entry, found := s.router.Lookup(category, families)
	if !found || !entry.Routable {
		return routeOutcome{}
	}

	stats := map[string]any{
		"n": entry.N, "agreed": entry.Agreed, "wilson_lb": entry.WilsonLB,
		"category": category, "families": families,
		"frontier_model": req.Model, "local_model": backendModel,
	}

	if s.router.ShouldShadow() {
		id, err := s.router.LogDecision(sessionID, nil, category, router.DecisionShadow, stats)
		if err != nil {
			log.Printf("splitter: proxy: router: logging shadow decision: %v", err)
			return routeOutcome{}
		}
		return routeOutcome{shadow: true, decisionID: id, stats: stats}
	}

	if s.serveLocally(ctx, w, req, sessionID, category, families, stats) {
		return routeOutcome{servedLocally: true}
	}
	// ServeLocal failed (e.g. the local backend is unreachable): fail
	// open, exactly like every other internal proxy error, and let the
	// caller fall through to the normal frontier pass-through.
	return routeOutcome{}
}

// checkEscalation inspects sessionID's pending locally-served turn (if
// any) against req, the next request in that session, for Phase 2's
// error-followup signal. When it fires: the category that turn was served
// from is disabled (persisted, disabled_reason "escalation"), the
// session's circuit breaker trips (this session goes frontier for the
// rest of this process's life), and a decision=escalated router_decisions
// row is logged. The pending record is consumed either way (checked at
// most once), matching HasErrorFollowup's own "the next call" semantics.
func (s *Server) checkEscalation(sessionID string, req anthropic.MessagesRequest) {
	pending, ok := s.router.TakePending(sessionID)
	if !ok {
		return
	}
	if !feature.HasErrorFollowup(pending.FilesTouched, req, s.repoPath) {
		return
	}

	s.router.TripBreaker(sessionID)
	if err := s.router.DisableCategory(pending.Category, pending.Families, "escalation"); err != nil {
		log.Printf("splitter: proxy: router: disabling category after escalation: %v", err)
	}
	if _, err := s.router.LogDecision(sessionID, nil, pending.Category, router.DecisionEscalated, map[string]any{
		"families":      pending.Families,
		"files_touched": pending.FilesTouched,
	}); err != nil {
		log.Printf("splitter: proxy: router: logging escalation decision: %v", err)
	}
}

// serveLocally calls the configured default backend for req, writes the
// translated response to w (SSE-synthesized when req.Stream), logs a
// decision=local router_decisions row, and records the served turn's
// touched files as sessionID's pending escalation check. It returns false
// (writing nothing to w) when the backend call itself fails, so the
// caller can fail open to the normal frontier pass-through instead.
func (s *Server) serveLocally(ctx context.Context, w http.ResponseWriter, req anthropic.MessagesRequest, sessionID, category, families string, stats map[string]any) bool {
	msgJSON, blocks, inputTokens, outputTokens, err := s.router.ServeLocal(ctx, req)
	if err != nil {
		log.Printf("splitter: proxy: router: local serve failed, falling back to frontier: %v", err)
		return false
	}

	stats["local_input_tokens"] = inputTokens
	stats["local_output_tokens"] = outputTokens
	if _, err := s.router.LogDecision(sessionID, nil, category, router.DecisionLocal, stats); err != nil {
		log.Printf("splitter: proxy: router: logging local decision: %v", err)
	}

	filesTouched := feature.FilesTouched(blocks, s.repoPath)
	s.router.RecordServedLocally(sessionID, category, families, filesTouched)

	if err := writeLocalResponse(w, req.Stream, msgJSON); err != nil {
		log.Printf("splitter: proxy: router: writing local response: %v", err)
	}
	return true
}

// writeLocalResponse writes msgJSON (a complete Anthropic message) to w:
// verbatim as application/json when stream is false, else synthesized as
// an SSE event stream (internal/anthropic.SynthesizeSSE) so a client that
// asked for stream:true sees the shape it expects.
func writeLocalResponse(w http.ResponseWriter, stream bool, msgJSON []byte) error {
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(msgJSON)
		return err
	}

	sse, err := anthropic.SynthesizeSSE(msgJSON)
	if err != nil {
		return fmt.Errorf("synthesizing sse: %w", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(sse); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// finishShadow hands the frontier response that was actually served for a
// dual-dispatched turn to the router's async shadow comparison.
func (s *Server) finishShadow(outcome routeOutcome, req anthropic.MessagesRequest, rawResponse []byte, isSSE bool) {
	frontierMsgJSON := rawResponse
	if isSSE {
		// AssembleSSE returns its best-effort partial assembly alongside
		// any error for a truncated stream; that partial assembly is
		// still a better comparison basis than the raw SSE bytes, so it
		// is used whenever non-empty regardless of err.
		if assembled, _, _, _ := anthropic.AssembleSSE(rawResponse); len(assembled) > 0 {
			frontierMsgJSON = assembled
		}
	}
	s.router.DispatchShadow(outcome.decisionID, req, frontierMsgJSON, outcome.stats)
}
