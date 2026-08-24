package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/replay"
	"github.com/freegle/splitter/internal/store"
)

func init() {
	register("replay", runReplay)
}

// runReplay runs one Phase 3 replay batch: translate and dispatch every
// eligible logged call to a replay backend, then score each new replay
// with the verification cascade.
func runReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	backendFlag := fs.String("backend", "", "replay backend name (overrides [replay].backend)")
	limit := fs.Int("limit", 0, "maximum calls to replay this run (overrides [replay].batch_size)")
	force := fs.Bool("force", false, "run even when the idle gate would otherwise refuse")
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

	summary, err := replay.Run(context.Background(), db, cfg, replay.Options{
		Backend: *backendFlag,
		Limit:   *limit,
		Force:   *force,
	})
	if err != nil {
		return err
	}

	printReplaySummary(summary)
	return nil
}

func printReplaySummary(s *replay.Summary) {
	fmt.Printf("splitter replay: backend=%s model=%s\n", s.Backend, s.Model)
	fmt.Printf("  calls selected:      %d\n", s.CallsSelected)
	fmt.Printf("  replies ok:          %d\n", s.RepliesOK)
	fmt.Printf("  replies error:       %d\n", s.RepliesError)
	fmt.Printf("  stage exact:         %d\n", s.StageExact)
	fmt.Printf("  stage ast:           %d\n", s.StageAST)
	fmt.Printf("  agreed:              %d\n", s.Agreed)
	fmt.Printf("  disagreed:           %d\n", s.Disagreed)
	fmt.Printf("  queued for judge:    %d\n", s.Banded)
	if s.CascadeErrors > 0 {
		fmt.Printf("  cascade errors:      %d\n", s.CascadeErrors)
	}
	if len(s.SweptWorktrees) > 0 {
		fmt.Printf("  swept stale worktrees: %d\n", len(s.SweptWorktrees))
	}
}
