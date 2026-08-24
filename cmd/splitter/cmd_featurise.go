package main

import (
	"flag"
	"fmt"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/feature"
	"github.com/freegle/splitter/internal/store"
)

func init() {
	register("featurise", runFeaturise)
}

// runFeaturise runs the Phase 2 featuriser over the call log: it processes
// calls with no features row yet, plus existing rows whose
// had_error_followup is still unknown, or (with --refresh) every call with
// a captured response. It prints the number of features rows written.
func runFeaturise(args []string) error {
	fs := flag.NewFlagSet("featurise", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	refresh := fs.Bool("refresh", false, "reprocess every call with a captured response, not just calls needing features")
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

	processed, err := feature.Run(db, cfg.RepoPath, *refresh)
	if err != nil {
		return fmt.Errorf("running featuriser: %w", err)
	}

	fmt.Printf("processed %d call(s)\n", processed)
	return nil
}
