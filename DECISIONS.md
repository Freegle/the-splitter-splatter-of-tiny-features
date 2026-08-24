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
