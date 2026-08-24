# Splitter design spec

Read BRIEF.md first. This file pins the concrete design so all components fit together.
When this file and BRIEF.md conflict, BRIEF.md wins; log the conflict in DECISIONS.md.

## Module

- Standalone repo (github.com/Freegle/splitter); the Go module is the repo root. `go 1.25`.
- Single static binary `splitter` with subcommands. No cgo (SQLite via `modernc.org/sqlite`).
- Dependencies (keep to exactly these plus stdlib):
  - `modernc.org/sqlite` (database/sql driver, pure Go)
  - `github.com/klauspost/compress/zstd`
  - `github.com/BurntSushi/toml`
- Raw HTTP for all model APIs. No provider SDKs: the proxy re-serves the Anthropic wire
  format verbatim, so this module works at the HTTP level by nature, and the brief's
  simplicity constraint rules out SDK dependency trees. (Recorded in DECISIONS.md.)

## Layout and ownership

```

go.mod
BRIEF.md DESIGN.md DECISIONS.md README.md
config.example.toml
.gitignore                  # *.db *.db-wal *.db-shm splitter binary
cmd/splitter/
  main.go                   # subcommand registry only (see below)
  cmd_proxy.go cmd_featurise.go cmd_replay.go cmd_judge.go
  cmd_report.go cmd_router.go cmd_import_history.go
internal/config/            # TOML + env loading
internal/store/             # SQLite open/migrate/queries + zstd helpers
internal/anthropic/         # Messages API types + SSE assembly
internal/backend/           # OpenAI-compatible clients + request/response translation
internal/proxy/             # Phase 1 pass-through proxy (+ Phase 4 routing path)
internal/feature/           # Phase 2 featuriser
internal/replay/            # Phase 3 replay worker
internal/verify/            # Phase 3 verification cascade
internal/judge/             # Phase 3 Haiku batch judge
internal/router/            # Phase 4 router statistics + decisions
systemd/                    # splitter-proxy.service, splitter-replay.service, splitter-replay.timer (user units)
scripts/install.sh
testdata/
```

### Subcommand registry (avoids merge conflicts between agents)

`main.go` holds only:

```go
var commands = map[string]func(args []string) error{}
func register(name string, fn func(args []string) error) { commands[name] = fn }
func main() { /* dispatch os.Args[1] via commands, print usage listing sorted names */ }
```

Each `cmd_*.go` file adds itself in `init()` via `register("proxy", runProxy)` etc.
Never edit main.go to add a command.

## Config

Loaded from `--config` flag, else `$SPLITTER_CONFIG`, else `~/.config/splitter/config.toml`,
else built-in defaults. Env file (`env_file`, default `~/.config/splitter/env`) is parsed as
KEY=VALUE lines and loaded into the process env at startup WITHOUT overriding already-set
vars. API keys are only ever read from env vars named by `api_key_env`. Secrets never
appear in the TOML, the DB, logs, or the repo.

```toml
listen = "127.0.0.1:9925"
upstream = "https://api.anthropic.com"
db_path = "~/.local/share/splitter/splitter.db"   # created 0600, parent dirs 0700
repo_path = "/home/edward/FreegleDockerWSL"        # target repo: HEAD capture + verify worktrees
env_file = "~/.config/splitter/env"

[replay]
backend = "ollama"            # default replay backend, -backend flag overrides
idle_minutes = 30             # replay refuses to run if a call was logged more recently
max_concurrent_worktrees = 2
batch_size = 100              # max calls per replay run

[backends.ollama]
base_url = "http://localhost:11434/v1"
model = "qwen2.5-coder:7b"

[backends.together]
base_url = "https://api.together.xyz/v1"
api_key_env = "TOGETHER_API_KEY"
model = "Qwen/Qwen2.5-Coder-32B-Instruct"

[backends.gemini]
base_url = "https://generativelanguage.googleapis.com/v1beta/openai"
api_key_env = "GEMINI_API_KEY"
model = "gemini-2.5-flash"

[backends.openai]
base_url = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"
model = "gpt-4o-mini"

[judge]
model = "claude-haiku-4-5"
api_key_env = "ANTHROPIC_API_KEY"
max_context_chars = 8000      # request context truncation for judge prompts

[thresholds]                  # cascade similarity thresholds
default_high = 0.9
default_low = 0.5
# per language+turn_type override, key "<lang>/<turn_type>":
# [thresholds.overrides]
# "go/single_file_edit" = { high = 0.92, low = 0.55 }

[tests]                       # optional per-subsystem test command for the verify cascade
# "iznik-server-go" = "true"  # placeholder; real commands are user-configured

[router]
min_n = 30
min_wilson_lb = 0.9
dual_dispatch_pct = 5
```

All four backends above ship in `config.example.toml`; keys already live in
`~/.config/splitter/env` on this machine (assembled from answerbot, ~/.config/freegle,
FreegleDockerWSL). Ollama needs no key.

## SQLite schema (internal/store)

One migration function, `PRAGMA user_version` versioning, WAL mode, busy_timeout 5s.
DB file chmod 0600 after create. Every table has a surrogate `id INTEGER PRIMARY KEY
AUTOINCREMENT` even where a natural key exists (house rule). Timestamps are RFC3339 UTC
TEXT. All large JSON is zstd-compressed BLOBs via store helpers `Compress/Decompress`.

