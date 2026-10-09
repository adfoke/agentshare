#!/usr/bin/env bash
# Build for Linux and ship it to a host that already ran install.sh once.
#
#   HOST=root@1.2.3.4 ./deploy/deploy.sh
#   HOST=root@1.2.3.4 ARCH=arm64 ./deploy/deploy.sh
#
# The token, the data directory and the config file are not touched: this only
# replaces the binary and restarts the unit.
set -euo pipefail

HOST="${HOST:-}"
ARCH="${ARCH:-amd64}"
SSH_OPTS="${SSH_OPTS:-}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -z "$HOST" ]; then
  echo "usage: HOST=root@your.server $0" >&2
  exit 2
fi

case "$ARCH" in
  amd64 | arm64) ;;
  *) echo "ARCH must be amd64 or arm64" >&2; exit 2 ;;
esac

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

echo "building linux/$ARCH"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
  -ldflags "-s -w -X github.com/adfoke/agentshare/internal/version.Commit=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)" \
  -o "$STAGE/agentshare-linux-$ARCH" "$ROOT/cmd/agentshare"

cp "$ROOT/deploy/install.sh" "$ROOT/deploy/agentshare.service" "$STAGE/"

# One connection for the whole deploy: a loop of separate ssh invocations is
# what gets a host to ban an address half way through.
echo "shipping to $HOST"
# shellcheck disable=SC2086
tar -C "$STAGE" -czf - . | ssh $SSH_OPTS "$HOST" \
  'rm -rf /tmp/agentshare-deploy && mkdir -p /tmp/agentshare-deploy && tar -xzf - -C /tmp/agentshare-deploy && \
   bash /tmp/agentshare-deploy/install.sh && rm -rf /tmp/agentshare-deploy'

echo
echo "deployed. Tail the log with:"
echo "  ssh $HOST 'journalctl -u agentshare -f -n 50'"
