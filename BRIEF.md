# Brief: Local/Frontier Model Routing Framework ("Splitter")

## Intent (one paragraph)

Build a system that learns which Claude Code requests can be served by a local model instead of the frontier API, to cut token costs without degrading output quality. It works by (1) transparently logging all Claude Code API traffic through a proxy, (2) replaying logged turns against a local model offline and mechanically verifying agreement, (3) training a conservative router from those comparisons, and (4) eventually routing whitelisted request types to the local model live, with automatic escalation to the frontier on failure. The developer works ~1.5 days/week; the other days are idle compute for offline replay.

## Environment & standing context

- Single developer, single machine: WSL2 Ubuntu on Windows. No multi-user concerns.
- Primary codebase under development: Freegle (Nuxt3/Vue frontend, Go + PHP backend, MySQL). The framework itself is codebase-agnostic but will be tuned on this repo.
- Claude Code is the harness. Do NOT modify or patch Claude Code itself.
- Local model serving: Ollama, already installed. Model choice is configurable; default to `qwen2.5-coder:7b`.
- Frontier: Anthropic API, Claude Code's normal auth passes through untouched.
- Existing tooling to preserve: `large-output-guard` PreToolUse/PostToolUse hooks. The proxy must not conflict with hooks.

## Architecture (five components, built in phases)

```
Claude Code ──ANTHROPIC_BASE_URL──▶ [1 Proxy] ──▶ Anthropic API
                                       │
                                       ▼ append-only
                                  [2 Capture log (SQLite)]
                                       │ idle-time batch
                                       ▼
                                  [3 Replay worker] ──▶ Ollama
                                       │ diffs
                                       ▼
                                  [4 Verification cascade] ──▶ scores back to log
                                       │
                                       ▼
                                  [5 Router] (trained offline; later consulted live by Proxy)
```

## Phase 1 — Pass-through logging proxy

A local HTTP proxy speaking the Anthropic Messages API surface. Claude Code points at it via `ANTHROPIC_BASE_URL`. It forwards everything verbatim (including streaming/SSE) and logs request/response pairs.

