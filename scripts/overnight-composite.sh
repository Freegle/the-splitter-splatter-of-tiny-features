#!/bin/bash
# Overnight composite arena sittings over ALL active tasks: tests grade
# where held-out tests exist, the judge grades the real diff otherwise.
# DeepSeek first; Opus follows only if DeepSeek's sitting completes sanely.
set -uo pipefail
LOG=$HOME/splitter-composite-$(date +%Y%m%d-%H%M).log
exec > >(tee -a "$LOG") 2>&1

# Wait for any in-flight splitter run to finish (single-writer policy).
while pgrep -x splitter > /dev/null; do sleep 30; done

# The idle-stack sweeper stops arena containers after an hour idle; bring
# them back and wait for the status API before any leg starts.
ARENA=/home/edward/FreegleDocker-eval-arena
(cd "$ARENA" && docker-compose start) || (cd "$ARENA" && docker-compose up -d)
PORT=$(grep -E '^PORT_STATUS=' "$ARENA/.env" | cut -d= -f2)
echo "== waiting for arena status API on :$PORT"
for i in $(seq 1 120); do
  curl -sf --max-time 5 "http://localhost:$PORT/" > /dev/null && break
  sleep 10
done
curl -sf --max-time 5 "http://localhost:$PORT/" > /dev/null || { echo "FATAL: arena API never came back"; exit 1; }

set -a; source "$HOME/.config/splitter/env"; set +a
OAT=$(python3 - << 'PY'
import json, time, os
d = json.load(open(os.path.expanduser('~/.claude/.credentials.json')))
o = d.get('claudeAiOauth', {})
if o.get('expiresAt', 0) / 1000 > time.time() + 3600:
    print(o.get('accessToken', ''))
PY
)

echo "== leg 1: deepseek, composite arena, all active tasks, $(date)"
export ANTHROPIC_API_KEY="${OAT:-$ANTHROPIC_API_KEY}"   # judge calls for no-test tasks
"$HOME/.local/bin/splitter" eval-agentic -arena -backend deepseek -max-tokens 40000000
DS=$?
echo "deepseek leg exit: $DS, $(date)"

if [ "$DS" -eq 0 ] && [ -n "$OAT" ]; then
  echo "== leg 2: opus, composite arena, $(date)"
  ANTHROPIC_API_KEY="$OAT" "$HOME/.local/bin/splitter" eval-agentic -arena -backend anthropic -model claude-opus-5 -max-tokens 40000000
  echo "opus leg exit: $?, $(date)"
else
  echo "opus leg skipped: deepseek exit $DS, token present: $([ -n "$OAT" ] && echo yes || echo no)"
fi

echo "== results, $(date)"
sqlite3 "$HOME/.local/share/splitter/splitter.db" \
  "select r.id, r.model, count(er.id), sum(er.passed), sum(er.tests_passed), sum(er.regressions) from eval_runs r join eval_results er on er.eval_run_id=r.id where er.mode='agentic' and r.id > 14 group by r.id"
