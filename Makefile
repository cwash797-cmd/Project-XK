# Makefile for ktalk monorepo
# Targets: build, test, lint, clean, web, package, docker
#
# Prerequisites: Go 1.22+, pnpm, gofmt, staticcheck
# Packaging:     nfpm (go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest)
# Docker:        docker buildx

.PHONY: all build build-core build-panel build-linux-amd64 build-linux-arm64 \
        test test-core test-panel lint web clean \
        package-deb package-rpm package package-arm64 \
        docker docker-push

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

# ─── Packaging (nfpm) ────────────────────────────────────────────────────────

XK_VERSION ?= dev
NFPM       ?= nfpm

# .deb for amd64 (requires dist/ktalk-core-linux-amd64 and dist/ktalk-panel-linux-amd64)
package-deb: build-linux-amd64
	@mkdir -p dist
	XK_VERSION=$(XK_VERSION) $(NFPM) package \
		--config nfpm.yaml --packager deb --target dist/

# .rpm for amd64
package-rpm: build-linux-amd64
	@mkdir -p dist
	XK_VERSION=$(XK_VERSION) $(NFPM) package \
		--config nfpm.yaml --packager rpm --target dist/

# Both formats
package: package-deb package-rpm

# arm64 packages — override arch in nfpm on the fly via env
package-arm64: build-linux-arm64
	@mkdir -p dist
	XK_VERSION=$(XK_VERSION) \
	  sed 's/^arch: "amd64"/arch: "arm64"/' nfpm.yaml > /tmp/nfpm-arm64.yaml && \
	  sed -i 's/linux-amd64/linux-arm64/g' /tmp/nfpm-arm64.yaml && \
	  $(NFPM) package --config /tmp/nfpm-arm64.yaml --packager deb --target dist/ && \
	  $(NFPM) package --config /tmp/nfpm-arm64.yaml --packager rpm --target dist/

# ─── Docker ───────────────────────────────────────────────────────────────────

DOCKER_IMAGE ?= ghcr.io/cwash797-cmd/project-xk

docker:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--tag $(DOCKER_IMAGE):$(XK_VERSION) \
		--tag $(DOCKER_IMAGE):latest \
		--load \
		.

docker-push:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--tag $(DOCKER_IMAGE):$(XK_VERSION) \
		--tag $(DOCKER_IMAGE):latest \
		--push \
		.

# ─── Clean ───────────────────────────────────────────────────────────────────

clean:
	rm -rf dist
	rm -rf $(WEB_DIR)/dist $(WEB_DIR)/node_modules $(WEB_DIR)/.svelte-kit
