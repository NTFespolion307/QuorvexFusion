#!/usr/bin/env bash
# install.sh: set up a QuorvexFusion cluster node.
#
#   ./install.sh                      interactive: asks which role
#   ./install.sh controller [opts]    install the controller (and optionally a worker)
#   ./install.sh worker [opts]        install a worker and join a controller
#   ./install.sh upgrade              rebuild/download the binary, restart services
#   ./install.sh uninstall [--purge]  remove everything (--purge also deletes data)
#
# Every question has a flag, so it can run unattended, e.g.
#   ./install.sh worker --controller 10.0.0.5:7443 --token cjt_... --ca-fingerprint sha256:... --yes
#
# Run `./install.sh help` for all options.
set -euo pipefail

REPO="NTFespolion307/QuorvexFusion"
BIN=/usr/local/bin/cluster
CONF_DIR=/etc/cluster
STATE_FILE=$CONF_DIR/install.state
CTL_USER=clusterctl      # runs the controller
TASK_USER=cluster        # runs tasks on workers (never root, never the controller's user)
CTL_DATA_DEFAULT=/var/lib/cluster
WORKER_DATA_DEFAULT=/var/lib/cluster-worker
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BUILD_TMP=""
trap '[ -n "$BUILD_TMP" ] && rm -rf "$BUILD_TMP"' EXIT

# --------------------------------------------------------------- output ---

if [ -t 1 ]; then
  BOLD=$'\e[1m' DIM=$'\e[2m' RED=$'\e[31m' GREEN=$'\e[32m' YELLOW=$'\e[33m' BLUE=$'\e[34m' RESET=$'\e[0m'
else
  BOLD="" DIM="" RED="" GREEN="" YELLOW="" BLUE="" RESET=""
fi
info()  { printf '%s==>%s %s\n' "$BLUE$BOLD" "$RESET" "$*"; }
ok()    { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$*"; }
warn()  { printf '%sWARNING:%s %s\n' "$YELLOW$BOLD" "$RESET" "$*" >&2; }
die()   { printf '%sERROR:%s %s\n' "$RED$BOLD" "$RESET" "$*" >&2; exit 1; }

# -------------------------------------------------------------- prompts ---
# Prompts read from the terminal. With --yes, or without a terminal, the
# default answer is used silently.

ASSUME_YES=0
interactive() { [ "$ASSUME_YES" = 0 ] && [ -t 0 ]; }

# ask VAR "Question" [default]
ask() {
  local __var=$1 question=$2 default=${3:-} answer=""
  if interactive; then
    if [ -n "$default" ]; then
      read -r -p "$question [$default]: " answer
    else
      read -r -p "$question: " answer
    fi
  fi
  printf -v "$__var" '%s' "${answer:-$default}"
}

# ask_secret VAR "Question"
ask_secret() {
  local __var=$1 question=$2 answer=""
  if interactive; then
    read -r -s -p "$question: " answer
    echo
  fi
  printf -v "$__var" '%s' "$answer"
}

# confirm "Question" y|n  (returns 0 for yes)
confirm() {
  local question=$1 default=$2 answer=""
  if interactive; then
    if [ "$default" = y ]; then read -r -p "$question [Y/n]: " answer; else read -r -p "$question [y/N]: " answer; fi
  fi
  answer=${answer:-$default}
  [[ "$answer" =~ ^[Yy] ]]
}

# --------------------------------------------------------- environment ---

need_root() {
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null || die "please run as root"
    info "Root is needed to install services; re-running with sudo"
    exec sudo -E bash "$0" "$@"
  fi
}

detect_arch() {
  [ "$(uname -s)" = Linux ] || die "only Linux is supported"
  case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) die "unsupported CPU architecture: $(uname -m) (amd64 and arm64 are supported)" ;;
  esac
}

have_systemd() { [ -d /run/systemd/system ] && command -v systemctl >/dev/null; }

# download URL FILE
download() {
  if command -v curl >/dev/null; then
    curl -fsSL --retry 3 -o "$2" "$1"
  elif command -v wget >/dev/null; then
    wget -q -O "$2" "$1"
  else
    die "need curl or wget"
  fi
}

