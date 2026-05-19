# ktalk — private relay over Kontour Talk

Internal monorepo for the server-side components.

## Repository layout

```
ktalk/
├── ktalk-core/          Go module — tunnel process (one per client)
│   ├── cmd/ktalk-core/  main binary
│   ├── internal/
│   │   ├── names/       Russian first-name generator
│   │   ├── config/      URI / YAML config, subscription protocol
│   │   ├── crypto/      ChaCha20-Poly1305 AEAD
│   │   ├── muxer/       DataChannel framing + multiplexer
│   │   ├── xmpp/        XMPP-over-WebSocket client (Jitsi/Prosody)
│   │   ├── jingle/      Jingle ↔ SDP conversion
│   │   ├── carrier/     WebRTC PeerConnection + DataChannel lifecycle
│   │   └── socks5/      SOCKS5 proxy (Joiner side)
│   └── mobile/          gomobile bindings for ktalk-client (Android)
│
├── ktalk-panel/         Go module — admin web panel
│   ├── cmd/ktalk-panel/ main binary (embeds web/dist)
│   ├── internal/
│   │   ├── config/      JSON config store
│   │   ├── auth/        bcrypt + session management
│   │   ├── supervisor/  ktalk-core process management
│   │   └── netns/       Linux network namespace isolation
│   └── web/             SvelteKit frontend (dark/light theme)
│       └── src/routes/  +page.svelte, login, setup
│
├── scripts/
│   └── install.sh       One-command VPS installer
│
├── docs/decisions/      ADR documents
├── .github/workflows/   CI (lint + test + build)
└── Makefile
```

## Quick start (dev)

```bash
# 1. Build frontend
cd ktalk-panel/web && pnpm install && pnpm build

# 2. Build binaries
make build

# 3. Run panel locally
./dist/ktalk-panel -config /tmp/ktalk-test.json -port 8888 -debug

# 4. Open http://localhost:8888/setup
```

## Production install

```bash
curl -fsSL https://releases.example.com/ktalk/install.sh | \
  sudo DOMAIN=panel.example.com EMAIL=admin@example.com bash
```

## Sprints

| # | Title | Status |
|---|-------|--------|
| 1 | Protocol RE + docs/protocol.md | 🔲 pending |
| 2 | XMPP client, MUC join | 🔶 scaffold |
| 3 | Jingle, SDP, PeerConnection | 🔶 scaffold |
| 4 | Tunnel protocol, DataChannel, muxer | ✅ scaffold |
| 5 | SOCKS5 frontend | ✅ scaffold |
| 6 | ktalk-panel web UI | ✅ scaffold |
| 7 | install.sh | ✅ draft |
| 8 | ktalk-client (separate repo) | 🔲 pending |
| 9 | Anti-detect | 🔲 pending |
| 10 | QA | 🔲 pending |
| 11 | Closed beta | 🔲 pending |

## Code quality

- Go: `gofmt` + `go vet` + `staticcheck`
- Frontend: `prettier` + `eslint`
- All public Go functions documented
- No hardcoded secrets — keys via config/env only
- Conventional commits: `feat:`, `fix:`, `chore:`
