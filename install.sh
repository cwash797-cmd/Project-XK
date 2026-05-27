#!/usr/bin/env bash
# install.sh — Unattended install for Project-XK (ktalk-panel + ktalk-core)
# Run as root on Debian/Ubuntu 22.04+ or RHEL/Rocky/Alma 8+.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/cwash797-cmd/Project-XK/main/install.sh | bash
#   -- OR --
#   bash install.sh [--panel-port 8888] [--listen 127.0.0.1] [--no-caddy]
#
# What it does:
#   1. Detects OS family (apt / dnf / yum)
#   2. Installs Caddy (reverse proxy + automatic HTTPS) unless --no-caddy
#   3. Downloads latest ktalk-core + ktalk-panel binaries from GitHub Releases
#   4. Creates system user "xk" (no login shell)
#   5. Writes /etc/ktalk-panel/config.json (first-run defaults)
#   6. Installs systemd units for ktalk-panel (and caddy if needed)
#   7. Opens firewall ports 80 + 443 (ufw / firewalld)
#   8. Starts + enables services
#
# Environment overrides:
#   XK_VERSION      — release tag to install (default: latest)
#   XK_PANEL_PORT   — internal panel port (default: 8888)
#   XK_LISTEN_ADDR  — internal listen address (default: 127.0.0.1)
#   XK_INSTALL_DIR  — binary install directory (default: /usr/local/bin)
#   XK_CONFIG_DIR   — config directory (default: /etc/ktalk-panel)
#   XK_DATA_DIR     — data directory (default: /var/lib/ktalk-panel)
#   XK_NO_CADDY     — set to 1 to skip Caddy install
#   DOMAIN          — domain name for Caddy TLS (e.g. panel.example.com)
#   EMAIL           — email for Let's Encrypt notifications
#
# Flags:
#   --unattended    — skip interactive prompts (requires DOMAIN env var)

set -euo pipefail

# ─── Colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[ OK ]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()   { echo -e "${RED}[FAIL]${NC}  $*" >&2; exit 1; }

# ─── Defaults ─────────────────────────────────────────────────────────────────
XK_VERSION="${XK_VERSION:-latest}"
XK_PANEL_PORT="${XK_PANEL_PORT:-8888}"
XK_LISTEN_ADDR="${XK_LISTEN_ADDR:-127.0.0.1}"
XK_INSTALL_DIR="${XK_INSTALL_DIR:-/usr/local/bin}"
XK_CONFIG_DIR="${XK_CONFIG_DIR:-/etc/ktalk-panel}"
XK_DATA_DIR="${XK_DATA_DIR:-/var/lib/ktalk-panel}"
XK_NO_CADDY="${XK_NO_CADDY:-0}"
XK_DOMAIN="${DOMAIN:-}"
XK_EMAIL="${EMAIL:-}"
XK_UNATTENDED=0

# ─── Parse CLI flags ──────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case $1 in
        --panel-port)  XK_PANEL_PORT="$2"; shift 2 ;;
        --listen)      XK_LISTEN_ADDR="$2"; shift 2 ;;
        --no-caddy)    XK_NO_CADDY=1; shift ;;
        --version)     XK_VERSION="$2"; shift 2 ;;
        --domain)      XK_DOMAIN="$2"; shift 2 ;;
        --email)       XK_EMAIL="$2"; shift 2 ;;
        --unattended)  XK_UNATTENDED=1; shift ;;
        *) warn "unknown flag: $1"; shift ;;
    esac
done

# ─── Root check ───────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || die "This script must be run as root."

