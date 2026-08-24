# splitter

Claude Code normally sends every request to Anthropic's frontier models. Some of
those requests genuinely need a frontier model. Plenty do not. Splitter's job is
to find out, with evidence rather than vibes, which of your everyday coding
requests a **cheaper model** could have handled just as well, and eventually to
route that slice of traffic to the cheaper model automatically, with an instant
fallback to the frontier the moment anything looks off.

"Cheaper" deliberately covers both kinds:

- **local models** running on your own machine via Ollama (marginal cost:
  electricity), and
- **cheaper hosted models** such as DeepSeek, GLM or Kimi, which cost real money
  but between 10x and 100x less than frontier rates (see `BACKENDS.md` for the
  current landscape).

The split is frontier vs cheaper, not cloud vs local.

How it learns is the important part. Nothing is routed anywhere on day one.
First it just watches: a small proxy sits between Claude Code and Anthropic,
changes nothing, and keeps a copy of every request and response. Later, while
your machine is idle, it replays those same requests against a cheaper model and
mechanically checks whether the cheaper answer would have been as good: same
text, or same code after linting and comparing the edits, with a cheap AI
tie-breaker only for the ambiguous middle. Only when a category of request has
weeks of evidence saying "the cheap model agrees with the frontier at least 90%
of the time here" does it become a candidate for live routing, and even then
behind an explicit switch, with a per-session circuit breaker and a permanent 5%
shadow-check for drift.

See `BRIEF.md` for the original intent and acceptance criteria, `DESIGN.md` for
the concrete design, `DECISIONS.md` for every judgement call made along the way,
and `BACKENDS.md` for the model/pricing research.

Single developer, single machine, WSL2. Simplicity beats generality throughout:
SQLite not Postgres, cron/systemd timers not queues, raw HTTP not provider SDKs.

## Architecture

```
Claude Code ──ANTHROPIC_BASE_URL──▶ [1 Proxy] ──▶ Anthropic API
                                       │
                                       ▼ append-only
                                  [2 Capture log (SQLite)]
                                       │ idle-time batch
                                       ▼
                                  [3 Replay worker] ──▶ Ollama / DeepSeek / any cheaper backend
                                       │ diffs
                                       ▼
                                  [4 Verification cascade] ──▶ scores back to log
                                       │
                                       ▼
                                  [5 Router] (trained offline; consulted live by the Proxy)
```

1. **Proxy** (`splitter proxy`): Claude Code is pointed at it via
   `ANTHROPIC_BASE_URL` and never notices it is there. Every request is
   forwarded verbatim, streaming included, and the proxy fails open: if
   anything inside it goes wrong, the request still goes through and only the
   logging is lost. Every `/v1/messages` call is logged with its git HEAD,
   session id and token usage.
2. **Featuriser** (`splitter featurise`): tags each logged call with what kind
   of turn it was (a single-file edit, a question, a plan...), which files and
   subsystem it touched, and whether the *next* call in the session looks like
   an error follow-up, which is the cheap signal that a model struggled.
3. **Replay + verification** (`splitter replay`, `splitter judge`): replays
   logged calls against a cheaper backend while the machine is idle and scores
   agreement mechanically: exact text match first; for code edits, both
   versions are applied in throwaway git worktrees, linted, and compared
   structurally; only the ambiguous middle band goes to a batched Claude Haiku
   judge at half price. Test results always outrank the judge's opinion.
4. **Router** (`splitter router update`, live routing in the proxy): turns the
   accumulated agreement statistics into per-category routing decisions using a
   conservative statistical bound, keyed by model *family* so a version bump
   does not reset weeks of learning. Live routing is off unless
   `SPLITTER_ROUTE=on`; any error signal after a locally-served turn disables
   that category and takes the whole session back to the frontier.
5. **Eval library** (`splitter eval ...`): a growing library of real tasks from
   this codebase's own history, including ones that tripped up frontier models,
   used to measure any new model quickly: cheap single-shot answers, or a full
   agentic mode where the candidate model gets a sandboxed worktree and must
   run and fix the repo's own tests. An easy-to-hard ladder stops spending
   tokens on models that are clearly out of their depth.

