#!/usr/bin/env bash
# scripts/install.sh — thin wrapper that calls the canonical installer.
#
# The canonical, up-to-date installer lives at /install.sh in the repo root.
# This file exists only for backwards-compatibility with any links that point
# to scripts/install.sh.
#
# Usage (recommended — always use the root installer directly):
#   curl -fsSL https://raw.githubusercontent.com/cwash797-cmd/Project-XK/main/install.sh | sudo bash

set -euo pipefail

CANONICAL="https://raw.githubusercontent.com/cwash797-cmd/Project-XK/main/install.sh"

echo "[INFO] Redirecting to canonical installer: ${CANONICAL}"
exec bash <(curl -fsSL "${CANONICAL}") "$@"
