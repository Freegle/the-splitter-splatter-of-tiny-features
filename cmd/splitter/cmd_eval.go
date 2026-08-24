// Command splitter eval manages the eval library (internal/evals): a
// growing set of tasks pinned to a git commit plus a brief, harvested from
// live capture or seeded from git history, replayed against any backend
// and scored with the same verification cascade Phase 3 uses.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
	"github.com/freegle/splitter/internal/judge"
	"github.com/freegle/splitter/internal/store"
)

func init() {
	register("eval", runEval)
}

// runEval dispatches to the eval sub-command named by args[0].
func runEval(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: splitter eval <harvest|add|seed-history|reverse-briefs|run|list> [args]")
	}

	switch args[0] {
	case "harvest":
		return runEvalHarvest(args[1:])
	case "add":
		return runEvalAdd(args[1:])
	case "seed-history":
		return runEvalSeedHistory(args[1:])
	case "reverse-briefs":
		return runEvalReverseBriefs(args[1:])
	case "run":
		return runEvalRun(args[1:])
	case "list":
		return runEvalList(args[1:])
	default:
		return fmt.Errorf("unknown eval sub-command %q, want harvest, add, seed-history, reverse-briefs, run or list", args[0])
	}
}

// loadEvalConfigAndStore loads splitter's config and opens/migrates its
// store, the common setup every eval sub-command needs. The caller owns
// closing the returned *sql.DB.
func loadEvalConfigAndStore(configPath string) (*config.Config, *sql.DB, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening store at %s: %w", cfg.DBPath, err)
	}
	if err := store.Migrate(db); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("migrating store: %w", err)
	}
	return cfg, db, nil
}