```sql
CREATE TABLE calls (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  session_id TEXT,
  model TEXT,
  stream INTEGER NOT NULL DEFAULT 0,
  request_zstd BLOB,
  response_zstd BLOB,          -- for SSE: the assembled complete message JSON
  input_tokens INTEGER,
  output_tokens INTEGER,
  latency_ms INTEGER,
  repo_head TEXT,              -- git HEAD of repo_path at capture time
  status INTEGER,              -- upstream HTTP status
  error TEXT                   -- proxy-internal error if any (fail-open trail)
);
CREATE INDEX idx_calls_session ON calls(session_id, id);

CREATE TABLE features (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  call_id INTEGER NOT NULL UNIQUE REFERENCES calls(id),
  turn_type TEXT NOT NULL,     -- tool_result_summary|single_file_edit|multi_file_edit|plan|question_answer|other
  files_touched TEXT NOT NULL DEFAULT '[]',   -- JSON array of repo-relative paths
  subsystem TEXT,              -- top-level dir of files_touched, '' when none
  context_tokens INTEGER,
  output_tokens INTEGER,
  had_error_followup INTEGER   -- nullable: 1/0, NULL when next call unknown yet
);

CREATE TABLE replays (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  call_id INTEGER NOT NULL REFERENCES calls(id),
  backend TEXT NOT NULL,
  model TEXT NOT NULL,
  response_zstd BLOB,          -- backend response translated to Anthropic message JSON
  latency_ms INTEGER,
  error TEXT,
  created_ts TEXT NOT NULL,
  UNIQUE(call_id, backend, model)
);

CREATE TABLE verifications (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  replay_id INTEGER NOT NULL UNIQUE REFERENCES replays(id),
  stage TEXT NOT NULL,         -- exact|ast|judge  (the stage that decided)
  similarity REAL,
  frontier_lint TEXT,          -- JSON, always separate per side
  local_lint TEXT,
  frontier_tests TEXT,
  local_tests TEXT,
  judge_verdict TEXT,          -- JSON {equivalent,confidence,reason}; NEVER merged into tests
  tests_judge_conflict INTEGER NOT NULL DEFAULT 0,  -- tests win, conflict counted
  agree INTEGER,               -- 1/0, NULL while queued for judge
  decided_ts TEXT
);

CREATE TABLE judge_batches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  batch_id TEXT NOT NULL,      -- Anthropic batch id
  submitted_ts TEXT NOT NULL,
  completed_ts TEXT,
  input_tokens INTEGER,
  output_tokens INTEGER,
  status TEXT NOT NULL DEFAULT 'submitted'
);

CREATE TABLE judge_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  judge_batch_id INTEGER REFERENCES judge_batches(id),  -- NULL while queued
  verification_id INTEGER NOT NULL UNIQUE REFERENCES verifications(id),
  custom_id TEXT NOT NULL,
  verdict TEXT,
  status TEXT NOT NULL DEFAULT 'queued',   -- queued|submitted|done|errored
  created_ts TEXT NOT NULL
);

CREATE TABLE router_state (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  category TEXT NOT NULL,          -- "<turn_type>|<subsystem>"
  families TEXT NOT NULL,          -- "<frontierFamily>><localFamily>", UNIQUE(category, families)
  n INTEGER NOT NULL DEFAULT 0,
  agreed INTEGER NOT NULL DEFAULT 0,
  wilson_lb REAL,
  routable INTEGER NOT NULL DEFAULT 0,
  disabled_reason TEXT,            -- e.g. escalation auto-disable
  updated_ts TEXT
);

CREATE TABLE router_decisions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  session_id TEXT,
  call_id INTEGER,
  category TEXT,
  decision TEXT NOT NULL,          -- local|frontier|shadow|escalated|killswitch
  stats TEXT                       -- JSON snapshot of the numbers that justified it
);
```

## internal/anthropic

Minimal structs for the Messages API subset we touch: MessagesRequest (model, system
string-or-blocks, messages, tools, max_tokens, stream, metadata), content blocks (text,
tool_use, tool_result, thinking, image — unknown block types must round-trip via
json.RawMessage, never dropped), Usage, and the SSE event set:
`message_start, content_block_start, content_block_delta (text_delta, input_json_delta,
thinking_delta, signature_delta), content_block_stop, message_delta, message_stop, ping, error`.

`AssembleSSE(raw []byte) (messageJSON []byte, usage Usage, stopReason string, err error)`
reconstructs the complete message from a captured SSE byte stream: message_start carries
the message skeleton + usage.input_tokens; deltas accumulate per content-block index
(input_json_delta partial_json strings concatenate into tool_use input); message_delta
carries output_tokens and stop_reason. Must tolerate unknown event/delta types by
skipping them (log once), and a truncated stream by returning what it has plus err.

## internal/proxy (Phase 1)

