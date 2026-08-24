// This file adds the "agreement" report sub-command: per turn_type x
// subsystem agreement rate, sample size and top disagreement reasons from
// judge output, plus the judge's share of edit turns and its spend per 100
// replays.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/judge"
	"github.com/freegle/splitter/internal/store"
)

// judgeInputPriceUSDPerMTok and judgeOutputPriceUSDPerMTok are
// claude-haiku-4-5 Message Batches API pricing: 50% of the standard $1/$5
// per-MTok input/output rate.
const (
	judgeInputPriceUSDPerMTok  = 0.50
	judgeOutputPriceUSDPerMTok = 2.50
)

// topReasonsPerCategory bounds how many disagreement reasons the report
// prints per turn_type x subsystem category.
const topReasonsPerCategory = 3

func init() {
	registerReport("agreement", runReportAgreement)
}

// runReportAgreement prints the Phase 3 verification cascade's agreement
// report: per turn_type x subsystem agreement rate and sample size with
// its top disagreement reasons, the judge's share of edit turns (the
// acceptance check is that this stays <= 30% on real data), and the
// judge's spend per 100 replays.
func runReportAgreement(args []string) error {
	fs := flag.NewFlagSet("report agreement", flag.ContinueOnError)
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

	categories, err := store.AgreementByCategory(db)
	if err != nil {
		return fmt.Errorf("querying agreement by category: %w", err)
	}
	disagreements, err := store.DisagreementRows(db)
	if err != nil {
		return fmt.Errorf("querying disagreement rows: %w", err)
	}
	editTotal, editJudged, err := store.EditTurnJudgeCounts(db)
	if err != nil {
		return fmt.Errorf("querying edit turn judge counts: %w", err)
	}
	inputTokens, outputTokens, err := store.JudgeSpendTotals(db)
	if err != nil {
		return fmt.Errorf("querying judge spend totals: %w", err)
	}
	totalReplays, err := store.TotalReplayCount(db)
	if err != nil {
		return fmt.Errorf("querying total replay count: %w", err)
	}

	reasons := topReasonsByCategory(disagreements)
	writeAgreementTable(os.Stdout, categories, reasons)
	writeEditTurnJudgeShare(os.Stdout, editTotal, editJudged)
	writeJudgeSpend(os.Stdout, inputTokens, outputTokens, totalReplays)

	return nil
}

// writeAgreementTable renders one row per turn_type x subsystem category:
// its sample size, agreement rate and top disagreement reasons.
func writeAgreementTable(w io.Writer, categories []store.CategoryAgreement, reasons map[string][]reasonCount) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "turn_type\tsubsystem\tn\tagreement_rate\ttop_disagreement_reasons")
	for _, c := range categories {
		rate := 0.0
		if c.N > 0 {
			rate = float64(c.Agreed) / float64(c.N)
		}
		key := categoryKey(c.TurnType, c.Subsystem)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.1f%%\t%s\n", c.TurnType, c.Subsystem, c.N, rate*100, formatReasons(reasons[key]))
	}
	tw.Flush()
}

// reasonCount is one judge_verdict.reason and how many disagreeing
// verifications in a category gave it.
type reasonCount struct {
	Reason string
	Count  int
}

// categoryKey builds the map key topReasonsByCategory and
// writeAgreementTable share for one turn_type x subsystem category.
func categoryKey(turnType, subsystem string) string {
	return turnType + "|" + subsystem
}

// topReasonsByCategory groups disagreement rows by turn_type x subsystem
// and returns, per category, its top topReasonsPerCategory judge_verdict
// reasons by count (ties broken alphabetically for a deterministic order).
// A disagreeing row with no judge_verdict (decided by the exact or ast
// stage, never sent to the judge) or an unparseable one contributes no
// reason.
func topReasonsByCategory(rows []store.DisagreementReason) map[string][]reasonCount {
	counts := make(map[string]map[string]int)
	for _, row := range rows {
		if !row.JudgeVerdict.Valid || row.JudgeVerdict.String == "" {
			continue
		}
		verdict, err := judge.ParseVerdict(row.JudgeVerdict.String)
		if err != nil || verdict.Reason == "" {
			continue
		}
		key := categoryKey(row.TurnType, row.Subsystem)
		if counts[key] == nil {
			counts[key] = make(map[string]int)
		}
		counts[key][verdict.Reason]++
	}

	out := make(map[string][]reasonCount, len(counts))
	for key, byReason := range counts {
		list := make([]reasonCount, 0, len(byReason))
		for reason, n := range byReason {
			list = append(list, reasonCount{Reason: reason, Count: n})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Count != list[j].Count {
				return list[i].Count > list[j].Count
			}
			return list[i].Reason < list[j].Reason
		})
		if len(list) > topReasonsPerCategory {
			list = list[:topReasonsPerCategory]
		}
		out[key] = list
	}
	return out
}

// formatReasons renders a category's top reasons as "reason (n); reason
// (n)", or "-" when there are none.
func formatReasons(list []reasonCount) string {
	if len(list) == 0 {
		return "-"
	}
	parts := make([]string, len(list))
	for i, r := range list {
		parts[i] = fmt.Sprintf("%s (%d)", r.Reason, r.Count)
	}
	return strings.Join(parts, "; ")
}

// writeEditTurnJudgeShare prints the judge's share of verified edit turns:
// the observable input to the <= 30% acceptance check.
func writeEditTurnJudgeShare(w io.Writer, total, judged int) {
	share := 0.0
	if total > 0 {
		share = float64(judged) / float64(total) * 100
	}
	fmt.Fprintf(w, "\njudge share of edit turns: %d/%d (%.1f%%)\n", judged, total, share)
}

// writeJudgeSpend prints total judge token usage and cost scaled to a
// rate per 100 replays, at claude-haiku-4-5 batch pricing.
func writeJudgeSpend(w io.Writer, inputTokens, outputTokens, totalReplays int64) {
	costUSD := float64(inputTokens)/1e6*judgeInputPriceUSDPerMTok + float64(outputTokens)/1e6*judgeOutputPriceUSDPerMTok
	if totalReplays == 0 {
		fmt.Fprintf(w, "judge spend per 100 replays: no replays recorded yet (%d input tokens, %d output tokens, $%.4f total)\n",
			inputTokens, outputTokens, costUSD)
		return
	}
	scale := 100.0 / float64(totalReplays)
	fmt.Fprintf(w, "judge spend per 100 replays: %.0f input tokens, %.0f output tokens, $%.4f\n",
		float64(inputTokens)*scale, float64(outputTokens)*scale, costUSD*scale)
}
