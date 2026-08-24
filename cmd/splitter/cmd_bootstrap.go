package main

import (
	"database/sql"
	"os"

	"context"
	"flag"
	"fmt"
	"github.com/freegle/splitter/internal/router"
	"strings"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
	"github.com/freegle/splitter/internal/judge"
	"github.com/freegle/splitter/internal/store"
)

func init() {
	register("bootstrap", runBootstrap)
}

// runBootstrap is the evals-first onboarding path: build an evaluation set
// from the target repo's own history, rewrite the briefs so commit
// messages do not give the fix away, evaluate the named cheaper backends
// against it, and fold the results into router statistics, so a fresh
// install has routing candidates the same day instead of after weeks of
// passive capture.
func runBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml")
	backends := fs.String("backends", "", "comma-separated backend names to evaluate (required)")
	since := fs.String("since", "", "seed commits since this date, YYYY-MM-DD (default: 2 years back)")
	maxTasks := fs.Int("max", 50, "maximum tasks to seed")
	skipBriefs := fs.Bool("skip-briefs", false, "skip the reverse-briefs rewrite (results will carry commit_subject briefs, which leak the fix's framing)")
	briefWait := fs.Duration("brief-wait", 20*time.Minute, "how long to wait for the reverse-briefs batch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *backends == "" {
		fs.Usage()
		return fmt.Errorf("bootstrap: -backends is required (e.g. -backends ollama,deepseek)")
	}

	step := func(n int, msg string) { fmt.Printf("\n== bootstrap step %d: %s\n", n, msg) }

	step(1, "seeding evaluation tasks from git history")
	seedArgs := []string{"seed-history", "-max", fmt.Sprint(*maxTasks)}
	if *configPath != "" {
		seedArgs = append(seedArgs, "-config", *configPath)
	}
	if *since != "" {
		seedArgs = append(seedArgs, "-since", *since)
	}
	if err := commands["eval"](seedArgs); err != nil {
		return fmt.Errorf("bootstrap: seeding history: %w", err)
	}

	var bootDB *sql.DB
	var bootCfg *config.Config
	if !*skipBriefs {
		step(2, "rewriting briefs so commit messages do not give the fix away")
		cfg, err := config.Load(*configPath)
		if err != nil {
			return fmt.Errorf("bootstrap: loading config: %w", err)
		}
		db, err := store.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("bootstrap: opening store: %w", err)
		}
		defer db.Close()
		bootDB, bootCfg = db, cfg
		jcfg := judge.Config{
			Upstream:        cfg.Upstream,
			APIKeyEnv:       cfg.Judge.APIKeyEnv,
			Model:           cfg.Judge.Model,
			MaxContextChars: cfg.Judge.MaxContextChars,
		}
		ctx := context.Background()
		sub, err := evals.ReverseBriefsSubmit(ctx, db, jcfg)
		if err != nil {
			return fmt.Errorf("bootstrap: submitting reverse-briefs: %w", err)
		}
		fmt.Printf("  submitted %d brief(s) for rewriting\n", sub.ItemCount)
		if sub.ItemCount > 0 {
			deadline := time.Now().Add(*briefWait)
			for {
				time.Sleep(30 * time.Second)
				poll, err := evals.ReverseBriefsPoll(ctx, db, jcfg)
				if err != nil {
					return fmt.Errorf("bootstrap: polling reverse-briefs: %w", err)
				}
				if poll.BatchesChecked == 0 || poll.BatchesEnded == poll.BatchesChecked {
					fmt.Printf("  rewritten: %d, errored: %d\n", poll.Rewritten, poll.Errored)
					break
				}
				if time.Now().After(deadline) {
					fmt.Printf("  batch still running after %s; continuing with current briefs (rerun `splitter eval reverse-briefs -poll` later)\n", briefWait)
					break
				}
			}
		}
	} else {
		step(2, "skipping reverse-briefs (per -skip-briefs)")
	}

	stepN := 3
	for _, b := range strings.Split(*backends, ",") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		step(stepN, "evaluating backend "+b)
		runArgs := []string{"run", "-backend", b}
		if *configPath != "" {
			runArgs = append(runArgs, "-config", *configPath)
		}
		if err := commands["eval"](runArgs); err != nil {
			return fmt.Errorf("bootstrap: evaluating %s: %w", b, err)
		}
		stepN++
	}

	step(stepN, "computing routing candidates from the evidence so far")
	// Reuses the connection opened for reverse-briefs above (or opens one
	// now if -skip-briefs left it nil): a fresh store.Open at this point
	// has twice hit a transient WAL disk I/O error right after the heavy
	// eval write load, so the registry's own "router update" is not used.
	if bootDB == nil {
		cfg, err := config.Load(*configPath)
		if err != nil {
			return fmt.Errorf("bootstrap: loading config: %w", err)
		}
		bootDB, err = store.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("bootstrap: opening store: %w", err)
		}
		defer bootDB.Close()
		bootCfg = cfg
	}
	result, err := router.Update(bootDB, bootCfg)
	if err != nil {
		return fmt.Errorf("bootstrap: router update: %w", err)
	}
	writeRouterUpdateTable(os.Stdout, result)

	fmt.Println("\nbootstrap done. Routable rows above are your candidates; categories")
	fmt.Println("below the bar need more tasks (seed more history, raise -max) or more")
	fmt.Println("evidence from live capture. Live routing stays off until SPLITTER_ROUTE=on.")
	return nil
}
