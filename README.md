# splitter

A local/frontier model routing framework for Claude Code. It learns which
Claude Code requests a local model can serve as well as the frontier
model, so a whitelisted slice of everyday coding traffic can eventually be
routed to a local model at zero marginal cost, with automatic escalation
back to the frontier the moment a session shows the local model struggling.
See `BRIEF.md` for the full intent and acceptance criteria, `DESIGN.md` for
the concrete design, and `DECISIONS.md` for every ambiguity resolved along
the way.

Single developer, single machine, WSL2. Simplicity beats generality
throughout: SQLite not Postgres, cron/systemd timers not queues, raw HTTP
not provider SDKs.

## Architecture

```
Claude Code ──ANTHROPIC_BASE_URL──▶ [1 Proxy] ──▶ Anthropic API
                                       │
                                       ▼ append-only
                                  [2 Capture log (SQLite)]
                                       │ idle-time batch
                                       ▼
                                  [3 Replay worker] ──▶ Ollama / other backends
                                       │ diffs
                                       ▼
                                  [4 Verification cascade] ──▶ scores back to log
                                       │
                                       ▼
                                  [5 Router] (trained offline; later consulted live by Proxy)
```

Five components, built in phases (`BRIEF.md`):

1. **Proxy** (`splitter proxy`) sits in front of `api.anthropic.com`. Claude
   Code is pointed at it via `ANTHROPIC_BASE_URL` and never notices it is
   there: every request is forwarded verbatim, streaming included, and
   fails open (a proxy-internal error forwards the request anyway and logs
   the error, it never breaks a coding session). Every `POST /v1/messages`
   call is logged to SQLite alongside its git HEAD, session id and token
   usage.
2. **Featuriser** (`splitter featurise`) tags each logged call with
   routing-relevant features: `turn_type`, files touched, subsystem, token
   counts, and whether the next call in the session looks like an error
   follow-up.
3. **Replay worker + verification cascade** (`splitter replay`,
   `splitter judge submit|poll`) replays logged calls against a local (or
   other configured) backend offline, and mechanically scores agreement:
   exact text match, then AST/lint/test comparison in ephemeral git
   worktrees, then (for the ambiguous middle band only) a cheap batched
   Claude Haiku judge call.