# The invoking user's home (when run with sudo), for Go and CLI config.
user_home() {
  if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
    getent passwd "$SUDO_USER" | cut -d: -f6
  else
    echo "$HOME"
  fi
}

state_set() { # key value
  mkdir -p "$CONF_DIR"
  touch "$STATE_FILE"
  grep -v "^$1=" "$STATE_FILE" > "$STATE_FILE.tmp" || true
  echo "$1=$2" >> "$STATE_FILE.tmp"
  mv "$STATE_FILE.tmp" "$STATE_FILE"
}
state_get() { [ -f "$STATE_FILE" ] && grep "^$1=" "$STATE_FILE" | tail -1 | cut -d= -f2- || true; }

# wait_port HOST PORT: wait up to 20s for a TCP port to accept connections.
wait_port() {
  local i
  for i in $(seq 1 40); do
    if (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null; then return 0; fi
    sleep 0.5
  done
  return 1
}

# --------------------------------------------------------- the binary ---
# In order of preference: an explicit --binary, a local Go build (Go 1.21+
# downloads the required toolchain itself), a GitHub release, and finally a
# temporary Go toolchain downloaded only for this build.

BINARY_SOURCE=""   # --binary PATH
FORCE_DOWNLOAD=0   # --download
BINARY_DONE=0

find_go() {
  local home cand
  home=$(user_home)
  for cand in "$(command -v go 2>/dev/null || true)" /usr/local/go/bin/go /usr/lib/go/bin/go /snap/bin/go \
              "$home/.local/go/bin/go" "$home/go/bin/go" "$home"/sdk/go*/bin/go; do
    [ -n "$cand" ] && [ -x "$cand" ] || continue
    # Go 1.21+ can fetch the newer toolchain go.mod asks for.
    local minor
    minor=$("$cand" env GOVERSION 2>/dev/null | sed -n 's/^go1\.\([0-9]*\).*/\1/p')
    if [ -n "$minor" ] && [ "$minor" -ge 21 ]; then
      echo "$cand"
      return 0
    fi
  done
  return 1
}

build_with_go() { # GO OUT
  local go=$1 out=$2 version
  version=$(git -c safe.directory="$SCRIPT_DIR" -C "$SCRIPT_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)
  info "Building cluster $version with $("$go" env GOVERSION) (first build downloads dependencies)"
  # GOTOOLCHAIN=auto overrides distro packages that pin "local", so an older
  # Go fetches the version go.mod requires. -buildvcs=false avoids git
  # "dubious ownership" errors when root builds a user's checkout.
  (cd "$SCRIPT_DIR" && GOTOOLCHAIN=auto CGO_ENABLED=0 GOFLAGS=-buildvcs=false "$go" build -trimpath \
    -ldflags "-s -w -X github.com/$REPO/internal/version.Version=$version" -o "$out" ./cmd/cluster)
}

download_release() { # OUT
  local base="https://github.com/$REPO/releases/latest/download"
  info "Downloading the latest release for linux/$ARCH"
  download "$base/cluster-linux-$ARCH" "$1" 2>/dev/null || return 1
  if download "$base/checksums.txt" "$1.sums" 2>/dev/null; then
    local want got
    want=$(grep "cluster-linux-$ARCH\$" "$1.sums" | cut -d' ' -f1)
    got=$(sha256sum "$1" | cut -d' ' -f1)
    [ -n "$want" ] && [ "$want" = "$got" ] || die "release checksum mismatch for cluster-linux-$ARCH"
  fi
}

download_go_and_build() { # OUT
  local tmp=$1.go ver
  ver=$(curl -fsSL "https://go.dev/VERSION?m=text" 2>/dev/null | head -1 || true)
  [ -n "$ver" ] || die "could not determine the current Go version from go.dev"
  info "No Go found and no release available: downloading $ver temporarily to build"
  mkdir -p "$tmp"
  download "https://go.dev/dl/$ver.linux-$ARCH.tar.gz" "$tmp/go.tgz"
  tar -C "$tmp" -xzf "$tmp/go.tgz"
  build_with_go "$tmp/go/bin/go" "$1"
}

install_binary() {
  [ "$BINARY_DONE" = 1 ] && return 0
  local tmp go
  tmp=$(mktemp -d)
  BUILD_TMP=$tmp # removed by the EXIT trap
  if [ -n "$BINARY_SOURCE" ]; then
    cp "$BINARY_SOURCE" "$tmp/cluster"
  elif [ "$FORCE_DOWNLOAD" = 0 ] && [ -f "$SCRIPT_DIR/go.mod" ] && go=$(find_go); then
    build_with_go "$go" "$tmp/cluster"
  elif download_release "$tmp/cluster"; then
    :
  elif [ -f "$SCRIPT_DIR/go.mod" ]; then
    download_go_and_build "$tmp/cluster"
  else
    die "no Go toolchain, no release download, and no source tree to build from"
  fi
  install -m 0755 "$tmp/cluster" "$BIN"
  ok "Installed $BIN ($("$BIN" version))"
  BINARY_DONE=1
}

# --------------------------------------------------------------- users ---

nologin_shell() {
  for s in /usr/sbin/nologin /sbin/nologin /bin/false; do [ -x "$s" ] && { echo "$s"; return; }; done
  echo /bin/false
}

# ensure_user NAME HOME CREATE_HOME(0|1)
ensure_user() {
  local name=$1 home=$2 create=$3
  id "$name" >/dev/null 2>&1 && return 0
  if command -v useradd >/dev/null; then
    if [ "$create" = 1 ]; then
      useradd --system --create-home --home-dir "$home" --shell "$(nologin_shell)" "$name"
    else
      useradd --system --no-create-home --home-dir "$home" --shell "$(nologin_shell)" "$name"
    fi
  else
    adduser -S -D -h "$home" -s "$(nologin_shell)" "$name"
  fi
  state_set "created_user_$name" 1
  ok "Created system user $name"
}

# ------------------------------------------------------------- systemd ---

install_unit() { # NAME DATA_DIR [USER]
  local name=$1 data=$2 user=${3:-} tmpl="$SCRIPT_DIR/deploy/systemd/$1.service"
  [ -f "$tmpl" ] || die "missing $tmpl (run install.sh from a checkout of the repository)"
  sed -e "s|@BIN@|$BIN|g" -e "s|@DATA_DIR@|$data|g" -e "s|@USER@|$user|g" "$tmpl" > "/etc/systemd/system/$name.service"
  systemctl daemon-reload
  systemctl enable "$name" >/dev/null 2>&1
  systemctl restart "$name"
  ok "Service $name enabled and started"
}

firewall_hint() { # PORT...
  if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
    warn "ufw is active. To let workers and browsers in, run:"
    for p in "$@"; do echo "    sudo ufw allow $p/tcp"; done
  elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
    warn "firewalld is active. To let workers and browsers in, run:"
    for p in "$@"; do echo "    sudo firewall-cmd --permanent --add-port=$p/tcp"; done
    echo "    sudo firewall-cmd --reload"
  fi
}

# ---------------------------------------------------------- controller ---

usage_controller() {
  cat <<EOF
Usage: ./install.sh controller [options]

  --data-dir DIR          data directory (default $CTL_DATA_DEFAULT)
  --node-port PORT        port workers connect to (default 7443)
  --http-port PORT        web UI / API port (default 8443)
  --listen-ip IP          only listen on this IP, e.g. a Tailscale/WireGuard address
                          (default: all interfaces)
  --public-addr HOST      domain or IP that workers and browsers use to reach this
                          machine (put in the certificate and join commands)
  --admin-password PW     admin password (or set CLUSTER_ADMIN_PASSWORD;
                          generated and printed if not given non-interactively)
  --no-mdns               don't advertise on the LAN
  --with-worker           also run a worker on this machine
  --no-worker             don't
  --worker-location NAME  location label for that worker
  --binary PATH           install this binary instead of building/downloading
  --download              download a release instead of building
  --yes                   accept defaults, no questions
EOF
}

cmd_controller() {
  local data="" node_port="" http_port="" listen_ip="" public_addr="" password="${CLUSTER_ADMIN_PASSWORD:-}"
  local mdns=1 with_worker="" worker_location="" generated_pw=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --data-dir) data=$2; shift ;;
      --node-port) node_port=$2; shift ;;
      --http-port) http_port=$2; shift ;;
      --listen-ip) listen_ip=$2; shift ;;
      --public-addr) public_addr=$2; shift ;;
      --admin-password) password=$2; shift ;;
      --no-mdns) mdns=0 ;;
      --with-worker) with_worker=1 ;;
      --no-worker) with_worker=0 ;;
      --worker-location) worker_location=$2; shift ;;
      --binary) BINARY_SOURCE=$2; shift ;;
      --download) FORCE_DOWNLOAD=1 ;;
      --yes | -y) ASSUME_YES=1 ;;
      -h | --help) usage_controller; exit 0 ;;
      *) die "unknown option $1 (see ./install.sh controller --help)" ;;
    esac
    shift
  done

  data=${data:-$(state_get controller_data)}
  local existing=0
  if [ -n "$data" ] && [ -f "$data/controller.json" ]; then
    existing=1
  fi

  echo
  echo "${BOLD}Installing the QuorvexFusion controller${RESET}"
  if [ "$existing" = 1 ]; then
    info "An existing controller was found in $data: upgrading the binary and restarting (data is kept)"
  else
    ask data "Data directory" "${data:-$CTL_DATA_DEFAULT}"
    if [ -f "$data/controller.json" ]; then existing=1; fi
  fi

  if [ "$existing" = 0 ]; then
    ask node_port "Port for workers" "${node_port:-7443}"
    ask http_port "Port for the web UI" "${http_port:-8443}"
    if [ -z "$listen_ip" ] && interactive; then
      echo "${DIM}Leave empty to listen on all interfaces, or give one IP (e.g. your Tailscale IP)"
      echo "to keep the controller reachable only over that network.${RESET}"
      ask listen_ip "Listen only on IP" ""
    fi
    if [ -z "$public_addr" ] && interactive; then
      echo "${DIM}Addresses on this machine: $(hostname -I 2>/dev/null || true)"
      echo "If workers reach this machine through a domain, public IP or Tailscale name, enter it.${RESET}"
      ask public_addr "Public address (optional)" ""
    fi
    if [ -z "$password" ]; then
      if interactive; then
        while :; do
          ask_secret password "Admin password (min 8 characters)"
          local again
          ask_secret again "Repeat password"
          [ "$password" = "$again" ] && [ ${#password} -ge 8 ] && break
          warn "passwords differ or are shorter than 8 characters"
        done
      else
        password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
        generated_pw=1
      fi
    fi
    if [ "$mdns" = 1 ] && [ -n "$listen_ip" ] && interactive; then
      confirm "Advertise this controller on the local network (mDNS)?" y || mdns=0
    fi
  fi
  if [ -z "$with_worker" ]; then
    local wd
    wd=$(state_get worker_data)
    if [ "$existing" = 1 ] && [ -n "$wd" ] && [ -f "$wd/worker.json" ]; then
      with_worker=1 # already has a worker: just upgrade it too
    elif [ "$existing" = 1 ]; then
      if confirm "Also run a worker on this machine (adds its CPUs/GPUs to the pool)?" n; then with_worker=1; else with_worker=0; fi
    else
      if confirm "Also run a worker on this machine (adds its CPUs/GPUs to the pool)?" y; then with_worker=1; else with_worker=0; fi
    fi
  fi

  install_binary
  ensure_user "$CTL_USER" "$data" 0
  state_set controller_data "$data"
  mkdir -p "$CONF_DIR"

  local token="" fp="" node_addr="" ui_url=""
  if [ "$existing" = 0 ]; then
    local args=(controller init --data-dir "$data" --node-listen "$listen_ip:$node_port"
      --http-listen "$listen_ip:$http_port" --password-stdin --cli-config "$CONF_DIR/cli.json" --json)
    [ -n "$public_addr" ] && args+=(--public-addr "$public_addr")
    [ "$mdns" = 0 ] && args+=(--no-mdns)
    local out
    out=$(printf '%s\n' "$password" | "$BIN" "${args[@]}")
    token=$(printf '%s' "$out" | sed -n 's/.*"join_token":"\([^"]*\)".*/\1/p')
    fp=$(printf '%s' "$out" | sed -n 's/.*"ca_fingerprint":"\([^"]*\)".*/\1/p')
    node_addr=$(printf '%s' "$out" | sed -n 's/.*"node_addr":"\([^"]*\)".*/\1/p')
    ui_url=$(printf '%s' "$out" | sed -n 's/.*"ui_url":"\([^"]*\)".*/\1/p')
    ok "Controller initialised in $data"
  else
    node_port=$(sed -n 's/.*"node_listen": *"[^"]*:\([0-9]*\)".*/\1/p' "$data/controller.json")
    http_port=$(sed -n 's/.*"http_listen": *"[^"]*:\([0-9]*\)".*/\1/p' "$data/controller.json")
    listen_ip=$(sed -n 's/.*"node_listen": *"\([^"]*\):[0-9]*".*/\1/p' "$data/controller.json")
  fi
  chown -R "$CTL_USER:$CTL_USER" "$data"
  chmod 700 "$data"
  chmod 600 "$CONF_DIR/cli.json" 2>/dev/null || true

  # Let the user who ran sudo use the CLI without sudo.
  local home
  home=$(user_home)
  if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] && [ -f "$CONF_DIR/cli.json" ]; then
    mkdir -p "$home/.config/cluster"
    cp "$CONF_DIR/cli.json" "$home/.config/cluster/cli.json"
    chown -R "$SUDO_USER:" "$home/.config/cluster"
    chmod 600 "$home/.config/cluster/cli.json"
  fi

  local connect_ip=${listen_ip:-127.0.0.1}
  if have_systemd; then
    install_unit cluster-controller "$data" "$CTL_USER"
    wait_port "$connect_ip" "$node_port" || die "the controller did not start; see: journalctl -u cluster-controller -n 50"
  else
    warn "systemd is not available. Start the controller under your process supervisor with:"
    echo "    runuser -u $CTL_USER -- $BIN controller --data-dir $data"
  fi
  firewall_hint "$node_port" "$http_port"

  if [ "$with_worker" = 1 ]; then
    if [ -z "$token" ] && [ ! -f "$(state_get worker_data)/worker.json" ]; then
      # Existing controller, new local worker: mint a single-use token.
      local created
      created=$("$BIN" --config "$CONF_DIR/cli.json" token create --description "local worker" --max-uses 1 --expires 1h --json)
      token=$(printf '%s' "$created" | grep -o 'cjt_[0-9a-f]*_[0-9a-f]*' | head -1)
      fp=$(printf '%s' "$created" | grep -o 'sha256:[0-9a-f]*' | head -1)
    fi
    local wargs=(--controller "$connect_ip:$node_port" --token "$token" --ca-fingerprint "$fp" --yes)
    [ -n "$worker_location" ] && wargs+=(--location "$worker_location")
    echo
    info "Setting up the worker on this machine"
    ( ASSUME_YES=1 cmd_worker "${wargs[@]}" )
  fi

  echo
  echo "${GREEN}${BOLD}Controller ready.${RESET}"
  if [ "$existing" = 0 ]; then
    cat <<EOF

  Web UI:          $ui_url   (user: admin)
  CA fingerprint:  $fp
  Worker address:  $node_addr
EOF
    if [ "$generated_pw" = 1 ]; then
      echo "  Admin password:  $password   ${YELLOW}(generated; change it with: sudo -u $CTL_USER $BIN controller passwd --data-dir $data)${RESET}"
    fi
    cat <<EOF

  The CLI is configured for root${SUDO_USER:+ and $SUDO_USER}: try ${BOLD}cluster status${RESET}

Join another machine to the pool (this token auto-approves and is valid 7 days):

  git clone https://github.com/$REPO.git && cd QuorvexFusion && \\
    sudo ./install.sh worker --controller $node_addr --token $token --ca-fingerprint $fp --yes

Create more tokens with ${BOLD}cluster token create${RESET} (or later in the web UI).
EOF
  fi
}