# ─── Domain / email prompt ────────────────────────────────────────────────────
if [[ "$XK_NO_CADDY" != "1" ]]; then
    if [[ -z "$XK_DOMAIN" && "$XK_UNATTENDED" == "1" ]]; then
        die "--unattended requires DOMAIN env var or --domain flag to be set."
    fi
    if [[ -z "$XK_DOMAIN" ]]; then
        read -rp "Enter your domain (e.g. panel.example.com): " XK_DOMAIN
        [[ -n "$XK_DOMAIN" ]] || die "Domain cannot be empty."
    fi
    if [[ -z "$XK_EMAIL" && "$XK_UNATTENDED" != "1" ]]; then
        read -rp "Enter email for Let's Encrypt notifications [optional, press Enter to skip]: " XK_EMAIL
    fi
    info "Domain: $XK_DOMAIN"
    [[ -n "$XK_EMAIL" ]] && info "Email:  $XK_EMAIL"

    # ─── DNS propagation check ────────────────────────────────────────────────
    info "Checking DNS propagation for $XK_DOMAIN..."
    SERVER_IP=$(curl -fsSL --max-time 5 https://ipinfo.io/ip 2>/dev/null \
                || hostname -I 2>/dev/null | awk '{print $1}' || true)
    DOMAIN_IP=$(dig +short "$XK_DOMAIN" 2>/dev/null | tail -1 || true)
    if [[ -z "$DOMAIN_IP" ]]; then
        warn "Cannot resolve $XK_DOMAIN — DNS may not have propagated yet."
        if [[ "$XK_UNATTENDED" != "1" ]]; then
            read -rp "Continue anyway? Caddy ACME will fail if domain does not resolve. [y/N] " CONFIRM
            [[ "$CONFIRM" == [yY]* ]] || die "Aborted by user."
        fi
    elif [[ -n "$SERVER_IP" && "$DOMAIN_IP" != "$SERVER_IP" ]]; then
        warn "$XK_DOMAIN resolves to $DOMAIN_IP but this server's IP is $SERVER_IP."
        warn "Caddy ACME (Let's Encrypt) will fail if the domain doesn't point here."
        if [[ "$XK_UNATTENDED" != "1" ]]; then
            read -rp "Continue anyway? [y/N] " CONFIRM
            [[ "$CONFIRM" == [yY]* ]] || die "Aborted by user."
        fi
    else
        ok "DNS: $XK_DOMAIN → $DOMAIN_IP"
    fi
fi

# ─── Detect arch ──────────────────────────────────────────────────────────────
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  ARCH_SUFFIX="linux-amd64" ;;
    aarch64) ARCH_SUFFIX="linux-arm64" ;;
    *) die "Unsupported architecture: $ARCH" ;;
esac
info "Architecture: $ARCH ($ARCH_SUFFIX)"

# ─── Detect OS ────────────────────────────────────────────────────────────────
PKG_MGR=""
if command -v apt-get &>/dev/null; then
    PKG_MGR="apt"
elif command -v dnf &>/dev/null; then
    PKG_MGR="dnf"
elif command -v yum &>/dev/null; then
    PKG_MGR="yum"
else
    die "No supported package manager found (apt/dnf/yum)."
fi
info "Package manager: $PKG_MGR"

# ─── Install system dependencies ──────────────────────────────────────────────
info "Installing system dependencies..."
case "$PKG_MGR" in
    apt)
        DEBIAN_FRONTEND=noninteractive apt-get update -qq
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
            curl wget ca-certificates gnupg lsb-release ufw dnsutils
        ;;
    dnf|yum)
        $PKG_MGR install -y -q curl wget ca-certificates firewalld bind-utils
        ;;
esac
ok "System dependencies installed"

# ─── Install Caddy ────────────────────────────────────────────────────────────
install_caddy() {
    info "Installing Caddy..."
    case "$PKG_MGR" in
        apt)
            curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
                | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
            curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
                | tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
            DEBIAN_FRONTEND=noninteractive apt-get update -qq
            DEBIAN_FRONTEND=noninteractive apt-get install -y -qq caddy
            ;;
        dnf|yum)
            $PKG_MGR install -y -q 'dnf-command(copr)' 2>/dev/null || true
            $PKG_MGR copr enable -y @caddy/caddy 2>/dev/null || \
                $PKG_MGR install -y -q \
                    "https://github.com/caddyserver/caddy/releases/latest/download/caddy_$(uname -r)_linux_${ARCH_SUFFIX}.rpm" || \
                warn "Caddy RPM install skipped — install manually from https://caddyserver.com/docs/install"
            ;;
    esac
    ok "Caddy installed"
}

