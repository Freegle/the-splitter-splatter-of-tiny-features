// This file adds the "router" command: currently just "router update",
// dispatched the same way "judge submit|poll" dispatches in cmd_judge.go.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/router"
	"github.com/freegle/splitter/internal/store"
)

func init() {
	register("router", runRouter)
}

// runRouter dispatches to the "update" sub-command named by args[0].
func runRouter(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: splitter router update [args]")
	}
	switch args[0] {
	case "update":
		return runRouterUpdate(args[1:])
	default:
		return fmt.Errorf("unknown router sub-command %q, want update", args[0])
	}
}

// runRouterUpdate recomputes every router_state row from verifications x
// features (internal/router.Update) and prints the resulting table, with
// any per-exact-version divergence flags.
func runRouterUpdate(args []string) error {
	fs := flag.NewFlagSet("router update", flag.ContinueOnError)
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

	result, err := router.Update(db, cfg)
	if err != nil {
		return fmt.Errorf("updating router state: %w", err)
	}

	writeRouterUpdateTable(os.Stdout, result)
	return nil
}

// writeRouterUpdateTable renders one row per (category, families) group,
// plus a divergence flag line for each flagged version.
func writeRouterUpdateTable(w io.Writer, result *router.UpdateResult) {
	if len(result.Rows) == 0 {
		fmt.Fprintln(w, "no decided verifications yet: nothing to update")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "category\tfamilies\tn\tagreed\tagreement_rate\twilson_lb\troutable\tdisabled_reason")
	for _, row := range result.Rows {
		rate := 0.0
		if row.N > 0 {
			rate = float64(row.Agreed) / float64(row.N) * 100
		}
		disabled := row.DisabledReason
		if disabled == "" {
			disabled = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.1f%%\t%.4f\t%v\t%s\n",
			row.Category, row.Families, row.N, row.Agreed, rate, row.WilsonLB, row.Routable, disabled)
	}
	tw.Flush()

	var diverged int
	for _, row := range result.Rows {
		for _, d := range row.Diverged {
			diverged++
			fmt.Fprintf(w, "divergence flagged: %s / %s: version %s has n=%d agreement=%.1f%% vs family %.1f%% (recomputed from this version's rows only)\n",
				row.Category, row.Families, d.Version, d.N, d.AgreementRate*100, d.FamilyRate*100)
		}
	}
	if diverged == 0 {
		fmt.Fprintln(w, "no per-exact-version divergence flagged")
	}
}