ADAPTED, NOT WRITTEN FROM SCRATCH (Edward: "don't reinvent proxy from scratch, we can
modify something gpl and credit"). Base: seifghazi/claude-code-proxy (MIT, Go), pinned
commit 02c9c766679eee75c861bbde11c6d8b5249d44a7, pristine clone available at
/tmp/claude-1000/-home-edward-FreegleDockerWSL/e9e40137-99c4-4556-a1e1-fb551f885d59/scratchpad/ccp.
Reuse its forwarding and SSE streaming approach (handler/handlers.go
handleStreamingResponse and handleNonStreamingResponse, provider/anthropic.go) and
adapt storage to our store schema. Attribution requirements: a NOTICE file at the repo
root carrying the upstream MIT license text and copyright, plus a header comment in
each derived file: "Adapted from seifghazi/claude-code-proxy (MIT), commit 02c9c766."
Their web dashboard, conversation views and model router are NOT taken (no web UI in
the brief; our router is Phase 4). Requirements below still define the acceptance bar
where they go beyond the upstream code (fail-open, async logging, repo HEAD capture,
session id heuristic, overhead budget).

- `net/http` server on `listen`. Every path is forwarded verbatim to `upstream` —
  method, path, query, all headers except hop-by-hop (Connection, Keep-Alive,
  Transfer-Encoding, Upgrade, Proxy-*), body untouched. Auth passes through untouched.
- Timeouts: no read/write timeout on the client-facing server for streaming; upstream
  dial timeout 30s. http.Server with IdleTimeout only.
- Streaming: read upstream body in <=32KB chunks, write+Flush each chunk immediately.
  Tee every chunk into a capture buffer (cap 32MB; beyond that mark truncated and stop
  capturing but keep forwarding).
- After the response completes, hand a CaptureRecord to an async logger goroutine over
  a buffered channel (size 64). If the channel is full, drop the record and count it
  (fail-open; never block the request path). The logger assembles SSE when
  Content-Type is text/event-stream, compresses request/response, reads repo HEAD
  (read .git/HEAD + ref file directly; never shell out), and inserts the row.
  Any logger error: log to stderr, drop, continue. DB deleted mid-run must not error
  requests (test this).
- Session id best-effort, in order: request `metadata.user_id` (Claude Code sends one);
  extract `session[_-][0-9a-f-]{8,}` substring if present, else the whole user_id;
  else SHA256 of (User-Agent + first system block text)[:16]. Document as heuristic.
- Panic recovery wraps the handler: on internal panic, fall back to a plain
  single-shot forward (no capture). Forwarding itself failing returns 502 with the
  upstream error text.
- Only POST /v1/messages responses are captured to `calls`; other paths (count_tokens,
  models, batches) forward without logging.
- p50 added overhead < 5ms: overhead test with a local mock upstream asserts p50 < 5ms
  over 200 requests.

## internal/backend

One OpenAI-compatible chat-completions client covers ollama/together/gemini/openai
(base_url + optional bearer key + model). Plus translation:

- `ToOpenAI(req anthropic.MessagesRequest, model string) openai.ChatRequest`:
  system → first system message (concatenate text blocks);
  user/assistant messages → same roles; tool_result blocks → role:"tool" messages with
  tool_call_id; assistant tool_use → assistant message with tool_calls (arguments =
  JSON-encoded input); Anthropic tools → OpenAI function tools (name, description,
  input_schema → parameters); thinking blocks dropped; images dropped with a marker
  string "[image omitted]"; max_tokens passed through; temperature default 0 for
  reproducibility.
- `FromOpenAI(resp openai.ChatResponse) anthropic message JSON`: choices[0].message
  content → text block; tool_calls → tool_use blocks (arguments JSON-decoded, id
  preserved); usage mapped; finish_reason mapped (stop→end_turn, tool_calls→tool_use,
  length→max_tokens).
- Non-streaming requests only for replay. Phase 4 live routing also uses non-streaming
  upstream, then synthesizes an SSE stream for the client when the original request
  had stream:true (message_start, one content_block per block with a single delta,
  message_delta with usage, message_stop).

## internal/feature (Phase 2)

Batch command over calls missing a features row (and `--refresh` to redo all).
Idempotent via INSERT ... ON CONFLICT(call_id) DO UPDATE.

turn_type rules, first match wins, computed from decompressed request+response:
1. response has >=2 tool_use blocks among {Edit, Write, MultiEdit, NotebookEdit}
   targeting >=2 distinct file_path values → multi_file_edit
2. response has exactly 1 distinct file among those tools → single_file_edit
3. response contains tool_use named ExitPlanMode, or system prompt contains
   "plan mode is active" → plan
4. last user message contains tool_result blocks and response content is text-only
   → tool_result_summary
5. response is text-only and last user message is plain text → question_answer
6. else → other

files_touched: file_path/notebook_path inputs of the edit-family tool_use blocks,
made repo-relative when under repo_path (else kept absolute).
subsystem: first path segment of the first repo-relative file, '' if none.
context_tokens = input_tokens; output_tokens copied from calls.
had_error_followup: look at the NEXT call with the same session_id: true when its
request contains a tool_result with is_error:true, or a tool_result whose text starts
with "Error"/"error:" in the first 200 chars, or an edit-family tool_use targeting the
same file as this response edited. NULL when there is no next call yet (re-running
featurise later fills it in; idempotency requirement).

`splitter report spend`: token totals + estimated cost grouped by turn_type
(pricing table constant, editable; frontier assumed the logged model).

Fixture: testdata/labelled/*.json — real captured calls with a `label` field.
A test asserts >= 80% agreement of the featuriser against labels and FAILS the build
below that. Until the real 50-call labelled sample lands (needs live capture), the
fixture holds synthetic-but-realistic examples and the test also enforces a minimum
fixture size constant that we bump to 50 when the real sample is committed.

## internal/replay + internal/verify + internal/judge (Phase 3)

`splitter replay [-backend name] [-limit N] [-force]`:
- refuses to run when the newest call is younger than idle_minutes (unless -force).
- picks calls with features, without a replay for (backend, model), turn_type != other,
  oldest first, LIMIT batch_size.
- sends translated request to the backend, records replays row (error non-fatal per call).
- then runs the verification cascade for each new replay.

Cascade (internal/verify):
1. Normalise whitespace (collapse runs, trim) over concatenated text+tool_use content
   of both responses; if equal → stage=exact, similarity=1, agree=1, done.
2. Edit turns (single/multi_file_edit): create TWO ephemeral worktrees of repo_path at
   calls.repo_head under /tmp/splitter-verify-<pid>-<rand>/{frontier,local} using
   `git worktree add --detach`; apply each side's edits (Edit = literal old→new
   replacement, count must be >=1 else edit-application failure recorded; Write =
   whole file). Run linters by extension: .go → golangci-lint run --fast on the
   package dir if binary present else `gofmt -l` + `go vet ./pkg`; .php → php -l;
   .js/.ts/.vue → eslint --no-eslintrc if configured... keep it simple: run the tool
   only if the binary exists, record {tool, ok, output-truncated-2KB} JSON per side.
   Optional per-subsystem test command from [tests] config, run in each worktree,
   PORT-offset env vars injected (SPLITTER_PORT_BASE + i), 10 min timeout.
   Similarity: difft --display json between the two resulting files when `difft`
   binary present (map difftastic change count to [0,1] via changed-hunks /
   total-lines heuristic); fallback when absent: token-level normalized Levenshtein
   similarity over the edited file contents. Multi-file: mean over union of files;
   a file edited on one side only scores 0.
   Non-edit turns that failed stage 1: token-level similarity over normalised text.
3. sim >= high → agree=1 (stage=ast). sim <= low → agree=0 (stage=ast). Thresholds
   from config by (language of first edited file, turn_type), fallback defaults.
4. Middle band → verifications row with agree=NULL + judge_items row status=queued.
- Worktrees: always removed via defer AND a `splitter replay` startup sweep that
  deletes stale /tmp/splitter-verify-* older than 1h (SIGKILL survivor cleanup; test
  simulates by creating a stale dir and asserting the sweep removes it, plus a
  subprocess SIGKILL test). `git worktree prune` runs after removal.
  Max max_concurrent_worktrees cascades in flight (semaphore).

Judge (internal/judge):
- `splitter judge submit`: all queued judge_items → one Anthropic Message Batches API
  call. POST {upstream}/v1/messages/batches, headers x-api-key: $ANTHROPIC_API_KEY,
  anthropic-version: 2023-06-01. Each request: custom_id "ji-<judge_items.id>",
  params: model=judge.model, max_tokens=512, single user message:
  request context (truncated to max_context_chars) + both responses +
  'Answer ONLY JSON {"equivalent": bool, "confidence": 0-1, "reason": "one line"}'.
- `splitter judge poll`: GET batch; when processing_status=="ended", stream
  results_url JSONL; key by custom_id (never order); parse verdict JSON leniently
  (extract first {...} block). Update judge_items + verifications:
  judge stage: agree = (equivalent && confidence >= 0.5). If lint/tests results
  conflict with the judge (both sides linted clean but judge says not equivalent, or
  either side failed lint/tests but judge says equivalent): tests win →
  agree follows tests, tests_judge_conflict=1.
  Track batch usage tokens into judge_batches for the spend report.
- `splitter report agreement`: per turn_type × subsystem: agreement rate, n, top 3
  disagreement reasons (from judge_verdict.reason of disagreeing rows). Also prints
  judge share of edit turns and judge spend per 100 replays.

## Model families (cross-cutting)

Statistics must survive model version bumps. Requirement from Edward: when a new
version of the same model family appears, assume it has similar characteristics,
until we have learned otherwise, which we should (keep measuring per exact version).

- `internal/router.Family(model string) string` normalises an exact model id to a
  family key: strip date suffixes (-YYYYMMDD and @YYYYMMDD), strip generation
  numbers (digits and dots that denote a version), keep variant words and parameter
  size tags, lowercase. Examples that must hold in tests:
  claude-opus-5 and claude-opus-5-20260101 -> claude-opus;
  claude-sonnet-4-6 -> claude-sonnet; claude-haiku-4-5 -> claude-haiku;
  qwen2.5-coder:7b and qwen3-coder:7b -> qwen-coder:7b;
  Qwen/Qwen2.5-Coder-32B-Instruct -> qwen-coder-32b-instruct;
  gemini-2.5-flash -> gemini-flash; gpt-4o-mini -> gpt-4o-mini stays (letter-adjacent
  digits that are part of a product name with no dot/dash separation stay; the rule is
  best-effort, config [families] table overrides the function per exact model id).
- Aggregation: router_state rows are scoped by family pair. Add a `families` column
  (TEXT NOT NULL, "frontierFamily>localFamily"). Stats for a category use all
  verifications whose frontier family and local (backend) family match the currently
  configured pair, whatever the exact versions were. A new same-family version
  therefore inherits the learned stats immediately.
- Learning otherwise: `router update` also computes per-exact-version agreement
  within each family-scoped category. When a specific version has n >= 10 and its
  agreement rate sits more than 10 points below the family aggregate, that
  category+version is flagged in the update output and the weekly report, and the
  category's stats are recomputed from that version's rows only (which may drop it
  below the routable bar and disable routing for it). The flag and the recomputed
  basis are recorded in router_state.disabled_reason or stats JSON so decisions stay
  auditable.

## internal/router (Phase 4)

- `splitter router update`: recompute router_state from verifications joined to
  features: category = turn_type|subsystem scoped by family pair (see Model
  families), Wilson lower bound at z=1.96;
  routable = wilson_lb >= min_wilson_lb && n >= min_n && disabled_reason IS NULL.
- Proxy consults router when env SPLITTER_ROUTE=on (anything else = pure pass-through;
  'off' documented as the kill switch). Lookup is an in-memory snapshot refreshed
  every 60s from router_state.
- Routable request → serve from default backend translated to Anthropic format
  (+ synthesized SSE when stream requested); log router_decisions row with stats JSON;
  5% of routable turns (hash(call ordinal) % 20 == 0) dual-dispatch: serve FRONTIER
  answer, fire local shadow replay async for drift detection (decision=shadow).
- Escalation: featuriser error-followup heuristic applied live: if the next request in
  a session shows an error signal after a locally-served turn → mark category
  outcome failure (router_state.disabled_reason='escalation', routable=0) AND set a
  session-level circuit breaker (in-memory set of session_ids; those sessions go pure
  frontier until proxy restart). Log decision=escalated.
- `splitter report weekly`: frontier tokens avoided (sum of local-served output
  tokens), estimated cost saved (pricing table), quality incidents (escalations),
  drift check results (shadow disagreement rate).

## Eval library (internal/evals)

Requirement from Edward: build up a library of specific tasks that have tripped up
models in this codebase, then use it to evaluate new models against them. Each task is
pinned by git commit plus a brief. This is the concrete mechanism behind the model
families rule: a new version inherits family stats, and running the eval library
against it is how we learn otherwise quickly.

Schema (part of the store migration, pre-release so still schema v1):

```sql
CREATE TABLE eval_tasks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_ts TEXT NOT NULL,
  call_id INTEGER REFERENCES calls(id),   -- NULL for manually added tasks
  repo_head TEXT,                          -- git commit of the target repo
  brief TEXT NOT NULL,                     -- one line: what the task was
  turn_type TEXT,
  subsystem TEXT,
  frontier_model TEXT,                     -- exact model that set the reference
  request_zstd BLOB NOT NULL,              -- frozen copy, survives calls pruning
  reference_response_zstd BLOB,            -- the frontier answer
  origin TEXT NOT NULL,                    -- disagreement|escalation|error_followup|clean|history|manual
  language TEXT,                           -- go|php|js|ts|vue|sql|shell|markdown|yaml|mixed
  layer TEXT,                              -- backend-api|frontend-ui|database|infra|tests|docs|build
  nature TEXT,                             -- bugfix|feature|refactor|test-writing|config|docs
  difficulty TEXT,                         -- challenging|simple|NULL unknown
  characteristics TEXT,                    -- JSON evidence: size {files,diff_lines,context_bytes}, tags, difficulty signals
  active INTEGER NOT NULL DEFAULT 1,
  UNIQUE(call_id, origin)
);
CREATE TABLE eval_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  backend TEXT NOT NULL,
  model TEXT NOT NULL,
  tasks_total INTEGER,
  tasks_passed INTEGER
);
CREATE TABLE eval_results (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  eval_run_id INTEGER NOT NULL REFERENCES eval_runs(id),
  eval_task_id INTEGER NOT NULL REFERENCES eval_tasks(id),
  passed INTEGER,
  stage TEXT,
  similarity REAL,
  response_zstd BLOB,
  error TEXT,
  UNIQUE(eval_run_id, eval_task_id)
);
```

Task characteristics (Edward: find tasks that were challenging or simple for Claude;
then, refining, "difficulty is too simple a dimension... maybe some models are really
good at API code like the Go, and some models are really good at user facing front
end. So think about the different characteristics of a problem"): every task gets a
mechanically derived profile across independent dimensions, no human labelling:
- language: from touched-file extensions ('mixed' when more than one).
- layer: path-prefix mapping, configurable as [layers] in the TOML with defaults for
  this codebase family (iznik-nuxt3/, components/, pages/, *.vue, *.css ->
  frontend-ui; iznik-server-go/, *api*/, *.go handlers -> backend-api; migrations/,
  *.sql -> database; docker*, .circleci/, *.yml -> infra; *_test.*, tests/, spec/ ->
  tests; docs/, *.md -> docs; Makefile, scripts/ -> build).
