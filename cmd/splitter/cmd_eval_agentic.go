// Command splitter eval-agentic runs the eval library's agentic mode
// (internal/agentic, DESIGN.md "Agentic eval mode"): a bounded tool loop
// driving a candidate backend through read/list/grep/edit/write/run_tests
// over a real, network-denied sandbox, graded fail-to-pass against each
// task's held-out tests.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/freegle/splitter/internal/agentic"
)

func init() {
	register("eval-agentic", runEvalAgentic)
}

func runEvalAgentic(args []string) error {
	fs := flag.NewFlagSet("eval-agentic", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	backendFlag := fs.String("backend", "", `backend to evaluate: a [backends] key, or "anthropic" for the native Anthropic client (required)`)
	modelFlag := fs.String("model", "", "model override (required with -backend anthropic)")
	limit := fs.Int("limit", 0, "maximum agentic-gradable tasks to attempt this run (0 = no limit)")
	allowNetwork := fs.Bool("allow-network", false, "skip unshare -rn network denial; marks every result untrusted (debugging, or the only way to run when unshare is unavailable)")
	maxTokens := fs.Int64("max-tokens", 0, "hard-cap total tokens_in+tokens_out for this run (0 = no cap)")
	tasksFlag := fs.String("tasks", "", "comma-separated task ids: run only these (a targeted supplement, e.g. re-sitting bound-capped tasks under new limits)")
	arena := fs.Bool("arena", false, "arena mode: run against [evals].arena_path's real FreegleDocker containers instead of an ephemeral local sandbox (DESIGN.md \"Agentic eval v2\"); refuses to start if the arena worktree or its status API is not available")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *backendFlag == "" {
		return fmt.Errorf("-backend is required")
	}

	cfg, db, err := loadEvalConfigAndStore(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var taskIDs []int64
	if *tasksFlag != "" {
		for _, part := range strings.Split(*tasksFlag, ",") {
			id, perr := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if perr != nil {
				return fmt.Errorf("-tasks: %q is not a task id", part)
			}
			taskIDs = append(taskIDs, id)
		}
	}

	summary, err := agentic.Run(context.Background(), db, cfg, agentic.RunOptions{
		Backend: *backendFlag, Model: *modelFlag, Limit: *limit, AllowNetwork: *allowNetwork, MaxTokens: *maxTokens, Arena: *arena,
		TaskIDs: taskIDs,
	})
	if err != nil {
		return fmt.Errorf("running agentic eval: %w", err)
	}

	printAgenticRunSummary(os.Stdout, summary)
	return nil
}

func printAgenticRunSummary(w io.Writer, s *agentic.RunSummary) {
	fmt.Fprintf(w, "splitter eval-agentic: run=%d backend=%s model=%s\n", s.RunID, s.Backend, s.Model)
	if s.Arena {
		fmt.Fprintln(w, "  arena mode: graded against the arena worktree's real Docker containers")
	}
	if s.AllowNetwork {
		fmt.Fprintln(w, "  -allow-network was set: every result is UNTRUSTED (network denial bypassed)")
	}
	fmt.Fprintf(w, "  tasks total:   %d\n", s.TasksTotal)
	fmt.Fprintf(w, "  tasks scored:  %d\n", s.TasksScored)
	fmt.Fprintf(w, "  tasks passed:  %d\n", s.TasksPassed)
	fmt.Fprintf(w, "  tasks skipped: %d (ladder futility / token budget)\n", s.TasksSkipped)
	if s.NotGradedNoTestCommand > 0 {
		fmt.Fprintf(w, "  not gradable (no derivable test command): %d\n", s.NotGradedNoTestCommand)
	}
	if len(s.SweptSandboxes) > 0 {
		fmt.Fprintf(w, "  swept stale sandboxes: %d\n", len(s.SweptSandboxes))
	}
	fmt.Fprintf(w, "  tokens in/out: %d / %d\n", s.TokensIn, s.TokensOut)

	fmt.Fprintln(w, "\n  ladder:")
	for _, track := range sortedKeysT(s.Ladder) {
		ts := s.Ladder[track]
		label := track
		if label == "" {
			label = "(none)"
		}
		if ts.StopRung == 0 {
			fmt.Fprintf(w, "    %s: completed every rung\n", label)
		} else {
			fmt.Fprintf(w, "    %s: stopped at rung %d (%s)\n", label, ts.StopRung, ts.Reason)
		}
	}

	fmt.Fprintln(w, "\n  scorecard by track:")
	fmt.Fprintln(w, "    track            tasks  passed  tests_ran  tests_passed  regressions  cheat_flagged  model_ran_tests")
	for _, track := range sortedTrackTallyKeys(s.ByTrack) {
		tt := s.ByTrack[track]
		label := track
		if label == "" {
			label = "(none)"
		}
		fmt.Fprintf(w, "    %-16s %5d  %6d  %9d  %12d  %11d  %13d  %15d\n",
			label, tt.Tasks, tt.Passed, tt.TestsRan, tt.TestsPassed, tt.Regressions, tt.CheatFlagged, tt.ModelRanTests)
	}
}

func sortedTrackTallyKeys(m map[string]*agentic.TrackTally) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
