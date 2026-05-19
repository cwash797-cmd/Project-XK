# Sprint 4 Handover — `ktalk-client` (cwash797-cmd/ktalk-client)

> **For:** Sprint 4 developers building the standalone client binary  
> **From:** Sprint 1-3 team (Project-XK / genspark_ai_developer branch)  
> **Date:** 2026-05-19  
> **Status:** Sprints 1-3 complete and passing. Sprint 4 = new repo.

---

## 1. What Project-XK Is (2-minute summary)

Project-XK tunnels arbitrary TCP traffic through **ktalk.ru** (a Jitsi-based video
conferencing service) without any dedicated server infrastructure.

```
Your app  ──SOCKS5──►  ktalk-core  ──WebRTC DataChannel──►  ktalk.ru cluster  ──►  ktalk-core  ──TCP──►  remote service
(curl, ssh, browser)   (Creator side)   (encrypted, muxed)                         (Joiner side)
```

The tunnel uses:
- **XMPP WebSocket** (Jitsi signaling) for Jingle session negotiation
- **Pion WebRTC** (ICE + DTLS + SCTP + DataChannel) as the transport
- **ChaCha20-Poly1305 AEAD** for application-level E2E encryption on top of DTLS
- **Custom muxer** to multiplex multiple TCP streams over one DataChannel

The user points it at a ktalk.ru room URL (e.g. `https://ilte0310.ktalk.ru/cb140blkff7i`).
Two instances of ktalk-core join the same XMPP conference room — one as the "Creator"
(SOCKS5 listener), one as the "Joiner" (dials the actual target service).

---

## 2. Repository Layout (Project-XK, the parent repo)

```
/
├── ktalk-core/                   # ← the Go module your client imports
│   ├── go.mod                    # module: github.com/private/ktalk-core
│   ├── cmd/ktalk-core/main.go    # standalone CLI (serve | connect | version)
│   └── internal/
│       ├── carrier/carrier.go    # Pion WebRTC + DataChannel orchestration
│       ├── config/config.go      # Config struct + YAML/env loader
│       ├── crypto/crypto.go      # ChaCha20-Poly1305 Cipher (Seal/Open)
│       ├── jingle/jingle.go      # XMPP Jingle state machine (XEP-0166/0176/0320)
│       ├── metrics/metrics.go    # Prometheus-style /health + /metrics
│       ├── muxer/
│       │   ├── frame.go          # Frame header encode/decode
│       │   └── muxer.go          # Session, Stream, RotateKey, SetOnIncomingStream
│       ├── names/                # Random room name generator
│       ├── roomapi/roomapi.go    # Anonymous join: GET /api/rooms/<id> → cookie
│       ├── socks5/server.go      # SOCKS5 proxy server (tunnel ingress)
│       └── xmpp/client.go       # XMPP WebSocket: SASL ANON, SM, Ping, Jingle
│
├── ktalk-panel/                  # Admin web panel (not needed for ktalk-client)
├── docs/                         # ← read these
│   ├── ARCHITECTURE.md
│   ├── SETUP.md
│   └── SPRINT4-HANDOVER.md      # this file
└── packaging/                    # systemd, install scripts
```

---

## 3. How a ktalk.ru Room URL is Used

Given a room URL like `https://ilte0310.ktalk.ru/cb140blkff7i`:

| Part | Value | Meaning |
|---|---|---|
| Subdomain | `ilte0310` | Identifies the ktalk.ru shard/cluster |
| Room ID | `cb140blkff7i` | Conference room identifier |

**Step-by-step anonymous join flow** (implemented in `roomapi/roomapi.go`):

```
1. GET https://ilte0310.ktalk.ru/api/rooms/cb140blkff7i
   Response headers:
     Set-Cookie: ngtoken=<jwt>; Path=/; HttpOnly
   Response body: { "wsUrl": "wss://...", "hosts": { "muc": "conference...." } }

2. Open WebSocket to wsUrl (the XMPP endpoint)
   → Pass ngtoken cookie for auth (SASL ANONYMOUS)

3. XMPP: MUC join → Jicofo conference-request → Jingle session-initiate
   → ICE gathering → DTLS → SCTP → DataChannel open

4. Muxer Session created with shared ChaCha20 key
   → SOCKS5 listener starts (Creator) or SetOnIncomingStream callback fires (Joiner)
```

