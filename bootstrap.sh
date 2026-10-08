#!/bin/sh
# bootstrap.sh: join this machine to a QuorvexFusion cluster in one line.
# Meant for cloud-init, vast.ai "on-start" scripts and similar:
#
#   curl -fsSL https://raw.githubusercontent.com/NTFespolion307/QuorvexFusion/main/bootstrap.sh \
#     | sh -s -- --controller controller.example.com --code 7KQ2-MX4P-9TRA-BH3W-C8NE --location vastai --ephemeral
#
# Everything after "--" is passed to the worker (see `cluster worker --help`):
# --controller, --code, --location, --ephemeral, --label k=v, --name, ...
#
# With systemd, the worker is installed as a service (via install.sh).
# Without systemd (e.g. inside containers such as vast.ai instances), it runs
# in the background under a restart loop, logging to /var/log/cluster-worker.log.
# Running the script again is safe: an existing identity is reused.
# Set BOOTSTRAP_NO_SYSTEMD=1 to use the background mode even with systemd.
set -eu

REPO="NTFespolion307/QuorvexFusion"
BIN=/usr/local/bin/cluster
LOG=/var/log/cluster-worker.log
DATA=/var/lib/cluster-worker

say() { printf '==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# Piped into sh there is no script file to re-run with sudo, so just ask.
[ "$(id -u)" -eq 0 ] || die "run as root: curl -fsSL .../bootstrap.sh | sudo sh -s -- ..."

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

fetch() { # URL FILE
  if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then wget -q -O "$2" "$1"
  else die "need curl or wget"; fi
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# The source tree: needed for install.sh (systemd) or to build the binary.
get_source() {
  [ -d "$TMP/src" ] && return 0
  say "Downloading the source"
  fetch "https://github.com/$REPO/archive/refs/heads/main.tar.gz" "$TMP/src.tgz"
  mkdir -p "$TMP/src"
  tar -xzf "$TMP/src.tgz" -C "$TMP/src" --strip-components=1
}

install_binary() {
  base="https://github.com/$REPO/releases/latest/download"
  if fetch "$base/cluster-linux-$ARCH" "$TMP/cluster" 2>/dev/null; then
    if fetch "$base/checksums.txt" "$TMP/sums" 2>/dev/null; then
      want=$(grep "cluster-linux-$ARCH\$" "$TMP/sums" | cut -d' ' -f1)
      got=$(sha256sum "$TMP/cluster" | cut -d' ' -f1)
      [ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch for the release binary"
    fi
    say "Downloaded the release binary"
  else
    # No release yet: build from source with a temporary Go toolchain.
    get_source
    ver=$(fetch "https://go.dev/VERSION?m=text" /dev/stdout | head -1)
    say "Building from source with $ver (takes a few minutes the first time)"
    fetch "https://go.dev/dl/$ver.linux-$ARCH.tar.gz" "$TMP/go.tgz"
    tar -xzf "$TMP/go.tgz" -C "$TMP"
    (cd "$TMP/src" && CGO_ENABLED=0 GOFLAGS=-buildvcs=false GOPATH="$TMP/gopath" GOCACHE="$TMP/gocache" \
      "$TMP/go/bin/go" build -trimpath -ldflags "-s -w" -o "$TMP/cluster" ./cmd/cluster)
  fi
  install -m 0755 "$TMP/cluster" "$BIN"
  say "Installed $BIN ($("$BIN" version))"
}

install_binary

# BOOTSTRAP_NO_SYSTEMD=1 forces the background mode even where systemd exists.
if [ -z "${BOOTSTRAP_NO_SYSTEMD:-}" ] && [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1 &&
  command -v bash >/dev/null 2>&1; then
  get_source
  say "systemd found: installing the worker service"
  exec bash "$TMP/src/install.sh" worker --binary "$BIN" --yes "$@"
fi

# No systemd: run in the background and restart on failure. Exit code 3
# means the node was revoked, so the loop stops then.
say "No systemd: starting the worker in the background (log: $LOG)"
pkill -f "^$BIN worker" 2>/dev/null || true
mkdir -p "$DATA"
chmod 711 "$DATA"
quoted=""
for a in "$@"; do quoted="$quoted '$(printf '%s' "$a" | sed "s/'/'\\\\''/g")'"; done
nohup sh -c "while true; do $BIN worker --data-dir $DATA --yes $quoted; [ \$? -eq 3 ] && exit 3; sleep 5; done" \
  >> "$LOG" 2>&1 &
sleep 5
if grep -q "connected to controller\|Joined\|joined cluster" "$LOG"; then
  say "Worker connected. Follow it with: tail -f $LOG"
else
  say "Worker started; check $LOG if it does not appear on the controller:"
  tail -5 "$LOG"
fi
