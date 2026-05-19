#!/bin/sh
# Pre-install: create system user if it doesn't exist
set -e
if ! id "xk" >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin xk
fi
