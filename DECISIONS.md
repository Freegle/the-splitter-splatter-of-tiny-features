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

## 2026-08-24 module skeleton continued (config, store, anthropic, cmd)

- **`[layers]` glob for "*.go handlers"**: DESIGN.md's layer mapping defaults list
  "iznik-server-go/, *api*/, *.go handlers -> backend-api" without a precise glob
  syntax for the last entry. Interpreted as `*handler*.go` (any Go file with
  "handler" in its name). `internal/config.Default`'s `Layers` map and
  `config.example.toml`'s `[layers]` table both carry the full DESIGN.md default
  set; matching this glob against touched files is left to whichever component
  implements the eval library's layer classification, per DESIGN.md.
- **`internal/anthropic.AssembleSSE`**: added the SSE event/delta types and the
  assembler alongside the existing `types.go` (new file `sse.go`), since
  `types.go` only had the Messages API request/response types from the prior
  build attempt. Truncation detection covers three cases: no `message_stop`
  event seen, an accumulated `tool_use` `partial_json` that never became valid
  JSON (preserved as a JSON string so the assembled message still marshals), and
  an explicit `error` SSE event (wrapped as `ErrUpstreamSSEError` rather than
  `ErrTruncatedStream`, since it is a distinct upstream-reported condition, not
  silent truncation). Usage merges message_start (input tokens) and
  message_delta (output tokens) field-by-presence (pointer fields) rather than
  overwriting the whole struct, so a value present in one event is not zeroed by
  the other's absence.
- **`eval_tasks`/`eval_runs`/`eval_results`**: added to `internal/store`'s v1
  migration verbatim from DESIGN.md's eval library schema; the prior build
  attempt's store only had the Phase 1-4 tables. No query helpers added for
  these tables in this pass (out of scope: this task covers schema only, per
  the working instructions to keep new store queries in the owning
  component's own file).
- **`cmd/splitter`**: created from scratch (`main.go`, `cmd_version.go`,
  `cmd_report.go`); none of these existed yet from the prior build attempt.
  `cmd_report.go` holds only the dispatch map as specified; it defines no
  report verb itself.

## 2026-08-24 characteristics validated against the literature

Edward: "research online whether your classification of problem aspects/
characteristics is likely to be correct - don't just assume the ones i gave".
Findings (searched 2026-08-24):
- Language as a dimension: VALIDATED. MultiPL-E finds performance correlates with
  language popularity with real per-language spread; Aider polyglot and McEval exist
  because single-language benchmarks hide this. https://nuprl.github.io/MultiPL-E/
  https://aider.chat/2024/12/21/polyglot.html
- Frontend/backend layer split: VALIDATED, and framework matters WITHIN a language:
  DesignBench measures React consistently ahead of Vue on identical tasks (this
  codebase is Vue/Nuxt, so a framework facet was added).
  https://arxiv.org/abs/2506.06251 https://arxiv.org/html/2505.07473v1
  Backend end-to-end services are their own axis (BackendForge: best model 55.4%
  local behaviours vs 28.6% complete services). https://arxiv.org/html/2607.11042
- Nature (bugfix/feature/refactor): VALIDATED. DesignBench separates generation/
  edit/repair with differing results; refactoring is benchmarked as a distinct
  capability (SmellBench, SWE-Refactor). https://arxiv.org/pdf/2606.05574
- Size: VALIDATED BUT NON-MONOTONIC. SWE-bench analyses: 51-200 line patches resolve
  at ~40% while 1-10 line precision edits resolve at ~16%, and multi-file patches
  drop sharply; 12 minimal single-file tasks were unsolved by ALL agents. Size is
  recorded and bucketed, never used to infer difficulty.
  https://arxiv.org/pdf/2604.02547 https://www.swebench.com/SWE-bench/
- ADDED spec_clarity: SWE-bench Verified was built because underspecified issue text
  confounds evaluation; the brief's specificity is now a recorded facet.
- ADDED task_date + contamination guard: LiveCodeBench shows stark post-cutoff
  performance drops and pre-cutoff memorisation inflation; these repos are public
  GitHub, so pre-cutoff history-seeded tasks are memorisation-suspect. Scorecards
  split by cutoff segment via a [model_cutoffs] config table.
  https://livecodebench.github.io/ https://arxiv.org/pdf/2403.07974
- ADDED localization facet: seeded tasks hand the model the files (given), live
  tasks made the agent find them (discovered); the two are reported separately.

## 2026-08-24 backend pricing research

- **Price-advantage research** (Edward: which models have a significant price
  advantage over the Claude subscription; subscriptions acceptable as they bound
  cost): full findings and sources in BACKENDS.md. Headlines: GLM Coding Plan Lite
  (~$10-18/mo, Anthropic-compatible drop-in, ~95%-of-Opus press claims needing local
  validation), Kimi Allegro ($99/mo ~ Anthropic $200 quota), DeepSeek V4 Flash
  ($0.14/$0.28 per MTok per-token), Gemini CLI free tier (1000 req/day, $0), local
  qwen3-coder:30b (free, best per-GB mid-2026, now pulled). GLM/Kimi/DeepSeek expose
  Anthropic-compatible endpoints, so a `kind = "anthropic"` backend type is the
  planned follow-up; no keys held for them yet (a signup decision for Edward), env
  names reserved GLM_API_KEY/KIMI_API_KEY/DEEPSEEK_API_KEY. Default replay stays
  local per the brief.

## 2026-08-24 ladder evaluation

- **Easy-to-hard ladder with futility stopping**: Edward: work upwards from easy
  tasks towards harder ones until hitting "the limit of wasting tokens on tasks that
  are way beyond the model". Implemented as per-track rung climbing (track =
  language by default, because a model can be beyond its ceiling on vue while still
  climbing go), Wilson-upper-bound futility stops plus a consecutive-failure fast
  exit, ladder_skipped rows for auditability, token accounting per run and a
  -max-tokens hard cap. Rungs combine the evidence-based difficulty label with
  coarse scope buckets only, never raw size ordering (size is non-monotonic).

