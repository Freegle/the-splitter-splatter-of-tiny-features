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

## 2026-08-24 first eval target: DeepSeek V4 Flash

- Edward: "We're going to want to test initially DeepSeek V4 Flash to see how
  bright it can get." Key discovered in ~/deepseek (Edward's own Claude-Code-via-
  DeepSeek env script), validated against /models and /chat/completions, added to
  ~/.config/splitter/env as DEEPSEEK_API_KEY, [backends.deepseek] wired
  (base https://api.deepseek.com, model deepseek-v4-flash). Flash is a REASONING
  model: responses carry reasoning_content and small max_tokens budgets yield empty
  answers; eval calls must keep the 4096-token default or higher, and the
  translation layer drops reasoning_content like thinking blocks. "How bright can
  it get" is precisely the per-track ladder question; its cutoff goes into
  [model_cutoffs] when known.

## 2026-08-24 Phase 2 featuriser (internal/feature)

- **"Response is text-only" includes a preceding thinking block**: DESIGN.md's
  rules 4-5 gate on "response content is text-only" without addressing extended
  thinking. Interpreted as: at least one text block, and no block of any type
  other than text or thinking (a thinking block never disqualifies). Otherwise a
  perfectly ordinary tool_result_summary or question_answer turn that happens to
  think first would misclassify as `other`.
- **"plan mode is active" and the tool_result error-text check are both
  case-insensitive**. DESIGN.md quotes the phrase verbatim and lists two literal
  forms ("Error"/"error:") for the error-text check; matching case-insensitively
  covers both quoted forms and any other capitalisation a system prompt or tool
  actually uses, and is strictly more permissive than picking one literal form.
- **files_touched is deduplicated, first-encounter order preserved**. DESIGN.md
  says "paths extracted from tool calls in the response" without stating whether
  a file edited twice (e.g. two Edit blocks on the same path) appears once or
  twice; a routing feature should describe which files were touched, not how many
  edit calls hit them, so duplicates collapse.
- **had_error_followup for a session's newest call stays NULL forever, and
  featurise reprocesses it on every run** until a further call arrives in that
  session. This falls directly out of "NULL when there is no next call yet
  (re-running featurise later fills it in)": there is no way to distinguish
  "not checked yet" from "no next call exists yet" without a separate marker,
  and DESIGN.md does not ask for one. The reprocessing is idempotent (same NULL
  result each time) and cheap, so it is left as the simpler behaviour rather than
  adding a sentinel to distinguish the two NULL cases.
- **PricingFor's family normaliser (`pricingFamily`) is intentionally narrower
  than the DESIGN.md "Model families" `internal/router.Family` spec**: it only
  strips date suffixes and pure-digit/dot version segments from a dash-separated
  model id, which is enough to satisfy the four required examples
  (claude-opus-5[-date] -> claude-opus, claude-sonnet-4-6 -> claude-sonnet,
  claude-haiku-4-5[@date] -> claude-haiku) but does not implement the
  colon-suffix or slash-namespace handling `Family` needs for qwen/together
  model ids, since PricingFor only ever prices the logged frontier (Claude)
  model. Kept in its own file behind `PricingFor(model string) Pricing` per the
  task brief, so the router component can later replace the body of
  `pricingFamily` with a call to `router.Family` without changing PricingFor's
  signature or any caller.
- **`splitter report spend` sorts turn_type rows by descending estimated cost**
  (ties broken alphabetically) rather than DESIGN.md's schema column order,
  since the report's stated purpose is "this is also the business case: it
  shows where the money goes" (BRIEF.md Phase 2 acceptance), which reads best
  biggest-spend-first.
- **testdata/labelled/ lives at the repo root**, not nested under
  internal/feature, matching the top-level `testdata/` entry in DESIGN.md's
  "Layout and ownership" tree; internal/feature's fixture-loading test reaches
  it via a `../../testdata/labelled` relative path (go test's working directory
  is always the package directory).
- **internal/config/config_test.go's `TestLoad_ExampleConfigParses` currently
  fails** (`len(Backends) = 5, want 4`): a concurrently-built task added
  `[backends.deepseek]` to config.example.toml (see "2026-08-24 first eval
  target: DeepSeek V4 Flash" above) without updating this hardcoded count. Left
  untouched: internal/config and its tests belong to a different component and
  this task's instructions are to add new files, not edit files owned by other
  components, without a task-specific reason to do otherwise.

## 2026-08-24 Phase 3 judge

- **Judge prompt layout**: DESIGN.md pins the prompt's content (truncated
  request context, both full responses, the exact JSON-only instruction
  string) but not its exact formatting. `BuildPrompt` uses four labelled
  sections in that order ("Request context:", "Frontier response:", "Local
  response:", then the instruction), each separated by a blank line, so the
  judge model can tell the three inputs apart unambiguously.
- **Response text fed to the judge**: rather than the full raw message JSON
  envelope (ids, roles, usage), `ExtractResponseText` renders each side's
  content blocks as plain text: text blocks verbatim, tool_use blocks as
  `tool_use <name>(<input JSON>)`. This matches the verification cascade's
  own "concatenated text+tool_use content" comparison basis (DESIGN.md's
  cascade step 1) and keeps judge prompts far shorter than shipping the
  whole envelope, at no loss of the information the judge needs to compare
  two coding responses.
- **Only the request context is truncated** to `max_context_chars`
  (rune-safe, with a `...[truncated]` marker); both responses are always
  included in full, since DESIGN.md's config comment scopes
  `max_context_chars` to "request context truncation" specifically and the
  responses are already the compact one-turn text described above rather
  than full transcripts.
- **A batch-level errored/canceled/expired result, or a judge reply whose
  text has no parseable verdict object even after the lenient `{...}`
  extraction**, marks that `judge_items` row `status='errored'` and leaves
  its verification untouched (`agree` stays NULL, `stage` stays whatever
  the cascade left it as). DESIGN.md does not specify this failure path;
  leaving the verification undecided rather than guessing a fallback
  verdict keeps a failed judge call visible (it will show up as a gap
  between `replays` count and decided `verifications` count) rather than
  silently miscounted as agreement or disagreement. Re-running the cascade
  or re-queuing is a follow-up concern, not handled by `judge poll` itself.
- **Tests-vs-judge conflict rule when a verification carries no lint/test
  information at all** (its four `verifications.frontier_lint`/
  `local_lint`/`frontier_tests`/`local_tests` columns are all NULL or
  empty, which is the normal case for a non-edit turn since only edit
  turns run worktree lint/tests per the cascade): the judge's own verdict
  decides outright and `tests_judge_conflict` stays 0. DESIGN.md's conflict
  examples ("both sides linted clean", "either side failed lint/tests")
  both presuppose lint/test data exists; with none recorded there is
  nothing for tests to "win" with, so falling through to the judge is the
  only verdict available.
- **`splitter judge poll` iterates every `judge_batches` row with
  status='submitted'** in one invocation (one GET per batch), rather than
  polling a single batch. DESIGN.md's "single poll per invocation, no
  internal loop" rule is about never busy-waiting on ONE batch's status;
  checking several outstanding batches once each in the same cron-driven
  invocation stays within that rule (no batch is polled more than once per
  invocation) while letting a backlog of batches drain over consecutive
  cron ticks instead of only ever tracking one at a time.

## 2026-08-24 Discourse briefs

- Edward: "You can maybe also find briefings from Discourse, 'cause some of the
  fixes we've done would just be us pointing you at the discourse thread."
  Brief-source priority is now discourse > session > call > reverse_engineered >
  commit_subject. Discourse linkage is mechanical (topic URLs in commit bodies, PR
  bodies which embed Discourse links by repo convention, or session messages);
  FIRST post only via the public topic JSON endpoint, because later posts carry the
  diagnosis and would leak the fix.

## 2026-08-24 Phase 3 replay worker and verification cascade

- **`verifications.stage` for a middle-band row stays `"ast"`, never a
  separate `"band"` value**: the schema comment on `verifications.stage`
  enumerates exactly `exact|ast|judge`. The cascade's own stage 2/3 (AST
  similarity plus threshold comparison) is what produced the score whether
  or not a threshold decided it outright; the middle band is represented by
  `agree IS NULL` with `decided_ts IS NULL`, not a fourth stage string.
  `internal/judge` independently rewrites `stage` to `"judge"` once
  arbitration resolves a banded row, confirming this reading.
- **`internal/replay.Options.Backend`/`.Limit` empty/zero fall back to
  config** (`[replay].backend`, `[replay].batch_size`) rather than being
  treated as "replay nothing": matches every other splitter subcommand's
  flag-overrides-config convention (e.g. `-listen` on `proxy`).
- **The idle gate is scoped to `calls.source = 'proxy'`**: DESIGN.md
  motivates it as "runs when no proxy traffic for >30 min" (a cron/systemd
  timer's decision to fire), so a bootstrap `import` row, however recent,
  never blocks a replay run.
- **A replay backend call failure still inserts a `replays` row** (with
  `error` set, `response_zstd` NULL, no cascade run) rather than leaving
  the call unrecorded: this is what "records replays row (error non-fatal
  per call)" in DESIGN.md means literally, and it also means
  `SelectReplayCandidates`'s `NOT EXISTS` filter naturally stops
  re-selecting a call that failed once, rather than retrying it forever
  on every future run. A future retry policy (if wanted) is out of scope
  here.
- **An edit turn (`single_file_edit`/`multi_file_edit`) with no recorded
  `calls.repo_head`** (never populated, e.g. a manually inserted or
  imported row) falls back to the same token-similarity comparison used
  for non-edit turns, rather than erroring the whole replay: DESIGN.md
  requires `repo_head` for worktree comparison but does not say what to do
  when it is absent, and skipping the cascade entirely would silently lose
  a scoreable pair.
- **NotebookEdit application is recorded as an unsupported apply failure**
  (per-side lint entry `{"tool":"apply","ok":false,...}`), not applied:
  the task scope for this component names literal Edit old->new
  replacement and Write; reconstructing `.ipynb` cell JSON surgery is a
  disproportionate addition here. MultiEdit IS supported (each of its
  entries is exactly the same old->new primitive as Edit, applied in
  sequence, atomic per file), since DESIGN.md's turn_type rules list it
  alongside Edit/Write as an edit-family tool and it costs nothing extra
  to reuse the same primitive.
- **`SPLITTER_PORT_BASE` values for the `[tests]` command**: DESIGN.md
  says "PORT-offset env vars injected (`SPLITTER_PORT_BASE` + i)" without
  pinning a base or spacing. Frontier gets 20000, local gets 21000
  (`testPortBase`/`testPortSpacing` in `internal/verify/testcmd.go`), far
  enough apart that a test command binding a handful of sequential ports
  from its base cannot collide between the two worktrees run for the same
  cascade.
- **Lint/test JSON capping** truncates each entry's `output` field first
  (400 bytes) and only drops whole entries if the array is still over the
  2KB cap after that, so one verbose tool does not silently erase every
  other side's lint result; the encoder always produces valid JSON (worst
  case `"[]"` or `{"truncated":true}`), never a hard byte-sliced string
  that could break the column's JSON contract.
