# Decisions log

Ambiguities resolved per the brief's working practices (simpler interpretation, noted
here, no blocking). Newest at the bottom.

## 2026-08-24 initial build

- **Location**: built as `splitter/` in the FreegleDocker monorepo (new top-level
  directory, own Go module). The framework is codebase-agnostic; living next to the
  repo it is tuned on keeps ops simple.
- **Single binary, subcommands**: `splitter proxy|featurise|replay|judge|report|router|import-history`
  rather than separate binaries. Matches the simplicity constraint.
- **SQLite driver**: `modernc.org/sqlite` (pure Go). Keeps the single-static-binary
  requirement without cgo.
- **Raw HTTP everywhere, no provider SDKs**: the proxy re-serves the Anthropic wire
  format verbatim, so this module already works at the HTTP level; adding the
  anthropic-sdk-go dependency tree for just the batch judge contradicts the
  "Go + shell not a framework" constraint. Wire format details were taken from
  current API documentation, not memory.
- **API keys and which user they belong to**: keys were located across the machine
  (answerbot, aidvice, advent-pdp11, ~/.config/freegle, FreegleDockerWSL). Only
  FREEGLE-scoped keys are wired into `~/.config/splitter/env` (chmod 600, outside the
  repo): Anthropic from FreegleDockerWSL/.env (judge only, the proxy passes Claude
  Code's own auth through untouched), Together from ~/.config/freegle/together.env,
  Gemini and OpenAI from FreegleDockerWSL/.env. The answerbot/aidvice keys bill the
  KindPhone project and were deliberately not used.
- **Backends beyond Ollama**: the brief says local-vs-frontier only. Together, Gemini
  and OpenAI are wired as additional REPLAY backends behind the same OpenAI-compatible
  client (one `-backend` flag), because the keys exist and offline comparison against
  a stronger cheap model is useful signal. Live routing (Phase 4) still defaults to
  the Ollama backend; routing between frontier tiers remains out of scope.
- **Session id** is a documented heuristic: request `metadata.user_id` when present,
  else a hash of stable request characteristics. Best-effort per the brief.
- **repo HEAD capture** reads `.git/HEAD` (and the ref file it points at) directly,
  no subprocess on the request path.
- **AST comparison**: difftastic (`difft`, installed to ~/.local/bin) when present,
  with a normalized token-level similarity fallback so the cascade never blocks on a
  missing tool. Tree-sitter tree edit distance was skipped as heavier than the job
  needs.
- **Linters**: golangci-lint is not installed on this machine; the cascade probes for
  it and falls back to `gofmt -l` plus `go vet`. Which linter actually ran is recorded
  in the verification row.
- **Repo test command in the cascade** is off by default. FreegleDocker's real suites
  need the Docker stack; wiring them into ephemeral verify worktrees is out of scope
  for the initial build. The `[tests]` config table exists and is honoured when set.
- **Judge model**: `claude-haiku-4-5` via the Message Batches API (50% price).
- **Phase ordering vs "no human intervention"**: acceptance criteria that need weeks
  of real traffic (50-call labelled sample, 100-call overnight replay, two weeks
  unattended, Phase 4 routability bar) cannot be satisfied at build time. Everything
  is built and unit/integration tested now, with automated acceptance tests where the
  brief marks them; the traffic-dependent checks are wired so they run/refresh as data
  accumulates. The labelled fixture starts synthetic-but-realistic and is designed to
  be replaced by 50 real labelled calls once capture has run (a fixture-size constant
  enforces the upgrade). Phase 4 live routing ships behind SPLITTER_ROUTE (default
  pure pass-through), which respects "only after 2 weeks of Phase 3 data" because
  enabling it is an explicit operator action informed by the report.
- **calls.source column** ('proxy' or 'import') distinguishes bootstrap-imported rows
  (best-effort ~/.claude transcript import, one-off command) from live capture, so the
  live pipeline never depends on transcript parsing.

## 2026-08-24 module skeleton

- **go.mod Go version**: DESIGN.md pins `go 1.23`, but the current `modernc.org/sqlite`
  (v1.57.0) requires `go 1.25.0` and the latest version compatible with go 1.23 is
  v1.38.0 (many releases behind, from before the current pure-Go driver hardening).
  Chose the simpler, better-maintained option: `go.mod` declares `go 1.25.0`; the local
  toolchain auto-downloads the matching `go` release (already cached on this machine,
  works offline-to-network as normal `go` behaviour, no manual step required). No other
  design constraint depends on the literal `1.23` value.

## 2026-08-24 separate repo

- **Standalone repository**: Edward redirected mid-build, the framework lives in its
  own repo (github.com/Freegle/splitter), not as a directory of the FreegleDocker
  monorepo. The Go module is the repo root. The FreegleDocker worktree used for the
  first attempt is being retired; `repo_path` config still points at the FreegleDocker
  checkout because that is the codebase the framework is tuned on.
- **Model families**: Edward: "when new versions of same model family come out, assume
  will have similar characteristics (until we have learned otherwise, which we
  should)". Implemented as family-normalised router statistics plus per-exact-version
  divergence tracking, see DESIGN.md "Model families".

## 2026-08-24 eval library

- **Trip-up eval library**: Edward: "build up a library of specific tasks that have
  tripped up models in our codebase to then use this to evaluate new models against
  them. git commit number + brief". Implemented as eval_tasks/eval_runs/eval_results
  plus `splitter eval harvest|add|run|list`, see DESIGN.md "Eval library". Harvested
  from local-model disagreements, live escalations, and frontier error-followups, so
  the library also captures tasks the frontier itself struggled with. Judge stage is
  excluded from eval scoring (mechanical verification only) so new-model scorecards
  stay cheap and deterministic; the middle band counts as a fail, which keeps the
  bar conservative.

## 2026-08-24 repo rename

- **Repo renamed** by Edward to github.com/Freegle/the-splitter-splatter-of-tiny-features
  (GitHub redirects the old Freegle/splitter URL). The local checkout stays at
  /home/edward/splitter and the Go module path stays github.com/freegle/splitter:
  the module is a binary nobody imports, and the shorter path keeps imports readable.

## 2026-08-24 history seeding

- **Eval seeding from git history**: Edward: "we can use historical commits to seed
  this evaluation". `splitter eval seed-history` turns small historical commits into
  eval tasks: parent sha as the starting state, commit message as the brief, the real
  diff (as Edit/Write tool calls) as the reference answer. Filters keep tasks tiny
  (default max 3 files, 120 diff lines, 20KB context) so a single-turn comparison is
  fair. The synthesized request uses a minimal generic coding-agent prompt, not a
  Claude Code transcript, so seeding never depends on transcript formats.

## 2026-08-24 proxy adopted, not reinvented

- **Proxy base**: Edward: "don't reinvent proxy from scratch, we can modify something
  gpl and credit". Adopted seifghazi/claude-code-proxy (MIT, Go, pass-through capture
  proxy for Claude Code with SQLite logging, ~500 stars), pinned commit 02c9c766.
  MIT is even more permissive than GPL, so modify-and-credit is clean: NOTICE file at
  the repo root with the upstream license and copyright, header comment in each
  derived file. We take the forwarding and SSE streaming approach and adapt storage to
  our schema; their web dashboard, conversation browser and model router are not
  taken. Our fail-open, async logging, repo HEAD capture and overhead requirements
  remain the acceptance bar on top.

## 2026-08-24 eval task characteristics

- **Characteristics, not one difficulty axis**: Edward: "difficulty is too simple a
  dimension... maybe some models are really good at API code like the Go, and some
  models are really good at user facing front end". eval_tasks carry mechanically
  derived language/layer/nature/difficulty columns plus evidence JSON; scorecards
  group by each dimension so each model gets a capability profile. Difficulty stays
  as one dimension (simple = sanity floor, challenging = discrimination), sourced
  from error-followups, escalations and git archaeology (fix-up commits within 14
  days). The router keeps (turn_type, subsystem, families) for now; promoting
  language/layer into the router category is the intended evolution once profiles
  show splits within a subsystem.