## 2026-08-24 briefs are the ask, not the answer

- **Brief derivation**: Edward: work out the brief from Claude session history (the
  initial instruction) where it exists; for historical commits "reverse engineer
  something". Session-sourced tasks take the session's initiating user message.
  Commit subjects prescribe the fix (post-hoc), so history-sourced briefs get
  rewritten by a cheap batched judge-model pass (`eval reverse-briefs`) into the
  problem statement a requester would have written before the change, with a hard
  rule against naming the fix's identifiers or approach. brief_source
  (session|call|commit_subject|reverse_engineered) is tracked and reported per eval
  run so guided and unguided results never mix silently.

## 2026-08-24 briefs are the ask, not the answer

- **Brief derivation**: Edward: work out the brief from Claude session history (the
  initial instruction) where it exists; for historical commits "reverse engineer
  something". Session-sourced tasks take the session's initiating user message.
  Commit subjects prescribe the fix (post-hoc), so history-sourced briefs get
  rewritten by a cheap batched judge-model pass (`eval reverse-briefs`) into the
  problem statement a requester would have written before the change, with a hard
  rule against naming the fix's identifiers or approach. brief_source
  (session|call|commit_subject|reverse_engineered) is tracked and reported per eval
  run so guided and unguided results never mix silently.

## 2026-08-24 leakage containment

- **Upstream discovery is an eval-validity threat**: Edward: a model given a
  disposable fork "might be smart enough to find the upstream repo and establish a
  fix. We don't want them to be able to do that." Three layers, see DESIGN.md
  "Leakage containment": payload scrub (no remotes/shas/org strings, scrub_terms
  config, leaky flag excludes from trusted scorecards), no provider-side browsing on
  eval calls, and a standing requirement that future agentic evals use a scrubbed
  git-archive export (synthetic root commit, no remotes) in a network-denied
  sandbox. Complements the [model_cutoffs] memorisation guard: cutoffs cover what
  the model already knows, scrubbing covers what it could go and look up.

## 2026-08-24 proxy strips Accept-Encoding upstream

- **internal/proxy always strips Accept-Encoding on the way to upstream**, rather
  than requesting gzip the way the upstream base project does (its
  provider/anthropic.go sets `Accept-Encoding: gzip` and decompresses the response
  itself). Our proxy tees every response chunk into a capture buffer as it relays it
  (DESIGN.md's <=32KB chunked relay), so decoding a compressed stream chunk by chunk
  while it is still arriving would need a stateful gzip reader threaded through both
  the client-relay path and the capture path, for a saving that does not matter on a
  single-user localhost hop between the proxy and api.anthropic.com. Stripping
  Accept-Encoding keeps both paths operating on the same plain bytes with no
  decompression step anywhere in the request path, at the cost of slightly more
  bytes on the wire between the proxy and Anthropic, which is not the bottleneck
  here. `cleanForwardHeaders` deletes Accept-Encoding only on the outbound
  (proxy-to-upstream) leg; nothing changes about what Claude Code itself sends or
  receives on the proxy-to-client leg.

## 2026-08-24 agentic eval mode

- **Run-and-fix-the-tests is a first-class eval**: Edward: "Part of what we want to
  evaluate is whether the models successfully run and fix the tests." Added
  DESIGN.md "Agentic eval mode": SWE-bench-style fail-to-pass grading using each
  commit's own held-out tests (parent tree + commit's tests applied, fix withheld),
  a bounded tool loop (read/list/grep/edit/write/run_tests, no bash, no network
  tools), sandbox = scrubbed git-archive export with deps pre-warmed online then
  the model loop network-denied via unshare -rn (verified working on this WSL2;
  -allow-network exists but marks results untrusted). tests_ran is reported as its
  own capability, separate from tests_passed and regressions. Single-turn mode
  remains for non-gradable tasks and cheap breadth.

## 2026-08-24 detection over sanitisation

- **Scrubbing scrapped, cheat detection instead**: Edward: models need real
  worktrees, "it's a bit doomed to try to sanitise the repo to keep the details
  out, so scrap that. Instead, if possible detect if the model has cheated by
  looking at the upstream or the main repo or later git commits." The scrub_terms
  mechanism is gone; agentic sandboxes are genuine git worktrees at the base commit
  (real names, real history in the shared object store). Validity now rests on:
  never shipping the answer key (fix sha, reference response), network denial
  (unshare -rn), and detectors over the fully-logged tool transcript: escape,
  git_poke, tool_smuggling, suspect_copy (near-verbatim match with the withheld
  fix), attempted_git (.git parked during run_tests, failed pokes surface in
  output). Flags demote results to the untrusted segment, humans audit the stored
  transcript.
