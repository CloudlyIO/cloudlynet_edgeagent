#!/usr/bin/env bash
#
# CloudlyNet Edge Agent uninstaller.
#
#   sudo ./scripts/uninstall.sh            # remove service + binary, keep data/config
#   sudo ./scripts/uninstall.sh --purge    # also remove config, data, and the user
#
set -euo pipefail

SERVICE="cloudlynet-edgeagent"
BINARY="cloudlynet-agent"
RUN_USER="cloudlynet"
BINDIR="/usr/local/bin"
CONFDIR="/etc/cloudlynet-agent"
DATADIR="/var/lib/cloudlynet-agent"
SYSTEMD_DIR="/etc/systemd/system"

PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

log() { printf '\033[36m[uninstall]\033[0m %s\n' "$*"; }
die() { printf '\033[31m[uninstall] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "must run as root (use sudo)."

log "stopping and disabling $SERVICE"
systemctl stop "$SERVICE" >/dev/null 2>&1 || true
systemctl disable "$SERVICE" >/dev/null 2>&1 || true
rm -f "$SYSTEMD_DIR/$SERVICE.service"
systemctl daemon-reload

log "removing binary"
rm -f "$BINDIR/$BINARY"

if [ "$PURGE" -eq 1 ]; then
	log "purging config, data, and user"
	rm -rf "$CONFDIR" "$DATADIR"
	if id -u "$RUN_USER" >/dev/null 2>&1; then
		userdel "$RUN_USER" >/dev/null 2>&1 || true
	fi
else
	log "kept $CONFDIR and $DATADIR (use --purge to remove)"
fi

log "done."
