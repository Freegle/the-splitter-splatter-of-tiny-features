#!/usr/bin/env bash
# Builds splitter, installs its config and user-level systemd units, and
# starts the proxy and the hourly replay timer. Safe to re-run: an already
# present config.toml is left untouched, and systemctl enable/start are
# idempotent by themselves.
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
bin_dir="${HOME}/.local/bin"
config_dir="${HOME}/.config/splitter"
unit_dir="${HOME}/.config/systemd/user"

echo "splitter install: building ${bin_dir}/splitter"
mkdir -p "${bin_dir}"
(cd -- "${repo_dir}" && go build -o "${bin_dir}/splitter" ./cmd/splitter)

echo "splitter install: config"
mkdir -p "${config_dir}"
if [ -f "${config_dir}/config.toml" ]; then
  echo "  ${config_dir}/config.toml already exists, leaving it untouched"
else
  cp -- "${repo_dir}/config.example.toml" "${config_dir}/config.toml"
  echo "  wrote ${config_dir}/config.toml (copied from config.example.toml)"
fi
if [ ! -f "${config_dir}/env" ]; then
  echo "  NOTE: ${config_dir}/env does not exist yet. Backend and judge API keys"
  echo "        (see config.example.toml's api_key_env names) are read from"
  echo "        there as KEY=VALUE lines. Ollama needs none; the proxy passes"
  echo "        Claude Code's own Anthropic auth through untouched."
fi

echo "splitter install: user systemd units"
mkdir -p "${unit_dir}"
cp -- "${repo_dir}/systemd/splitter-proxy.service" "${unit_dir}/"
cp -- "${repo_dir}/systemd/splitter-replay.service" "${unit_dir}/"
cp -- "${repo_dir}/systemd/splitter-replay.timer" "${unit_dir}/"

systemctl --user daemon-reload
systemctl --user enable --now splitter-proxy.service
systemctl --user enable --now splitter-replay.timer

cat <<EOF

splitter install: done.

Point Claude Code at the proxy by setting, in the shell (or profile) Claude
Code runs from:

  export ANTHROPIC_BASE_URL=http://127.0.0.1:9925

Watch the capture log grow with:

  sqlite3 "\$(grep -m1 '^db_path' ${config_dir}/config.toml | cut -d'"' -f2 | sed "s|^~|\$HOME|")" 'select count(*) from calls'

Kill switch: unset ANTHROPIC_BASE_URL (or point it back at
https://api.anthropic.com) to bypass the proxy entirely at any time; the
proxy itself also fails open on any internal error, forwarding the request
untouched rather than breaking a coding session. Phase 4 live routing is
separately gated off by default (SPLITTER_ROUTE=on is required to enable
it; anything else, including unset, is pure pass-through).

Service status: systemctl --user status splitter-proxy.service splitter-replay.timer
EOF
