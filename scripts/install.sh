#!/bin/sh
# teploy-dash installer. Usage:
#   curl -sL https://raw.githubusercontent.com/useteploy/teploy-dash/main/scripts/install.sh | sh
#
# Flags (set as env vars):
#   TEPLOY_DASH_VERSION=v0.1.0       pin a release tag (default: latest)
#   TEPLOY_DASH_PREFIX=/usr/local/bin install location for the binary
#   TEPLOY_DASH_NO_SERVICE=1         skip creating the systemd unit
#
# The script is intentionally a POSIX shell (not bash) so it runs on minimal
# Alpine/Debian/macOS installs.

set -eu

TEPLOY_DASH_VERSION="${TEPLOY_DASH_VERSION:-latest}"
TEPLOY_DASH_PREFIX="${TEPLOY_DASH_PREFIX:-/usr/local/bin}"
TEPLOY_DASH_NO_SERVICE="${TEPLOY_DASH_NO_SERVICE:-}"
REPO="useteploy/teploy-dash"

#
# ─── helpers ────────────────────────────────────────────────────────────
#

log() { printf '\033[1;34m==\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m!!\033[0m %s\n' "$*" >&2; exit 1; }

require() {
  command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"
}

# A62: the prefix becomes part of the systemd unit's ExecStart, so it must be
# an absolute path of boring characters — a relative or crafted prefix
# produced a unit that started the wrong binary or none.
validate_prefix() {
  case "$TEPLOY_DASH_PREFIX" in
    /*) ;;
    *) die "TEPLOY_DASH_PREFIX must be an absolute path" ;;
  esac
  case "$TEPLOY_DASH_PREFIX" in
    *[!A-Za-z0-9_./-]*) die "TEPLOY_DASH_PREFIX contains unsupported characters" ;;
  esac
}

need_sudo() {
  if [ "$(id -u)" -ne 0 ]; then
    if command -v sudo >/dev/null 2>&1; then
      SUDO=sudo
    else
      die "run as root or install sudo"
    fi
  else
    SUDO=
  fi
}

#
# ─── detect platform ────────────────────────────────────────────────────
#

detect() {
  UNAME_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  UNAME_ARCH=$(uname -m)
  case "$UNAME_OS" in
    linux)  OS=linux ;;
    darwin) OS=darwin ;;
    *) die "unsupported OS: $UNAME_OS" ;;
  esac
  case "$UNAME_ARCH" in
    x86_64|amd64) ARCH=amd64; ARCHIVE_ARCH=x86_64 ;;
    aarch64|arm64) ARCH=arm64; ARCHIVE_ARCH=arm64 ;;
    *) die "unsupported arch: $UNAME_ARCH" ;;
  esac
  log "Detected platform: $OS/$ARCH"
}

#
# ─── fetch release ──────────────────────────────────────────────────────
#

fetch_url() {
  if [ "$TEPLOY_DASH_VERSION" = latest ]; then
    # F068: discovery gets the same fail/timeout discipline as asset
    # downloads, and the redirect must land on a release tag of THIS repo —
    # anything else (error page, off-repo redirect) must not become a tag
    # string that is later interpolated into download URLs.
    effective=$(curl -fsSL --proto '=https' --proto-redir '=https' \
      --connect-timeout 10 --max-time 30 --retry 2 \
      -o /dev/null -w '%{url_effective}' \
      "https://github.com/$REPO/releases/latest") \
      || die "could not resolve latest release"
    case "$effective" in
      "https://github.com/$REPO/releases/tag/"*) TAG=${effective##*/} ;;
      *) die "latest release did not resolve to a release tag" ;;
    esac
    [ -n "$TAG" ] || die "could not determine latest release"
  else
    TAG="$TEPLOY_DASH_VERSION"
  fi
  case "$TAG" in
    v[0-9]*) ;;
    *) die "expected a version release tag (vX.Y.Z)" ;;
  esac
  case "$TAG" in
    *[!A-Za-z0-9._+-]*) die "invalid release tag" ;;
  esac
  STRIPPED_TAG="${TAG#v}"
  URL="https://github.com/$REPO/releases/download/$TAG/teploy-dash_${STRIPPED_TAG}_${OS}_${ARCHIVE_ARCH}.tar.gz"
  log "Downloading $URL"
}

install_binary() {
  # A62: the destination directory is created before the install, and the
  # unit below references this same prefix.
  need_sudo
  $SUDO install -d -m 0755 "$TEPLOY_DASH_PREFIX"

  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT

  # A61: both assets come from the SAME resolved tag, and the archive is
  # verified against the release's checksum before anything is extracted —
  # verifying only the installer script (the documented bootstrap) said
  # nothing about the bytes it later installed. F001: the archive is saved
  # under its release filename so `sha256sum -c` verifies the file that was
  # actually downloaded (the previous teploy-dash.tar.gz name made every
  # normal release install fail at the checksum step).
  archive_file="$(basename "$URL")"
  curl -fL --proto '=https' --proto-redir '=https' \
    --connect-timeout 10 --max-time 180 --retry 2 \
    -o "$TMP/$archive_file" "$URL" || die "archive download failed"
  curl -fL --proto '=https' --proto-redir '=https' \
    --connect-timeout 10 --max-time 60 --retry 2 \
    -o "$TMP/checksums.txt" "https://github.com/$REPO/releases/download/$TAG/checksums.txt" \
    || die "checksum download failed"
  awk -v f="$archive_file" '$2 == f {print; n++} END {if (n != 1) exit 1}' \
    "$TMP/checksums.txt" > "$TMP/selected.sha256" || die "missing or duplicate checksum for $archive_file"
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$TMP" && sha256sum -c selected.sha256) || die "checksum mismatch"
  else
    require shasum
    (cd "$TMP" && shasum -a 256 -c selected.sha256) || die "checksum mismatch"
  fi
  tar -xzf "$TMP/$archive_file" -C "$TMP" teploy-dash
  [ -f "$TMP/teploy-dash" ] && [ ! -L "$TMP/teploy-dash" ] || die "archive missing teploy-dash binary"

  $SUDO install -m 0755 "$TMP/teploy-dash" "$TEPLOY_DASH_PREFIX/teploy-dash"
  log "Installed $TEPLOY_DASH_PREFIX/teploy-dash"
}