- **difft integration** shells out to `difft --display json OLD NEW` with
  `DFT_UNSTABLE=yes` (required by the installed 0.70.0 binary; unset, it
  refuses JSON output entirely) and maps `status:"unchanged"` to
  similarity 1, else `1 - (total changed line-pairs across all chunks) /
  len(aligned_lines)`, clamped to >= 0. DESIGN.md names a "changed-hunks /
  total-lines heuristic" without pinning difft's exact JSON shape; this is
  the closest mechanical reading of the real (unstable, may change)
  output format, verified against the actual installed binary rather than
  assumed.
- **Throwaway git repos built for `internal/verify`'s tests carry a
  `.gitattributes` with `* text=auto eol=lf`**, mirroring splitter's own
  repo-root file: this WSL host's global `core.autocrlf=true` otherwise
  converts every `git worktree add` checkout of a fresh `t.TempDir()`
  repo (which has no `.gitattributes` of its own) to CRLF, making every
  `gofmt -l` check in the cascade spuriously report "not formatted" for
  content that is actually correctly formatted.

## 2026-08-24 Phase 4 router and live routing

- **`internal/router.Family` signature takes the `[families]` overrides
  map as an explicit second argument** (`Family(model string, overrides
  map[string]string) string`) rather than reading config internally: the
  function has no other config dependency, and every caller (proxy at
  request time, `router update`, `internal/feature.PricingFor` via
  `pricingFamily`) already has the right map (`cfg.Families`, or `nil` for
  the pricing seam, which never applies live-routing overrides to a pure
  pricing lookup) sitting right there. `Category`/`FamilyPair` in the same
  package build the two router_state key strings from `Family`'s output,
  matching the schema comments verbatim ("<turn_type>|<subsystem>",
  "<frontierFamily>><localFamily>").
