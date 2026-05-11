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
  if [ "$TEPLOY_DASH_VERSION" = "latest" ]; then
    TAG=$(curl -sL -o /dev/null -w '%{url_effective}' \
      "https://github.com/$REPO/releases/latest" \
      | sed 's#.*/tag/##')
    [ -n "$TAG" ] || die "could not determine latest release"
  else
    TAG="$TEPLOY_DASH_VERSION"
  fi
  STRIPPED_TAG="${TAG#v}"
  URL="https://github.com/$REPO/releases/download/$TAG/teploy-dash_${STRIPPED_TAG}_${OS}_${ARCHIVE_ARCH}.tar.gz"
  log "Downloading $URL"
}

install_binary() {
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT

  curl -fL -o "$TMP/teploy-dash.tar.gz" "$URL" || die "download failed"
  tar -xzf "$TMP/teploy-dash.tar.gz" -C "$TMP"
  [ -f "$TMP/teploy-dash" ] || die "archive missing teploy-dash binary"

  need_sudo
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
    PASS=$(head -c 12 /dev/urandom | base64 | tr -d '/+=' | cut -c1-16)
    $SUDO tee /etc/teploy-dash/teploy-dash.env >/dev/null <<EOF
TEPLOY_DASH_USER=admin
TEPLOY_DASH_PASSWORD=$PASS
EOF
    $SUDO chmod 0600 /etc/teploy-dash/teploy-dash.env
    $SUDO chown teploy-dash:teploy-dash /etc/teploy-dash/teploy-dash.env
    GENERATED_PASSWORD="$PASS"
  fi

  $SUDO tee /etc/systemd/system/teploy-dash.service >/dev/null <<'UNIT'
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
ExecStart=/usr/local/bin/teploy-dash --port 3456 --host 0.0.0.0 --data /var/lib/teploy-dash
Restart=always
RestartSec=5s
TimeoutStopSec=30s
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
    echo "  (stored in /etc/teploy-dash/teploy-dash.env — rotate by editing the file"
    echo "   and running: systemctl restart teploy-dash)"
  fi
  echo
  echo "  By default the dashboard reads CLI deployment state from /deployments/."
  echo "  Override with --deployments /path or set up the teploy CLI on this host."
else
  echo "  Run:     TEPLOY_DASH_PASSWORD=\$(openssl rand -base64 24) teploy-dash"
fi
echo
echo "  UI:      http://$(hostname):3456"
