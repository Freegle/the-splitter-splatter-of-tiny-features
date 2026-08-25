// Arena mode (DESIGN.md "Agentic eval v2: the real loop") drives the same
// bounded tool loop as v1, but against a live FreegleDocker worktree's real
// Docker containers instead of an ephemeral, unshare-network-denied local
// worktree: Edward, "the models must run the actual tests using the Docker
// environment, just like it did in the real session". The arena worktree is
// created once (`./freegle worktree create eval-arena`) and reused across
// tasks and sittings; every task leases it for the duration of one task
// (checked out detached at that task's repo_head, HEAD restored afterwards)
// rather than getting its own fresh worktree.
package agentic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/evals"
	"github.com/freegle/splitter/internal/store"
)

// arenaMainProjectName is the FreegleDockerWSL default COMPOSE_PROJECT_NAME
// (CLAUDE.md: "Container names: Prefixed by COMPOSE_PROJECT_NAME (default:
// freegle)"). An arena worktree's own .env must never resolve to this: it
// would mean every docker exec / lane call in this file quietly ran
// against the MAIN instance's containers instead of the isolated arena,
// breaking the worktree isolation rule DESIGN.md calls "absolute".
const arenaMainProjectName = "freegle"

// ArenaConfig is one resolved, validated arena worktree. ResolveArenaConfig
// builds it from [evals].arena_path / arena_status_port; runOneArenaTask
// never reads those config fields directly, so tests can build an
// ArenaConfig by hand without a config.Config at all.
type ArenaConfig struct {
	Path       string
	Project    string
	StatusPort int
	BaseURL    string // http://localhost:<StatusPort>, the status API base
}