# -------------------------------------------------------------- worker ---

usage_worker() {
  cat <<EOF
Usage: ./install.sh worker [options]

  --controller HOST:PORT  controller node address (default port 7443); if omitted,
                          controllers on the LAN are listed to choose from
  --token TOKEN           join token (not needed if this machine already joined)
  --ca-fingerprint FP     expected controller CA fingerprint (sha256:...)
  --location NAME         location label, e.g. home, vastai, gcp-us-central1
  --name NAME             node name (default: hostname)
  --label KEY=VALUE       node label (repeatable)
  --ephemeral             rented/cloud node: removed automatically when gone too long
  --shared-storage PATH   storage shared with the controller (skips file transfers)
  --task-user USER        run tasks as this user (default: $TASK_USER, created if missing)
  --data-dir DIR          identity and cache directory (default $WORKER_DATA_DEFAULT)
  --binary PATH           install this binary instead of building/downloading
  --download              download a release instead of building
  --yes                   accept defaults, no questions (trusts the controller CA
                          without confirmation if no --ca-fingerprint is given)
EOF
}

cmd_worker() {
  local controller="" token="" fp="" location="" name="" labels="" ephemeral=0 shared="" task_user="" data=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --controller) controller=$2; shift ;;
      --token) token=$2; shift ;;
      --ca-fingerprint) fp=$2; shift ;;
      --location) location=$2; shift ;;
      --name) name=$2; shift ;;
      --label) labels="${labels:+$labels,}$2"; shift ;;
      --ephemeral) ephemeral=1 ;;
      --shared-storage) shared=$2; shift ;;
      --task-user) task_user=$2; shift ;;
      --data-dir) data=$2; shift ;;
      --binary) BINARY_SOURCE=$2; shift ;;
      --download) FORCE_DOWNLOAD=1 ;;
      --yes | -y) ASSUME_YES=1 ;;
      -h | --help) usage_worker; exit 0 ;;
      *) die "unknown option $1 (see ./install.sh worker --help)" ;;
    esac
    shift
  done
  data=${data:-$(state_get worker_data)}
  data=${data:-$WORKER_DATA_DEFAULT}

  echo
  echo "${BOLD}Installing a QuorvexFusion worker${RESET}"
  install_binary

  local joined=0
  [ -f "$data/node.crt" ] || [ -f "$data/worker.json" ] && joined=1
  if [ "$joined" = 1 ]; then
    info "This machine already joined a controller (identity in $data); keeping it."
    echo "    To join a different controller instead: sudo ./install.sh uninstall --purge, then install again."
  fi

  if [ "$joined" = 0 ] && [ -z "$controller" ]; then
    if interactive; then
      choose_controller
    else
      die "--controller is required"
    fi
  fi
  if [ "$joined" = 0 ] && [ -z "$token" ]; then
    interactive || die "--token is required for the first join"
    ask_secret token "Join token (from the controller install output, the web UI, or 'cluster token create')"
    [ -n "$token" ] || die "a join token is required"
  fi
  if [ "$joined" = 0 ]; then
    [ -n "$location" ] || ask location "Location label for this machine (e.g. home, office, vastai; optional)" ""
    if [ "$ephemeral" = 0 ] && interactive; then
      confirm "Is this a rented/cloud machine that may disappear (ephemeral)?" n && ephemeral=1
    fi
    if [ -z "$labels" ] && interactive; then
      ask labels "Labels as key=value,key=value (optional, e.g. gpu=4090)" ""
    fi
  fi

  # Tasks run as an unprivileged user, separate from the controller's.
  if [ -z "$task_user" ]; then
    ensure_user "$TASK_USER" "/home/$TASK_USER" 1
    task_user=$TASK_USER
  fi

  mkdir -p "$data" "$CONF_DIR"
  chmod 700 "$data"
  state_set worker_data "$data"

  if [ "$joined" = 0 ]; then
    local args=(worker join --data-dir "$data" --controller "$controller" --token "$token")
    [ -n "$fp" ] && args+=(--ca-fingerprint "$fp")
    [ "$ASSUME_YES" = 1 ] && args+=(--yes)
    [ -n "$location" ] && args+=(--location "$location")
    [ -n "$name" ] && args+=(--name "$name")
    [ "$ephemeral" = 1 ] && args+=(--ephemeral)
    # Runs the join (and, interactively, the fingerprint confirmation).
    "$BIN" "${args[@]}" || die "joining the controller failed"
  fi

  # Settings the service reads on every start (edit and restart to change).
  {
    echo "# QuorvexFusion worker settings, read by cluster-worker.service."
    echo "# Edit, then: sudo systemctl restart cluster-worker"
    echo "CLUSTER_TASK_USER=\"$task_user\""
    [ -n "$location" ] && echo "CLUSTER_LOCATION=\"$location\""
    [ -n "$name" ] && echo "CLUSTER_NODE_NAME=\"$name\""
    [ -n "$labels" ] && echo "CLUSTER_LABELS=\"$labels\""
    [ "$ephemeral" = 1 ] && echo "CLUSTER_EPHEMERAL=1"
    [ -n "$shared" ] && echo "CLUSTER_SHARED_STORAGE=\"$shared\""
    true
  } > "$CONF_DIR/worker.env.new"
  if [ "$joined" = 1 ] && [ -f "$CONF_DIR/worker.env" ] && [ -z "$location$name$labels$shared" ] && [ "$ephemeral" = 0 ]; then
    rm "$CONF_DIR/worker.env.new" # upgrade without new settings: keep the existing file
  else
    mv "$CONF_DIR/worker.env.new" "$CONF_DIR/worker.env"
  fi

  if have_systemd; then
    install_unit cluster-worker "$data"
    sleep 2
    if systemctl is-active --quiet cluster-worker; then
      ok "Worker is running. Follow its log with: journalctl -u cluster-worker -f"
    else
      warn "the worker service is not running; see: journalctl -u cluster-worker -n 50"
    fi
  else
    warn "systemd is not available (e.g. inside a container). Run the worker in the foreground"
    echo "    or under your process supervisor (it reads the same settings from the environment):"
    echo
    echo "    set -a; . $CONF_DIR/worker.env; set +a; $BIN worker --data-dir $data"
  fi
}