- nature: commit-subject keywords plus diff shape (fix/bug/revert -> bugfix,
  add/feat/new -> feature, refactor/rename/move/extract -> refactor, only test files
  changed -> test-writing, docs -> docs, bump/config/setting -> config).
- difficulty: challenging or simple from evidence (below), NULL when unknown.
- size and tags live in the characteristics JSON with the evidence for every label.

Research-validated refinements (checked against the benchmark literature 2026-08-24,
citations in DECISIONS.md; these go in the characteristics JSON, no extra columns):
- framework: within a language the framework skews results materially (DesignBench:
  React consistently beats Vue on the same tasks, and this codebase is Vue/Nuxt).
  Derived from paths/extensions: .vue or nuxt paths -> vue-nuxt, *.blade.php ->
  laravel-blade, plain go stdlib vs gorm/fiber tags from imports when cheap.
- spec_clarity: SWE-bench Verified exists because underspecified issue text confounds
  results. Mechanical proxy: does the brief name a file, function or route; brief
  length bucket (terse <60 chars, normal, detailed >240). Recorded, and scorecards
  group by it.
- size is NON-MONOTONIC: SWE-bench analyses show 51-200 line patches resolve BETTER
  than 1-10 line precision edits, and multi-file patches drop sharply. So size
  (files, hunks, diff_lines) is recorded and bucketed in scorecards but NEVER used
  to derive the difficulty label; difficulty stays evidence-based only.