## Quick start

```
./scripts/install.sh
```

This builds `~/.local/bin/splitter`, copies `config.example.toml` to
`~/.config/splitter/config.toml` if you do not already have one, installs the
user-level systemd units, and starts the proxy plus the hourly replay timer.
Then:

1. Point Claude Code at the proxy and use it as normal:

   ```
   export ANTHROPIC_BASE_URL=http://127.0.0.1:9925
   ```

   Nothing changes about your session: same models, same streaming, same tool
   use, under 5ms added latency. Your Claude subscription login passes through
   untouched.

2. Confirm it is capturing (run a few Claude Code turns first):

   ```
   sqlite3 ~/.local/share/splitter/splitter.db 'select count(*) from calls'
   ```

3. To stop using it, unset `ANTHROPIC_BASE_URL`. Live routing additionally has
   its own switch: until you set `SPLITTER_ROUTE=on`, splitter never changes
   which model answers you, no matter what it has learned.

Everything else runs unattended on the timer: replay when the proxy has been
idle 30 minutes, judge batches, router statistics.

## A worked example

This is the sequence from a fresh install to a model scorecard, with real
output from this repo's first day. Say you want to know how bright DeepSeek V4
Flash actually is on *your* codebase, not on a public leaderboard.

Seed evaluation tasks from your repo's own git history. Each small,
self-contained historical commit becomes a task: the model gets the code as it
was *before* the fix and a plain-English brief, and its answer is compared
against what the real fix did (and, in agentic mode, graded by whether the
commit's own tests pass):

```
$ splitter eval seed-history -since 2026-08-19 -max 3
  inserted:               3

$ splitter eval seed-history -grep 'apply the pagination cursor' -since 2026-08-01 -max 1
  inserted:               1
```

Commit messages describe the fix after the fact, which would give the game
away, so have a cheap batched model rewrite each brief into what the request
would have looked like *before* the fix existed (the Discourse thread or Claude
session that prompted a fix is used directly when available):

```
$ splitter eval reverse-briefs
submitted 5 brief(s) for reversal as batch msgbatch_0158wdfw...
$ splitter eval reverse-briefs -poll
checked 1 batch(es), 1 ended, 5 rewritten, 0 errored
```

Run the ladder against the model you are curious about:

```
$ splitter eval run -backend deepseek
splitter eval run: run=1 backend=deepseek model=deepseek-v4-flash
  tasks total:   6
  tasks scored:  6
  tasks passed:  3
  tokens in/out: 35633 / 43738

  ladder:
    go: completed every rung
      rung 6: 1/1 passed
    markdown: completed every rung
      rung 2: 1/2 passed
    mixed: completed every rung
      rung 6: 0/1 passed
    php: completed every rung
      rung 6: 1/2 passed
```

That run cost about one cent. The reading: DeepSeek V4 Flash solved a
hard Go backend fix and a PHP batch fix from this repo's real history, but
failed the Vue frontend task that once also needed a couple of attempts from a
frontier model. The ladder is per-language exactly because models are lopsided
like this; a model that flunks easy Vue tasks stops being given harder Vue
tasks, so a hopeless track costs a few cents, not a benchmark bill.

The full scorecard (not shown above) breaks the same results down by language,
layer (frontend vs backend vs database...), kind of change, difficulty, brief
source, and whether the task post-dates the model's training cutoff, which is
what stops a model looking clever by having memorised your public repo.

Meanwhile the passive side accumulates on its own. After a while:

```
splitter report spend        # where your frontier tokens actually go, by turn type
splitter report agreement    # how often the cheap model matched, by category
splitter router update       # which categories now clear the routability bar
splitter report weekly       # once routing: tokens avoided, escalations, drift
```

When a category clears the bar and you are satisfied with the evidence, opt in
with `SPLITTER_ROUTE=on`. Everything else keeps going to the frontier.

## Subcommands

Every subcommand takes `-config path` (falls back to `$SPLITTER_CONFIG`, then
`~/.config/splitter/config.toml`, then built-in defaults). Run any of them with
`-h` for flags.

| Command | What it does |
|---|---|
| `splitter proxy [-listen addr]` | The pass-through logging proxy. Blocks until SIGINT/SIGTERM, then drains cleanly. |
| `splitter featurise [-refresh]` | Tags logged calls (`turn_type`, `files_touched`, `subsystem`, `had_error_followup`). Idempotent. |
| `splitter replay [-backend name] [-limit N] [-force]` | Replays logged calls against a cheaper backend and runs the verification cascade. Refuses to run until the proxy has been idle `[replay].idle_minutes`, unless `-force`. |
| `splitter judge submit` / `judge poll` | Sends the ambiguous middle band to a batched Claude Haiku judge; applies finished verdicts. Tests outrank the judge on conflict. |
| `splitter router update` | Recomputes routability per category (Wilson lower bound, family-scoped) and flags any model version diverging from its family. |
| `splitter report spend` | Token totals and estimated cost by turn type: where the money goes. |
| `splitter report agreement` | Agreement rate per category, judge share and judge spend. |
| `splitter report weekly` | Once routing: frontier tokens avoided, cost saved, escalations, drift. |
| `splitter eval seed-history [-since] [-grep] [-max] ...` | Turns small historical commits into eval tasks. |
| `splitter eval harvest [-include-clean N]` | Turns live capture into eval tasks: disagreements, escalations, the frontier's own struggles, plus sampled easy tasks as a sanity floor. |
| `splitter eval reverse-briefs [-poll]` | Rewrites commit-derived briefs into pre-fix problem statements via a cheap batch. |
| `splitter eval add -commit <sha> -brief "..." -request <file.json>` | Manually adds one task. |
| `splitter eval run -backend <name> [-model m]` | Single-shot ladder evaluation of a model over the library. |
| `splitter eval-agentic -backend <name> [-model m] [-allow-network]` | Full agentic evaluation: the model gets a sandboxed worktree and tools (read/edit/write/run_tests) and is graded by making the task's own held-out tests pass, with cheat detection (see below). |
| `splitter eval list` | The library: id, origin, commit, brief, per-model pass rates. |
| `splitter import-history [-dir path]` | One-off **best-effort bootstrap** from Claude Code's own transcripts, see below. |
| `splitter version` | Prints the build version. |

### Agentic evaluation, sandboxing and cheating

In agentic mode the candidate model works inside a throwaway git worktree of
your repo, with the network switched off (`unshare -rn`) and `.git` hidden
while tests run. Because this repo's history is public, a model could in
principle cheat by knowing or finding the real fix; splitter does not try to
disguise the repo (that is a losing game) but instead **detects** cheating:
every tool call is transcribed, and detectors flag path escapes, `.git`
snooping, smuggled git/network calls in written code, and answers
suspiciously identical to the withheld real fix. Flagged results are reported
in a separate untrusted bucket, next to results on tasks that predate the
model's training cutoff.

### `import-history` (best-effort bootstrap only)

Reads Claude Code's session transcripts (`~/.claude/projects/*/*.jsonl`) and
reconstructs approximate `calls` rows tagged `source='import'`, so a fresh
install has something to featurise and replay before live capture accumulates.
Claude Code's transcript format is internal and undocumented; this command is
deliberately the only thing that parses it, it is never part of the live
pipeline, and anything unparseable is skipped and counted rather than fatal.

## Config reference

Resolution order: `-config` flag, then `$SPLITTER_CONFIG`, then
`~/.config/splitter/config.toml`, then built-in defaults.
`config.example.toml` documents every field inline.

| Field | Meaning |
|---|---|
| `listen` | Proxy listen address, default `127.0.0.1:9925`. |
| `upstream` | Frontier API base URL the proxy forwards to. |
| `db_path` | SQLite file. Created `0600`, parent directories `0700`. |
| `repo_path` | The target codebase: where git HEAD is read and worktrees are made. |
| `env_file` | KEY=VALUE secrets file, default `~/.config/splitter/env`. Never committed; the TOML only ever names env *variables*, never holds keys. |
| `[replay]` | `backend`, `idle_minutes`, `max_concurrent_worktrees`, `batch_size`. |
| `[backends.<name>]` | One per cheaper backend: `base_url`, `api_key_env`, `model`. Ships with `ollama`, `together`, `gemini`, `openai`, `deepseek`. |
| `[judge]` | Haiku judge: `model`, `api_key_env`, `max_context_chars`. |
| `[thresholds]` | Cascade similarity thresholds plus per language/turn-type overrides. |
| `[tests]` | Optional per-subsystem test command for the verify cascade. |
| `[router]` | `min_n`, `min_wilson_lb`, `dual_dispatch_pct`. |
| `[families]` | Per-model-id override for family normalisation. |
| `[model_cutoffs]` | Model/family to training-cutoff month, for the trusted/untrusted eval split. |
| `[layers]` | Path prefix to layer name (frontend-ui, backend-api, ...) for eval characteristics. |
| `[evals]` | Ladder and eval knobs: `max_answer_tokens` (floor for candidate answers, default 16384: reasoning models spend output tokens thinking and return nothing under small budgets), `seed_context_bytes`, ladder stop parameters, agentic bounds. |

## Using your Claude subscription for the judge

The judge (and `-backend anthropic` eval runs) needs Anthropic auth. You can
use a metered API key, or your existing Claude subscription via a long-lived
token, the same `setup-token` approach used elsewhere in Freegle tooling:

```
claude setup-token          # prints a token starting sk-ant-oat...
```

Put the result in `~/.config/splitter/env` as the variable named by
`[judge].api_key_env` (default `ANTHROPIC_API_KEY`). Splitter recognises the
`sk-ant-oat` prefix automatically and authenticates with it as a subscription
bearer token instead of an API key. The proxy itself needs neither: it always
passes Claude Code's own auth through untouched.

## Model families: version bumps do not reset learning

Per-exact-model statistics would reset to zero every time a model gets a
version bump. Instead, statistics are keyed by model *family*
(`claude-opus-5` and a dated `claude-opus-5-20260101` are the same family;
so are `qwen2.5-coder:7b` and `qwen3-coder:7b`), so a new same-family version
inherits its learned routability immediately, on the assumption it behaves
similarly **until proven otherwise**, and "proven otherwise" is enforced:
`router update` tracks each exact version inside its family and flags any
version whose agreement falls well below the family aggregate, recomputing
that category from the divergent version's own rows, which can un-route it.
The eval library is the fast complement: run the ladder against the new
version the day it appears instead of waiting weeks for live replay evidence.

## Coexistence with `large-output-guard` hooks

Freegle's `large-output-guard` PreToolUse/PostToolUse hooks and splitter's
proxy operate at different layers and never interact: hooks wrap individual
tool calls inside the Claude Code process; the proxy wraps the API transport
outside it. Running both together is the normal case.

## Key provenance

- Secrets live only in `~/.config/splitter/env` (chmod 600), never in the
  TOML, the database, logs, or this repo. A pre-commit hook in the repo
  refuses anything key-shaped.
- Every key wired on this machine is Freegle-scoped (see `DECISIONS.md`):
  keys billing other projects were deliberately not used even where present.
- The judge is the only component using Anthropic auth of its own (API key or
  subscription token, above). The proxy forwards Claude Code's own auth
  untouched and holds no key.
- Each cheaper backend reads its own key from the env var its config names;
  Ollama needs none.

## Attribution

`internal/proxy` and `cmd/splitter/cmd_proxy.go` are adapted, not written from
scratch, from
[seifghazi/claude-code-proxy](https://github.com/seifghazi/claude-code-proxy)
(MIT License), pinned at commit
[`02c9c766`](https://github.com/seifghazi/claude-code-proxy/commit/02c9c766679eee75c861bbde11c6d8b5249d44a7).
Its pass-through forwarding and SSE streaming approach is reused, restructured
around splitter's own SQLite schema, async fail-open logger, git HEAD capture
and session id heuristic; its web dashboard, conversation browser and model
router were not taken. Every adapted source file carries a header comment
naming the commit; the full upstream MIT license text is in [`NOTICE`](NOTICE).
This project is licensed GPL-2.0 ([`LICENSE`](LICENSE)), matching Freegle's
`iznik` convention; MIT is GPL-compatible, so the adapted portions carry both
notices as `NOTICE` describes.
