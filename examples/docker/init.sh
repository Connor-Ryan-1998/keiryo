#!/bin/sh
# One-time setup for compose.yml: shared agent token and data directories.
# Uses sudo because the containers run as UID 65532 (hub) and 0 (agent), and
# token files must be 0600.
set -eu
cd "$(dirname "$0")"

if [ -e hub/local.token ] || [ -e agent/local.token ]; then
  echo "Tokens already exist; leaving them unchanged." >&2
  exit 0
fi

token="$(openssl rand -hex 32)"
mkdir -p data/hub data/agent
printf '%s\n' "$token" | sudo install -m 0600 -o 65532 -g 65532 /dev/stdin hub/local.token
printf '%s\n' "$token" | sudo install -m 0600 -o 0 -g 0 /dev/stdin agent/local.token
sudo chown 65532:65532 data/hub
# The agent runs as root without DAC_OVERRIDE, so it must own its queue directory.
sudo chown 0:0 data/agent
echo "Ready. Run: docker compose up -d   then open http://127.0.0.1:9080 (admin / admin)."
