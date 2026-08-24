# Backend and pricing landscape (researched 2026-08-24)

Market research for Edward's question: which models are likely to have a significant
price advantage over the Claude subscription, with subscription models acceptable
because they bound the cost. This page goes stale; re-run the research before acting
on it later. The eval library (`splitter eval run`) is the tool for testing any of
these claims on OUR codebase (Vue/Nuxt frontend, Go/PHP backend) rather than trusting
public benchmarks.

Baseline: Claude subscription bounds cost at $20/mo (Pro) to $100-200/mo (Max tiers).
The splitter's frontier traffic rides that subscription via Claude Code's own auth,
so "savings" here means replacing marginal Claude usage (quota headroom) or replacing
a Max tier with something cheaper.

## Bounded-cost options (subscriptions), significant advantage first

| Option | Price | Bound | Anthropic-compatible | Notes |
|---|---|---|---|---|
| Ollama local (qwen3-coder:30b, glm-4.7-flash) | electricity | hardware | via splitter translation | Free, private, idle-compute replay corpus. qwen3-coder:30b is the consensus best quality/GB for a 24-32GB tier in mid-2026 |
| Gemini CLI free tier | $0 | 1000 req/day, 60/min | no (OAuth CLI, not API key) | Gemini 3 Pro class, 1M context, no card. Our GEMINI_API_KEY free tier is the lesser cousin: ~250 req/day Flash-only, still useful for replay |
| OpenCode Go | $5-10/mo | plan quota | partially (varies) | Aggregator of GLM/Kimi/Qwen/DeepSeek behind one plan; cheapest bounded multi-model |
| GLM Coding Plan (Z.ai) | Lite ~$10-18/mo, Pro ~$30-72, Max ~$80-160 (sources disagree, post-Feb-2026 reset reportedly Lite $10) | plan quota (~10x Claude prompt volume claimed at Lite) | YES, drop-in | GLM-5.1 scored 94.6% of Claude Opus 4.6 on the Claude Code internal eval per press; the strongest bounded-cost claim on the market. MUST be validated with eval run before trust |
| Kimi (Moonshot) Allegro | $99/mo | plan quota (~matches Anthropic $200 plan per press) | YES: https://api.kimi.com/coding/v1 | K3 (2.8T, 1M ctx) flagship; K2.7-code for volume |
| ChatGPT Plus + Codex | $20/mo | plan quota | no | Comparable to Claude Pro, only an advantage vs Max tiers |

## Per-token options (unbounded, but tiny numbers)

Claude for scale: Opus $5/$25, Sonnet $3/$15, Haiku $1/$5 per MTok.

| Model | $/MTok in/out | Anthropic-compatible | Key held? |
|---|---|---|---|
| DeepSeek V4 Flash | $0.14/$0.28 | YES: api.deepseek.com/anthropic (documented Claude Code integration) | no |
| DeepSeek V4 Pro | ~$0.87 out | YES | no |
| Kimi K2.6 / K2.7-code | $0.95/$4.00 | YES | no |
| Kimi K3 | $3/$15 ($0.30 cache hits) | YES | no |
| Gemini 3.1 Pro | ~$12 out | no (OpenAI-compat shim) | GEMINI_API_KEY (Freegle) |
| Together (Qwen coder family) | provider-dependent, low $ | no (OpenAI-compat) | TOGETHER_API_KEY (Freegle) |

## Integration implications for the splitter

- GLM, Kimi and DeepSeek expose ANTHROPIC-COMPATIBLE endpoints: routing to them
  needs no request translation at all, only an upstream URL and auth swap in the
  proxy. The planned follow-up is a `kind = "anthropic"` backend type (the
  anthropic-native client already exists for eval runs); until then the
  OpenAI-compatible surfaces work through the existing translation layer.
- No keys are held for GLM/Kimi/DeepSeek yet. Wiring them is a signup decision for
  Edward; env names reserved: GLM_API_KEY, KIMI_API_KEY, DEEPSEEK_API_KEY.
- The contamination guard applies to validation: public-benchmark claims (94.6% of
  Opus etc.) say nothing about Vue/Nuxt + Go on this codebase; post-cutoff
  eval-library segments are the trustworthy comparison.

## Recommendation (2026-08-24)

1. Keep replay on local qwen3-coder:30b (free) alongside qwen2.5-coder:7b; both are
   pulled. Two local tiers give a free capability ladder in the agreement data.
2. If replay shows the local ceiling is too low for routable categories, the GLM
   Coding Plan Lite is the standout bounded-cost candidate to trial next ($10-18/mo,
   drop-in Anthropic API); validate with `splitter eval run` on post-cutoff tasks
   before routing anything live to it.
3. DeepSeek V4 Flash is the volume play if per-token is acceptable; at $0.14/$0.28
   even heavy replay/routing costs pennies per day, though it is unbounded by design.