[[ "$XK_NO_CADDY" == "1" ]] || install_caddy

# ─── Resolve latest release tag ───────────────────────────────────────────────
if [[ "$XK_VERSION" == "latest" ]]; then
    info "Resolving latest release..."
    XK_VERSION=$(curl -fsSL \
        https://api.github.com/repos/cwash797-cmd/Project-XK/releases/latest \
        | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": "\(.*\)".*/\1/')
    [[ -n "$XK_VERSION" ]] || die "Could not resolve latest version from GitHub API."
fi
info "Installing version: $XK_VERSION"

RELEASE_BASE="https://github.com/cwash797-cmd/Project-XK/releases/download/${XK_VERSION}"

# ─── Download binaries ────────────────────────────────────────────────────────
info "Downloading binaries..."
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

download() {
    local name="$1"
    local url="${RELEASE_BASE}/${name}"
    info "  → $url"
    curl -fsSL --retry 3 -o "${TMP}/${name}" "$url" || die "Failed to download $name"
}

download "ktalk-core-${ARCH_SUFFIX}"
download "ktalk-panel-${ARCH_SUFFIX}"

chmod +x "${TMP}/ktalk-core-${ARCH_SUFFIX}" "${TMP}/ktalk-panel-${ARCH_SUFFIX}"
install -m 0755 "${TMP}/ktalk-core-${ARCH_SUFFIX}"  "${XK_INSTALL_DIR}/ktalk-core"
install -m 0755 "${TMP}/ktalk-panel-${ARCH_SUFFIX}" "${XK_INSTALL_DIR}/ktalk-panel"
ok "Binaries installed to ${XK_INSTALL_DIR}"

# ─── Create system user ───────────────────────────────────────────────────────
if ! id "xk" &>/dev/null; then
    useradd --system --no-create-home --shell /usr/sbin/nologin xk
    ok "System user 'xk' created"
else
    info "System user 'xk' already exists"
fi

# ─── Create directories ───────────────────────────────────────────────────────
mkdir -p "${XK_CONFIG_DIR}" "${XK_DATA_DIR}"
chown xk:xk "${XK_CONFIG_DIR}" "${XK_DATA_DIR}"
chmod 750 "${XK_CONFIG_DIR}" "${XK_DATA_DIR}"
ok "Directories created"

# ─── Write default config (only if not exists) ────────────────────────────────
CONFIG_FILE="${XK_CONFIG_DIR}/config.json"
if [[ ! -f "$CONFIG_FILE" ]]; then
    cat > "$CONFIG_FILE" <<EOF
{
  "version": 1,
  "port": ${XK_PANEL_PORT},
  "listen_addr": "${XK_LISTEN_ADDR}",
  "clients": [],
  "admin_hash": ""
}
EOF
    chown xk:xk "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    ok "Config written to ${CONFIG_FILE}"
else
    info "Config already exists at ${CONFIG_FILE}, skipping"
fi

# ─── Write systemd unit for ktalk-panel ───────────────────────────────────────
info "Installing systemd unit for ktalk-panel..."
cat > /etc/systemd/system/ktalk-panel.service <<EOF
[Unit]
Description=Project-XK Admin Panel
Documentation=https://github.com/cwash797-cmd/Project-XK
After=network.target
Wants=network.target

[Service]
Type=simple
User=xk
Group=xk
ExecStart=${XK_INSTALL_DIR}/ktalk-panel -config ${CONFIG_FILE} -addr ${XK_LISTEN_ADDR} -port ${XK_PANEL_PORT}
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
LimitNOFILE=65536

# Hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=${XK_CONFIG_DIR} ${XK_DATA_DIR}
PrivateTmp=yes
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOF
ok "Systemd unit written"

# ─── Write Caddyfile (reverse proxy → panel) ──────────────────────────────────
if [[ "$XK_NO_CADDY" != "1" ]]; then
    CADDY_CONF=/etc/caddy/Caddyfile
    if [[ ! -f "$CADDY_CONF" ]] || ! grep -q "ktalk-panel" "$CADDY_CONF" 2>/dev/null; then
        # Build optional TLS directive
        TLS_DIRECTIVE=""
        if [[ -n "$XK_EMAIL" ]]; then
            TLS_DIRECTIVE="    tls ${XK_EMAIL}"
        fi
        cat > "$CADDY_CONF" <<EOF
# Project-XK — managed by install.sh
# Caddy will auto-obtain TLS from Let's Encrypt for ${XK_DOMAIN}.

${XK_DOMAIN} {
    reverse_proxy ${XK_LISTEN_ADDR}:${XK_PANEL_PORT}
${TLS_DIRECTIVE}

    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains; preload"
        X-Content-Type-Options "nosniff"
        X-Frame-Options "DENY"
    }

    log {
        output file /var/log/caddy/xk-access.log
        format json
    }
}
EOF
        ok "Caddyfile written to ${CADDY_CONF} for domain ${XK_DOMAIN}"
    else
        info "Caddyfile already configured, skipping"
    fi
