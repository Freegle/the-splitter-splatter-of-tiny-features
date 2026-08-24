#!/bin/bash
# Overnight looped evaluation: DeepSeek V4 Flash in ARENA mode, where the
# model runs the project's real tests in the eval-arena worktree's Docker
# stack, exactly like a real session. Prepared 2026-08-24; run after reboot.
set -euo pipefail

ARENA=/home/edward/FreegleDocker-eval-arena
REPO=/home/edward/the-splitter-splatter-of-tiny-features
LOG=$HOME/splitter-overnight-$(date +%Y%m%d-%H%M).log
exec > >(tee -a "$LOG") 2>&1

echo "== overnight deepseek arena run, $(date)"

[ -d "$ARENA" ] || { echo "FATAL: arena worktree missing at $ARENA (./freegle worktree create eval-arena)"; exit 1; }

echo "== starting arena containers"
(cd "$ARENA" && docker-compose up -d)

PORT=$(grep -E '^PORT_STATUS=' "$ARENA/.env" | cut -d= -f2)
[ -n "$PORT" ] || { echo "FATAL: no PORT_STATUS in arena .env"; exit 1; }
echo "== waiting for arena status API on :$PORT (up to 20 min)"
for i in $(seq 1 120); do
  curl -sf --max-time 5 "http://localhost:$PORT/" > /dev/null && break
  sleep 10
done
curl -sf --max-time 5 "http://localhost:$PORT/" > /dev/null || { echo "FATAL: arena status API never came up"; exit 1; }

echo "== building splitter"
cd "$REPO" && go build -o "$HOME/.local/bin/splitter" ./cmd/splitter

echo "== ensuring arena config keys"
python3 - << 'PY'
import os
p = os.path.expanduser('~/.config/splitter/config.toml')
s = open(p).read()
port = None
for line in open('/home/edward/FreegleDocker-eval-arena/.env'):
    if line.startswith('PORT_STATUS='):
        port = line.strip().split('=')[1]
if 'arena_path' not in s:
    s = s.replace('[evals]', '[evals]\narena_path = "/home/edward/FreegleDocker-eval-arena"\narena_status_port = %s' % port, 1)
    open(p, 'w').write(s)
    print('arena config written, status port', port)
else:
    print('arena config already present')
PY

set -a; source "$HOME/.config/splitter/env"; set +a

echo "== leg 1: deepseek, arena mode, all active tasks"
"$HOME/.local/bin/splitter" eval-agentic -arena -backend deepseek -max-tokens 2000000

echo "== leg 2: opus, arena mode (subscription token from the live login)"
OAT=$(python3 - << 'PY'
import json, time, os
d = json.load(open(os.path.expanduser('~/.claude/.credentials.json')))
o = d.get('claudeAiOauth', {})
if o.get('expiresAt', 0) / 1000 > time.time() + 600:
    print(o.get('accessToken', ''))
PY
)
if [ -n "$OAT" ]; then
  ANTHROPIC_API_KEY="$OAT" "$HOME/.local/bin/splitter" eval-agentic -arena -backend anthropic -model claude-opus-5 -max-tokens 2000000
else
  echo "WARN: no valid subscription token in ~/.claude/.credentials.json (expired or missing);"
  echo "      opus leg not run. Start any Claude Code session to refresh the token, then:"
  echo "      ANTHROPIC_API_KEY=<token> splitter eval-agentic -arena -backend anthropic -model claude-opus-5"
fi

echo "== done, $(date). Results:"
sqlite3 "$HOME/.local/share/splitter/splitter.db" \
  "select r.id, r.model, count(er.id), sum(er.passed), sum(er.tests_ran), sum(er.regressions) from eval_runs r join eval_results er on er.eval_run_id=r.id where er.mode='agentic' group by r.id"
