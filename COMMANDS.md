# Command reference

Every subcommand of `splitter`, including the plumbing you rarely need.
The short list of commands you actually start with is in [README.md](README.md).

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

## Agentic evaluation, sandboxing and cheating

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

## `import-history` (best-effort bootstrap only)

Reads Claude Code's session transcripts (`~/.claude/projects/*/*.jsonl`) and
reconstructs approximate `calls` rows tagged `source='import'`, so a fresh
install has something to featurise and replay before live capture accumulates.
Claude Code's transcript format is internal and undocumented; this command is
deliberately the only thing that parses it, it is never part of the live
pipeline, and anything unparseable is skipped and counted rather than fatal.

