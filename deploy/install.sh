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

# Deployment knobs, read from the environment so that a particular host's setup
# lives in the command that installed it rather than in a hand-edited file on
# the server. They are only consulted when the config file is first written.
#
#   ASH_BASE_URL=https://files.example.com \
#   ASH_BEHIND_PROXY=true \
#   ASH_MAX_UPLOAD=100M \
#   ASH_PROXY_MAX_UPLOAD=100M \
#   ASH_DIRECT_URL=https://dl.example.com \
#   ASH_DEFAULT_TTL=30d \
#     bash install.sh
: "${ASH_BASE_URL:=}"
: "${ASH_BEHIND_PROXY:=false}"
: "${ASH_MAX_UPLOAD:=}"
: "${ASH_PROXY_MAX_UPLOAD:=}"
: "${ASH_DIRECT_URL:=}"
: "${ASH_DEFAULT_TTL:=}"
: "${ASH_TRUSTED_PROXIES:=127.0.0.1/32,::1/128}"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

if [ ! -f "$BIN_SRC" ]; then
  echo "binary not found at $BIN_SRC (set BIN_SRC=/path/to/agentshare)" >&2
  exit 1
fi

# --- configuration file ---------------------------------------------------
# Written key by key and updated in place, so re-running this with different
# settings changes those settings and nothing else — most particularly not the
# root token, which every existing client is authenticating with. (An earlier
# version only wrote the file on first install, which meant the documented
# overrides below silently did nothing on a host that was already installed.)
touch "$ENV_FILE"
chmod 600 "$ENV_FILE"

set_env_var() { # KEY VALUE — replace that key's line, or append it; drop duplicates
    python3 - "$ENV_FILE" "$1" "$2" <<'PYHELPER'
import os, pathlib, sys
path, key, value = sys.argv[1], sys.argv[2], sys.argv[3]
p = pathlib.Path(path)
lines = p.read_text().splitlines() if p.exists() else []
out, seen = [], False
for line in lines:
    if line.startswith(key + "="):
        if seen:
            continue          # two values for one key is a silent coin flip
        out.append(key + "=" + value)
        seen = True
    else:
        out.append(line)
if not seen:
    out.append(key + "=" + value)
tmp = p.with_name(p.name + ".new")
tmp.write_text("\n".join(out) + "\n")
os.chmod(tmp, 0o600)
os.replace(tmp, p)            # atomic: an interrupted run cannot leave it half-written
PYHELPER
}

if [ ! -s "$ENV_FILE" ]; then
    cat > "$ENV_FILE" <<'HEADER'
# agentshare configuration. Keep this file out of version control and out of
# backups that other people can read: AGENTSHARE_ADMIN_TOKEN is the root
# credential.
HEADER
    chmod 600 "$ENV_FILE"
fi

EXISTING_TOKEN=""
if [ -f "$ENV_FILE" ]; then
    EXISTING_TOKEN="$(grep -oP '(?<=^AGENTSHARE_ADMIN_TOKEN=).*' "$ENV_FILE" || true)"
fi

GENERATED=0
if [ -n "$EXISTING_TOKEN" ]; then
    echo "keeping the existing root token in $ENV_FILE"
    TOKEN="$EXISTING_TOKEN"
else
    TOKEN="ash_$(head -c 32 /dev/urandom | base64 | tr -dc 'a-z0-9' | head -c 40)"
    GENERATED=1
    set_env_var AGENTSHARE_LISTEN 127.0.0.1:8080
    set_env_var AGENTSHARE_DATA_DIR "$STATE_DIR"
    set_env_var AGENTSHARE_TRUSTED_PROXIES "$ASH_TRUSTED_PROXIES"
fi
set_env_var AGENTSHARE_ADMIN_TOKEN "$TOKEN"

# Anything supplied on this run overrides what was already configured. Written
# as explicit ifs rather than `[ -n "$X" ] && set_env_var ...`: the latter reads
# as though an empty value could abort the script under `set -e`. It cannot
# (bash exempts everything but the last command of an AND-OR list) but that is a
# subtlety nobody should have to re-derive while reading a deploy script.
if [ -n "$ASH_BASE_URL" ]; then set_env_var AGENTSHARE_BASE_URL "$ASH_BASE_URL"; fi
if [ -n "$ASH_MAX_UPLOAD" ]; then set_env_var AGENTSHARE_MAX_UPLOAD "$ASH_MAX_UPLOAD"; fi
if [ -n "$ASH_PROXY_MAX_UPLOAD" ]; then set_env_var AGENTSHARE_PROXY_MAX_UPLOAD "$ASH_PROXY_MAX_UPLOAD"; fi
if [ -n "$ASH_DIRECT_URL" ]; then set_env_var AGENTSHARE_DIRECT_URL "$ASH_DIRECT_URL"; fi
if [ -n "$ASH_DEFAULT_TTL" ]; then set_env_var AGENTSHARE_DEFAULT_TTL "$ASH_DEFAULT_TTL"; fi
if [ "$ASH_BEHIND_PROXY" = "true" ]; then set_env_var AGENTSHARE_BEHIND_PROXY true; fi

if [ "$GENERATED" = 1 ]; then
    echo
    echo "  root token written to $ENV_FILE (mode 600):"
    echo "    $TOKEN"
    echo "  Mint narrower tokens for individual agents with POST /v1/tokens."
    echo
fi

# --- the service ----------------------------------------------------------
install -m 0755 "$BIN_SRC" "$BIN_DST"
install -m 0644 "$UNIT_SRC" "$UNIT_DST"

# The data directory is deliberately NOT created here. The unit uses
# DynamicUser= together with StateDirectory=, so systemd creates and owns it.
# Pre-creating it makes systemd find a pre-existing "public" directory and
# migrate it to /var/lib/private on first start, which works but moves the data
# and logs a scary-looking notice about it.

systemctl daemon-reload
# enable + restart rather than `enable --now`: --now only *starts* a unit that
# is not already running. On an upgrade the service is already up, so the new
# binary would be installed and the old process left serving it — a deploy that
# reports success and changes nothing at all.
systemctl enable agentshare.service >/dev/null
systemctl restart agentshare.service

PORT="$(grep -oE '^AGENTSHARE_LISTEN=.*' "$ENV_FILE" | cut -d= -f2- || true)"
PORT="${PORT##*:}"
PORT="${PORT:-8080}"

# Wait for the service to actually answer, rather than for systemd to call it
# active. Those are not the same moment: a unit is reported active as soon as
# it has exec'd the binary, which is well before the process has bound its
# listening socket. Checking the unit state here and then firing off a request
# races itself, and the loser looks exactly like a service that failed to
# start.
ready=0
for _ in $(seq 1 40); do
  if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.25
done
if [ "$ready" != "1" ]; then
  echo "agentshare did not answer on 127.0.0.1:$PORT; last 30 log lines:" >&2
  journalctl -u agentshare.service -n 30 --no-pager >&2
  exit 1
fi

echo "agentshare is ($(systemctl is-active agentshare.service)) and answering"
echo "  binary   $BIN_DST"
echo "  unit     $UNIT_DST"
echo "  data     $STATE_DIR (owned by a dynamic user; read the token as root)"
echo "  config   $ENV_FILE"
echo
echo "  GET /healthz -> $(curl -sS --max-time 5 "http://127.0.0.1:$PORT/healthz")"