// runEvalHarvest creates eval_tasks from live capture: disagreements,
// escalations and frontier error-followups always, plus -include-clean N
// sampled clean tasks.
func runEvalHarvest(args []string) error {
	fs := flag.NewFlagSet("eval harvest", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	includeClean := fs.Int("include-clean", 0, "also sample up to N clean (origin=clean) tasks")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, db, err := loadEvalConfigAndStore(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	summary, err := evals.Harvest(db, cfg, *includeClean)
	if err != nil {
		return fmt.Errorf("harvesting eval tasks: %w", err)
	}

	fmt.Println("splitter eval harvest:")
	fmt.Printf("  disagreements:      %d\n", summary.Disagreements)
	fmt.Printf("  escalations:        %d\n", summary.Escalations)
	fmt.Printf("  error followups:    %d\n", summary.ErrorFollowups)
	if *includeClean > 0 {
		fmt.Printf("  clean sampled:      %d\n", summary.CleanSampled)
	}
	fmt.Printf("  inserted:           %d\n", summary.Inserted)
	fmt.Printf("  already harvested:  %d\n", summary.Deduped)
	return nil
}

// runEvalAdd manually adds one eval task from a commit sha, a brief, and a
// frozen request (and optional reference response) JSON file.
func runEvalAdd(args []string) error {
	fs := flag.NewFlagSet("eval add", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	commit := fs.String("commit", "", "git commit sha this task is pinned to")
	brief := fs.String("brief", "", "one line: what the task was (required)")
	requestPath := fs.String("request", "", "path to a JSON file containing an Anthropic MessagesRequest (required)")
	referencePath := fs.String("reference", "", "path to a JSON file containing the reference Anthropic message (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *brief == "" {
		return fmt.Errorf("-brief is required")
	}
	if *requestPath == "" {
		return fmt.Errorf("-request is required")
	}

	requestJSON, err := os.ReadFile(*requestPath)
	if err != nil {
		return fmt.Errorf("reading -request file: %w", err)
	}
	var referenceJSON []byte
	if *referencePath != "" {
		referenceJSON, err = os.ReadFile(*referencePath)
		if err != nil {
			return fmt.Errorf("reading -reference file: %w", err)
		}
	}

	cfg, db, err := loadEvalConfigAndStore(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	id, err := evals.Add(db, cfg, *commit, *brief, requestJSON, referenceJSON)
	if err != nil {
		return fmt.Errorf("adding eval task: %w", err)
	}
	fmt.Printf("added eval task %d\n", id)
	return nil
}

// defaultSeedHistorySinceYears bounds how far back seed-history looks when
// -since is not given, per DESIGN.md.
const defaultSeedHistorySinceYears = 2

// runEvalSeedHistory seeds the eval library from the target repo's git
// history.
func runEvalSeedHistory(args []string) error {
	fs := flag.NewFlagSet("eval seed-history", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	repoPath := fs.String("repo", "", "target repo path (overrides repo_path in config)")
	since := fs.String("since", "", "only consider commits since this date, YYYY-MM-DD (default: 2 years back)")
	max := fs.Int("max", 0, "stop once this many new tasks are inserted (0 = no limit)")
	maxFiles := fs.Int("max-files", 3, "skip a commit touching more than this many eligible files")
	maxDiffLines := fs.Int("max-diff-lines", 120, "skip a commit with more than this many changed lines")
	grep := fs.String("grep", "", "only consider commits whose message matches this pattern")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, db, err := loadEvalConfigAndStore(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	repo := *repoPath
	if repo == "" {
		repo = cfg.RepoPath
	}
	sinceDate := *since
	if sinceDate == "" {
		sinceDate = time.Now().AddDate(-defaultSeedHistorySinceYears, 0, 0).Format("2006-01-02")
	}

	summary, err := evals.SeedHistory(db, cfg, evals.SeedHistoryOptions{
		RepoPath: repo, Since: sinceDate, Max: *max, MaxFiles: *maxFiles, MaxDiffLines: *maxDiffLines, Grep: *grep,
	})
	if err != nil {
		return fmt.Errorf("seeding eval history: %w", err)
	}

	fmt.Println("splitter eval seed-history:")
	fmt.Printf("  considered:             %d\n", summary.Considered)
	fmt.Printf("  inserted:               %d\n", summary.Inserted)
	fmt.Printf("  already seeded:         %d\n", summary.Deduped)
	fmt.Printf("  skipped merge/root:     %d\n", summary.SkippedMergeOrRoot)
	fmt.Printf("  skipped no code files:  %d\n", summary.SkippedNoCodeFiles)
	fmt.Printf("  skipped oversize:       %d\n", summary.SkippedOversize)
	fmt.Printf("  skipped context cap:    %d\n", summary.SkippedContextCap)
	return nil
}

// runEvalReverseBriefs submits (or, with -poll, checks) the batched
// rewrite of every commit_subject-sourced brief into a pre-fix problem
// statement, reusing internal/judge's exported batch client.
func runEvalReverseBriefs(args []string) error {
	fs := flag.NewFlagSet("eval reverse-briefs", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	poll := fs.Bool("poll", false, "poll outstanding batches and apply results, instead of submitting new ones")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, db, err := loadEvalConfigAndStore(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	jcfg := judge.Config{
		Upstream:        cfg.Upstream,
		APIKeyEnv:       cfg.Judge.APIKeyEnv,
		Model:           cfg.Judge.Model,
		MaxContextChars: cfg.Judge.MaxContextChars,
	}

	if *poll {
		result, err := evals.ReverseBriefsPoll(context.Background(), db, jcfg)
		if err != nil {
			return fmt.Errorf("polling reverse-briefs: %w", err)
		}
		fmt.Printf("checked %d batch(es), %d ended, %d rewritten, %d errored\n",
			result.BatchesChecked, result.BatchesEnded, result.Rewritten, result.Errored)
		return nil
	}

	result, err := evals.ReverseBriefsSubmit(context.Background(), db, jcfg)
	if err != nil {
		return fmt.Errorf("submitting reverse-briefs: %w", err)
	}
	if result.ItemCount == 0 {
		fmt.Println("no eligible commit_subject briefs to reverse")
		return nil
	}
	fmt.Printf("submitted %d brief(s) for reversal as batch %s\n", result.ItemCount, result.BatchID)
	return nil
}

// runEvalRun replays every active eval task against the named backend and
// scores it with the reusable verification cascade.
func runEvalRun(args []string) error {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	backendFlag := fs.String("backend", "", `backend to evaluate: a [backends] key, or "anthropic" for the native Anthropic client (required)`)
	modelFlag := fs.String("model", "", "model override (required with -backend anthropic)")
	maxTokens := fs.Int64("max-tokens", 0, "hard-cap total tokens_in+tokens_out for this run (0 = no cap)")
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

	summary, err := evals.Run(context.Background(), db, cfg, evals.RunOptions{
		Backend: *backendFlag, Model: *modelFlag, MaxTokens: *maxTokens,
	})
	if err != nil {
		return fmt.Errorf("running eval: %w", err)
	}

	printEvalRunSummary(os.Stdout, summary)
	return nil
}

// runEvalList prints every eval task with a short commit reference, its
// characteristics summary, and its per-model pass rate so far.
func runEvalList(args []string) error {
	fs := flag.NewFlagSet("eval list", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, db, err := loadEvalConfigAndStore(*configPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := evals.List(db)
	if err != nil {
		return fmt.Errorf("listing eval tasks: %w", err)
	}

	writeEvalListTable(os.Stdout, rows)
	return nil
}

func writeEvalListTable(w io.Writer, rows []evals.ListRow) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "id\torigin\tsha\tcharacteristics\tbrief\tpass_rates")
	for _, r := range rows {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Origin, r.ShortSHA, r.Characteristics, truncateOneLine(r.Brief, 60), formatPassRates(r.PassRates))
	}
	tw.Flush()
}

func truncateOneLine(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

func formatPassRates(rates map[string]evals.PassRate) string {
	if len(rates) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(rates))
	for k := range rates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		pr := rates[k]
		parts[i] = fmt.Sprintf("%s:%d/%d", k, pr.Passed, pr.Total)
	}
	return strings.Join(parts, ", ")
}

// scorecardDimensions is the print order for eval run's scorecard section.
var scorecardDimensions = []string{
	"language", "layer", "nature", "difficulty", "turn_type",
	"framework", "spec_clarity", "size_bucket", "cutoff_segment",
}

func printEvalRunSummary(w io.Writer, s *evals.RunSummary) {
	fmt.Fprintf(w, "splitter eval run: run=%d backend=%s model=%s\n", s.RunID, s.Backend, s.Model)
	fmt.Fprintf(w, "  tasks total:   %d\n", s.TasksTotal)
	fmt.Fprintf(w, "  tasks scored:  %d\n", s.TasksScored)
	fmt.Fprintf(w, "  tasks passed:  %d\n", s.TasksPassed)
	fmt.Fprintf(w, "  tasks skipped: %d (ladder futility / token budget)\n", s.TasksSkipped)
	fmt.Fprintf(w, "  tokens in/out: %d / %d\n", s.TokensIn, s.TokensOut)

	fmt.Fprintln(w, "\n  brief sources:")
	for _, k := range sortedKeys(s.BriefSources) {
		fmt.Fprintf(w, "    %-20s %d\n", k, s.BriefSources[k])
	}

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
		rungKeys := make([]string, 0, len(ts.Rungs))
		for r := range ts.Rungs {
			rungKeys = append(rungKeys, r)
		}
		sort.Strings(rungKeys)
		for _, r := range rungKeys {
			rs := ts.Rungs[r]
			fmt.Fprintf(w, "      rung %s: %d/%d passed\n", r, rs.Passed, rs.N)
		}
	}

	fmt.Fprintln(w, "\n  scorecard:")
	for _, dim := range scorecardDimensions {
		values := s.Scorecard.ByDimension[dim]
		if len(values) == 0 {
			continue
		}
		fmt.Fprintf(w, "    %s:\n", dim)
		for _, k := range sortedKeysE(values) {
			e := values[k]
			fmt.Fprintf(w, "      %-16s %d/%d\n", k, e.Passed, e.N)
		}
	}

	if s.PriorModel == "" {
		fmt.Fprintln(w, "\n  no prior run from a different model to compare against")
		return
	}
	fmt.Fprintf(w, "\n  regressions vs prior run (model=%s): %d\n", s.PriorModel, len(s.Regressions))
	for _, r := range s.Regressions {
		fmt.Fprintf(w, "    task %d (%s): %s\n", r.TaskID, r.ShortSHA, truncateOneLine(r.Brief, 80))
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysT(m map[string]evals.TrackSummary) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysE(m map[string]evals.ScorecardEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
