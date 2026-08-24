# splitter

Claude Code sends everything to Anthropic's frontier models. Some requests need
that; plenty don't. Splitter finds out, with evidence from your own codebase,
which requests a **cheaper model** could handle just as well, and can then
route that slice of traffic to the cheaper model with an instant fallback to
the frontier when anything looks off.

"Cheaper" means both kinds: **local models** via Ollama (costs electricity),
and **cheaper hosted models** like DeepSeek, GLM or Kimi (10x to 100x below
frontier rates; see [BACKENDS.md](BACKENDS.md)). The split is frontier vs
cheaper, not cloud vs local.

## Try it in an afternoon

You don't wait weeks of passive watching to learn something. The evaluation
builder turns your repo's own git history into a test set and tells you the
same day which models are worth considering:

```
./scripts/install.sh                         # build, config, systemd units, proxy

splitter bootstrap -backends ollama,deepseek # the whole evals-first pipeline
```

`bootstrap` does four things and narrates as it goes:

1. **Builds an evaluation set** from your repo's history: small real commits
   become tasks; the model gets the code as it was *before* each fix, plus a
   brief.
2. **Rewrites the briefs** (cheap batched Haiku call) into what you'd have
   asked *before* the fix existed, so commit messages don't give the game
   away.
3. **Evaluates each named backend** over the set, easy tasks first per
   language, stopping early on any track where the model is clearly out of
   its depth so you never burn tokens on hopeless models. A typical run
   costs about a cent on DeepSeek and nothing on Ollama.
4. **Computes routing candidates**: which categories of work (per turn type,
   subsystem and model family) already clear the statistical bar.

Read the scorecard, and you have candidates: "this model handles our Go
backend fixes, keep it away from the Vue frontend" is a typical day-one
result.

For the deeper verdict there's an agentic mode, where the candidate model gets
a sandboxed copy of the repo and must actually run and fix the tests
(`splitter eval-agentic -backend deepseek`), with cheat detection since your
repo is public: see [COMMANDS.md](COMMANDS.md).

## Then let the passive side confirm it

Point Claude Code at the proxy and work normally:

```
export ANTHROPIC_BASE_URL=http://127.0.0.1:9925
```

Nothing changes: same models, same streaming, your subscription auth passes
through untouched, under 5ms added, and it fails open so a splitter bug can
never break a coding session. Every call is captured, and while your machine
is idle splitter replays your *real* traffic against the cheaper model and
scores agreement mechanically (text match, then lint plus structural diff of
the edits in throwaway worktrees, then a batched Haiku judge for the ambiguous
middle only). That evidence flows into the same router statistics as the
evals, so day-one candidates either firm up or get quietly disproved.

Live routing stays **off** until you set `SPLITTER_ROUTE=on`. When on:
routable categories go to the cheaper model, everything else to the frontier,
any error signal disables the category and sends the whole session back to the
frontier, and 5% of routable turns are permanently double-checked for drift.

## The commands you'll actually use

| Command | What it does |
|---|---|
| `splitter bootstrap -backends a,b` | The evals-first pipeline above, end to end. |
| `splitter proxy` | The pass-through capture proxy (systemd runs this for you). |
| `splitter eval run -backend <name>` | Re-evaluate one model over the task library. |
| `splitter eval-agentic -backend <name>` | The run-and-fix-the-tests evaluation. |
| `splitter report spend` | Where your frontier tokens actually go, by turn type. |
| `splitter report agreement` | How often the cheap model matched, by category. |
| `splitter router update` | Recompute routing candidates from all evidence. |
| `splitter report weekly` | Once routing: tokens avoided, escalations, drift. |

Everything else (harvesting eval tasks from live capture, manual task entry,
transcript bootstrap, judge plumbing) is in [COMMANDS.md](COMMANDS.md).
All settings are in [CONFIG.md](CONFIG.md).

## A real first run

From this repo's own first day, evaluating DeepSeek V4 Flash:

```
$ splitter eval run -backend deepseek
  tasks total:   6
  tasks passed:  3
  tokens in/out: 35633 / 43738

  ladder:
    go:    rung 6: 1/1 passed
    php:   rung 6: 1/2 passed
    mixed: rung 6: 0/1 passed
```

Cost: about a cent. Reading: it solved a genuinely hard Go backend fix and a
PHP batch fix from real history, and failed the Vue frontend task (one that
once took a frontier model two attempts too). Models are lopsided; the
per-language ladder is what catches that, and the scorecard also splits out
results on tasks older than the model's training cutoff, so a model can't look
clever by having memorised your public repo.

## Using your Claude subscription for the judge

The brief-rewriting and judging steps need Anthropic auth. Either a metered
API key, or your subscription via the usual token approach:

```
claude setup-token     # prints a token starting sk-ant-oat...
```

Put the result in `~/.config/splitter/env` as `ANTHROPIC_API_KEY`. Splitter
recognises the `sk-ant-oat` prefix and authenticates it as a subscription
bearer token automatically. The proxy itself needs no key of its own, ever.

## Version bumps don't reset learning

Statistics are keyed by model *family* (`claude-opus-5` and a dated snapshot
of it are one family; so are `qwen2.5-coder:7b` and `qwen3-coder:7b`), so a
new version inherits its family's learned routability immediately, assumed
similar **until proven otherwise**, and that's enforced: any version whose
live agreement falls well below its family gets flagged and its categories
recomputed from its own rows, which can un-route it. The fast check is simply
re-running the eval ladder against the new version the day it appears.

## Notes

- **Secrets**: only ever in `~/.config/splitter/env` (chmod 600). The repo's
  pre-commit hook refuses anything key-shaped; every key wired on this
  machine is Freegle-scoped (see DECISIONS.md).
- **Coexists with `large-output-guard` hooks**: hooks wrap tool calls inside
  Claude Code; the proxy wraps the API transport outside it. Both together is
  the normal case.
- **Docs**: [BRIEF.md](BRIEF.md) the original intent; [DESIGN.md](DESIGN.md)
  the concrete design; [DECISIONS.md](DECISIONS.md) every judgement call;
  [BACKENDS.md](BACKENDS.md) the model/pricing landscape;
  [SEED-CANDIDATES.md](SEED-CANDIDATES.md) hand-picked first eval tasks.
- **Simplicity beats generality**: SQLite not Postgres, systemd timers not
  queues, raw HTTP not SDKs. Single developer, single machine, WSL2.

## Attribution

`internal/proxy` is adapted from
[seifghazi/claude-code-proxy](https://github.com/seifghazi/claude-code-proxy)
(MIT), pinned at commit
[`02c9c766`](https://github.com/seifghazi/claude-code-proxy/commit/02c9c766679eee75c861bbde11c6d8b5249d44a7):
its pass-through forwarding and SSE streaming approach, restructured around
splitter's own storage, fail-open logging and session tracking; its web
dashboard and router were not taken. Full license text in [NOTICE](NOTICE).
This project is GPL-2.0 ([LICENSE](LICENSE)), matching Freegle's `iznik`
convention.