# choose_controller sets $controller from LAN discovery or manual entry.
choose_controller() {
  info "Looking for controllers on the local network..."
  local lines=() line i=1 choice
  while IFS= read -r line; do [ -n "$line" ] && lines+=("$line"); done < <("$BIN" discover --plain --timeout 3s 2>/dev/null || true)
  if [ ${#lines[@]} -gt 0 ]; then
    for line in "${lines[@]}"; do
      IFS=$'\t' read -r addr lfp lname <<< "$line"
      printf '  %d) %-22s %-20s %s\n' "$i" "$addr" "$lname" "${lfp:0:23}..."
      i=$((i + 1))
    done
    echo "  m) enter an address manually"
    ask choice "Controller" 1
    if [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le ${#lines[@]} ]; then
      IFS=$'\t' read -r controller _ _ <<< "${lines[$((choice - 1))]}"
      return
    fi
  else
    echo "  None found (multicast may be blocked; that's normal across VPNs and clouds)."
  fi
  ask controller "Controller address (IP, hostname or Tailscale name, with :port if not 7443)" ""
  [ -n "$controller" ] || die "a controller address is required"
  [[ "$controller" == *:* ]] || controller="$controller:7443"
}

# ------------------------------------------------------------- upgrade ---

cmd_upgrade() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --binary) BINARY_SOURCE=$2; shift ;;
      --download) FORCE_DOWNLOAD=1 ;;
      *) die "unknown option $1" ;;
    esac
    shift
  done
  install_binary
  if have_systemd; then
    local s
    for s in cluster-controller cluster-worker; do
      if systemctl is-enabled --quiet "$s" 2>/dev/null; then
        # Refresh the unit in case the template changed.
        if [ "$s" = cluster-controller ]; then
          install_unit "$s" "$(state_get controller_data)" "$CTL_USER"
        else
          install_unit "$s" "$(state_get worker_data)"
        fi
      fi
    done
  fi
}

