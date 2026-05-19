# ── Stage 1: build frontend ───────────────────────────────────────────────────
FROM node:20-alpine AS web-builder
WORKDIR /src
RUN corepack enable && corepack prepare pnpm@9 --activate
COPY ktalk-panel/web/package.json ktalk-panel/web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY ktalk-panel/web/ ./
RUN pnpm build

# ── Stage 2: build Go binaries ────────────────────────────────────────────────
FROM golang:1.22-alpine AS go-builder
WORKDIR /src

# Build ktalk-core
COPY ktalk-core/go.mod ktalk-core/go.sum ./ktalk-core/
RUN cd ktalk-core && go mod download

COPY ktalk-core/ ./ktalk-core/
RUN cd ktalk-core && \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/ktalk-core \
        ./cmd/ktalk-core

# Build ktalk-panel (needs pre-built web/dist embedded)
COPY ktalk-panel/go.mod ktalk-panel/go.sum ./ktalk-panel/
RUN cd ktalk-panel && go mod download

COPY ktalk-panel/ ./ktalk-panel/
# Inject the pre-built frontend
COPY --from=web-builder /src/dist ./ktalk-panel/web/dist/
RUN cd ktalk-panel && \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/ktalk-panel \
        ./cmd/ktalk-panel

# ── Stage 3: minimal runtime image ───────────────────────────────────────────
FROM alpine:3.19
LABEL org.opencontainers.image.source="https://github.com/cwash797-cmd/Project-XK"
LABEL org.opencontainers.image.licenses="MIT"

# ca-certs for TLS connections to XMPP/Jitsi servers
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -s /sbin/nologin xk

COPY --from=go-builder /out/ktalk-core  /usr/local/bin/ktalk-core
COPY --from=go-builder /out/ktalk-panel /usr/local/bin/ktalk-panel

# Config and data volumes
RUN mkdir -p /etc/ktalk-panel /var/lib/ktalk-panel && \
    chown xk:xk /var/lib/ktalk-panel

VOLUME ["/etc/ktalk-panel", "/var/lib/ktalk-panel"]

USER xk

# ktalk-panel serves on 8888 by default; override with -port flag
EXPOSE 8888

# Default: run ktalk-panel. Override CMD to run ktalk-core instead.
# Example: docker run ... ktalk-core -config /etc/ktalk-panel/core.yaml serve
ENTRYPOINT ["/usr/local/bin/ktalk-panel"]
CMD ["-config", "/etc/ktalk-panel/config.json"]