fi

# ─── Firewall ─────────────────────────────────────────────────────────────────
info "Configuring firewall..."
if command -v ufw &>/dev/null; then
    ufw allow 80/tcp   comment 'HTTP (Caddy ACME)'  >/dev/null 2>&1 || true
    ufw allow 443/tcp  comment 'HTTPS (Caddy)'       >/dev/null 2>&1 || true
    ufw allow 443/udp  comment 'HTTP/3 (Caddy QUIC)' >/dev/null 2>&1 || true
    ufw --force enable >/dev/null 2>&1 || true
    ok "ufw rules applied"
elif command -v firewall-cmd &>/dev/null; then
    systemctl enable --now firewalld >/dev/null 2>&1 || true
    firewall-cmd --permanent --add-service=http  >/dev/null 2>&1 || true
    firewall-cmd --permanent --add-service=https >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
    ok "firewalld rules applied"
else
    warn "No firewall manager found — open ports 80 and 443 manually."
fi

# ─── Enable & start services ──────────────────────────────────────────────────
info "Enabling and starting services..."
systemctl daemon-reload

if [[ "$XK_NO_CADDY" != "1" ]] && systemctl list-unit-files caddy.service &>/dev/null; then
    systemctl enable caddy  >/dev/null 2>&1 || true
    systemctl restart caddy >/dev/null 2>&1 || true
    ok "Caddy enabled and (re)started"
fi

systemctl enable ktalk-panel  >/dev/null 2>&1
systemctl restart ktalk-panel >/dev/null 2>&1
ok "ktalk-panel enabled and started"

# ─── Post-install summary ─────────────────────────────────────────────────────
echo ""
echo -e "${GREEN}══════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Project-XK installed successfully!                  ${NC}"
echo -e "${GREEN}══════════════════════════════════════════════════════${NC}"
echo ""
echo "  Binaries:   ${XK_INSTALL_DIR}/ktalk-core"
echo "              ${XK_INSTALL_DIR}/ktalk-panel"
echo "  Config:     ${CONFIG_FILE}"
echo "  Service:    systemctl status ktalk-panel"
echo ""
if [[ "$XK_NO_CADDY" != "1" ]]; then
    echo -e "${YELLOW}  NEXT STEPS:${NC}"
    echo "  1. Open https://${XK_DOMAIN}/admin in your browser"
    echo "  2. Set your admin password on first login"
else
    echo -e "${YELLOW}  NEXT STEPS:${NC}"
    echo "  1. Configure a reverse proxy to ${XK_LISTEN_ADDR}:${XK_PANEL_PORT}"
    echo "  2. Open http://${XK_LISTEN_ADDR}:${XK_PANEL_PORT}/admin in your browser"
    echo "  3. Set your admin password on first login"
fi
echo ""
echo "  Logs:  journalctl -u ktalk-panel -f"
echo ""
