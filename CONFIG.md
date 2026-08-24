# Configuration reference

Resolution order: `-config` flag, then `$SPLITTER_CONFIG`, then
`~/.config/splitter/config.toml`, then built-in defaults.
`config.example.toml` documents every field inline.

| Field | Meaning |
|---|---|
| `listen` | Proxy listen address, default `127.0.0.1:9925`. |
| `upstream` | Frontier API base URL the proxy forwards to. |
| `db_path` | SQLite file. Created `0600`, parent directories `0700`. |
| `repo_path` | The target codebase: where git HEAD is read and worktrees are made. |
| `env_file` | KEY=VALUE secrets file, default `~/.config/splitter/env`. Never committed; the TOML only ever names env *variables*, never holds keys. |
| `[replay]` | `backend`, `idle_minutes`, `max_concurrent_worktrees`, `batch_size`. |
| `[backends.<name>]` | One per cheaper backend: `base_url`, `api_key_env`, `model`. Ships with `ollama`, `together`, `gemini`, `openai`, `deepseek`. |
| `[judge]` | Haiku judge: `model`, `api_key_env`, `max_context_chars`. |
| `[thresholds]` | Cascade similarity thresholds plus per language/turn-type overrides. |
| `[tests]` | Optional per-subsystem test command for the verify cascade. |
| `[router]` | `min_n`, `min_wilson_lb`, `dual_dispatch_pct`. |
| `[families]` | Per-model-id override for family normalisation. |
| `[model_cutoffs]` | Model/family to training-cutoff month, for the trusted/untrusted eval split. |
| `[layers]` | Path prefix to layer name (frontend-ui, backend-api, ...) for eval characteristics. |
| `[evals]` | Ladder and eval knobs: `max_answer_tokens` (floor for candidate answers, default 16384: reasoning models spend output tokens thinking and return nothing under small budgets), `seed_context_bytes`, ladder stop parameters, agentic bounds. |