The `ngtoken` cookie is stored in a `*cookiejar.Jar` and injected automatically into
the WebSocket upgrade request. See `carrier.go → prepareRoom()`.

---

## 4. ktalk-core as a Go Module

Import path: `github.com/private/ktalk-core`

### Key types your client will use

```go
// Config — load from YAML or build programmatically
type Config struct {
    RoomURL        string        // "https://ilte0310.ktalk.ru/cb140blkff7i"
    SharedKey      string        // hex-encoded 32-byte ChaCha20 key
    SOCKS5Addr     string        // "127.0.0.1:1080" (Creator side)
    Label          string        // human-readable tunnel name
    WatchdogPeriod time.Duration // reconnect interval (default: 35m)
    LogLevel       string        // "debug" | "info" | "warn" | "error"
}

// Carrier — WebRTC + XMPP orchestration
// Create with: carrier.New(cfg, logger)
// Start with:  carrier.Start(ctx)  → blocks until ctx cancelled or fatal error
// The carrier:
//   - Fetches room API, gets cookie
//   - Opens XMPP WebSocket
//   - Negotiates Jingle session
//   - Creates muxer.Session
//   - Starts SOCKS5 server (Creator) or calls SetOnIncomingStream (Joiner)

// muxer.Session — multiplexing over DataChannel
sess.OpenStream(ctx, "host:port")          // Creator: open new TCP-over-DC stream
sess.SetOnIncomingStream(func(st *Stream)) // Joiner: called for each CmdOpen frame
sess.RotateKey(newKey32bytes)              // rotate ChaCha20 key live

// muxer.Stream — implements io.ReadWriteCloser
st.Read(buf)
st.Write(buf)
st.Close()
st.Target() // returns "host:port" from the CmdOpen payload (Joiner side)
```

### Minimal client skeleton

```go
package main

import (
    "context"
    "log/slog"
    "os"

    "github.com/private/ktalk-core/internal/carrier"
    "github.com/private/ktalk-core/internal/config"
)

func main() {
    cfg := config.Config{
        RoomURL:    os.Getenv("KTALK_ROOM_URL"),  // https://ilte0310.ktalk.ru/cb140blkff7i
        SharedKey:  os.Getenv("KTALK_KEY"),        // 64 hex chars = 32 bytes
        SOCKS5Addr: "127.0.0.1:1080",
        Label:      "my-client",
    }
    log := slog.New(slog.NewTextHandler(os.Stderr, nil))
    c := carrier.New(cfg, log)
    if err := c.Start(context.Background()); err != nil {
        log.Error("carrier failed", "err", err)
        os.Exit(1)
    }
}
```

---

## 5. Config Format (YAML — for file-based config)

```yaml
# /etc/ktalk-client/config.yaml
room_url: "https://ilte0310.ktalk.ru/cb140blkff7i"
shared_key: "a1b2c3d4e5f6..."   # 64 hex chars (32 bytes)
socks5_addr: "127.0.0.1:1080"
label: "prod-tunnel-1"
watchdog_period: "35m"
log_level: "info"
```

Environment variable overrides (same names, uppercased with `KTALK_` prefix):
```
KTALK_ROOM_URL
KTALK_SHARED_KEY
KTALK_SOCKS5_ADDR
KTALK_LABEL
KTALK_LOG_LEVEL
```

---

## 6. The Subscription Config (`/sub/:id/:token`)

When `ktalk-panel` creates a tunnel client, it generates a **subscription config**
endpoint. The end-user fetches this URL to get a ready-to-use config:

```bash
curl https://your-panel.example.com/sub/abc123/<sub_token>
```

Response (JSON):
```json
{
  "room_url": "https://ilte0310.ktalk.ru/cb140blkff7i",
  "shared_key": "deadbeef...",
  "socks5_addr": "127.0.0.1:1080",
  "label": "my-tunnel"
}
```

