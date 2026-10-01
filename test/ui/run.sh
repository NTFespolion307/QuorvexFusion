#!/usr/bin/env bash
# Browser test of the web UI: starts a dev cluster (./dev.sh), creates some
# jobs, then drives every page in headless Chromium, in both themes.
# Screenshots land in test/ui/shots/.
#
# One-time setup (Linux/WSL): Node.js 18+, then in test/ui:
#   npm install playwright@1 && npx playwright install --with-deps chromium
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
UI=$PWD
cd ../..
./dev.sh clean >/dev/null
./dev.sh up 2 >/dev/null
trap './dev.sh clean >/dev/null' EXIT
C() { ./dev.sh cli "$@"; }
C submit --wait --array 1-12 --cpus 0.5 -- 'sleep 1; echo hello from task {i} on $(hostname)' >/dev/null 2>&1
C submit -- 'echo something went wrong >&2; exit 3' >/dev/null 2>&1
C submit --name "long running" -- 'for i in $(seq 1 60); do echo line $i; sleep 1; done' >/dev/null 2>&1
sleep 3
NODE_ID=$(C nodes --json | grep -oE '"id": "n[0-9a-f]+' | head -1 | cut -d'"' -f4)
JOBS=$(C jobs --json | grep -oE '"id": "j[0-9a-f]+' | cut -d'"' -f4)
export NODE_ID
export LOG_TASK_ID="$(echo "$JOBS" | sed -n 1p).0"
export JOB_ID="$(echo "$JOBS" | sed -n 3p)"
export TASK_ID="$JOB_ID.5"
mkdir -p "$UI/shots"
cd "$UI" && node uitest.js "$UI/shots"