- **`internal/feature.PricingFor`'s `pricingFamily` seam now calls
  `router.Family(model, nil)`** (the earlier narrower stand-in described
  in the Phase 2 entry above is gone): `PricingFor`'s signature and every
  caller are unchanged, and all four of its own pinned examples still
  normalise identically since they are a subset of `Family`'s pinned
  examples. Overrides are not applied to pricing lookups since
  `PricingFor` never receives a config; a family override changes which
  router_state row a live request matches, not how that request's logged
  spend is priced.
- **Per-exact-version divergence detection tracks the LOCAL (backend)
  model as the "version"**, not the frontier model and not a joint
  (frontier, local) pair. DESIGN.md's "Model families" section names "a
  specific version" without saying which side; the motivating scenario
  throughout (Edward's framing, and the qwen2.5-coder:7b -> qwen3-coder:7b
  example) is a local model upgrade, and the local side is the one a live
  routing decision actually chooses, so it is the axis worth re-measuring
  per exact version. A frontier version bump is still covered by the
  family-level aggregation (family inheritance applies to both sides
  identically); only per-version *divergence tracking* is local-only.
- **A flagged divergence always sets `disabled_reason` and therefore
  always forces `routable=false`** for that category (via `Routable`'s
  `disabledReason == ""` requirement), rather than leaving routability to
  fall out incidentally from whether the recomputed (divergent-rows-only)
  n/wilson_lb happen to still clear the bar. DESIGN.md's own wording
  ("may drop it below the routable bar and disable routing for it") reads
  as two things happening together, and a version doing materially worse
  than its family's history is exactly the situation the auto-disable
  mechanism exists for regardless of whether the smaller divergent-only
  sample happens to still scrape past minN/minWilsonLB by chance. The flag
  and its recomputed basis (version, n, its rate, the family rate it
  diverged from) are rendered into `disabled_reason` as one auditable
  string (`"divergent_version:<version>(n=..,rate=..%,family=..%)"`),
  since `router_state` has no separate stats JSON column to put them in
  instead (DESIGN.md's "disabled_reason or stats JSON" phrasing is read as
  offering that choice; `router_decisions.stats` is the JSON column that
  exists, and it is used for logging individual *live* decisions, not for
  an offline `router update` run's per-category divergence bookkeeping).
- **`internal/feature.RequestOnly`, the request-only turn_type/subsystem
  inference Phase 4 needs, is genuinely new classification logic, not a
  call to the existing `ClassifyTurnType(req, nil)`.** Tried literally,
  `ClassifyTurnType` with an empty response block slice can only ever
  return "plan" or "other": its tool_result_summary/question_answer rules
  both require the response to already be text-only, which is unknowable
  before that response exists, so passing `nil` makes them permanently
  false and the live router would never route anything but the rare "plan
  mode is active" turn. `RequestOnly` instead treats the last message's
  own shape as the signal for those two categories (a trailing
  `tool_result` predicts "tool_result_summary", trailing plain text
  predicts "question_answer"), dropping the unknowable response-text-only
  condition rather than asserting it false. It can never return
  single_file_edit/multi_file_edit at all (those describe tool calls the
  pending response makes, not derivable from the request), which is an
  accepted, permanent limitation of routing ahead of generation: those
  categories, even when routable per `router_state`, are never selected
  live. subsystem is derived from the most recently touched file across
  every assistant message already in the request's own history (Claude
  Code resends the full transcript every turn, so this is genuinely
  request-only, no capture-log lookup involved).
- **A locally-served or escalated live-routing turn never writes a
  `calls` row**, only a `router_decisions` row. `calls` is Phase 1's
  record of an actual proxy<->upstream exchange (its `model`/`status`
  columns and the featuriser/replay pipeline built on it all assume that);
  a locally-served turn never talks to upstream at all, and inserting a
  misleading row (client-requested model, locally-generated content)
  would corrupt those downstream assumptions for no benefit, since
  `router_decisions.stats` already carries everything the weekly report
  needs (frontier tokens avoided, local token counts). A dual-dispatched
  ("shadow") turn *does* still write a normal `calls` row, since it goes
  through the ordinary upstream pass-through/capture path unchanged; only
  its extra `router_decisions` row and the async shadow replay are new.
