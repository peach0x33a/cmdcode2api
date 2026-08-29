#!/usr/bin/env bash
set -euo pipefail

# ─────────────────────────────────────────────────────────
# cmdcode2api installer / updater
#
#   Fresh install : download the latest release binary, create a service
#                   user, write a systemd unit, enable + start it.
#   --update      : replace the installed binary with the latest release
#                   (or --version TAG) and restart the service, with
#                   automatic rollback if it fails to come back up.
#   --os-upgrade  : run a full OS package upgrade first (apt/dnf/pacman/zypper).
#
# No Go toolchain required — this pulls prebuilt archives from GitHub
# Releases. For a source build + deploy from a checkout, use deploy.sh.
# ─────────────────────────────────────────────────────────

REPO="${CMDCODE2API_REPO:-FlightlessWeasel/cmdcode2api}"
DIR="/opt/cmdcode2api"
SERVICE="cmdcode2api"
SVC_USER="cmdcode2api"
HOST="0.0.0.0"
VERSION=""          # empty => latest
DO_UPDATE=false
DO_OS_UPGRADE=false
NO_START=false
FORCE=false

usage() {
  cat <<EOF
Usage: install.sh [options]

  --update            Update an existing install to the latest release, then restart.
  --os-upgrade        Upgrade all OS packages first (apt / dnf / pacman / zypper).
  --version TAG       Install/update to a specific release tag (e.g. v1.2.3). Default: latest.
  --repo OWNER/NAME   GitHub repo to fetch releases from. Default: ${REPO}.
  --dir PATH          Install directory. Default: ${DIR}.
  --service NAME      systemd service name. Default: ${SERVICE}.
  --user NAME         Service account to create and run as. Default: ${SVC_USER}.
  --host HOST         Listen host passed to the binary. Default: ${HOST}.
  --no-start          Install and enable the unit but do not start it now.
  --force             Reinstall / rewrite the unit even if already at the target version.
  -h, --help          Show this help.

Environment:
  CMDCODE2API_REPO   Same as --repo.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --update)       DO_UPDATE=true; shift ;;
    --os-upgrade)   DO_OS_UPGRADE=true; shift ;;
    --version)      VERSION="${2:?--version needs a tag}"; shift 2 ;;
    --repo)         REPO="${2:?--repo needs owner/name}"; shift 2 ;;
    --dir)          DIR="${2:?--dir needs a path}"; shift 2 ;;
    --service)      SERVICE="${2:?--service needs a name}"; shift 2 ;;
    --user)         SVC_USER="${2:?--user needs a name}"; shift 2 ;;
    --host)         HOST="${2:?--host needs a value}"; shift 2 ;;
    --no-start)     NO_START=true; shift ;;
    --force)        FORCE=true; shift ;;
    -h|--help)      usage; exit 0 ;;
    *)              echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

TARGET="${DIR}/cmdcode2api"
BACKUP="${TARGET}.bak"
VERSION_FILE="${DIR}/.version"
UNIT_FILE="/etc/systemd/system/${SERVICE}.service"

die()  { echo "error: $*" >&2; exit 1; }
info() { echo "==> $*"; }

[[ "$(id -u)" -eq 0 ]] || die "must run as root (try: sudo $0 $*)"

for bin in curl tar sha256sum systemctl; do
  command -v "$bin" >/dev/null 2>&1 || die "required command not found: $bin"
done

# ── OS package upgrade ───────────────────────────────────
os_upgrade() {
  info "upgrading OS packages"
  if   command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get -y upgrade
  elif command -v dnf >/dev/null 2>&1; then
    dnf -y upgrade
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Syu --noconfirm
  elif command -v zypper >/dev/null 2>&1; then
    zypper --non-interactive update
  else
    die "no supported package manager found (apt/dnf/pacman/zypper)"
  fi
}

# ── arch detection ──────────────────────────────────────
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)   echo amd64 ;;
    aarch64|arm64)  echo arm64 ;;
    *)              die "unsupported architecture: $(uname -m)" ;;
  esac
}

