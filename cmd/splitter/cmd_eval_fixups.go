package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
	"github.com/freegle/splitter/internal/store"
)

// runEvalRefreshRequests rebuilds history tasks' synthesized requests with
// the current prompt template and briefs (eval refresh-requests).
func runEvalRefreshRequests(args []string) error {
	fs := flag.NewFlagSet("eval refresh-requests", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		return err
	}
	sum, err := evals.RefreshRequests(db, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("refresh-requests: considered %d, refreshed %d, skipped %d\n", sum.Considered, sum.Refreshed, sum.Skipped)
	return nil
}

// runEvalJudgeFails re-grades failed eval results with a judge model
// (eval judge-fails).
func runEvalJudgeFails(args []string) error {
	fs := flag.NewFlagSet("eval judge-fails", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml")
	model := fs.String("model", "", "judge model (default: [judge].model)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		return err
	}
	m := *model
	if m == "" {
		m = cfg.Evals.JudgeModel
	}
	if m == "" {
		m = cfg.Judge.Model
	}
	sum, err := evals.JudgeFails(context.Background(), db, cfg, m)
	if err != nil {
		return err
	}
	fmt.Printf("judge-fails: considered %d, judged %d, flipped to pass %d, flipped to fail %d, errored %d\n", sum.Considered, sum.Judged, sum.FlippedToPass, sum.FlippedToFail, sum.Errored)
	return nil
}