4. **Router** (Phase 4, design finished in `DESIGN.md`, not yet
   implemented in this checkout: see [Implementation status](#implementation-status))
   turns accumulated agreement statistics into routing decisions, with a
   session-level circuit breaker on any escalation signal and a hard
   `SPLITTER_ROUTE` kill switch.
5. **Eval library** (design finished, not yet implemented: see
   [Implementation status](#implementation-status)) builds a persistent
   library of tasks that have tripped up models on this codebase (from
   live disagreements, escalations, git archaeology, and Discourse
   reports), then evaluates new models against it, easy tasks first, until
   a per-track ladder hits futility.

## Quick start

```
./scripts/install.sh
```

This builds `~/.local/bin/splitter`, copies `config.example.toml` to
`~/.config/splitter/config.toml` if you do not already have one, installs
the user-level systemd units under `systemd/`, and starts the proxy plus
the hourly replay timer. It prints the exact next steps when it finishes;
in short:

1. Point Claude Code at the proxy:

   ```
   export ANTHROPIC_BASE_URL=http://127.0.0.1:9925
   ```

   Put that wherever your shell profile lives (or export it just for a
   session) and start Claude Code as normal. It behaves identically,
   including streaming and tool use: the proxy adds under 5ms p50 and
   fails open on any internal error.

2. Watch the capture log grow:

   ```
   sqlite3 ~/.local/share/splitter/splitter.db 'select count(*) from calls'
   ```

   Run that again after a few Claude Code turns; the count should have
   gone up. The acceptance check from `BRIEF.md` Phase 1: a sampled row
   round-trips to valid JSON, which you can confirm directly:

   ```
   sqlite3 ~/.local/share/splitter/splitter.db \
     "select hex(request_zstd) from calls order by id desc limit 1" | \
     xxd -r -p | zstd -d | python3 -m json.tool | head
   ```

3. Kill switch: unset `ANTHROPIC_BASE_URL` (or point it back at
   `https://api.anthropic.com`) to bypass the proxy entirely at any point.
   Phase 4 live routing has its own separate switch,
   `SPLITTER_ROUTE=on`; anything else (including unset) is pure
   pass-through, so installing and running splitter today changes nothing
   about which model answers your prompts until you explicitly opt a
   category in once the Phase 3 report supports it.

Everything after that runs unattended: the timer replays newly logged
calls against the local model whenever the proxy has been idle for 30
minutes (`[replay].idle_minutes`), submits and polls the middle-band judge
batch, and (once built) recomputes router statistics. See
`systemd/splitter-replay.service` for the exact chain and why its first
step is allowed to "fail" harmlessly on a busy day.

## Subcommands

Every subcommand takes `-config path` (falls back to `$SPLITTER_CONFIG`,
then `~/.config/splitter/config.toml`, then built-in defaults).
Implemented in this checkout, run `splitter <command>` with no further
flags to see each one's own `-h`:

| Command | What it does |
|---|---|
| `splitter proxy [-listen addr]` | Runs the Phase 1 pass-through logging proxy. Blocks until SIGINT/SIGTERM, then drains in-flight requests and the async logger before exiting. |
| `splitter featurise [-refresh]` | Batch-tags logged calls into the `features` table (`turn_type`, `files_touched`, `subsystem`, `had_error_followup`). Idempotent; without `-refresh` it only processes calls missing features plus calls whose `had_error_followup` is still unresolved. |
| `splitter replay [-backend name] [-limit N] [-force]` | Sends unreplayed logged calls to a replay backend and runs the verification cascade over the results. Refuses to run when the newest proxy call is younger than `[replay].idle_minutes`, unless `-force`. |
| `splitter judge submit` | Submits every queued middle-band verification (from the cascade) as one Anthropic Message Batches API call. |
| `splitter judge poll` | Checks every outstanding judge batch once and applies any results that have finished. |
| `splitter report spend` | Token totals and estimated cost grouped by `turn_type`: the business case, showing where the money goes. |
| `splitter report agreement` | Per turn_type x subsystem agreement rate, sample size, and top disagreement reasons from judge output, plus the judge's share of edit turns and its spend per 100 replays. |
| `splitter import-history [-dir path]` | One-off **best-effort bootstrap only**, see [`cmd import-history`](#cmd-import-history-best-effort-bootstrap-only) below. |
| `splitter version` | Prints the build version. |

Designed in `DESIGN.md` but not yet implemented in this checkout (see
[Implementation status](#implementation-status)):

| Command | What it will do |
|---|---|
| `splitter router update` | Recomputes `router_state` (Wilson lower bound per turn_type x subsystem x model-family pair) from accumulated verifications. |
| `splitter report weekly` | Frontier tokens avoided, estimated cost saved, quality incidents (escalations), drift check results. |
| `splitter eval harvest [-include-clean N]` | Seeds the eval library from live capture: local-model disagreements, live escalations, and frontier error-followups (the frontier's own struggles), optionally sampled clean tasks too. |
| `splitter eval seed-history [-repo path] [-since date] [-max N] ...` | Seeds the eval library from the target repo's own git history: small, single-concern historical commits become tasks, graded the same way as live captures. |
| `splitter eval reverse-briefs` | Rewrites a history-sourced task's mechanical commit-subject brief into the problem statement a requester would have written before the fix existed, via a cheap batched judge pass. |
| `splitter eval add -commit <sha> -brief "..." -request <file.json>` | Manually adds one eval task. |
| `splitter eval run -backend <name> [-model <override>] [-mode single\|agentic]` | Replays every active eval task against a backend/model, scored by the same verification cascade (or, in agentic mode, a bounded read/edit/write/run_tests tool loop graded fail-to-pass against the task's own held-out tests). Climbs a per-track difficulty ladder until futility, so cost stays bounded on models well past their ceiling. |
| `splitter eval list` | Lists tasks: id, origin, repo HEAD short sha, brief, pass rate per model so far. |

### `cmd import-history` (best-effort bootstrap only)

```
splitter import-history [-dir path] [-config path]
```

Reads Claude Code's own session transcripts
(`~/.claude/projects/*/*.jsonl` by default, override with `-dir`) and
reconstructs approximate `calls` rows tagged `source='import'`, so a
freshly installed splitter has some bootstrap data to featurise and
replay against instead of starting from zero. It is printed as
**BEST-EFFORT** in its own `-h` output and its banner every time it runs,
because it is the *only* place in the codebase that parses this format:

- Claude Code's transcript JSONL is an internal implementation detail, not
  a documented API contract, and can change between releases without
  notice. `BRIEF.md`'s Constraints section is explicit that this format
  must never be parsed by the live pipeline for exactly that reason; this
  command exists precisely because it is a one-off, not a dependency.
- A reconstructed "request" only ever carries the session's prior turns as
  messages. It never had the real system prompt or tool definitions
  (transcripts do not record either), so a couple of `turn_type` rules
  that key off the system prompt text are unavailable for imported rows;
  everything else (tool_use-based classification, `files_touched`,
  `subsystem`) works normally.
- Subagent (sidechain) turns are intentionally excluded, not imported:
  they belong to a nested conversation the main session transcript never
  saw directly.
- Anything unparseable (a malformed JSON line, an assistant turn with no
  content blocks, no timestamp, and so on) is skipped and counted, never
  fatal to the run. The end-of-run summary reports files scanned,
  assistant turns seen, calls imported, and both skip counts.
- `calls.source` distinguishes `'import'` rows from `'proxy'` (live
  capture) rows. Imported rows are fully usable by `featurise` and
  `replay`; they are excluded only from proxy overhead/latency stats,
  which they never had real numbers for anyway.

Test fixtures live in `testdata/transcripts/fixture-project/`: small,
hand-written JSONL files that mirror the real shape (see the doc comment
above the tests in `cmd/splitter/cmd_import_history_test.go` for exactly
what each fixture line exercises) without being copied from any real
session.

## Config reference

Resolution order: `-config` flag, then `$SPLITTER_CONFIG`, then
`~/.config/splitter/config.toml`, then built-in defaults
(`internal/config.Default`). `config.example.toml` documents every field
inline; copy it to get started (`scripts/install.sh` does this for you).
Top level:

| Field | Meaning |
|---|---|
| `listen` | Proxy's local listen address, default `127.0.0.1:9925`. |
| `upstream` | Frontier API base URL the proxy forwards to. |
| `db_path` | SQLite file. Created `0600`, parent directories `0700`. |
| `repo_path` | The target codebase: where `repo_head` is read from and where verification worktrees are created. |
| `env_file` | KEY=VALUE lines loaded into the process environment at startup, without overriding anything already set. Never committed, never in the TOML itself: secrets only ever come from here or the ambient environment. |
| `[replay]` | `backend` (default replay backend), `idle_minutes` (the "no traffic" gate), `max_concurrent_worktrees`, `batch_size`. |
| `[backends.<name>]` | One entry per OpenAI-compatible replay/routing backend: `base_url`, `api_key_env` (the *name* of an env var, never a literal key), `model`. Ships with `ollama`, `together`, `gemini`, `openai`, `deepseek`. |
| `[judge]` | `model` (Haiku via the Batches API), `api_key_env`, `max_context_chars` (request-context truncation for judge prompts only; both full responses are always included). |
| `[thresholds]` | Cascade similarity thresholds, `default_high`/`default_low` plus per `"<language>/<turn_type>"` overrides. |
| `[tests]` | Optional per-subsystem test command the verify cascade runs inside each worktree. Off by default: FreegleDocker's real suites need the Docker stack, out of scope for ephemeral worktrees today. |
| `[router]` | `min_n`, `min_wilson_lb`, `dual_dispatch_pct` (Phase 4). |
| `[families]` | Per-exact-model-id override for family normalisation (`internal/router.Family`, once built). |
| `[layers]` | Path prefix/glob to eval-task layer name (`frontend-ui`, `backend-api`, `database`, `infra`, `tests`, `docs`, `build`), defaulted for this codebase family. |

## Model-family behaviour summary

Per-exact-model statistics would reset to zero every time a model gets a
version bump, which defeats the point of accumulating weeks of agreement
data. `internal/router.Family` (design finished, ships with the router
component) normalises an exact model id down to a family key: date and
generation-number suffixes stripped, variant words and parameter-size tags
kept, e.g. `claude-opus-5` and `claude-opus-5-20260101` both become
`claude-opus`; `qwen2.5-coder:7b` and `qwen3-coder:7b` both become
`qwen-coder:7b`. `router_state` is scoped by family pair
(`"<frontierFamily>><localFamily>"`), so a new same-family version
inherits its learned routability immediately, on the assumption it behaves
similarly until proven otherwise.

"Until proven otherwise" is enforced, not just assumed:
`splitter router update` also computes per-exact-version agreement inside
each family-scoped category, and when a specific version has n >= 10 and
sits more than 10 points below its family's aggregate, that
category+version is flagged (in the update output and the weekly report)
and the category's stats are recomputed from that version's rows alone,
which can drop it below the routability bar. The eval library's ladder
runs are the fast, cheap complement: "is this new version still similar to
its family" is exactly the question `splitter eval run -backend <name>`
against the trip-up library answers directly, without waiting on weeks of
live replay data.

`internal/feature.PricingFor` (used by `report spend`, already
implemented) carries a narrower version of the same idea today, scoped to
the frontier (Claude) model families it needs to price; see the doc
comment on `pricingFamily` in `internal/feature/pricing.go`.

## Implementation status

This is a from-scratch build against `DESIGN.md`, worked in phases. As of
this checkout, `splitter <command>` with no arguments lists what actually
exists:

```
$ splitter
usage: splitter <command> [args]

commands:
  featurise
  import-history
  judge
  proxy
  replay
  report
  version
```

Phases 1-3 (proxy, featuriser, replay worker, verification cascade,
Haiku judge, `report spend`/`report agreement`) are implemented and
tested. Phase 4 (`internal/router`, `splitter router update`,
`splitter report weekly`, live routing in the proxy) and the eval library
(`internal/evals`, `internal/agentic`, `splitter eval ...`) are fully
specified in `DESIGN.md` but not yet built; the systemd units and this
README describe the finished design (per BRIEF.md's phased working
practice, each phase's acceptance checks are meant to go green before the
next phase starts) so the ops surface does not need rewriting again once
they land. `splitter-replay.service`'s `router update` step will simply
start succeeding once that command exists; nothing else needs to change.

## Coexistence with `large-output-guard` hooks

Freegle's `large-output-guard` PreToolUse/PostToolUse hooks and splitter's
proxy operate at different layers and do not interact: hooks wrap
individual Claude Code *tool calls* (Read, Bash, Edit, ...) inside the
Claude Code process itself, while the proxy wraps the *API transport*
between Claude Code and Anthropic, entirely outside that process. Neither
one can see or intercept the other. Running both together is the normal
case, not a special configuration.

## Key provenance

- Secrets live only in `~/.config/splitter/env` (KEY=VALUE lines,
  `env_file` in the config), never in the TOML, the database, logs, or
  this repo. `.gitignore` excludes `*.db*` and the built binary.
- Every key wired into that file is Freegle-scoped (see `DECISIONS.md`,
  "API keys and which user they belong to"): keys that would bill a
  different project (e.g. the answerbot/aidvice KindPhone keys) are
  deliberately not used here, even where they existed on the same
  machine.
- The **judge is the only component that uses an Anthropic API key**
  (`[judge].api_key_env`, for the Message Batches API). The proxy never
  holds or needs an Anthropic key of its own: it forwards Claude Code's
  own `x-api-key`/auth headers through to `upstream` untouched, on every
  request, exactly as Claude Code sent them.
- Replay/routing backends (Ollama, Together, Gemini, OpenAI, DeepSeek)
  each read their own key from the env var named by their
  `[backends.<name>].api_key_env`; Ollama needs none.

## Attribution

`internal/proxy` and `cmd/splitter/cmd_proxy.go` are adapted, not written
from scratch, from
[seifghazi/claude-code-proxy](https://github.com/seifghazi/claude-code-proxy)
(MIT License), pinned at commit
[`02c9c766`](https://github.com/seifghazi/claude-code-proxy/commit/02c9c766679eee75c861bbde11c6d8b5249d44a7).
Its pass-through forwarding and SSE streaming approach is reused,
restructured around splitter's own SQLite schema, an async fail-open
logger, git HEAD capture, and a session id heuristic; its web dashboard,
conversation browser and model router were not taken (splitter has no web
UI, and its own router is a separate, offline-trained design). Every
adapted source file carries a header comment naming the commit. Full
attribution text, including the upstream MIT license and copyright notice
in full, is in [`NOTICE`](NOTICE). This project itself is licensed under
the GPL-2.0, see [`LICENSE`](LICENSE), matching Freegle's `iznik`
convention; MIT is compatible with that licensing (MIT is a
GPL-compatible permissive license), so the adapted portions carry both
notices as `NOTICE` describes.