# ── release resolution ──────────────────────────────────
latest_tag() {
  # Parse tag_name from the releases/latest API without requiring jq.
  local api="https://api.github.com/repos/${REPO}/releases/latest"
  local hdr=(-fsSL)
  [[ -n "${GITHUB_TOKEN:-}" ]] && hdr+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  curl "${hdr[@]}" "$api" \
    | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name" *: *"([^"]+)".*/\1/'
}

# download_release <tag> <arch> <destdir> -> sets $EXTRACTED to the binary path
download_release() {
  local tag="$1" arch="$2" dest="$3"
  local asset="cmdcode2api_${tag}_linux_${arch}.tar.gz"
  local base="https://github.com/${REPO}/releases/download/${tag}"

  info "downloading ${asset}"
  curl -fSL -o "${dest}/${asset}" "${base}/${asset}" \
    || die "download failed: ${base}/${asset}"

  if curl -fsSL -o "${dest}/SHA256SUMS.txt" "${base}/SHA256SUMS.txt"; then
    info "verifying checksum"
    ( cd "$dest" && grep " ${asset}\$" SHA256SUMS.txt | sha256sum -c - ) \
      || die "checksum verification failed for ${asset}"
  else
    echo "    (no SHA256SUMS.txt in release — skipping checksum verify)" >&2
  fi

  tar -C "$dest" -xzf "${dest}/${asset}"
  [[ -f "${dest}/cmdcode2api" ]] || die "archive did not contain 'cmdcode2api'"
  EXTRACTED="${dest}/cmdcode2api"
}

# ── systemd unit ────────────────────────────────────────
write_unit() {
  info "writing ${UNIT_FILE}"
  cat > "$UNIT_FILE" <<EOF
[Unit]
Description=cmdcode2api — OpenAI-compatible gateway for Command Code
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SVC_USER}
WorkingDirectory=${DIR}
ExecStart=${TARGET} --host ${HOST}
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ReadWritePaths=${DIR}

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

ensure_user() {
  if ! id "$SVC_USER" >/dev/null 2>&1; then
    info "creating service user ${SVC_USER}"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
  fi
}

install_binary() {
  local src="$1"
  mkdir -p "$DIR"
  if [[ -f "$TARGET" ]]; then
    cp -p "$TARGET" "$BACKUP"
    echo "    backed up existing binary to ${BACKUP}"
  fi
  local tmp
  tmp="$(mktemp "${TARGET}.new.XXXXXX")"
  install -m 0755 "$src" "$tmp"
  mv -f "$tmp" "$TARGET"
}

rollback_binary() {
  [[ -f "$BACKUP" ]] || { echo "    no backup to roll back to" >&2; return; }
  info "rolling back ${TARGET}"
  local tmp
  tmp="$(mktemp "${TARGET}.rollback.XXXXXX")"
  cp -p "$BACKUP" "$tmp"
  mv -f "$tmp" "$TARGET"
  systemctl restart "$SERVICE" || true
}

wait_active() {
  local i
  for ((i = 0; i < 10; i++)); do
    systemctl is-active --quiet "$SERVICE" && return 0
    sleep 1
  done
  return 1
}

# ── main ────────────────────────────────────────────────
$DO_OS_UPGRADE && os_upgrade

ARCH="$(detect_arch)"

TAG="$VERSION"
if [[ -z "$TAG" ]]; then
  info "resolving latest release for ${REPO}"
  TAG="$(latest_tag)"
  [[ -n "$TAG" ]] || die "could not determine latest release tag"
fi
info "target version: ${TAG} (linux/${ARCH})"

CURRENT=""
[[ -f "$VERSION_FILE" ]] && CURRENT="$(cat "$VERSION_FILE")"

if [[ "$TAG" == "$CURRENT" && "$FORCE" == false ]]; then
  echo "already at ${TAG} — nothing to do (use --force to reinstall)"
  exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
EXTRACTED=""
download_release "$TAG" "$ARCH" "$WORK"

if $DO_UPDATE; then
  [[ -f "$TARGET" ]] || die "--update: no existing install at ${TARGET} (run without --update first)"
  install_binary "$EXTRACTED"
  echo "$TAG" > "$VERSION_FILE"
  if systemctl is-enabled "$SERVICE" >/dev/null 2>&1; then
    info "restarting ${SERVICE}"
    if ! systemctl restart "$SERVICE" || ! wait_active; then
      echo "    service did not come back up" >&2
      rollback_binary
      echo "$CURRENT" > "$VERSION_FILE"
      die "update failed — rolled back to previous binary"
    fi
    echo "    restarted on ${TAG}"
  else
    echo "    service ${SERVICE} not enabled — binary updated, start it yourself"
  fi
  info "done"
  exit 0
fi

# Fresh install (or --force reinstall).
ensure_user
install_binary "$EXTRACTED"
echo "$TAG" > "$VERSION_FILE"
chown -R "$SVC_USER":"$SVC_USER" "$DIR"

if [[ ! -f "$UNIT_FILE" || "$FORCE" == true ]]; then
  write_unit
else
  echo "    ${UNIT_FILE} already exists — leaving it untouched (use --force to rewrite)"
fi

systemctl enable "$SERVICE" >/dev/null

if $NO_START; then
  echo "    --no-start: not starting ${SERVICE}"
else
  info "starting ${SERVICE}"
  systemctl restart "$SERVICE"
  if wait_active; then
    echo "    active on ${TAG}"
  else
    systemctl status "$SERVICE" --no-pager || true
    die "service failed to start — check: journalctl -u ${SERVICE} -e"
  fi
fi

if [[ ! -f "${DIR}/config.yaml" ]]; then
  cat <<EOF

Next step: no ${DIR}/config.yaml yet. Create one, then authorize an account:

  sudo -u ${SVC_USER} ${TARGET} --oauth --account Default
  sudo systemctl restart ${SERVICE}
EOF
fi

info "done"