- task_date + contamination guard: LiveCodeBench shows stark pass-rate drops after a
  model's training cutoff and memorisation-driven inflation before it. This repo
  family is PUBLIC GitHub, so history-seeded tasks predating a model's cutoff may be
  answered from memory. Every task records task_date (commit author date for
  history, capture ts otherwise) in the characteristics JSON. Config gains a
  [model_cutoffs] table (family or exact model -> YYYY-MM). eval run splits every
  scorecard into post-cutoff (trusted) and pre-cutoff (memorisation-suspect) segments
  and marks models with unknown cutoffs. Live-harvested tasks are post-cutoff by
  construction and form the trustworthy core; seed-history prefers recent commits
  (-since defaults to 2 years back).
- localization: seeded tasks hand the model the touched files (localization removed
  by construction), live tasks let the agent find them. Recorded as
  localization: given|discovered so the two are never naively compared.

Scorecards (eval run, eval list) report pass rates grouped by EACH dimension
(the four columns plus framework, spec_clarity, size bucket and cutoff segment from
the JSON), giving a capability profile per model: simple tasks are the sanity floor,
and the language x layer x framework breakdown is what reveals "good at Go API, weak
at Vue frontend".
The router keeps (turn_type, subsystem, families) as its category key for now;
subsystem already approximates layer in this codebase. When eval profiles show a
model splitting along language or layer within one subsystem, promoting those
dimensions into the router category is the intended evolution (note in the report
when observed).