# ----------------------------------------------------------- uninstall ---

cmd_uninstall() {
  local purge=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --purge) purge=1 ;;
      --yes | -y) ASSUME_YES=1 ;;
      *) die "unknown option $1" ;;
    esac
    shift
  done
  local ctl_data worker_data
  ctl_data=$(state_get controller_data)
  worker_data=$(state_get worker_data)

  echo "${BOLD}Removing QuorvexFusion from this machine${RESET}"
  if [ "$purge" = 0 ] && interactive; then
    confirm "Also delete all data (controller database, logs, CA, worker identity)?" n && purge=1
  fi

  if have_systemd; then
    systemctl disable --now cluster-worker cluster-controller >/dev/null 2>&1 || true
    systemctl stop 'cluster-task-*.scope' >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/cluster-worker.service /etc/systemd/system/cluster-controller.service
    systemctl daemon-reload
    ok "Services removed"
  fi
  pkill -f "^$BIN (controller|worker)" 2>/dev/null || true
  rm -f "$BIN"
  ok "Removed $BIN"

  if [ "$purge" = 1 ]; then
    [ -n "$ctl_data" ] && rm -rf "$ctl_data" && ok "Deleted controller data $ctl_data"
    [ -n "$worker_data" ] && rm -rf "$worker_data" && ok "Deleted worker data $worker_data"
    local u
    for u in "$CTL_USER" "$TASK_USER"; do
      if [ "$(state_get "created_user_$u")" = 1 ] && id "$u" >/dev/null 2>&1; then
        userdel -r "$u" >/dev/null 2>&1 || userdel "$u" >/dev/null 2>&1 || true
        ok "Removed user $u"
      fi
    done
    rm -rf "$CONF_DIR"
  else
    rm -f "$CONF_DIR/worker.env" "$CONF_DIR/cli.json"
    echo "Data kept: ${ctl_data:-none} ${worker_data:-} and $STATE_FILE (use --purge to delete)."
  fi
  [ -n "${SUDO_USER:-}" ] && [ -f "$(user_home)/.config/cluster/cli.json" ] &&
    echo "Your CLI config $(user_home)/.config/cluster/cli.json was left in place."
  ok "Uninstalled"
}

