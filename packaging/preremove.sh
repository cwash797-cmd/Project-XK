#!/bin/sh
# Pre-remove: stop and disable service before removal
set -e
if command -v systemctl >/dev/null 2>&1; then
    systemctl stop    ktalk-panel 2>/dev/null || true
    systemctl disable ktalk-panel 2>/dev/null || true
fi