Requirements:
- Language: Go (fits the maintainer's stack; single static binary).
- Fail-open: any internal proxy error → forward the request anyway and log the error. Logging must never break a coding session.
- Streaming: SSE responses must stream through with no added buffering latency; assemble the full response for logging after the stream completes.
- Log to SQLite, one row per API call: timestamp, session id (derive from metadata or connection heuristics; best-effort), full request JSON (zstd-compressed), full response JSON (compressed), model requested, input/output token counts from the usage block, latency ms.
- Redact nothing (single-user local machine), but the DB file must be chmod 600 and .gitignored.
- Config via env vars or a single TOML file: upstream URL, DB path, listen port.

Acceptance (all must pass):
- [ ] A normal Claude Code session (edit a file, run a command, ask a question) through the proxy is indistinguishable from a direct session, including streaming behaviour and tool use.
- [ ] `sqlite3 splitter.db 'select count(*) from calls'` grows during a session; a sampled row round-trips to valid JSON.
- [ ] Killing the DB file mid-session (simulate disk error) does not interrupt the Claude Code session (fail-open verified by test).
- [ ] Unit tests cover: SSE reassembly, fail-open path, compression round-trip.

## Phase 2 — Turn featurisation

A classifier (rule-based first, no ML) that tags each logged call with routing-relevant features, stored in a `features` table keyed to the call.

Features:
- `turn_type`: one of {tool_result_summary, single_file_edit, multi_file_edit, plan, question_answer, other} — inferred from request structure (presence/type of tool results, system prompt shape, response content blocks).
- `files_touched`: paths extracted from tool calls in the response.
- `subsystem`: top-level directory bucket of files_touched.
- `context_tokens`, `output_tokens`.
- `had_error_followup`: true if the next call in the same session contains an error message or retry of the same edit (cheap proxy for "frontier struggled").

Acceptance:
- [ ] Featuriser runs as a batch command over the existing log; idempotent (re-running updates, doesn't duplicate).
- [ ] On a manually labelled sample of 50 real calls, turn_type agreement ≥ 80%. Include the labelled sample as a fixture and a test that enforces this.
- [ ] A summary command prints token spend grouped by turn_type (this is also the business case: it shows where the money goes).

## Phase 3 — Replay worker + verification cascade

Batch process (systemd timer or cron, runs when no proxy traffic for >30 min): for each unreplayed logged call, send the identical request to Ollama, capture the local model's response, and score agreement.

Verification cascade, in order, cheapest first:
1. Exact/near-exact text match (normalised whitespace) → agree, stop.
2. For turns whose frontier response contained file edits: apply BOTH the frontier's edit and the local edit in two ephemeral git worktrees of the repo at the recorded commit (log the repo HEAD at capture time in Phase 1 — add this column). Run `golangci-lint` / `php -l` / `eslint` as appropriate, plus the repo's test command if configured for that subsystem. Then AST-compare the two resulting files with difftastic (or tree-sitter tree edit distance) → a similarity score in [0,1].
3. Similarity ≥ high threshold → agree. ≤ low threshold → disagree. Thresholds per (language, turn_type), config file, defaults 0.9/0.5.
4. Middle band → queue for LLM arbitration: batched call to Claude Haiku via the Anthropic Batch API, prompt: given the request context, are these two responses functionally equivalent? Answer JSON {equivalent: bool, confidence: 0-1, reason: one line}.
- Store per replay: local response, cascade stage that decided, similarity score, lint/test results for both sides separately, judge verdict SEPARATE from test results (never merge; if tests and judge conflict, tests win and the conflict is counted).
- Worktrees: created under /tmp, port-offset env injected if the test command needs services, always torn down (defer/trap), max N concurrent (config, default 2).

Acceptance:
- [ ] Replaying 100 logged calls overnight completes unattended and writes 100 scored rows.
- [ ] Zero worktrees left behind after a run, including when a verification step is killed (test with SIGKILL mid-run).
- [ ] Judge is invoked for ≤ 30% of edit turns on real data (thresholds doing their job); judge spend per 100 replays is reported.
- [ ] A report command prints, per turn_type × subsystem: agreement rate, sample size, and the top 3 disagreement reasons from judge output.

## Phase 4 — Router + live routing (only after ≥ 2 weeks of Phase 3 data)

- Router = per-(turn_type, subsystem) agreement statistics with Wilson lower confidence bound. A category is routable when its lower bound ≥ 0.9 with n ≥ 30. No neural nets; a SQL view is acceptable.
- Proxy consults the router live. Routable categories go to Ollama; the response is translated to Anthropic Messages format. Everything else passes through unchanged.
- Escalation: if a locally-served turn is followed within the session by an error/retry signal (Phase 2 heuristic), mark that category's recent outcome as failure AND immediately stop routing that session locally (session-level circuit breaker).
- Dual-dispatch 5% of routable turns (serve frontier answer, shadow local) forever, to detect drift.
- Kill switch: `SPLITTER_ROUTE=off` env var → pure pass-through.

Acceptance:
- [ ] With routing on, a full working session completes with no user-visible errors; escalation observed working via injected fault (test: force local model to emit garbage for one category, category auto-disables).
- [ ] Weekly report: frontier tokens avoided, estimated cost saved, quality incidents (escalations), drift check results.
- [ ] Router decisions are logged with the statistics that justified them (auditable).

## Constraints

- Simplicity beats generality: SQLite not Postgres, cron not queues, Go + shell not a framework. Total system should be maintainable by one person in odd hours.
- Never parse `~/.claude` transcript files in the live pipeline (internal format, changes between releases). A one-off historical import script MAY read them, clearly marked as best-effort, for bootstrap data only.
- The proxy must add < 5 ms p50 overhead to forwarded calls.
- No telemetry, no external services beyond Anthropic API and local Ollama.
- All secrets stay in env; nothing written to the repo.

## Non-goals (do not build)

- Fine-tuning the local model (future work; the replay corpus enables it later).
- Multi-machine or multi-user support.
- A web UI. CLI reports only.
- Routing between frontier tiers (Haiku/Sonnet/Opus) — local-vs-frontier only for now.

## Definition of done

Phases 1–3 running unattended for two weeks, producing the Phase 3 report, with the report showing at least one (turn_type × subsystem) category meeting the Phase 4 routability bar — or demonstrating conclusively that none does, which is an equally valid result and should be stated plainly.

## Working practices for the implementing agent

- Build phases strictly in order; each phase's acceptance checks green (as automated tests where marked) before starting the next.
- When a requirement here is ambiguous, choose the simpler interpretation, note the decision in DECISIONS.md, and continue — do not block.
- Anything you cannot satisfy, say so explicitly in DECISIONS.md rather than silently approximating.