- **Dual dispatch's "hash(call ordinal) % 20 == 0" is generalised to
  `hash(ordinal) % 100 < pct`** (`internal/router.IsDualDispatchOrdinal`),
  exactly equivalent to the literal "% 20 == 0" at the default 5%
  ([router].dual_dispatch_pct), but honouring a differently configured
  percentage instead of hardcoding 20. `hash` is a fixed 64-bit
  multiplicative (Knuth) hash of the ordinal, not the identity function:
  the ordinal is a simple atomically-incrementing per-process counter, and
  hashing it keeps the selection independent of any future change to how
  ordinals are assigned, at zero cost. The counter itself is process-local
  and resets on restart (matches the session circuit breaker's own
  "until proxy restart" scope; DESIGN.md does not ask for cross-restart
  persistence of the 5% cursor).
- **The escalation check consumes a session's pending locally-served
  record at most once** (`LiveRouter.TakePending` deletes on read), mirroring
  Phase 2's `had_error_followup`, which only ever looks at "the next
  call" after the one being featurised. DESIGN.md's live-routing wording
  ("if a locally-served turn is followed within the session by an
  error/retry signal") does not say whether a second, third, ... later
  request in the same session should also be checked against the same
  pending record; checking once, on the very next request, is the
  literal Phase 2 semantics this is explicitly reusing.
- **Shadow drift comparison (`internal/router.roughAgree`) is a small,
  self-contained cheap comparison, not a call into `internal/verify`'s
  cascade.** `internal/verify.Verifier` needs ephemeral git worktrees,
  lint tooling and (optionally) a test command; running it synchronously
  from a per-request background goroutine on every dual-dispatched live
  turn would be far heavier than the drift signal needs (DESIGN.md itself
  only asks for "async local shadow" for "drift detection", not a
  verification-grade score). `roughAgree` decodes both sides' Anthropic
  content blocks, and agrees on an exact normalised-text match or (else) a
  token-set Jaccard similarity >= 0.6; this feeds only
  `router_decisions.stats.shadow_agree`, the weekly report's disagreement
  rate, never anything that disables a category or trips a breaker.
- **No `router_decisions` row is ever logged for a plain pass-through or
  for the kill switch**, despite `decision TEXT` naming `frontier` and
  `killswitch` as valid values in its schema comment: those values are
  documented outcomes of the decision space, not outcomes this
  implementation writes a row for on every single request. Live routing
  when off (or gated off by the session breaker, or by no routable
  category matching) is designed to be indistinguishable from Phase 1
  pass-through at the DB level, matching "anything else = pure
  pass-through" (BRIEF.md/DESIGN.md); logging a row per ordinary request
  would also make `router_decisions` grow at full traffic volume instead
  of only at the rate of actual local/shadow/escalated decisions.
- **`gofmt -w` was applied to three pre-existing, untracked
  `internal/evals/*.go` files** (`ladder.go`, `run.go`,
  `seed_history.go`) left behind by an unrelated, separately-scoped build
  attempt for the eval library (not part of this task): whitespace-only
  struct-field alignment fixes, no logic touched. Left unformatted, they
  fail the "gofmt -l is clean for the whole module" exit bar this task
  (like every task here) must leave green; nothing else in that package
  was read or modified.

## 2026-08-24 eval library: characteristics, brief derivation, ladder evaluation

Implemented `internal/evals` (task characteristics, brief derivation, ladder
evaluation, harvest/add/seed-history/reverse-briefs/run/list) and
`cmd/splitter/cmd_eval.go`, per DESIGN.md "Eval library". Deviations and
interpretation choices, since DESIGN.md leaves several mechanics unpinned:

- **Discourse-sourced briefs are NOT implemented in this pass.** DESIGN.md
  describes a `brief_source='discourse'` path (topic URL in commit/PR body
  or session message, fetched via the topic `.json` endpoint), but the task
  scoping this work explicitly enumerated only session walk-back,
  commit-subject-marked-for-reversal, and `eval reverse-briefs` as in
  scope, and DECISIONS.md already records the Discourse API key is not on
  this machine yet (held on the prod batch host). `BriefSourceDiscourse` is
  not defined; brief_source values produced are session, call,
  commit_subject, reverse_engineered and manual (harvest never emits
  discourse, so its stated source priority — discourse > session > call >
  reverse_engineered > commit_subject — collapses to session > call for
  live tasks with the discourse tier simply never populated yet). Adding it
  is a follow-up: it slots in as another `sessionInitiatingText`-style
  lookup tried before the session walk-back in `DeriveBrief`.
- **Layer is derived from the FIRST touched file only** (`Layer(files,
  layers)` mirrors `internal/feature.Subsystem`'s "first touched file"
  convention), not a vote or a "mixed" value across files, since DESIGN.md
  states this rule for `language` explicitly but is silent for `layer`.
  Layer-pattern matching (`layerPatternMatches`) treats a pattern ending in
  "/" as matching any path segment (glob-aware) anywhere in the path, and
  any other pattern as matching the file's base name (falling back to the
  whole path); patterns are tried in sorted-key order for determinism when
  more than one would match the same file, since `map[string]string`
  iteration order is not stable.
- **Nature's keyword match is a plain case-insensitive substring test**
  against the whole subject line (e.g. "add" matches "added"), the
  simplest mechanical reading of DESIGN.md's "commit-subject keywords"; the
  five keyword groups are checked in DESIGN.md's listed order (fix/bug/
  revert first) before the "only test files changed" diff-shape fallback.
  For a **harvested (live) task, which has no commit subject**, the
  already-derived brief text stands in for it: the closest available
  mechanical proxy for "what this task is about", since a live captured
  call carries no commit message at all. Seed-history feeds the real git
  commit subject instead, matching DESIGN.md literally.
