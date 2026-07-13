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
INSTALL_TMP=""
ROLLBACK_TMP=""
cleanup() {
  rm -rf "$BUILD_DIR"
  if [ -n "$INSTALL_TMP" ]; then
    rm -f "$INSTALL_TMP"
  fi
  if [ -n "$ROLLBACK_TMP" ]; then
    rm -f "$ROLLBACK_TMP"
  fi
}
trap cleanup EXIT
go build -o "$BUILD_DIR/cmdcode2api" ./cmd/cmdcode2api

if $FORCE && systemctl is-enabled "$SERVICE" &>/dev/null; then
  echo "==> stop ${SERVICE} (--force)"
  systemctl stop "$SERVICE"
  echo "    stopped"
fi

echo "==> install to ${TARGET}"
TARGET_DIR=$(dirname "$TARGET")
mkdir -p "$TARGET_DIR"
if [ -f "$TARGET" ]; then
  cp -p "$TARGET" "$BACKUP"
  echo "    backed up existing binary to ${BACKUP}"
fi
INSTALL_TMP=$(mktemp "${TARGET}.new.XXXXXX")
install -m 0755 "$BUILD_DIR/cmdcode2api" "$INSTALL_TMP"
mv -f "$INSTALL_TMP" "$TARGET"
INSTALL_TMP=""

rollback() {
  if [ ! -f "$BACKUP" ]; then
    echo "    no backup available for rollback" >&2
    return
  fi

  echo "==> rollback ${TARGET}" >&2
  ROLLBACK_TMP=$(mktemp "${TARGET}.rollback.XXXXXX")
  cp -p "$BACKUP" "$ROLLBACK_TMP"
  mv -f "$ROLLBACK_TMP" "$TARGET"
  ROLLBACK_TMP=""
  systemctl restart "$SERVICE" || true
}

wait_for_service() {
  local attempt
  for ((attempt = 0; attempt < 10; attempt++)); do
    if systemctl is-active --quiet "$SERVICE"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

echo "==> restart ${SERVICE}"
if systemctl is-enabled "$SERVICE" &>/dev/null; then
  if ! systemctl restart "$SERVICE"; then
    rollback
    exit 1
  fi
  if ! wait_for_service; then
    echo "    service did not become active" >&2
    rollback
    exit 1
  fi
  echo "    restarted"
  systemctl status "$SERVICE" --no-pager || true
else
  echo "    service ${SERVICE} not found — skip restart"
fi

echo "==> done"
