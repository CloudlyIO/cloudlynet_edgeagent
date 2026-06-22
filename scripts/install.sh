#!/usr/bin/env bash
#
# CloudlyNet Edge Agent installer for Ubuntu 22.04 LTS.
#
# Idempotent. Run as root:
#   sudo ./scripts/install.sh
#
# Flow:
#   1. Ensure Go >= 1.23 (download official tarball to /usr/local/go if absent).
#   2. Build the agent binary from source (CGO disabled, pure-Go SQLite).
#   3. Create the cloudlynet system user and /etc + /var/lib directories.
#   4. Install binary, config, rules, and the systemd unit.
#   5. Enable the service; start it only once a real enrollment token is set.
#
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.23.11}"
GO_MIN_MINOR=23

SERVICE="cloudlynet-edgeagent"
BINARY="cloudlynet-agent"
RUN_USER="cloudlynet"
PREFIX="/usr/local"
BINDIR="$PREFIX/bin"
CONFDIR="/etc/cloudlynet-agent"
DATADIR="/var/lib/cloudlynet-agent"
SYSTEMD_DIR="/etc/systemd/system"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

log()  { printf '\033[36m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[install] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[install] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
	[ "$(id -u)" -eq 0 ] || die "must run as root (use sudo)."
}

go_arch() {
	case "$(uname -m)" in
		x86_64|amd64) echo "amd64" ;;
		aarch64|arm64) echo "arm64" ;;
		armv7l|armv6l) echo "armv6l" ;;
		*) die "unsupported architecture: $(uname -m)" ;;
	esac
}

# Echo a usable `go` path if one satisfies the minimum minor version.
find_go() {
	local candidate
	for candidate in "$(command -v go 2>/dev/null || true)" "/usr/local/go/bin/go"; do
		[ -n "$candidate" ] && [ -x "$candidate" ] || continue
		local ver minor
		ver="$("$candidate" version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | head -n1 | sed 's/go//')"
		minor="${ver#*.}"
		if [ -n "$minor" ] && [ "$minor" -ge "$GO_MIN_MINOR" ] 2>/dev/null; then
			echo "$candidate"
			return 0
		fi
	done
	return 1
}

ensure_go() {
	if GO_BIN="$(find_go)"; then
		log "using existing Go: $("$GO_BIN" version)"
		return
	fi
	local arch tarball url
	arch="$(go_arch)"
	tarball="go${GO_VERSION}.linux-${arch}.tar.gz"
	url="https://go.dev/dl/${tarball}"
	log "Go >= 1.${GO_MIN_MINOR} not found; installing Go ${GO_VERSION} (${arch})"
	command -v curl >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq curl ca-certificates; }
	local tmp
	tmp="$(mktemp -d)"
	curl -fsSL "$url" -o "$tmp/$tarball" || die "failed to download $url"
	rm -rf /usr/local/go
	tar -C /usr/local -xzf "$tmp/$tarball"
	rm -rf "$tmp"
	GO_BIN="/usr/local/go/bin/go"
	[ -x "$GO_BIN" ] || die "Go install failed"
	log "installed $("$GO_BIN" version)"
}

build_agent() {
	log "building $BINARY from source"
	mkdir -p "$REPO_DIR/bin"
	( cd "$REPO_DIR/goagent" && CGO_ENABLED=0 GOTOOLCHAIN=local "$GO_BIN" build -trimpath -o "$REPO_DIR/bin/$BINARY" ./cmd/agent )
	[ -x "$REPO_DIR/bin/$BINARY" ] || die "build produced no binary"
}

create_user() {
	if ! id -u "$RUN_USER" >/dev/null 2>&1; then
		log "creating system user $RUN_USER"
		useradd --system --no-create-home --shell /usr/sbin/nologin "$RUN_USER"
	fi
}

install_files() {
	log "installing directories, binary, and config"
	install -d -m0755 "$CONFDIR"
	install -d -m0750 -o "$RUN_USER" -g "$RUN_USER" "$DATADIR"

	install -m0755 "$REPO_DIR/bin/$BINARY" "$BINDIR/$BINARY"

	# Config files: do not clobber an operator-edited install.
	if [ ! -f "$CONFDIR/agent.yaml" ]; then
		install -m0644 "$REPO_DIR/deploy/agent.yaml" "$CONFDIR/agent.yaml"
	else
		install -m0644 "$REPO_DIR/deploy/agent.yaml" "$CONFDIR/agent.yaml.default"
		log "kept existing $CONFDIR/agent.yaml (new default at agent.yaml.default)"
	fi
	if [ ! -f "$CONFDIR/rules.yaml" ]; then
		install -m0644 "$REPO_DIR/config/rules.yaml" "$CONFDIR/rules.yaml"
	else
		install -m0644 "$REPO_DIR/config/rules.yaml" "$CONFDIR/rules.yaml.default"
		log "kept existing $CONFDIR/rules.yaml (new default at rules.yaml.default)"
	fi

	# Secrets env file: create once, mode 0600, never overwrite.
	if [ ! -f "$CONFDIR/agent.env" ]; then
		install -m0600 -o "$RUN_USER" -g "$RUN_USER" "$REPO_DIR/deploy/agent.env.example" "$CONFDIR/agent.env"
		log "created $CONFDIR/agent.env — set CLOUDLYNET_ENROLLMENT_TOKEN before starting"
	fi
}

install_service() {
	log "installing systemd unit"
	install -m0644 "$REPO_DIR/deploy/$SERVICE.service" "$SYSTEMD_DIR/$SERVICE.service"
	systemctl daemon-reload
	systemctl enable "$SERVICE" >/dev/null 2>&1 || true
}

token_is_set() {
	grep -qE '^CLOUDLYNET_ENROLLMENT_TOKEN=.+' "$CONFDIR/agent.env" 2>/dev/null
}

start_service() {
	if token_is_set; then
		log "starting $SERVICE"
		systemctl restart "$SERVICE"
		systemctl --no-pager --lines=0 status "$SERVICE" || true
	else
		warn "CLOUDLYNET_ENROLLMENT_TOKEN is empty in $CONFDIR/agent.env."
		warn "Edit it, then run: sudo systemctl start $SERVICE"
	fi
}

main() {
	require_root
	ensure_go
	build_agent
	create_user
	install_files
	install_service
	start_service
	log "done. Logs: journalctl -u $SERVICE -f"
}

main "$@"