The `ktalk-client` binary (Sprint 4) should accept this URL as a flag:
```bash
ktalk-client --sub https://panel.example.com/sub/abc123/TOKEN
# fetches config, writes to ~/.config/ktalk-client/config.yaml, starts tunnel
```

This is the **primary UX**: admin distributes a single URL, user runs one command.

---

## 7. ktalk-panel REST API (for reference / integration)

Base URL: `https://your-panel.example.com`  
Auth: session cookie (login via `POST /api/login`) or `Authorization: Bearer <token>`

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/login` | `{"password":"..."}` → sets session cookie |
| `GET` | `/api/clients` | List all tunnel clients |
| `POST` | `/api/clients` | Create client: `{"room_url":"...","label":"..."}` |
| `GET` | `/api/clients/:id` | Get client details (includes `sub_token`, `shared_key`) |
| `DELETE` | `/api/clients/:id` | Remove client |
| `POST` | `/api/clients/:id/start` | Start tunnel process |
| `POST` | `/api/clients/:id/stop` | Stop tunnel process |
| `GET` | `/api/clients/:id/state` | `{"running":true,"restarts":0,...}` |
| `GET` | `/api/events` | SSE stream: `state` / `log` / `ping` events |
| `GET` | `/sub/:id/:token` | Public — subscription config (no auth needed) |
| `GET` | `/health` | `{"status":"ok","uptime":"2h15m"}` |
| `GET` | `/metrics` | Prometheus text format |

### SSE Event format
```
data: {"type":"state","data":{"client_id":"abc","running":true,"restarts":0}}
data: {"type":"log","data":{"t":"2026-05-19T12:00:00Z","line":"[abc] ICE connected"}}
data: {"type":"ping","data":"2026-05-19T12:00:01Z"}
```

---

## 8. Muxer Frame Protocol (reference)

Frames flow over a single WebRTC DataChannel (reliable, ordered).

```
 0         1         2         3
┌──────────────────────────────────────────────────────────────────┐
│                    Stream ID (32 bits)                           │
├──────────────────────────────────────────────────────────────────┤
│  Cmd (8)  │  Flags (8) │        Length (16 bits)                 │
├──────────────────────────────────────────────────────────────────┤
│                    Seq (64 bits)  ← AEAD nonce index             │
├──────────────────────────────────────────────────────────────────┤
│            Payload (Length bytes, AEAD ciphertext)               │
└──────────────────────────────────────────────────────────────────┘

Commands:
  0x01  CmdOpen      payload: "host:port" (plaintext target for Joiner)
  0x02  CmdData      payload: ciphertext application data
  0x03  CmdClose     payload: empty
  0x04  CmdPing      payload: empty (keepalive)
  0x05  CmdPong      payload: empty
  0x06  CmdError     payload: error string
  0x07  CmdKeyRotate payload: 32-byte new key (sealed with current key)
```

**CRITICAL BUG (fixed in this sprint):** The `Seq` field in the frame header
must match the nonce used by `ChaCha20-Poly1305.Seal()`. Previously `sendFrame()`
always wrote `seq=0` into the header while `Seal()` used an incrementing counter
internally — causing AEAD auth failures for all frames after the first. Fixed:
`Seal()` now returns `(ciphertext, seq)` and `sendFrame()` uses the returned seq.

---

## 9. Encryption Details

```
Cipher:     ChaCha20-Poly1305 (AEAD, RFC 8439)
Key size:   32 bytes (256-bit) — out-of-band, never sent unencrypted
Nonce:      12 bytes, constructed from monotonic uint64 seq:
              nonce[0:8]  = seq (little-endian)
              nonce[8:12] = 0x00 (padding)
