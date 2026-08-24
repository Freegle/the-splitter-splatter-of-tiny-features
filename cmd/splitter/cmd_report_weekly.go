// This file adds the "weekly" report sub-command: Phase 4 live routing's
// operational summary over the last 7 days (frontier tokens avoided,
// estimated cost saved, quality incidents, drift check results), plus the
// current per-exact-version divergence flags from the last "router update".
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/router"
	"github.com/freegle/splitter/internal/store"
)

// weeklyReportWindow is how far back "splitter report weekly" looks.
const weeklyReportWindow = 7 * 24 * time.Hour

func init() {
	registerReport("weekly", runReportWeekly)
}

// weeklySummary holds everything runReportWeekly prints, computed from the
// last weeklyReportWindow of router_decisions plus the current
// router_state divergence flags.
type weeklySummary struct {
	FrontierTokensAvoided int64
	EstimatedCostSavedUSD float64
	QualityIncidents      int
	ShadowTotal           int
	ShadowDisagreed       int
	ShadowPending         int
	Divergences           []store.RouterStateRow
}

// runReportWeekly prints the Phase 4 weekly operational report.
func runReportWeekly(args []string) error {
	fs := flag.NewFlagSet("report weekly", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening store at %s: %w", cfg.DBPath, err)
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		return fmt.Errorf("migrating store: %w", err)
	}

	since := time.Now().UTC().Add(-weeklyReportWindow).Format(time.RFC3339)
	decisions, err := store.RouterDecisionsSince(db, since)
	if err != nil {
		return fmt.Errorf("querying router decisions: %w", err)
	}
	divergences, err := store.RouterStateDivergences(db)
	if err != nil {
		return fmt.Errorf("querying router state divergences: %w", err)
	}

	summary := summarizeWeekly(decisions, divergences)
	writeWeeklyReport(os.Stdout, summary)
	return nil
}

// summarizeWeekly aggregates decisions (already filtered to the report
// window) and the current divergence rows into a weeklySummary.
func summarizeWeekly(decisions []store.RouterDecisionRow, divergences []store.RouterStateRow) weeklySummary {
	var s weeklySummary
	s.Divergences = divergences

	for _, d := range decisions {
		stats := decodeStats(d.Stats)

		switch d.Decision {
		case router.DecisionLocal:
			outTok := statInt(stats, "local_output_tokens")
			s.FrontierTokensAvoided += outTok
			frontierModel := statString(stats, "frontier_model")
			s.EstimatedCostSavedUSD += feature.PricingFor(frontierModel).Cost(0, outTok)

		case router.DecisionEscalated:
			s.QualityIncidents++

		case router.DecisionShadow:
			s.ShadowTotal++
			if agree, ok := stats["shadow_agree"].(bool); ok {
				if !agree {
					s.ShadowDisagreed++
				}
			} else {
				s.ShadowPending++
			}
		}
	}
	return s
}

// decodeStats parses a router_decisions.stats JSON column, tolerating an
// invalid or NULL value (returns an empty map rather than erroring the
// whole report over one bad row).
func decodeStats(raw sql.NullString) map[string]any {
	stats := map[string]any{}
	if !raw.Valid || raw.String == "" {
		return stats
	}
	if err := json.Unmarshal([]byte(raw.String), &stats); err != nil {
		return map[string]any{}
	}
	return stats
}

func writeWeeklyReport(w io.Writer, s weeklySummary) {
	fmt.Fprintf(w, "splitter report weekly (last %s)\n\n", weeklyReportWindow)
	fmt.Fprintf(w, "frontier tokens avoided: %d\n", s.FrontierTokensAvoided)
	fmt.Fprintf(w, "estimated cost saved:    $%.4f\n", s.EstimatedCostSavedUSD)
	fmt.Fprintf(w, "quality incidents:       %d (escalations)\n", s.QualityIncidents)

	if s.ShadowTotal == 0 {
		fmt.Fprintln(w, "drift check:             no dual-dispatched turns this period")
	} else {
		decided := s.ShadowTotal - s.ShadowPending
		rate := 0.0
		if decided > 0 {
			rate = float64(s.ShadowDisagreed) / float64(decided) * 100
		}
		fmt.Fprintf(w, "drift check:              %d/%d shadow turns disagreed (%.1f%%), %d still pending comparison\n",
			s.ShadowDisagreed, decided, rate, s.ShadowPending)
	}

	fmt.Fprintln(w)
	if len(s.Divergences) == 0 {
		fmt.Fprintln(w, "per-version divergence flags: none")
		return
	}
	fmt.Fprintln(w, "per-version divergence flags:")
	for _, d := range s.Divergences {
		fmt.Fprintf(w, "  %s / %s: %s\n", d.Category, d.Families, d.DisabledReason)
	}
}

func statInt(stats map[string]any, key string) int64 {
	v, ok := stats[key].(float64)
	if !ok {
		return 0
	}
	return int64(v)
}

func statString(stats map[string]any, key string) string {
	v, _ := stats[key].(string)
	return v
}