#
# ─── optional: systemd unit ─────────────────────────────────────────────
#

install_service() {
  if [ "$OS" != "linux" ]; then return 0; fi
  if [ -n "$TEPLOY_DASH_NO_SERVICE" ]; then return 0; fi
  if ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping service install"
    return 0
  fi

  need_sudo
  log "Creating teploy-dash service account and directories"
  if ! id -u teploy-dash >/dev/null 2>&1; then
    $SUDO useradd --system --home /var/lib/teploy-dash --shell /usr/sbin/nologin teploy-dash
  fi
  $SUDO mkdir -p /var/lib/teploy-dash /etc/teploy-dash
  $SUDO chown teploy-dash:teploy-dash /var/lib/teploy-dash

  if [ ! -f /etc/teploy-dash/teploy-dash.env ]; then
    # F068: the temp credential file is created only in the branch that
    # uses it — an existing service environment previously left an empty
    # mktemp file behind on every installer run.
    TMP_SERVICE_ENV=$(mktemp)
    PASS=$(head -c 12 /dev/urandom | base64 | tr -d '/+=' | cut -c1-16)
    # A62: credentials are created privately from the outset (umask 077 in
    # the installer's temp dir, installed 0600) — tee-then-chmod left a
    # world-readable window.
    umask 077
    printf 'TEPLOY_DASH_USER=admin\nTEPLOY_DASH_PASSWORD=%s\n' "$PASS" > "$TMP_SERVICE_ENV"
    $SUDO install -m 0600 -o root -g root "$TMP_SERVICE_ENV" /etc/teploy-dash/teploy-dash.env
    rm -f "$TMP_SERVICE_ENV"
    GENERATED_PASSWORD="$PASS"
  fi

  $SUDO tee /etc/systemd/system/teploy-dash.service >/dev/null <<UNIT
[Unit]
Description=Teploy Dash — self-hosted deployment dashboard with uptime monitoring
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=teploy-dash
Group=teploy-dash
WorkingDirectory=/var/lib/teploy-dash
EnvironmentFile=-/etc/teploy-dash/teploy-dash.env
ExecStart=${TEPLOY_DASH_PREFIX}/teploy-dash --port 3456 --host 127.0.0.1 --data /var/lib/teploy-dash
Restart=always
RestartSec=5s
# F023: the application's own shutdown sequence (HTTP drain 15s + a shared
# 120s worker/join budget + slack) can legitimately exceed 30s when a
# deploy or restore verification is in flight; SIGKILL mid-drain is exactly
# the data-loss path the graceful sequence exists to avoid. Keep the
# supervisor budget above the application budget.
TimeoutStopSec=180s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/teploy-dash
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=teploy-dash

[Install]
WantedBy=multi-user.target
UNIT

  $SUDO systemctl daemon-reload
  $SUDO systemctl enable teploy-dash.service >/dev/null 2>&1 || true
  log "Installed systemd unit: teploy-dash.service"
}

#
# ─── go ─────────────────────────────────────────────────────────────────
#

require curl
require tar
detect
validate_prefix
fetch_url
install_binary
install_service

echo
log "teploy-dash installed successfully."
echo
echo "  Binary:  $TEPLOY_DASH_PREFIX/teploy-dash"
if [ "$OS" = "linux" ] && [ -z "$TEPLOY_DASH_NO_SERVICE" ] && command -v systemctl >/dev/null 2>&1; then
  echo "  Service: systemctl start teploy-dash"
  echo "  Logs:    journalctl -u teploy-dash -f"
  if [ -n "${GENERATED_PASSWORD:-}" ]; then
    echo
    echo "  Initial admin password: $GENERATED_PASSWORD"
    echo "  (stored in /etc/teploy-dash/teploy-dash.env; it becomes the first stored"
    echo "   admin account on first start — rotate it from Settings > Security,"
    echo "   not by editing the env file)"
  fi
  echo
  echo "  By default the dashboard reads CLI deployment state from /deployments/."
  echo "  Override with --deployments /path or set up the teploy CLI on this host."
  echo
  # F068: the generated unit binds loopback only — printing the hostname URL
  # invited operators to an address nothing listens on.
  echo "  UI:      http://127.0.0.1:3456 (loopback only)"
  echo "  Remote access: SSH tunnel (ssh -L 3456:127.0.0.1:3456 <host>) or a"
  echo "  reverse proxy with explicit auth; do not remove the loopback bind"
  echo "  without one."
else
  echo "  Run:     TEPLOY_DASH_PASSWORD=\$(openssl rand -base64 24) teploy-dash"
  echo "  UI:      http://127.0.0.1:3456"
fi
echo
