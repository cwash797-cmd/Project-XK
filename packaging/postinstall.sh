#!/bin/sh
# Post-install: write default config if absent, reload systemd, enable service
set -e

CONFIG=/etc/ktalk-panel/config.json
DEFAULT=/etc/ktalk-panel/config.json.default

# Only write config on fresh install (not upgrade)
if [ ! -f "$CONFIG" ] && [ -f "$DEFAULT" ]; then
    cp "$DEFAULT" "$CONFIG"
    chown xk:xk "$CONFIG"
    chmod 600 "$CONFIG"
fi

# Fix ownership of data dir
chown -R xk:xk /var/lib/ktalk-panel || true

# Reload and enable systemd service
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl enable ktalk-panel
    echo ""
    echo "Project-XK installed. Start with:"
    echo "  systemctl start ktalk-panel"
    echo "  journalctl -u ktalk-panel -f"
fi