Subcommands (cmd_eval.go, `splitter eval <verb>`):
- `eval harvest [-include-clean N]`: creates eval_tasks from live capture, deduped by
  (call_id, origin): verifications with agree=0 (the local model tripped up,
  difficulty=challenging when the frontier also had an error followup, else NULL),
  router_decisions with decision='escalated' (challenging), features with
  had_error_followup=1 (the FRONTIER itself struggled: challenging, evidence records
  the followup). With -include-clean N it also samples up to N clean tasks
  (origin='clean', difficulty=simple): single_file_edit turns with
  had_error_followup=0 and, where a verification exists, agree=1. All harvested tasks
  get the full characteristics profile derived from the touched files and request.
  repo_head, turn_type, subsystem, frontier model and the frozen request/reference
  come from the source call.

Brief derivation (Edward: "you're also going to need to work out the brief for a
task... where you have [Claude] session history you can tell what I told you about a
task in like the initial brief. For historical commits you might need to reverse
engineer something"). The brief must be the ASK, never the answer:
- Session-sourced tasks (harvest, transcript import): walk calls with the same
  session_id back to the EARLIEST call and take its last plain-text user block, which
  is the human's initiating instruction; fall back to the first 120 chars of the
  task call's own last user text when no session chain exists. brief_source
  ('session' or 'call') is recorded in the characteristics JSON.
- History-sourced tasks: the commit subject describes the CHANGE post-hoc and often
  prescribes the fix, which would turn the eval into instruction-following. So
  seed-history stores the mechanical commit-subject brief (brief_source
  'commit_subject') and marks the task for reversal, and `eval reverse-briefs`
  (submit and -poll modes, reusing the exported batch client from internal/judge,
  judge model, cheap) rewrites each marked brief as the request a person would have
  made BEFORE the change existed: state the observed problem or desired behaviour in
  the requester's voice, never name the functions, variables or approach of the
  actual fix. The prompt passes the diff, touched-file context and commit message;
  the response replaces brief and sets brief_source 'reverse_engineered'. Tasks
  remain usable with the mechanical brief while reversal is pending; eval run
  reports how many tasks ran with each brief_source so guided-brief results are
  never silently mixed with reverse-engineered ones.
- `eval add -commit <sha> -brief "..." -request <file.json> [-reference <file.json>]`:
  manual entry.
Ladder evaluation (Edward: "conduct our evaluation by working upwards from easy
tasks towards harder tasks until we hit the limit of wasting tokens on tasks that
are way beyond the model"):
- Every task gets a rung, computed mechanically: rung 1 = difficulty simple,
  single_file_edit, context under 8KB; rung 2 = simple, larger or multi-file;
  rung 3 = difficulty unknown, single-file; rung 4 = unknown, multi-file or large;
  rung 5 = challenging, small; rung 6 = challenging, multi-file or large. Size feeds
  the rung only through these coarse buckets (size alone is non-monotonic, see
  above; the difficulty label stays evidence-based).
- eval run climbs rungs in order PER TRACK, where a track is the task's language
  ('mixed' is its own track), because capability differs by dimension: a model can
  be past its ceiling on the vue track while still climbing the go track. Config
  [evals]: ladder_track = "language" (or "layer" or "none" for one global ladder),
  stop_wilson_upper = 0.2, stop_min_n = 8, futility_consecutive_fails = 6.
- Stopping: within a track, after each task update the rung's pass count; abandon
  the rung and all higher rungs of that track when the Wilson UPPER bound of the
  rung's pass rate drops below stop_wilson_upper with n >= stop_min_n, or
  immediately after futility_consecutive_fails consecutive failures with zero
  passes. Skipped tasks are recorded in eval_results with error='ladder_skipped'
  (passed NULL) so reruns and other models still see them.
- Token accounting: usage from every backend response accumulates into eval_runs
  (tokens_in, tokens_out); the run summary prints tokens spent, tasks skipped by
  the ladder, and the estimated tokens saved (skipped count times the run's mean
  task cost). A -max-tokens flag hard-caps total spend for the run (stop
  everything, mark the rest ladder_skipped).
- eval_runs gains ladder TEXT (JSON: per-track stop rung and evidence) plus
  tokens_in/tokens_out INTEGER columns. The DB is pre-release, so extend the v1
  migration in place and keep the migrate tests green.

- `eval run -backend <name> [-model <override>]`: replays every active task against
  the named backend/model and scores each with the SAME verification cascade
  (worktrees at the task's repo_head, lint, similarity thresholds; judge stage is
  skipped, middle band counts as not passed, stage records 'band'). Writes eval_runs
  and eval_results, prints a scorecard per turn_type x subsystem plus a comparison
  against the most recent prior run of any other model (per-task regressions listed
  by id, commit and brief).
- `eval seed-history [-repo path] [-since date] [-max N] [-max-files 3]
  [-max-diff-lines 120] [-grep pattern]`: seeds the library from the target repo's
  git history (origin='history', deduped by commit sha stored in repo_head plus
  brief). Each selected commit becomes a task: repo_head = the commit's PARENT sha
  (the starting state), brief = the commit subject plus first body lines, request =
  a synthesized Anthropic Messages request (minimal coding-agent system prompt with
  Edit and Write tool definitions, user message = the brief plus the parent-state
  content of the touched files, total context capped at 20KB, commits exceeding the
  cap are skipped and counted), reference response = a synthesized assistant message
  whose Edit/Write tool_use blocks reconstruct the commit's actual diff. Selection:
  skips merges, binary and non-code files, diffs larger than the limits; turn_type
  and subsystem derived from the touched files. The commit sha is recorded so
  `eval list` shows "git commit number + brief" per Edward's framing. Historic
  commits are graded with the same cascade as everything else.
  Characteristics from git archaeology: language/layer/nature per the profile rules;
  difficulty=challenging when a later commit within 14 days touches the same files
  with a subject matching (?i)(fix|bug|revert|typo|oops|correct|broke), or when the
  commit itself is such a fix (it encodes a task a previous change got wrong);
  difficulty=simple when the commit was never revisited that way. The characteristics
  JSON records the follow-up shas or matched pattern as evidence.
Leakage containment (Edward, two rounds). Round one: a model "might be smart enough
to find the upstream repo and establish a fix". Round two, superseding the
sanitisation idea: "it's a bit doomed to try to sanitise the repo to keep the
details out, so scrap that. Instead, if possible detect if the model has cheated by
looking at the upstream or the main repo or later git commits." So: the sandbox
contains the AUTHENTIC repo (real names, real content, no scrubbing, no synthetic
identities), and validity comes from network denial plus CHEAT DETECTION:
- Never hand over the answer key: the fix commit sha and the reference response are
  stored in the DB only and never included in any payload or sandbox. (Not
  sanitisation, just not shipping the answer.)
- No provider-side retrieval: eval calls never set web search / browsing flags; our
  requests carry only our own tool definitions. Backends with always-on server-side
  retrieval cannot produce trusted scores; note per backend in BACKENDS.md.
- Network denial for anything that executes locally: model-triggered commands run
  under unshare -rn (verified working on this WSL2). -allow-network exists for
  debugging and marks every result untrusted.
- Cheat detection (agentic mode; detectors, not verdicts, all recorded as
  cheat_flags JSON on eval_results and any flag demotes the result to the untrusted
  segment alongside pre-cutoff):
  1. The full tool transcript is stored (transcript_zstd) so every flag is
     auditable by a human.
  2. escape: any tool path argument that resolves outside the sandbox root (after
     symlink resolution), e.g. reaching for the main checkout or another worktree.
  3. git_poke: any read_file/grep/list_dir touching .git paths.
  4. tool_smuggling: model-written file content that introduces subprocess/network
     reach into the test run: added code matching git invocation, curl/wget/http
     client calls, or DNS lookups in files the task did not previously have them in
     (diff-based, per written file).
  5. suspect_copy: for history tasks, the final model patch is compared to the
     withheld upstream fix with the normal similarity machinery; near-verbatim
     match (similarity above 0.98 INCLUDING comments and identifier choices) on a
     post-cutoff task raises the flag, weighted by patch size (trivial patches
     converge legitimately; the flag threshold rises as patches shrink).
  6. During run_tests the harness parks .git (renames it away for the duration), so
     test-spawned git commands fail closed; "not a git repository" in test output
     raises attempted_git.
- The [model_cutoffs] contamination guard covers memorised fixes; the detectors
  cover active lookup. Both demote to the untrusted segment, never delete.

## Agentic eval mode (internal/agentic)

Edward: "Part of what we want to evaluate is whether the models successfully run and
fix the tests." Single-turn diff similarity is the weak signal; the strong signal is
the loop: run the tests, read the failure, iterate to green. Sandboxing means
contained execution, not no execution.

Grading is fail-to-pass, SWE-bench style, using the repo's own tests:
- seed-history splits each commit's diff into TEST files and NON-test files (the
  existing test-file classification). When a commit includes test changes, the task
  stores them separately (holdout_tests_zstd) and is agentic-gradable: the sandbox
  gets the PARENT tree plus the commit's TESTS applied, the model gets the brief and
  must make those tests pass without ever seeing the fix. Grade = fail-to-pass on
  the held-out tests AND no new failures in the task package's pre-existing tests
  (both recorded separately). Commits with no test changes stay single-turn-only.
- Harvested live tasks are agentic-gradable when their subsystem has a configured
  test command; grade = that command in the sandbox.

Sandbox lifecycle per task (authentic repo, detection over sanitisation, see
Leakage containment):
1. Worktree: git worktree add --detach of repo_path at repo_head under
   /tmp/splitter-agentic-<pid>-<rand>/. Real content, real names, real history in
   the shared object store; the answer key (fix sha, reference response) is simply
   never shipped, and detectors watch for the model going looking (.git pokes,
   escapes, smuggled git/network calls in written files).
2. Prep online: install/warm dependencies from lockfiles before the model is
   involved (go mod download with GOMODCACHE bind, npm ci if package-lock exists,
   composer install if configured). Prep failures mark the task agentic_ready=0.
3. Loop network-denied: every model-triggered command runs under unshare -rn.
   Refuse -mode agentic when unshare is unavailable unless -allow-network is given,
   which marks every result untrusted. During each run_tests the harness parks
   .git (rename away, restore after) so test code cannot read history; tool calls
   never resolve outside the sandbox root (escape attempts are flagged, not
   followed).
4. Teardown always (defer + the same startup sweep pattern as verify worktrees,
   plus git worktree prune on the source repo).

The loop (eval run -mode agentic): our harness drives the OpenAI-compatible (or
anthropic-kind) backend in a tool loop: tools = read_file, list_dir, edit (literal
old->new), write, run_tests (the task's test command), grep. NO general bash and no
network tools: the tool surface is the sandbox boundary for what the model can do,
unshare is the boundary for what its commands can reach. Tool results truncated to
8KB each. Bounds per task from [evals]: max_turns (default 20), max task tokens
(default 200k), wall clock (default 10 min); exceeding any = fail with reason.
eval_results gains mode TEXT ('single'|'agentic'), turns INTEGER,
tests_ran INTEGER, tests_passed INTEGER (held-out), regressions INTEGER
(pre-existing test failures introduced), transcript_zstd BLOB (full tool
transcript) and cheat_flags TEXT (JSON array, see Leakage containment; any flag
demotes the result to the untrusted segment). The ladder, token accounting, cutoff and
leaky segmentation all apply unchanged. Scorecards report the two modes separately;
"ran the tests at all" (tests_ran) is itself a reported capability, per Edward.

- `eval list`: id, origin, repo_head short sha, brief, pass rate per model so far.
- Evaluating a new ANTHROPIC model works through a minimal native client (raw HTTP
  POST /v1/messages, non-streaming, x-api-key from judge.api_key_env) selected with
  `-backend anthropic -model <id>`; this client is for eval runs only, live routing
  never uses it.

## cmd import-history

One-off bootstrap, clearly marked BEST-EFFORT in --help and output: reads
~/.claude/projects/*/*.jsonl transcript files (internal format, may change between
releases — this is why it must never be part of the live pipeline), reconstructs
approximate request/response pairs into calls rows with source='import' in the error
column... no: add a dedicated `source TEXT NOT NULL DEFAULT 'proxy'` column on calls
('proxy'|'import'). Imported rows are usable for featurisation and replay but excluded
from proxy overhead/latency stats.

## Working rules for every implementing agent

- Work ONLY under /home/edward/splitter (standalone repo, branch
  feature/initial-implementation). Never write to /home/edward/FreegleDockerWSL or
  any FreegleDocker worktree.
- cd /home/edward/splitter for all Go work.
- go build ./... and go test ./... must pass before you finish. gofmt -l must be empty.
- Table-driven tests, httptest servers for HTTP, real SQLite in t.TempDir().
  No mocking libraries; hand-rolled fakes only.
- Comments state current behaviour, never history or "added/changed" notes.
- No em-dashes anywhere (code, comments, docs). Use commas or parentheses.
- Errors: wrap with %w and context; never panic in library code.
- Keep functions small; no globals except the cmd registry.
- If you must deviate from this spec, append a dated bullet to DECISIONS.md.
