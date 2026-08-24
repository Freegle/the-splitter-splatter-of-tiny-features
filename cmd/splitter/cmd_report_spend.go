package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/store"
)

func init() {
	registerReport("spend", runReportSpend)
}

// runReportSpend prints token totals and estimated cost grouped by
// turn_type: the business case for the featuriser, showing where the
// tokens (and money) go.
func runReportSpend(args []string) error {
	fs := flag.NewFlagSet("report spend", flag.ContinueOnError)
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

	rows, err := store.SpendByTurnType(db)
	if err != nil {
		return fmt.Errorf("querying spend: %w", err)
	}

	writeSpendTable(os.Stdout, feature.Spend(rows))
	return nil
}

// writeSpendTable renders summaries as a text table, one row per turn_type
// plus a TOTAL row, to w.
func writeSpendTable(w io.Writer, summaries []feature.SpendSummary) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "turn_type\tcalls\tcontext_tokens\toutput_tokens\test_cost_usd")

	var totalCalls int
	var totalContext, totalOutput int64
	var totalCost float64
	for _, s := range summaries {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%.4f\n", s.TurnType, s.Calls, s.ContextTokens, s.OutputTokens, s.CostUSD)
		totalCalls += s.Calls
		totalContext += s.ContextTokens
		totalOutput += s.OutputTokens
		totalCost += s.CostUSD
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\t%.4f\n", totalCalls, totalContext, totalOutput, totalCost)

	tw.Flush()
}
