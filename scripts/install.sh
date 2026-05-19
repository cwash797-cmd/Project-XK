#!/usr/bin/env bash
# install.sh — ktalk-panel server installer
#
# Usage:
#   curl -fsSL https://your-release-server/install.sh | sudo DOMAIN=panel.example.com bash
#   sudo ./install.sh
#   sudo ./install.sh --upgrade
#   sudo ./install.sh --uninstall
#
# Environment variables:
#   DOMAIN        FQDN for TLS (required for automatic cert)
#   EMAIL         Let's Encrypt contact email
#   PANEL_PORT    Internal HTTP port (default: 8888)
#   BIND_ADDRESS  Panel listen address (default: 127.0.0.1)
#   RELEASE_URL   Base URL of the release server

set -euo pipefail

# ── colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'
BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${BLUE}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}   $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
die()   { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ── defaults ─────────────────────────────────────────────────────────────────
DOMAIN="${DOMAIN:-}"
EMAIL="${EMAIL:-}"
PANEL_PORT="${PANEL_PORT:-8888}"
BIND_ADDRESS="${BIND_ADDRESS:-127.0.0.1}"
RELEASE_URL="${RELEASE_URL:-https://releases.example.com/ktalk}"
CONFIG_DIR="/etc/ktalk-panel"
CONFIG_FILE="${CONFIG_DIR}/config.json"
BIN_DIR="/usr/local/bin"
SYSTEMD_DIR="/etc/systemd/system"
SERVICE_NAME="ktalk-panel"
CADDY_CONF="/etc/caddy/Caddyfile"

# ── argument parsing ─────────────────────────────────────────────────────────
MODE="install"
for arg in "$@"; do
  case "$arg" in
    --upgrade)   MODE="upgrade" ;;
    --uninstall) MODE="uninstall" ;;
    --help|-h)
      echo "Usage: $0 [--upgrade|--uninstall]"
      exit 0
      ;;
  esac
done

# ── pre-flight checks ─────────────────────────────────────────────────────────
check_root() {
  [[ "$EUID" -eq 0 ]] || die "This script must be run as root"
}

check_os() {
  if [[ -f /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    case "$ID" in
      debian)
        [[ "${VERSION_ID:-0}" -ge 11 ]] || die "Debian 11+ required (got $VERSION_ID)"
        ;;
      ubuntu)
        major="${VERSION_ID%%.*}"
        [[ "$major" -ge 22 ]] || die "Ubuntu 22.04+ required (got $VERSION_ID)"
        ;;
      *)
        die "Unsupported OS: $ID. Supported: Debian 11+, Ubuntu 22.04+"
        ;;
    esac
    ok "OS: $PRETTY_NAME"
  else
    die "/etc/os-release not found"
  fi
}

# ── uninstall ─────────────────────────────────────────────────────────────────
do_uninstall() {
  info "Uninstalling ktalk-panel…"

  systemctl stop "$SERVICE_NAME"   2>/dev/null || true
  systemctl disable "$SERVICE_NAME" 2>/dev/null || true
  rm -f "${SYSTEMD_DIR}/${SERVICE_NAME}.service"
  systemctl daemon-reload 2>/dev/null || true

  rm -f "${BIN_DIR}/ktalk-panel" "${BIN_DIR}/ktalk-core"

  # Remove Caddy config block if present
  if [[ -f "$CADDY_CONF" ]] && grep -q "ktalk-panel" "$CADDY_CONF" 2>/dev/null; then
    warn "Caddy config at $CADDY_CONF may still contain a ktalk block — please review manually."
  fi

  warn "Config directory $CONFIG_DIR was NOT removed. Remove manually if desired:"
  warn "  rm -rf $CONFIG_DIR"

  ok "Uninstall complete."
}

# ── install packages ──────────────────────────────────────────────────────────
install_packages() {
  info "Updating package lists…"
  apt-get update -qq

  info "Installing dependencies…"
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    curl ca-certificates iproute2 iptables

  # Install Caddy
  if ! command -v caddy &>/dev/null; then
    info "Installing Caddy…"
    curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key \
      | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    echo "deb [signed-by=/usr/share/keyrings/caddy-stable-archive-keyring.gpg] \
https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version main" \
      > /etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq caddy
    ok "Caddy installed"
  else
    ok "Caddy already installed: $(caddy version)"
  fi
}

# ── download binaries ─────────────────────────────────────────────────────────
download_binaries() {
  info "Downloading ktalk binaries from ${RELEASE_URL}…"

  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64)  ARCH_SUFFIX="amd64" ;;
    aarch64) ARCH_SUFFIX="arm64" ;;
    *)       die "Unsupported architecture: $ARCH" ;;
  esac

  for bin in ktalk-core ktalk-panel; do
    url="${RELEASE_URL}/linux-${ARCH_SUFFIX}/${bin}"
    info "  Downloading ${bin}…"
    curl -fsSL -o "${BIN_DIR}/${bin}" "$url" \
      || die "Failed to download ${bin} from ${url}"
    chmod 0755 "${BIN_DIR}/${bin}"
    ok "  ${bin} → ${BIN_DIR}/${bin}"
  done
}

