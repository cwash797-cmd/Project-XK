# Makefile for ktalk monorepo
# Targets: build, test, lint, clean, web
#
# Prerequisites: Go 1.22+, pnpm, gofmt, staticcheck

.PHONY: all build build-core build-panel test test-core test-panel lint web clean

GOFLAGS := -trimpath -ldflags="-s -w"
CORE_DIR := ktalk-core
PANEL_DIR := ktalk-panel
WEB_DIR   := ktalk-panel/web

all: build

# ─── Build ───────────────────────────────────────────────────────────────────

build: build-core build-panel

build-core:
	@echo "Building ktalk-core…"
	cd $(CORE_DIR) && go build $(GOFLAGS) -o ../dist/ktalk-core ./cmd/ktalk-core

build-panel: web
	@echo "Building ktalk-panel…"
	cd $(PANEL_DIR) && go build $(GOFLAGS) -o ../dist/ktalk-panel ./cmd/ktalk-panel

# Cross-compile for Linux amd64 (release artifact)
build-linux-amd64:
	@mkdir -p dist
	cd $(CORE_DIR) && GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o ../dist/ktalk-core-linux-amd64 ./cmd/ktalk-core
	cd $(PANEL_DIR) && GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o ../dist/ktalk-panel-linux-amd64 ./cmd/ktalk-panel

build-linux-arm64:
	@mkdir -p dist
	cd $(CORE_DIR) && GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o ../dist/ktalk-core-linux-arm64 ./cmd/ktalk-core
	cd $(PANEL_DIR) && GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o ../dist/ktalk-panel-linux-arm64 ./cmd/ktalk-panel

# ─── Frontend ────────────────────────────────────────────────────────────────

web:
	@echo "Building SvelteKit frontend…"
	cd $(WEB_DIR) && pnpm install --frozen-lockfile
	cd $(WEB_DIR) && pnpm build
	@# The built files land in ktalk-panel/web/dist which is embedded by Go.

web-dev:
	cd $(WEB_DIR) && pnpm dev

# ─── Test ─────────────────────────────────────────────────────────────────────

test: test-core test-panel

test-core:
	@echo "Testing ktalk-core…"
	cd $(CORE_DIR) && go test ./...

test-panel:
	@echo "Testing ktalk-panel…"
	cd $(PANEL_DIR) && go test ./...

# ─── Lint ─────────────────────────────────────────────────────────────────────

lint:
	@echo "Linting Go…"
	cd $(CORE_DIR)  && gofmt -l . | tee /dev/stderr | (! grep .)
	cd $(PANEL_DIR) && gofmt -l . | tee /dev/stderr | (! grep .)
	@if command -v staticcheck >/dev/null; then \
	  cd $(CORE_DIR)  && staticcheck ./...; \
	  cd $(PANEL_DIR) && staticcheck ./...; \
	fi
	@echo "Linting frontend…"
	cd $(WEB_DIR) && pnpm lint || true

fmt:
	cd $(CORE_DIR)  && gofmt -w .
	cd $(PANEL_DIR) && gofmt -w .
	cd $(WEB_DIR)   && pnpm format || true

# ─── Clean ───────────────────────────────────────────────────────────────────

clean:
	rm -rf dist
	rm -rf $(WEB_DIR)/dist $(WEB_DIR)/node_modules $(WEB_DIR)/.svelte-kit
