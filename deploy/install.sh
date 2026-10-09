#!/usr/bin/env bash
# Install agentshare as a systemd service on a Linux host.
#
# Run as root, from a directory that holds the binary and this unit:
#
#   scp agentshare-linux-amd64 install.sh agentshare.service root@host:/tmp/ash/
#   ssh root@host 'bash /tmp/ash/install.sh'
#
# It is idempotent: running it again upgrades the binary and leaves the
# existing root token and data directory alone.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_SRC="${BIN_SRC:-$HERE/agentshare-linux-amd64}"
BIN_DST="/usr/local/bin/agentshare"
ENV_FILE="/etc/agentshare.env"
UNIT_SRC="$HERE/agentshare.service"
UNIT_DST="/etc/systemd/system/agentshare.service"
STATE_DIR="/var/lib/agentshare"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

if [ ! -f "$BIN_SRC" ]; then
  echo "binary not found at $BIN_SRC (set BIN_SRC=/path/to/agentshare)" >&2
  exit 1
fi

# --- the root token -------------------------------------------------------
# Generated once and kept, because re-running the installer must not lock out
# every client that is already using the old one.
if [ -f "$ENV_FILE" ] && grep -q '^AGENTSHARE_ADMIN_TOKEN=' "$ENV_FILE"; then
  echo "keeping the existing root token in $ENV_FILE"
else
  TOKEN="ash_$(head -c 32 /dev/urandom | base64 | tr -dc 'a-z0-9' | head -c 40)"
  umask 077
  cat > "$ENV_FILE" <<EOF
# agentshare configuration. Keep this file out of version control and out of
# backups that other people can read: AGENTSHARE_ADMIN_TOKEN is the root
# credential.
AGENTSHARE_LISTEN=127.0.0.1:8080
AGENTSHARE_DATA_DIR=$STATE_DIR
AGENTSHARE_ADMIN_TOKEN=$TOKEN
# Set these when a reverse proxy terminates TLS in front of agentshare:
#AGENTSHARE_BASE_URL=https://files.example.com
#AGENTSHARE_BEHIND_PROXY=true
#AGENTSHARE_DEFAULT_TTL=30d
EOF
  chmod 600 "$ENV_FILE"
  echo
  echo "  root token written to $ENV_FILE (mode 600):"
  echo "    $TOKEN"
  echo "  Mint narrower tokens for individual agents with POST /v1/tokens."
  echo
fi

# --- the service ----------------------------------------------------------
install -m 0755 "$BIN_SRC" "$BIN_DST"
install -m 0644 "$UNIT_SRC" "$UNIT_DST"
install -d -m 0750 "$STATE_DIR"

systemctl daemon-reload
systemctl enable --now agentshare.service >/dev/null

# enable --now returns as soon as the unit is queued, so ask the service
# directly rather than assuming it came up.
for _ in $(seq 1 20); do
  systemctl is-active --quiet agentshare.service && break
  sleep 0.5
done
if ! systemctl is-active --quiet agentshare.service; then
  echo "agentshare did not start; last 30 log lines:" >&2
  journalctl -u agentshare.service -n 30 --no-pager >&2
  exit 1
fi

echo "agentshare is ($(systemctl is-active agentshare.service))"
echo "  binary   $BIN_DST"
echo "  unit     $UNIT_DST"
echo "  data     $STATE_DIR (owned by a dynamic user; read the token as root)"
echo "  config   $ENV_FILE"

# Prove it answers, through the address nginx will use.
PORT="$(grep -oE '^AGENTSHARE_LISTEN=.*' "$ENV_FILE" | cut -d= -f2- || true)"
PORT="${PORT##*:}"
if command -v curl >/dev/null 2>&1; then
  echo
  echo "  GET /healthz ->"
  curl -sS --max-time 5 "http://127.0.0.1:${PORT:-8080}/healthz" || echo "  (no answer)"
  echo
fi