Auth tag:   16 bytes appended to ciphertext
Direction:  Each direction has its own seq counter (no cross-direction reuse)
Rotation:   CmdKeyRotate frame sealed with OLD key; both sides swap atomically
```

---

## 10. Integration Tests (how to verify muxer works)

See `ktalk-core/tests/integration_test.go` for three canonical tests:

| Test | What it verifies |
|---|---|
| `TestEchoTunnel` | Single stream: Creator→DataChannel→Joiner→TCP echo server→back |
| `TestMultiStreamEcho` | 5 concurrent streams over one DataChannel |
| `TestKeyRotationLive` | `RotateKey()` mid-session; post-rotation data still flows |

Run them:
```bash
cd ktalk-core
go test -race -timeout=80s ./tests/...
```

The tests use **in-process Pion loopback** (two `PeerConnection`s connected via
in-memory SDP exchange — no real network, no ktalk.ru needed).

---

## 11. What Sprint 4 Needs to Build

### Repo: `cwash797-cmd/ktalk-client`

**Goal:** A standalone, cross-platform CLI + optional tray app that:

1. **Fetches** subscription config from `/sub/:id/:token` URL
2. **Stores** config locally (`~/.config/ktalk-client/config.yaml`)
3. **Starts** `ktalk-core` carrier (import the Go module directly)
4. **Exposes** SOCKS5 proxy on configured port
5. **Optional:** System tray icon showing tunnel status (connected/reconnecting)
6. **Optional:** `ktalk-client status` CLI command (hits local `/health`)

### Suggested flags

```
ktalk-client --sub   URL        # fetch + save subscription config, start tunnel
ktalk-client --config FILE      # use existing config file
ktalk-client --socks5 ADDR      # override SOCKS5 listen address (default 127.0.0.1:1080)
ktalk-client status             # show tunnel status
ktalk-client version            # print version
```

### Import ktalk-core

In `go.mod`:
```
require github.com/private/ktalk-core v0.x.y
```

Or use a `replace` directive pointing to a local checkout during development:
```
replace github.com/private/ktalk-core => ../Project-XK/ktalk-core
```

---

## 12. Build Targets (from Makefile in Project-XK)

```bash
make build          # builds ktalk-core + ktalk-panel
make test           # go test -race ./...
make fmt            # gofmt
make package        # .deb + .rpm (amd64)
make package-arm64  # .deb + .rpm (arm64)
make docker         # Docker image
```

For `ktalk-client` you'll want to add:
```makefile
build-client:
	go build -o dist/ktalk-client ./cmd/ktalk-client

package-client:
	nfpm package --config nfpm-client.yaml --packager deb --target dist/
```

---

## 13. Known Limitations / Gotchas

| Issue | Detail |
|---|---|
| Free-tier call limit | ktalk.ru may cut calls at ~40 min. Watchdog reconnects at 35 min. |
| Anonymous token TTL | `ngtoken` cookie TTL unknown; `401` triggers auto-reconnect |
| Shard changes | `x-jitsi-shard` header change detected → auto-reconnect |
| Single room per process | Each carrier process = one room. Panel manages multiple. |
| UDP firewall | ICE requires outbound UDP 3478 (STUN) and 10000-60000 (JVB media) |
| No audio/video | DataChannel only. Jitsi audio/video streams are ignored. |

---

## 14. Test the Whole Stack (E2E smoke test)

```bash
# Terminal 1 — start ktalk-panel (Creator side)
cd ktalk-panel && go run ./cmd/ktalk-panel -config config.json

# Terminal 2 — check health
curl http://localhost:8888/health
# {"status":"ok","uptime":"3s"}

# Terminal 3 — create tunnel via API
curl -X POST http://localhost:8888/api/clients \
  -H "Authorization: Bearer <token>" \
  -d '{"room_url":"https://ilte0310.ktalk.ru/cb140blkff7i","label":"test"}'

# Terminal 4 — test SOCKS5 tunnel (once connected)
curl --socks5 127.0.0.1:1080 http://example.com
```

---

## 15. Contact / Handover Notes

- All Sprint 1-3 work is on branch `genspark_ai_developer`, PR #1 (Project-XK repo)
- `ktalk-core` module is stable and tested (all tests pass with `-race`)
- The AEAD nonce bug is **fixed** — do not port the old `Seal()` signature
- Use `SetOnIncomingStream()` on the Joiner session to receive incoming streams
- Use `st.Target()` to get the dial target (`host:port`) for each stream
- Key rotation tested and working — use `RotateKey(key32)` any time mid-session
