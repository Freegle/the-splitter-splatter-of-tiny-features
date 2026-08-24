// Command splitter judge submits the verification cascade's queued
// middle-band items to the Anthropic Message Batches API ("judge submit")
// and, on a later invocation, checks for and applies their results
// ("judge poll"). Cron drives repeated invocations of both; neither loops
// internally. See internal/judge.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/judge"
	"github.com/freegle/splitter/internal/store"
)

func init() {
	register("judge", runJudge)
}

// runJudge dispatches to the "submit" or "poll" sub-command named by
// args[0].
func runJudge(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: splitter judge <submit|poll> [args]")
	}

	switch args[0] {
	case "submit":
		return runJudgeSubmit(args[1:])
	case "poll":
		return runJudgePoll(args[1:])
	default:
		return fmt.Errorf("unknown judge sub-command %q, want submit or poll", args[0])
	}
}

// runJudgeSubmit loads every queued judge_items row and submits it as one
// Message Batches API request.
func runJudgeSubmit(args []string) error {
	fs := flag.NewFlagSet("judge submit", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, db, err := loadJudgeConfigAndStore(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := judge.Submit(context.Background(), db, cfg)
	if err != nil {
		return fmt.Errorf("submitting judge batch: %w", err)
	}

	if result.ItemCount == 0 {
		fmt.Println("no queued judge items")
		return nil
	}
	fmt.Printf("submitted %d judge item(s) as batch %s\n", result.ItemCount, result.BatchID)
	return nil
}

// runJudgePoll checks every judge_batches row not yet completed, once per
// invocation, applying results for any that have ended.
func runJudgePoll(args []string) error {
	fs := flag.NewFlagSet("judge poll", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, db, err := loadJudgeConfigAndStore(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := judge.Poll(context.Background(), db, cfg)
	if err != nil {
		return fmt.Errorf("polling judge batches: %w", err)
	}

	fmt.Printf("checked %d batch(es), %d ended, %d item(s) succeeded, %d errored, %d input tokens, %d output tokens\n",
		result.BatchesChecked, result.BatchesEnded, result.ItemsSucceeded, result.ItemsErrored, result.InputTokens, result.OutputTokens)
	return nil
}

// loadJudgeConfigAndStore loads splitter's config and opens/migrates its
// store, returning the judge.Config subset the judge sub-commands need.
// The caller owns closing the returned *sql.DB.
func loadJudgeConfigAndStore(configPath string) (judge.Config, *sql.DB, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return judge.Config{}, nil, fmt.Errorf("loading config: %w", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return judge.Config{}, nil, fmt.Errorf("opening store at %s: %w", cfg.DBPath, err)
	}
	if err := store.Migrate(db); err != nil {
		db.Close()
		return judge.Config{}, nil, fmt.Errorf("migrating store: %w", err)
	}

	jcfg := judge.Config{
		Upstream:        cfg.Upstream,
		APIKeyEnv:       cfg.Judge.APIKeyEnv,
		Model:           cfg.Judge.Model,
		MaxContextChars: cfg.Judge.MaxContextChars,
	}
	return jcfg, db, nil
}