// ResolveArenaConfig validates and resolves [evals].arena_path /
// arena_status_port into an ArenaConfig: arena_path must be configured,
// exist, and be a linked git worktree (never the main checkout); its .env
// must name a COMPOSE_PROJECT_NAME other than the main instance's; and its
// status API must respond. Any failure refuses to start arena mode rather
// than falling back to a guessed default, per DESIGN.md's isolation rule.
func ResolveArenaConfig(cfg *config.Config) (*ArenaConfig, error) {
	path := cfg.Evals.ArenaPath
	if path == "" {
		return nil, fmt.Errorf("[evals].arena_path is not configured")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("arena worktree %s: %w (create it with ./freegle worktree create eval-arena)", path, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := verifyArenaIsWorktree(ctx, path); err != nil {
		return nil, err
	}

	project, err := readComposeProjectName(path)
	if err != nil {
		return nil, err
	}
	if project == arenaMainProjectName {
		return nil, fmt.Errorf("arena worktree %s COMPOSE_PROJECT_NAME is %q, the main instance's project name; refusing to run arena mode against it", path, project)
	}

	port := cfg.Evals.ArenaStatusPort
	if port <= 0 {
		return nil, fmt.Errorf("[evals].arena_status_port is not configured")
	}
	baseURL := fmt.Sprintf("http://localhost:%d", port)
	if err := probeStatusAPI(http.DefaultClient, baseURL); err != nil {
		return nil, fmt.Errorf("arena status API at %s did not respond: %w", baseURL, err)
	}

	return &ArenaConfig{Path: path, Project: project, StatusPort: port, BaseURL: baseURL}, nil
}

// gitOutput runs `git <args>` in dir and returns its combined output,
// erroring (with that output folded into the error text) on a nonzero
// exit: every call site here treats a git plumbing failure as fatal to
// resolving or leasing the arena, unlike CommandRunner's graded-outcome
// convention for the model's own commands.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

func gitRun(ctx context.Context, dir string, args ...string) error {
	_, err := gitOutput(ctx, dir, args...)
	return err
}

// verifyArenaIsWorktree refuses arenaPath unless it is a linked git
// worktree, never the main checkout that owns the shared .git directory
// (DESIGN.md: "refuse otherwise"). A linked worktree's --show-toplevel
// differs from its main checkout's directory (the parent of
// --git-common-dir); the main checkout's own toplevel equals that same
// directory, since its .git lives directly inside it. This holds
// regardless of which repository or path is involved, so nothing here is
// hardcoded to FreegleDockerWSL specifically.
func verifyArenaIsWorktree(ctx context.Context, arenaPath string) error {
	toplevel, err := gitOutput(ctx, arenaPath, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("arena_path %s is not a git working tree: %w", arenaPath, err)
	}
	commonDir, err := gitOutput(ctx, arenaPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolving arena_path %s git common dir: %w", arenaPath, err)
	}

	mainCheckout := filepath.Dir(strings.TrimSpace(commonDir))
	if strings.TrimSpace(toplevel) == mainCheckout {
		return fmt.Errorf("arena_path %s is the main checkout (%s), not a linked worktree; refusing to run arena mode against it", arenaPath, mainCheckout)
	}
	return nil
}

// arenaIsDirty reports whether arenaPath's working tree has any
// uncommitted changes (tracked or untracked): NewArenaSandbox refuses to
// touch a dirty arena rather than risk clobbering work left there.
func arenaIsDirty(ctx context.Context, arenaPath string) (bool, error) {
	out, err := gitOutput(ctx, arenaPath, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// readComposeProjectName reads COMPOSE_PROJECT_NAME from arenaPath's .env.
// There is deliberately no fallback default (unlike status-nuxt's own
// `process.env.COMPOSE_PROJECT_NAME || 'freegle'`): a missing or empty
// value must refuse arena mode, never silently resolve to the main
// instance's project name.
func readComposeProjectName(arenaPath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(arenaPath, ".env"))
	if err != nil {
		return "", fmt.Errorf("reading arena .env: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "COMPOSE_PROJECT_NAME="); ok {
			v = strings.TrimSpace(v)
			if v == "" {
				return "", fmt.Errorf("arena .env has an empty COMPOSE_PROJECT_NAME")
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("COMPOSE_PROJECT_NAME not set in arena .env")
}

// probeStatusAPI does a bare GET against baseURL to confirm the arena's
// status API is listening, per DESIGN.md/the task brief: "refuses to start
// when ... its status API does not respond".
func probeStatusAPI(client *http.Client, baseURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// arenaHeadRestoreTimeout bounds the git checkout that restores the
// arena's original HEAD in Teardown: this must complete even if the task's
// own context has already been cancelled or its deadline has passed, so
// Teardown uses a fresh background context of its own.
const arenaHeadRestoreTimeout = 30 * time.Second

// ArenaSandbox is one task's lease on the shared, persistent arena
// worktree. Unlike v1's Sandbox (a fresh ephemeral worktree per task),
// arena mode reuses the SAME checkout across every task in a run, so
// NewArenaSandbox records enough of its original state (the branch it was
// on, or its exact commit if it was already detached) to check it back out
// afterwards.
type ArenaSandbox struct {
	Dir string

	originalBranch string // empty when the arena was already detached
	originalSHA    string
}

// NewArenaSandbox refuses a dirty arena worktree, records its current
// HEAD, checks out repoHead detached, and returns the sandbox. Callers
// must call Teardown (typically via defer, immediately after a successful
// return) exactly once, to restore the original HEAD regardless of how the
// task using it turns out.
func NewArenaSandbox(ctx context.Context, arenaPath, repoHead string) (*ArenaSandbox, error) {
	dirty, err := arenaIsDirty(ctx, arenaPath)
	if err != nil {
		return nil, fmt.Errorf("checking arena worktree cleanliness: %w", err)
	}
	if dirty {
		return nil, fmt.Errorf("arena worktree %s has uncommitted changes; refusing to touch it", arenaPath)
	}

	sha, err := gitOutput(ctx, arenaPath, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("reading arena HEAD: %w", err)
	}
	// A detached arena reports no symbolic ref; that failure is expected
	// and not fatal, so its error is discarded rather than propagated.
	branch, _ := gitOutput(ctx, arenaPath, "symbolic-ref", "-q", "--short", "HEAD")

	if err := gitRun(ctx, arenaPath, "checkout", "--detach", repoHead); err != nil {
		return nil, fmt.Errorf("checking out task commit %s in arena: %w", repoHead, err)
	}

	return &ArenaSandbox{Dir: arenaPath, originalBranch: strings.TrimSpace(branch), originalSHA: strings.TrimSpace(sha)}, nil
}

// Teardown restores the arena's original HEAD (the branch it was on, or
// its exact detached commit), regardless of how the task using it turned
// out. A task leaves the working tree modified (held-out test files
// applied, the model's own edits, possibly new untracked files from a
// write tool call), so restoring HEAD alone is not enough to leave the
// arena ready for the NEXT task: Teardown also discards every tracked
// change (`git reset --hard`) and removes every untracked file/directory
// (`git clean -fd`) before reattaching to the original branch, if there
// was one. Safe to call on a nil *ArenaSandbox (a NewArenaSandbox failure).
func (a *ArenaSandbox) Teardown() error {
	if a == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), arenaHeadRestoreTimeout)
	defer cancel()

	if err := gitRun(ctx, a.Dir, "reset", "--hard", a.originalSHA); err != nil {
		return fmt.Errorf("resetting arena to its original HEAD: %w", err)
	}
	if err := gitRun(ctx, a.Dir, "clean", "-fd"); err != nil {
		return fmt.Errorf("cleaning arena working tree: %w", err)
	}
	if a.originalBranch != "" {
		if err := gitRun(ctx, a.Dir, "checkout", a.originalBranch); err != nil {
			return fmt.Errorf("reattaching arena to its original branch %s: %w", a.originalBranch, err)
		}
	}
	return nil
}

// DockerExecer runs one docker command and returns its combined output and
// exit status; err is non-nil only when the command could not be run at
// all, the same contract as CommandRunner.Run. Real code shells out to the
// docker CLI (execDockerExecer); tests inject a fake so no real Docker is
// required.
type DockerExecer interface {
	Exec(ctx context.Context, args []string) (output string, ok bool, err error)
}

// execDockerExecer is the real DockerExecer.
type execDockerExecer struct{}

func (execDockerExecer) Exec(ctx context.Context, args []string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err == nil {
		return output, true, nil
	}
	if _, isExit := err.(*exec.ExitError); isExit {
		return output, false, nil
	}
	return output, false, fmt.Errorf("running docker command: %w", err)
}

// arenaContainerFor maps a task's subsystem to the arena service its
// scoped, in-container run_tests targets, and the working directory to
// exec into (matching status-nuxt's own go.post.ts/vitest.post.ts, which
// both `docker exec -w /app`; laravel.post.ts uses no -w, relying on the
// batch image's default WORKDIR). ok is false when this subsystem has no
// known arena container.
func arenaContainerFor(subsystem string) (service, workdir string, ok bool) {
	switch subsystem {
	case "iznik-server-go":
		return "apiv2", "/app", true
	case "iznik-nuxt3":
		return "modtools-dev-local", "/app", true
	case "iznik-batch":
		return "batch", "", true
	default:
		return "", "", false
	}
}

// laneForSubsystem maps a task's subsystem to the arena status API's full
// test lane name (status-nuxt/server/api/tests/{go,vitest,laravel}.post.ts).
// The iznik-batch lane's real route is "laravel": there is no "php"
// endpoint (see DECISIONS.md, correcting the task brief's shorthand). ok is
// false when this subsystem has no full-lane check.
func laneForSubsystem(subsystem string) (lane string, ok bool) {
	switch subsystem {
	case "iznik-server-go":
		return "go", true
	case "iznik-nuxt3":
		return "vitest", true
	case "iznik-batch":
		return "laravel", true
	default:
		return "", false
	}
}

// arenaFileSyncDelay is how long ArenaRunner waits before running a test
// command, so a file just written on the host (an edit tool call, or a
// held-out test just applied) has propagated through the dev containers'
// file-sync before the container reads it (CLAUDE.md: "Dev containers:
// File sync via freegle-host-scripts"). Per the task brief: "wait up to
// ~10s after edits for file-sync before running".
const arenaFileSyncDelay = 10 * time.Second

// ArenaRunner implements TestExecutor by running command inside the
// arena's own containers via docker exec, targeting Service (and Workdir,
// when set) under Project. It never parks .git: DESIGN.md's arena section
// says "do NOT rename .git in a live worktree", since this checkout is
// shared and long-lived, unlike v1's ephemeral per-task worktree.
// ToolExecutor.runTests still applies containsAttemptedGit's output
// scanning uniformly on top of whatever RunTests returns, so that detector
// stays active even without the parking that backs it in v1. ArenaRunner
// also does not deny network: containment here comes from every model
// action being tool-mediated and run_tests only ever reaching the arena's
// own containers, per DESIGN.md.
type ArenaRunner struct {
	Project string
	Service string
	Workdir string
	Execer  DockerExecer
	Timeout time.Duration

	// Sleep is the file-sync propagation wait; nil uses time.Sleep. Tests
	// inject a no-op recorder so a run doesn't actually pause.
	Sleep func(time.Duration)
}

// RunTests implements TestExecutor.
func (r ArenaRunner) RunTests(ctx context.Context, dir, command string) (output string, ok bool, err error) {
	sleepFn := r.Sleep
	if sleepFn == nil {
		sleepFn = time.Sleep
	}
	sleepFn(arenaFileSyncDelay)

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = gradingTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return r.Execer.Exec(cctx, arenaDockerExecArgs(r.Project, r.Service, r.Workdir, command))
}

// arenaDockerExecArgs builds one `docker exec` argument list targeting
// <project>-<service>. Never touches docker's network subcommands: this
// file only ever execs a shell command inside an already-running
// container, so the arena's containers never gain any new network
// connectivity as a side effect of grading a task.
func arenaDockerExecArgs(project, service, workdir, command string) []string {
	container := project + "-" + service
	args := []string{"exec"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	return append(args, container, "sh", "-c", command)
}

// arenaLaneTimeout bounds one full-lane run via the arena status API, per
// the task brief: "30 min timeout".
const arenaLaneTimeout = 30 * time.Minute

// LaneResult is one arena status-API test lane's terminal outcome:
// DESIGN.md's final-grading full-lane check.
type LaneResult struct {
	Lane         string
	Success      bool
	FailureNames []string
	TimedOut     bool
}

// laneStatusResponse mirrors status-nuxt's GET /api/tests/<lane>/status
// response shape (status-nuxt/server/api/tests/*/status.get.ts): status is
// "running", "completed" or "failed".
type laneStatusResponse struct {
	Status  string `json:"status"`
	Success bool   `json:"success"`
	Logs    string `json:"logs"`
	Message string `json:"message"`
}

// LaneRunner drives one arena status-API test lane to completion.
type LaneRunner struct {
	BaseURL      string
	Client       *http.Client
	PollInterval time.Duration
}

// Run starts lane (POST /api/tests/<lane>, tolerating 409 "already
// running": another task's leftover run, or a concurrent sitting) and
// polls GET /api/tests/<lane>/status until it reports a terminal status, up
// to timeout. A timeout returns LaneResult{TimedOut: true} rather than an
// error: a graded outcome, not a harness failure.
func (l LaneRunner) Run(ctx context.Context, lane string, timeout time.Duration) (LaneResult, error) {
	client := l.Client
	if client == nil {
		client = http.DefaultClient
	}
	interval := l.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	startReq, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+"/api/tests/"+lane, nil)
	if err != nil {
		return LaneResult{}, fmt.Errorf("building %s lane start request: %w", lane, err)
	}
	startResp, err := client.Do(startReq)
	if err != nil {
		return LaneResult{}, fmt.Errorf("starting %s lane: %w", lane, err)
	}
	startResp.Body.Close()
	if startResp.StatusCode != http.StatusOK && startResp.StatusCode != http.StatusConflict {
		return LaneResult{}, fmt.Errorf("starting %s lane: unexpected status %d", lane, startResp.StatusCode)
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return LaneResult{Lane: lane, TimedOut: true}, nil
		}

		st, err := pollLaneStatus(ctx, client, l.BaseURL, lane)
		if err != nil {
			return LaneResult{}, err
		}
		if st.Status == "completed" || st.Status == "failed" {
			return LaneResult{Lane: lane, Success: st.Success, FailureNames: ParseLaneFailures(lane, st.Logs)}, nil
		}

		select {
		case <-ctx.Done():
			return LaneResult{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func pollLaneStatus(ctx context.Context, client *http.Client, baseURL, lane string) (laneStatusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tests/"+lane+"/status", nil)
	if err != nil {
		return laneStatusResponse{}, fmt.Errorf("building %s lane status request: %w", lane, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return laneStatusResponse{}, fmt.Errorf("polling %s lane status: %w", lane, err)
	}
	defer resp.Body.Close()
	var st laneStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return laneStatusResponse{}, fmt.Errorf("decoding %s lane status: %w", lane, err)
	}
	return st, nil
}

// Lane failure-name patterns, matching each status API endpoint's own
// output/parsing (status-nuxt/server/api/tests/{go,vitest,laravel}.post.ts):
// go's top-level "--- FAIL: Name" (anchored at column 0 by (?m)^, so an
// indented subtest line like "    --- FAIL:" never matches, mirroring
// go.post.ts's own top-level-only exclusion); vitest's verbose reporter's
// failing line "  × name (12ms)"; PHPUnit's default text reporter's
// failure list entries "1) Class::method".
var (
	goLaneFailPattern      = regexp.MustCompile(`(?m)^--- FAIL: (\S+)`)
	vitestLaneFailPattern  = regexp.MustCompile(`(?m)^\s*[×✗✘]\s+(.+?)\s*(?:\(\d+m?s\))?\s*$`)
	laravelLaneFailPattern = regexp.MustCompile(`(?m)^\d+\)\s+(\S+::\S+)`)
)

// ParseLaneFailures extracts failing test/spec names from logs, the arena
// status API's accumulated stdout+stderr for lane. Returns nil for an
// unrecognised lane name, or for logs with no matches (a fully green lane
// has none).
func ParseLaneFailures(lane, logs string) []string {
	var re *regexp.Regexp
	switch lane {
	case "go":
		re = goLaneFailPattern
	case "vitest":
		re = vitestLaneFailPattern
	case "laravel":
		re = laravelLaneFailPattern
	default:
		return nil
	}

	seen := map[string]bool{}
	var names []string
	for _, m := range re.FindAllStringSubmatch(logs, -1) {
		name := strings.TrimSpace(m[1])
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// laneRegressions counts lane failure names that are not among the task's
// held-out test names: "no new failures in the lane".
func laneRegressions(failureNames, heldOutNames []string) int {
	heldOut := make(map[string]bool, len(heldOutNames))
	for _, n := range heldOutNames {
		heldOut[n] = true
	}
	count := 0
	for _, n := range failureNames {
		if !heldOut[n] {
			count++
		}
	}
	return count
}

// ArenaTaskEnv is everything runOneArenaTask needs beyond the task itself.
// Run() builds one per task via newArenaTaskEnv (the in-container target
// varies by subsystem); tests construct one directly with a fake Tests/Lane
// to exercise runOneArenaTask without Docker or a real status API.
type ArenaTaskEnv struct {
	ArenaPath   string
	Tests       TestExecutor
	Lane        LaneRunner
	LaneName    string // "" skips the full-lane check
	LaneTimeout time.Duration
}

// newArenaTaskEnv resolves one task's ArenaTaskEnv from arena: the
// container/workdir arenaContainerFor derives from subsystem, wired into a
// real ArenaRunner and LaneRunner. Returns an error when subsystem has no
// known arena container (arena mode cannot grade it).
func newArenaTaskEnv(arena *ArenaConfig, subsystem string) (ArenaTaskEnv, error) {
	service, workdir, ok := arenaContainerFor(subsystem)
	if !ok {
		return ArenaTaskEnv{}, fmt.Errorf("arena mode has no container mapping for subsystem %q", subsystem)
	}
	lane, _ := laneForSubsystem(subsystem)
	return ArenaTaskEnv{
		ArenaPath: arena.Path,
		Tests: ArenaRunner{
			Project: arena.Project, Service: service, Workdir: workdir,
			Execer: execDockerExecer{}, Timeout: gradingTimeout,
		},
		Lane:        LaneRunner{BaseURL: arena.BaseURL},
		LaneName:    lane,
		LaneTimeout: arenaLaneTimeout,
	}, nil
}

// runOneArenaTask runs one task's full arena lifecycle: check out
// task.RepoHead in the shared arena worktree (restoring the original HEAD
// afterwards, even on error), apply held-out tests, grade baseline, run the
// tool loop against the arena's own containers, grade final (the scoped
// held-out tests, the same fail-to-pass check as v1, plus once, the full
// relevant lane via the arena status API), score, detect cheating. It never
// returns a hard error: any failure is recorded in the returned outcome's
// Error field, matching runOneTask's convention.
func runOneArenaTask(ctx context.Context, cfg *config.Config, doReplay evals.ReplayFunc, model string, task store.EvalTaskRow, testCmd string, bounds TaskBounds, env ArenaTaskEnv) taskOutcome {
	if testCmd == "" {
		return taskOutcome{Error: "no test command available for this task"}
	}

	sandbox, err := NewArenaSandbox(ctx, env.ArenaPath, task.RepoHead.String)
	if err != nil {
		return taskOutcome{Error: fmt.Sprintf("preparing arena sandbox: %v", err)}
	}
	defer sandbox.Teardown()

	hc, err := prepareHeldOutContext(task, sandbox.Dir)
	if err != nil {
		return taskOutcome{Error: err.Error(), AgenticReady: true}
	}

	var baseline GradeResult
	if testCmd != "" {
		baseline, err = RunGrading(ctx, env.Tests, sandbox.Dir, testCmd)
		if err != nil {
			return taskOutcome{Error: "baseline grading: " + err.Error(), AgenticReady: true}
		}
	}

	// Tools are rooted at the task's subsystem, not the whole monorepo:
	// the first arena leg drowned its token budget in monorepo-wide grep
	// floods (11 greps, one landing in .circleci, one edit, one test run
	// before the cap). The request text shows subsystem-prefixed paths, so
	// the executor also resolves those tolerantly (StripPrefix below).
	toolRoot := sandbox.Dir
	if task.Subsystem.Valid && task.Subsystem.String != "" {
		toolRoot = filepath.Join(sandbox.Dir, task.Subsystem.String)
	}
	exec := NewToolExecutor(toolRoot, env.Tests, testCmd)
	if task.Subsystem.Valid {
		exec.StripPrefix = task.Subsystem.String + "/"
	}
	loopResult := RunLoop(ctx, doReplay, exec, task.Brief, bounds)

	transcriptZstd, cerr := store.Compress(loopResult.TranscriptJSON)
	if cerr != nil {
		transcriptZstd = nil
	}

	var final GradeResult
	if testCmd != "" {
		final, err = RunGrading(ctx, env.Tests, sandbox.Dir, testCmd)
	}
	if err != nil {
		return taskOutcome{
			Error: "final grading: " + err.Error(), AgenticReady: true,
			Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
			CheatFlags: exec.CheatFlags(), TranscriptZstd: transcriptZstd, ModelRanTests: exec.TestsRanByModel(),
		}
	}

	testsRan, testsPassed, _ := ScoreFailToPass(baseline, final, hc.Names)

	judgeVerdictJSON := ""
	judgePassed := false
	if testCmd == "" {
		diff, derr := arenaWorktreeDiff(sandbox.Dir)
		switch {
		case derr != nil:
			return taskOutcome{
				Error: "capturing candidate diff: " + derr.Error(), AgenticReady: true,
				Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
				CheatFlags: exec.CheatFlags(), TranscriptZstd: transcriptZstd, ModelRanTests: exec.TestsRanByModel(),
			}
		case strings.TrimSpace(diff) == "":
			judgeVerdictJSON = `{"equivalent":false,"confidence":1,"reason":"candidate made no change to the working tree"}`
		default:
			refJSON, rerr := store.Decompress(task.ReferenceResponseZstd)
			if rerr != nil {
				return taskOutcome{Error: "decompressing reference: " + rerr.Error(), AgenticReady: true}
			}
			verdict, jerr := evals.JudgeCandidateChange(ctx, cfg, cfg.Evals.JudgeModel, task.Brief, refJSON, diff)
			if jerr != nil {
				return taskOutcome{
					Error: "judging candidate diff: " + jerr.Error(), AgenticReady: true,
					Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
					CheatFlags: exec.CheatFlags(), TranscriptZstd: transcriptZstd, ModelRanTests: exec.TestsRanByModel(),
				}
			}
			judgePassed = verdict.Agree()
			if vj, merr := json.Marshal(verdict); merr == nil {
				judgeVerdictJSON = string(vj)
			}
		}
	}

	regressions := 0
	laneErrText := ""
	if env.LaneName != "" {
		laneResult, lerr := env.Lane.Run(ctx, env.LaneName, env.LaneTimeout)
		switch {
		case lerr != nil:
			laneErrText = fmt.Sprintf("running %s lane: %v", env.LaneName, lerr)
		case laneResult.TimedOut:
			laneErrText = fmt.Sprintf("%s lane timed out after %s", env.LaneName, env.LaneTimeout)
		default:
			regressions = laneRegressions(laneResult.FailureNames, hc.Names)
			if regressions > 0 {
				laneErrText = fmt.Sprintf("%s lane regressions: %s", env.LaneName, strings.Join(laneResult.FailureNames, ", "))
			}
		}
	}

	cheatFlags := exec.CheatFlags()
	if flag := detectSuspectCopyForTask(task, cfg, model, sandbox.Dir, hc); flag != nil {
		cheatFlags = append(cheatFlags, *flag)
	}

	errText := loopResult.Error
	if laneErrText != "" {
		if errText != "" {
			errText += "; " + laneErrText
		} else {
			errText = laneErrText
		}
	}

	// Bounds stop the loop but execution results decide the grade. Tasks
	// with held-out tests are graded fail-to-pass; tasks without are
	// graded by the judge over the model's actual working-tree diff. The
	// lane guards regressions in either case when one exists.
	var passed bool
	if testCmd != "" {
		passed = testsRan > 0 && testsPassed == testsRan && regressions == 0 && laneErrText == ""
	} else {
		passed = judgePassed && regressions == 0 && laneErrText == ""
	}

	return taskOutcome{
		Passed: passed, TestsRan: testsRan, TestsPassed: testsPassed, Regressions: regressions,
		Turns: loopResult.Turns, TokensIn: loopResult.TokensIn, TokensOut: loopResult.TokensOut,
		CheatFlags: cheatFlags, Error: errText, TranscriptZstd: transcriptZstd, AgenticReady: true,
		ModelRanTests: exec.TestsRanByModel(), JudgeVerdictJSON: judgeVerdictJSON,
	}
}

// arenaWorktreeDiff returns the arena checkout's uncommitted diff: the
// candidate's actual change, including new files (intent-to-add so
// untracked files appear in the diff).
func arenaWorktreeDiff(dir string) (string, error) {
	add := exec.Command("git", "-C", dir, "add", "-AN")
	if out, err := add.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add -AN: %v: %s", err, out)
	}
	cmd := exec.Command("git", "-C", dir, "diff")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return string(out), nil
}