# ── create config ─────────────────────────────────────────────────────────────
create_config() {
  mkdir -p "$CONFIG_DIR"
  chmod 750 "$CONFIG_DIR"

  if [[ -f "$CONFIG_FILE" ]]; then
    ok "Config already exists at $CONFIG_FILE — preserving."
    return
  fi

  info "Creating initial config at $CONFIG_FILE…"
  cat > "$CONFIG_FILE" <<EOF
{
  "version": 1,
  "port": ${PANEL_PORT},
  "listen_addr": "${BIND_ADDRESS}",
  "clients": []
}
EOF
  chmod 600 "$CONFIG_FILE"
  ok "Config created."
}

# ── systemd service ───────────────────────────────────────────────────────────
create_systemd_service() {
  SERVICE_FILE="${SYSTEMD_DIR}/${SERVICE_NAME}.service"

  info "Writing systemd service to ${SERVICE_FILE}…"
  cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=ktalk Panel — private relay admin panel
After=network.target

[Service]
Type=simple
ExecStart=${BIN_DIR}/ktalk-panel -config ${CONFIG_FILE} -addr ${BIND_ADDRESS} -port ${PANEL_PORT}
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=ktalk-panel
# Security hardening
NoNewPrivileges=false
ProtectSystem=false
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable --quiet "$SERVICE_NAME"
  ok "systemd service created and enabled."
}

# ── caddy config ──────────────────────────────────────────────────────────────
configure_caddy() {
  if [[ -z "$DOMAIN" ]]; then
    warn "DOMAIN not set — skipping Caddy TLS configuration."
    warn "Panel will be reachable on http://${BIND_ADDRESS}:${PANEL_PORT} only."
    return
  fi

  # Check if this domain block already exists
  if grep -q "\"$DOMAIN\"" "$CADDY_CONF" 2>/dev/null || grep -q "$DOMAIN" "$CADDY_CONF" 2>/dev/null; then
    info "Caddy already configured for $DOMAIN — skipping."
    return
  fi

  info "Configuring Caddy reverse proxy for $DOMAIN…"

  # Append to Caddyfile
  if [[ -n "$EMAIL" ]]; then
    EMAIL_BLOCK="email ${EMAIL}"
  else
    EMAIL_BLOCK=""
  fi

  cat >> "$CADDY_CONF" <<EOF

# ktalk-panel
${DOMAIN} {
    ${EMAIL_BLOCK}
    reverse_proxy ${BIND_ADDRESS}:${PANEL_PORT}
    encode gzip
    header {
        -Server
        X-Content-Type-Options nosniff
        X-Frame-Options DENY
    }
}
EOF

  caddy validate --config "$CADDY_CONF" &>/dev/null \
    || { warn "Caddy config validation failed — check $CADDY_CONF"; return; }

  systemctl reload caddy 2>/dev/null || systemctl restart caddy
  ok "Caddy configured for https://${DOMAIN}"
}

# ── start service ─────────────────────────────────────────────────────────────
start_service() {
  info "Starting ${SERVICE_NAME}…"
  systemctl restart "$SERVICE_NAME"

  # Wait for service to be up
  for i in $(seq 1 10); do
    if systemctl is-active --quiet "$SERVICE_NAME"; then
      ok "${SERVICE_NAME} is running."
      return
    fi
    sleep 1
  done

  warn "${SERVICE_NAME} did not start in 10s — check: journalctl -u ${SERVICE_NAME} -n 50"
}

# ── banner ────────────────────────────────────────────────────────────────────
print_banner() {
  echo ""
  echo -e "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
  echo -e "${BOLD}   ktalk-panel installed successfully${NC}"
  echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
  echo ""
  if [[ -n "$DOMAIN" ]]; then
    echo -e "  🌐 Open ${BOLD}https://${DOMAIN}/setup${NC} in your browser"
    echo -e "     to set your admin password."
  else
    echo -e "  🌐 Open ${BOLD}http://localhost:${PANEL_PORT}/setup${NC} in your browser"
    echo -e "     to set your admin password."
    echo ""
    echo -e "  ℹ️  Set DOMAIN= to enable automatic TLS:"
    echo -e "     DOMAIN=panel.example.com ./install.sh"
  fi
  echo ""
  echo -e "  📋 Config: ${CONFIG_FILE}"
  echo -e "  📋 Logs:   journalctl -u ${SERVICE_NAME} -f"
  echo ""
}

# ── main ──────────────────────────────────────────────────────────────────────
main() {
  echo -e "${BOLD}ktalk-panel installer${NC} (mode: ${MODE})"
  echo ""

  check_root
  check_os

  case "$MODE" in
    uninstall)
      do_uninstall
      exit 0
      ;;
    upgrade)
      info "Upgrade mode: downloading new binaries and restarting."
      download_binaries
      systemctl restart "$SERVICE_NAME" || true
      ok "Upgrade complete."
      exit 0
      ;;
    install)
      install_packages
      download_binaries
      create_config
      create_systemd_service
      configure_caddy
      start_service
      print_banner
      ;;
  esac
}

main "$@"