- **Rung's "small" size band (`turn_type == single_file_edit && context <
  8KB`) is applied uniformly across all three difficulty bands**, even
  though DESIGN.md's rung list states the context-size qualifier
  explicitly only for rungs 1-2 ("single_file_edit, context under 8KB")
  and drops it in the rung 3-6 wording ("single-file" / "multi-file or
  large"). Reusing one size predicate for every band is the simpler
  reading and keeps the six rungs a clean 3 (difficulty) x 2 (size)
  grid, rather than inventing a second, unstated size rule for rungs 3-6.
- **DESIGN.md's seed-history dedup key contradicts its own repo_head
  rule**: the seed-history bullet says tasks are "deduped by commit sha
  stored in repo_head plus brief" and separately that `repo_head = the
  commit's PARENT sha (the starting state)". Both cannot be true at once
  (repo_head cannot simultaneously be the commit's own sha for dedup and
  its parent's sha for worktree checkout). Resolved by function, not by
  picking one sentence over the other: `repo_head` stays the PARENT sha
  (required for `eval run`'s worktree checkout, and consistent with every
  other origin's repo_head meaning "the state this task starts from"), and
  the commit's OWN sha is recorded separately as `characteristics.
  commit_sha`, which is what seed-history's dedup check
  (`loadExistingHistoryShas`) and `eval list`'s "short sha" display
  (`shortSHA`) actually use for a history-origin task. DESIGN.md's final
  seed-history line ("The commit sha is recorded so `eval list` shows 'git
  commit number + brief'") only makes sense under this reading.
- **Seed-history's "skip... non-code" filter is per-file, not
  whole-commit**: a commit's binary or non-"code"-extension files
  (`seedTextExtensions`, a broad text/config allowlist: go/php/vue/js/ts/
  sql/sh/py/rb/md/yml/yaml/json/css/scss/html/toml) are simply excluded
  from its touched-file set; the whole commit is skipped only when that
  leaves zero eligible files, or the remaining files exceed `-max-files`/
  `-max-diff-lines`/the 20KB context cap. Skipping any commit that touches
  even one non-code file (e.g. a `.png` alongside a `.go` fix) would
  discard far more real commits than DESIGN.md's stated intent ("tiny,
  single-turn-fair" commits) requires.
- **Renamed files are not modelled**: `changedFiles` runs `git diff
  --no-renames --name-status`, so a rename surfaces as a plain delete plus
  add rather than an `R100` status this package would otherwise need a
  third code path for. Deleted files are dropped (nothing to build an
  Edit/Write task from); DESIGN.md does not mention renames or deletions.
- **Reverse-briefs' submit/poll state lives entirely inside
  `eval_tasks.characteristics.reverse_brief`** (status/batch_id/custom_id),
  not a new table: the task instructions for this component scoped store
  migration changes to "the eval_runs ladder/tokens_in/tokens_out columns
  and any eval-table COLUMNS DESIGN.md requires that the skeleton's
  migration lacks", not new tables. Since `characteristics` is already a
  free-form JSON column on every eval task, tracking one pending batch's
  progress there avoids a schema change entirely; `eval reverse-briefs
  -poll` groups tasks by `characteristics.reverse_brief.batch_id` (loaded
  via `EvalTasksByOrigin` and filtered/grouped in Go, matching this
  package's existing convention of doing JSON-shaped logic in Go rather
  than relying on SQLite's JSON1 extension).
- **`eval run`'s middle-band stage is written as literal `"band"`** into
  `eval_results.stage` (DESIGN.md: "stage 'band'"), which is a plain TEXT
  column with no enum constraint, unlike `verifications.stage` (constrained
  by that table's own comment to `exact|ast|judge`, per the Phase 3
  component's own decision to keep a banded verification's stage at
  `"ast"`). `internal/verify.Result.Stage` itself never reports "band"; the
  override happens in `evals.scoreOneTask` purely for the eval_results row.
- **A backend call failure or a verification-cascade error during `eval
  run` is recorded as `passed = 0` (not passed) with the error text in
  `eval_results.error`**, rather than leaving `passed` NULL (which DESIGN.md
  reserves for `ladder_skipped` rows only). An attempted-but-failed task is
  not the same as a never-attempted one: the ladder needs a concrete
  pass/fail outcome to keep climbing correctly, and "the candidate backend
  could not produce a scoreable answer" is itself a failure worth counting
  against it, not a gap to leave undecided.
- **Harvested (live) tasks' `characteristics.size.diff_lines` is a coarse
  estimate**, not a real diff: the sum of old+new line counts across the
  frontier response's edit-family tool_use blocks (a Write's whole content
  counts as pure addition). Live capture has no "diff" concept the way a
  git commit does; this is recorded only for the non-monotonic size bucket
  in scorecards, never used to derive difficulty, so an approximate count
  is acceptable.
- **`internal/config` gained `EvalsConfig` (`[evals]`) and `ModelCutoffs`
  (`[model_cutoffs]`)**, and `config.example.toml`/`internal/config`'s
  `Default()` were extended to match: DESIGN.md's ladder (`ladder_track`,
  `stop_wilson_upper`, `stop_min_n`, `futility_consecutive_fails`) and
  contamination-guard (`[model_cutoffs]`) settings belong to the eval
  library specifically and no other component defines them; `internal/
  config` is shared infrastructure (not one phase's owned file) and this
  task's own config surface could not exist without it.
- **Fixed a pre-existing, unrelated test failure**:
  `internal/config/config_test.go`'s `TestLoad_ExampleConfigParses`
  asserted `len(Backends) == 4` against `config.example.toml`, which
  already had 5 backends (`deepseek` added by a concurrently-built task,
  noted in this file's own "2026-08-24 Phase 2 featuriser" entry as a
  known, deliberately-left failure belonging to a different component).
  Since this task's own exit bar is "`go test ./...` passes for the whole
  module" and the fix is a one-line, unambiguous correction (matching
  reality, not a design change), it was corrected to 5 and the backend-name
  loop extended to include `deepseek`, rather than left broken.
- **`internal/evals.Family`/`ModelCutoff`/`CutoffSegment` duplicate
  `internal/router.Family`'s algorithm locally**, same rationale already
  recorded for `internal/feature.PricingFor`'s `pricingFamily`: at the time
  this task started, `internal/router` (Phase 4) did not exist yet, so a
  build dependency on it was not available; both implementations were
  independently validated against DESIGN.md's own worked examples
  (`claude-opus-5[-date]`, `qwen2.5-coder:7b`, `Qwen/Qwen2.5-Coder-32B-
  Instruct`, `gemini-2.5-flash`, `gpt-4o-mini`) and should agree for every
  model id either package touches. `internal/router` has since landed
  (built concurrently); consolidating `evals` to call `router.Family`
  directly is a natural, low-risk follow-up but out of scope for this task
  (which owns `internal/evals`, not `internal/router`).
- **Manual (`eval add`) tasks get `localization = given` and `brief_source
  = manual`**: DESIGN.md's localization rule ("seeded tasks hand the model
  the files ... live tasks let the agent find them") only names the
  seeded/live split; a manually-supplied request is closer in spirit to a
  seeded one (the operator hands over the request directly), and `manual`
  is the natural `brief_source` value for an operator-authored brief
  (DESIGN.md's brief-derivation rules cover session/call/history but not
  this manual path explicitly).

## 2026-08-24 agentic eval mode implementation (internal/agentic)

Implemented `internal/agentic` (sandbox lifecycle, tool loop, fail-to-pass
grading, cheat detectors) and `cmd/splitter/cmd_eval_agentic.go`
(`eval-agentic` subcommand), per DESIGN.md "Agentic eval mode". Deviations
and interpretation choices:

- **`tests_ran`/`tests_passed` are the harness's own final grading pass**,
  not a count of the model's own run_tests calls: DESIGN.md pairs them as
  one triple ("record tests_ran, tests_passed (held-out), regressions...")
  and calls tests_ran itself "ran the tests at all" as a reported
  capability. Read literally as one grading operation's output: for a
  Go held-out task, tests_ran/tests_passed count how many of the held-out
  test names the FINAL `go test -json` pass actually observed
  running/passing (a build failure that prevents a test from running at
  all is "0 ran", not "0 passed"); for a coarse (non-Go, or harvested live
  task) grade with no per-test data, they become 1/0 from the final
  command's exit code. Separately, since "ran the tests at all" also reads
  naturally as a MODEL-behaviour signal (did it bother to check its own
  work), `ToolExecutor.TestsRanByModel()` tracks the model's own run_tests
  invocations during the loop too; this is surfaced as `ModelRanTests` in
  the eval-agentic scorecard (`TrackTally`/`taskOutcome`) but not added to
  the `eval_results` schema, keeping the DB column's meaning matched to
  DESIGN.md's literal grading-triple reading.
- **git_poke REFUSES the call, not just flags it**: DESIGN.md's detector
  list says escape is "flag and refuse the call" and describes git_poke as
  only "any read_file/grep/list_dir touching .git paths" (flag implied,
  refusal not stated). Refusing git_poke too (ToolExecutor returns
  isError=true and never actually reads/lists/greps the .git path) is a
  stricter, safer reading that costs nothing: DESIGN's own contamination
  worry ("might be smart enough to find the upstream repo and establish a
  fix") is better served by never handing over .git content at all, on top
  of recording the flag for audit.
- **Held-out test commands are Go-only in this pass**: `evals.
  buildHoldoutPayload` only derives a `TestCmd` (`go test -json
  ./pkg/...`) when every held-out test file in a commit is `.go`.
  DESIGN.md's "Prep online" step mentions `composer install if
  configured`, implying PHP support was in scope, but deriving a safe,
  generic phpunit/vitest/jest invocation for an arbitrary historical
  commit (correct config file, correct working directory, correct
  autoloader state) needs per-repo tooling knowledge seed-history has no
  safe way to guess; a wrong guess would silently mis-grade every non-Go
  history task rather than just skipping it. Non-Go holdout payloads are
  still stored (`holdout_tests_zstd` populated, `TestCmd` empty) for a
  future pass to add real support for; `selectAgenticTasks` skips them
  (counted in `RunSummary.NotGradedNoTestCommand`) rather than attempting
  a coarse exit-code grade against unknown tooling. `PrepDependencies`
  itself still runs `go mod download`/`npm ci` unconditionally when their
  lockfiles are present (useful prep for a task's non-test dependencies
  regardless of held-out test language); `composer install` is not wired
  in, since no held-out grading path can use its result yet.
- **`internal/verify`'s worktree git plumbing (`addWorktree`/
  `removeWorktree`/`pruneWorktrees`) is duplicated, not exported and
  reused**: those three helpers are unexported, and this package's sandbox
  shape (one worktree per task, not `verify`'s frontier/local pair, no
  concurrency semaphore) does not fit `verify`'s own `Verifier`-scoped
  lifecycle cleanly. The git plumbing itself (three `exec.Command` calls,
  ~10 lines each) is mechanical and identical; duplicating it was judged
  lower-risk than editing a component this task does not own to export
  internals for a single external caller. `internal/verify`'s own tests
  and behaviour are untouched.
- **`tokenSimilarity`/`tokenLevenshtein` are duplicated from
  `internal/verify`, not exported and reused**, same rationale: both are
  unexported in `internal/verify/similarity.go`, and this package needs
  the same normalized-Levenshtein primitive for `suspect_copy`'s
  file-content comparison. A small, generic string-similarity utility, not
  business logic; kept local rather than touching another component's
  file for one reuse.
- **`evals.BuildRunBackend`, `evals.ReplayFunc`, `evals.ScheduleTasks`/
  `ScheduledTask`, and `evals.ApplyReconstructedEdits` were exported**
  (renamed from `buildRunBackend`/`replayFunc`/`scheduleTasks`/
  `scheduledTask`/`applyReconstructedEdits`), each a pure rename with no
  behaviour change, their own tests untouched apart from the one call site
  `applyReconstructedEdits` had in `seed_history_test.go`. This is the
  "reuse rather than duplicate" the task brief asked for: agentic's tool
  loop drives the same backend resolution `eval run` uses (so an
  OpenAI-compatible or `-backend anthropic` model works unchanged),
  `eval-agentic` climbs the same per-track ladder ordering as `eval run`,
  and `suspect_copy` reconstructs the withheld reference fix's final file
  content with the same apply logic `seed-history`'s own round-trip test
  already relied on.
- **`SplitTestFiles`, `HoldoutPayload`/`HoldoutFile`, `DecodeHoldoutPayload`
  added to `internal/evals`** (new file `holdout.go`), and `seed_history.go`
  extended (not rewritten) to populate `eval_tasks.holdout_tests_zstd` and
  `characteristics.agentic_test_cmd` for a commit with test-file changes.
  `SplitTestFiles` reuses the existing `layerForPath` "tests" classification
  (DESIGN.md's own `*_test.*`/`tests/`/`spec/` layer defaults), so no new
  test-file heuristic was invented. `HoldoutFile.Hunks` keeps the
  package-private `diffHunk` element type (`Old`/`New` exported fields,
  readable cross-package via range/field access without naming the type);
  exporting `diffHunk` itself was unnecessary.
- **`agentic_ready`/`holdout_tests_zstd` (eval_tasks) and `mode`/`turns`/
  `tests_ran`/`tests_passed`/`regressions`/`transcript_zstd`/`cheat_flags`
  (eval_results) were added directly to `internal/store`'s v1 migration**
  (the DB is pre-release; DESIGN.md's agentic section explicitly sanctions
  this: "extend the v1 migration in place"), and `EvalTaskRow`/
  `EvalResultRow` plus their existing scan/insert functions in
  `eval_store.go` were extended to carry the new columns, alongside the
  brief's own `internal/store/agentic_store.go` for the two genuinely new
  queries (`UpdateEvalTaskAgenticReady`, `AgenticGradableEvalTasks`):
  the new columns belong to tables every existing caller of `EvalTaskRow`/
  `EvalResultRow` already reads/writes in full, so the struct/column list
  could not be extended without touching the file that owns them.
  `InsertEvalResult`'s `mode` column is always named in its INSERT
  statement (a per-row `UNIQUE`/join target, not sparse), so SQLite's own
  `DEFAULT 'single'` never applies once a value (even an unset Go zero
  string) is supplied in the column list; `InsertEvalResult` substitutes
  `"single"` for an empty `row.Mode` itself, so every pre-existing
  single-turn caller (`internal/evals.Run`, never edited) keeps its
  original behaviour with no code change on its side.
- **`suspect_copy`'s size weighting uses the task's own
  `characteristics.size.diff_lines`** (already computed at seed-history
  time) rather than a fresh character count of the compared patch text:
  DESIGN.md asks for "weighted by patch size" without pinning a unit, and
  the eval library already has a maintained, evidence-based diff-lines
  figure for exactly this purpose (its own non-monotonic size bucketing);
  reusing it avoids a second, potentially inconsistent size measure.
- **`-allow-network` skips `unshare -rn` entirely rather than merely
  "marking" a live network-denied run untrusted**: DESIGN.md names the
  flag's two purposes ("for debugging, or as the only way to proceed when
  unshare is unavailable") without saying whether it should still attempt
  unshare when available. Skipping it unconditionally when the flag is
  passed keeps the flag's behaviour identical regardless of whether
  unshare happens to be installed (debugging network-dependent test
  commands works the same way either environment), at the cost of a
  literal read of "marks every result untrusted" being satisfied via a
  `CheatFlagAllowNetwork` entry appended to every task's `cheat_flags`
  in that run, rather than a separate boolean.
- **`eval-agentic` is a standalone top-level subcommand** (`register(
  "eval-agentic", ...)` in its own `cmd_eval_agentic.go`), not a verb
  nested under the existing `splitter eval <verb>` dispatcher in
  `cmd_eval.go`: the task brief names it literally ("cmd_eval_agentic.go:
  'eval-agentic' subcommand"), and it has a materially different flag
  surface (`-allow-network`, no `-backend anthropic`-style verb sharing
  with `harvest`/`add`/`seed-history`/etc.) that does not fit
  `runEval`'s existing switch cleanly.
- **PHP/JS lockfile prep (`npm ci`) runs at the sandbox root only**, plus
  one optional subsystem-named subdirectory for a `go.mod` (a monorepo
  component in its own Go module); DESIGN.md's "Prep online" step does not
  specify how deep into a monorepo to search, and the eval library's own
  `subsystem` field (first path segment of the touched files) is the only
  mechanical signal available without walking the whole tree.

## 2026-08-24 fix: eval requests need a real max_tokens floor

- **Bug (proven live against a real DeepSeek V4 Flash eval run)**: every
  synthesized eval request (seed-history's `buildSeedRequest`) carried no
  `max_tokens`, so `internal/backend.ToOpenAI`'s own 4096 fallback applied
  on dispatch. DeepSeek V4 Flash is a REASONING model: its reasoning
  tokens bill as output, so reasoning alone consumed the entire 4096
  before any answer, and two of five tasks in a real run came back
  `stop_reason=max_tokens` with zero content blocks, silently scored as
  model failures the model never got a chance to attempt. Evidence:
  eval_results run 2, task ids 1 and 5, `response_zstd` decompresses to
  `content:[]` with `stop_reason:"max_tokens"`.
- **Fix**: added `[evals].max_answer_tokens` (default 16384) to
  `internal/config`. `internal/evals.applyMaxAnswerTokensFloor` raises a
  request's `MaxTokens` to this floor whenever it is lower (covering both
  an absent value and an old, too-low one from before this floor
  existed), called from `scoreOneTask` (`eval run`) right before
  dispatch, so already-seeded tasks are fixed without re-seeding.
  `buildSeedRequest` (seed-history) also now takes the resolved floor as
  a parameter instead of hardcoding 4096, so freshly-seeded tasks carry
  the right value from the start too. `internal/agentic`'s tool loop
  gained the same floor as `TaskBounds.MaxAnswerTokens`
  (`BoundsFromConfig` resolves it from the same config key, defaulting
  when unset so a caller that skips `BoundsFromConfig` — e.g. a test
  building `TaskBounds{}` directly — still never sends `max_tokens=0`).
  Harvested (live-capture) requests were not touched: they carry the real
  request Claude Code itself sent, with its own real `max_tokens`, not a
  synthesized one.
- **Tests**: `internal/evals.TestApplyMaxAnswerTokensFloor` (table-driven:
  absent/low/equal/explicit-larger `max_tokens`) plus
  `TestRun_FloorsMaxTokensOnDispatch` (httptest end-to-end: one stored
  task with no `max_tokens` arrives at the fake backend with 16384, one
  with an explicit 50000 keeps it), and the `internal/agentic` mirrors
  (`TestRunLoop_RequestsCarryMaxAnswerTokens`,
  `TestRunLoop_UnsetMaxAnswerTokensFallsBackToDefault`,
  `TestBoundsFromConfig_MaxAnswerTokens`).
- **Also in this pass** (small items in files this task already owns):
  - `[evals].seed_context_bytes` (default 65536, was a hardcoded 20KB)
    replaces the previous unconditional `seedContextCapBytes` constant: 3
    of 5 hand-picked seed candidates were blocked by the old cap because
    a single real touched source file alone exceeded 20KB. Tested by
    `TestSeedHistory_ContextCapIsConfigurable` (the raised default admits
    a commit that modifies an existing ~28KB file; a caller-configured
    lower cap still excludes it).
  - `[layers]` defaults gained `"iznik-batch/" = "backend-api"`: a
    harvested PHP task under `iznik-batch/` came back `layer=""`.
    DESIGN.md's layer taxonomy has no separate "batch" category, and
    `iznik-batch` is Laravel scheduled-job/business logic (CLAUDE.md:
    "runs Laravel scheduled jobs against production DB"), the same kind
    of server-side code `iznik-server-go/` already maps to backend-api,
    so it was folded into that bucket rather than inventing a new layer
    value DESIGN.md's scorecard grouping does not expect.

## 2026-08-24 evals-first onboarding

- **Watching-first was too slow**: Edward: "The approach of watching for weeks
  before you actually do anything is too slow... you'd install this, you'd run
  the evaluation builder, then you'd evaluate some models, then you'd at least
  have some candidates." Changes: (1) `splitter bootstrap -backends a,b` chains
  seed-history, reverse-briefs (bounded wait), eval run per backend, and router
  update; (2) router update now counts TRUSTED eval results (scored, no error,
  no cheat flags) as evidence alongside replay verifications, with
  history-seeded tasks carrying frontier family "human" because their reference
  is the real committed fix; (3) README reframed evals-first, with the passive
  capture/replay side positioned as ongoing confirmation. Guided
  (commit_subject) briefs still count toward candidacy; the brief_source mix
  remains visible in eval reporting, and reverse-briefs runs inside bootstrap
  by default precisely to keep that evidence honest.
- **README split**: Edward: too many subcommands and config items cluttering
  the README. Full tables moved to COMMANDS.md and CONFIG.md; the README keeps
  only the eight commands someone trying it would actually run.

## 2026-08-24 brief quality pass

- **Reverse-brief leaks**: Edward asked to see the actual briefs. Three of 18
  leaked the answer (one stated the fixed value outright, one handed over the
  root-cause diagnosis, one described the post-fix behaviour). The rewrite
  instruction now also forbids explaining why the problem happens and
  describing what the code does "now". Existing briefs keep working; re-run
  reverse-briefs after a re-seed to regenerate with the stronger prompt.
- **Docs commits dominated the sweep** (10 of 18 tasks were markdown, mostly
  plans/ scratch): size-filtered sweeps love tiny docs commits, and
  "reproduce this plan document" is weak eval material. seed-history now caps
  docs-nature tasks at a configurable share of inserted tasks
  (-max-docs-share, default 0.3), with the skip counted and printed.

- **Repair outcome**: the three leaky briefs were reset and regenerated under
  the stronger prompt. Two came back clean; one (the no-progress batch loop)
  still volunteered a diagnosis sentence, milder than before. Reverse-brief
  quality is bounded by the judge model; [judge].model is the lever (Sonnet
  instead of Haiku) when brief quality matters more than pennies.
