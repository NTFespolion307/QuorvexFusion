#!/usr/bin/env bash
# dev.sh: a throwaway local cluster built from source, for development.
# Nothing is installed; everything lives in ./dev-data. Linux only (WSL works).
#
#   ./dev.sh up [N]     build, then start a controller and N workers (default 1)
#   ./dev.sh down       stop them
#   ./dev.sh clean      stop them and delete ./dev-data
#   ./dev.sh cli ARGS   run a CLI command against the dev cluster, e.g.
#                       ./dev.sh cli submit -f -- echo hello
#
# Workers run as your user, so CPU/memory limits are not enforced (that
# needs root + systemd; use install.sh for the real thing).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

DIR=$PWD/dev-data
BIN=$PWD/bin/cluster
NODE_PORT=17443
HTTP_PORT=18443
CLI=("$BIN" --config "$DIR/cli.json")

start_bg() { # NAME CMD... : run in the background, remember the PID
  local name=$1
  shift
  "$@" > "$DIR/$name.log" 2>&1 &
  echo $! > "$DIR/$name.pid"
}

stop_all() {
  local f
  for f in "$DIR"/*.pid; do
    [ -e "$f" ] || continue
    kill "$(cat "$f")" 2>/dev/null || true
    rm -f "$f"
  done
}

up() {
  local workers=${1:-1}
  command -v go >/dev/null || [ -x /usr/local/go/bin/go ] || { echo "Go is required"; exit 1; }
  local go
  go=$(command -v go || echo /usr/local/go/bin/go)
  echo "==> Building"
  CGO_ENABLED=0 "$go" build -o "$BIN" ./cmd/cluster
  stop_all
  mkdir -p "$DIR"

  if [ ! -f "$DIR/ctl/controller.json" ]; then
    echo "==> Initialising controller (admin password: devpassword)"
    CLUSTER_ADMIN_PASSWORD=devpassword "$BIN" controller init --data-dir "$DIR/ctl" --cli-config "$DIR/cli.json" \
      --node-listen "127.0.0.1:$NODE_PORT" --http-listen "127.0.0.1:$HTTP_PORT" --no-mdns --json > "$DIR/init.json"
  fi
  start_bg controller "$BIN" controller --data-dir "$DIR/ctl"
  sleep 1

  local token fp i
  token=$(grep -o '"join_token":"[^"]*' "$DIR/init.json" | cut -d'"' -f4)
  fp=$(grep -o '"ca_fingerprint":"[^"]*' "$DIR/init.json" | cut -d'"' -f4)
  for i in $(seq 1 "$workers"); do
    start_bg "worker$i" "$BIN" worker --data-dir "$DIR/w$i" --name "dev-worker-$i" --location dev \
      --controller "127.0.0.1:$NODE_PORT" --token "$token" --ca-fingerprint "$fp"
  done
  sleep 3
  "${CLI[@]}" nodes
  cat <<EOF

Dev cluster running (logs in $DIR/*.log). Try:
  ./dev.sh cli submit -f -- echo hello from the cluster
  ./dev.sh cli submit --wait --array 1-20 -- 'sleep 1; echo task {i}'
  ./dev.sh cli jobs
Web/API: https://127.0.0.1:$HTTP_PORT (password: devpassword)
Stop with: ./dev.sh down
EOF
}

case "${1:-}" in
  up) up "${2:-1}" ;;
  down) stop_all; echo "stopped" ;;
  clean) stop_all; rm -rf "$DIR"; echo "removed $DIR" ;;
  cli) shift; exec "${CLI[@]}" "$@" ;;
  *) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