# ---------------------------------------------------------------- main ---

usage() {
  sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
  echo
  echo "Role options: ./install.sh controller --help, ./install.sh worker --help"
}

main() {
  local role=${1:-}
  [ $# -gt 0 ] && shift
  case "$role" in
    help | -h | --help) usage; exit 0 ;;
  esac
  need_root "$role" "$@"
  detect_arch

  if [ -z "$role" ]; then
    [ -t 0 ] || die "no role given (controller, worker, upgrade, uninstall)"
    echo "${BOLD}QuorvexFusion installer${RESET}"
    echo "  1) controller  - the master: schedules jobs, serves the web UI"
    echo "  2) worker      - adds this machine's CPUs/RAM/GPUs to a cluster"
    echo "  3) uninstall"
    local choice
    read -r -p "Role [1]: " choice
    case "${choice:-1}" in
      1 | controller) role=controller ;;
      2 | worker) role=worker ;;
      3 | uninstall) role=uninstall ;;
      *) die "unknown choice" ;;
    esac
  fi

  case "$role" in
    controller) cmd_controller "$@" ;;
    worker) cmd_worker "$@" ;;
    upgrade) cmd_upgrade "$@" ;;
    uninstall) cmd_uninstall "$@" ;;
    *) die "unknown role '$role' (controller, worker, upgrade, uninstall)" ;;
  esac
}

main "$@"
