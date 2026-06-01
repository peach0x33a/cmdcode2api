#!/usr/bin/env bash
set -euo pipefail

# ─────────────────────────────────────────────────────────
# cmdcode2api deploy script
#   git pull → go build → atomic replace → restart
# ─────────────────────────────────────────────────────────

FORCE=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --force)
      FORCE=true
      shift
      ;;
    *)
      break
      ;;
  esac
done

REPO_DIR="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
TARGET="${2:-/opt/cmdcode2api/cmdcode2api}"
SERVICE="${3:-cmdcode2api}"
BACKUP="${TARGET}.bak"

cd "$REPO_DIR"

echo "==> git pull"
git pull

echo "==> go vet"
go vet ./...

echo "==> go test"
go test ./...

echo "==> go build (${REPO_DIR}/cmd/cmdcode2api)"
BUILD_DIR=$(mktemp -d)
trap 'rm -rf "$BUILD_DIR"' EXIT
go build -o "$BUILD_DIR/cmdcode2api" ./cmd/cmdcode2api

if $FORCE && systemctl is-enabled "$SERVICE" &>/dev/null; then
  echo "==> stop ${SERVICE} (--force)"
  systemctl stop "$SERVICE"
  echo "    stopped"
fi

echo "==> install to ${TARGET}"
mkdir -p "$(dirname "$TARGET")"
if [ -f "$TARGET" ]; then
  cp "$TARGET" "$BACKUP"
  echo "    backed up existing binary to ${BACKUP}"
fi
cp "$BUILD_DIR/cmdcode2api" "$TARGET"

echo "==> restart ${SERVICE}"
if systemctl is-enabled "$SERVICE" &>/dev/null; then
  systemctl restart "$SERVICE"
  echo "    restarted"
  sleep 1
  systemctl status "$SERVICE" --no-pager
else
  echo "    service ${SERVICE} not found — skip restart"
fi

echo "==> done"
