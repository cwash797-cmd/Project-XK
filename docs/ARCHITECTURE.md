# Architecture — Project XK

> WebRTC/XMPP tunnel over ktalk.ru (Jitsi-based conferencing infrastructure)

---

## Overview

Project XK creates a **bidirectional TCP-over-DataChannel tunnel** that uses the  
ktalk.ru WebRTC conferencing infrastructure as a transport layer.

```
Client (SOCKS5)         ktalk-core (initiator)       ktalk.ru XMPP/WebRTC
    │                         │                              │
    │  TCP connect             │   SASL ANONYMOUS             │
    │─────────────────────────►│──────────────────────────────►│
    │                         │   Jingle session-initiate    │
    │  SOCKS5 request          │◄─────────────────────────────│
    │─────────────────────────►│   ICE + DTLS handshake       │
    │                         │◄─────────────────────────────►│
    │  Data (proxied)          │   DataChannel open           │
    │◄─────────────────────────│◄─────────────────────────────►│
    │                         │   Muxer frames (ChaCha20)    │
```

---

## Components

### ktalk-core

Low-level WebRTC/XMPP tunnel engine. Implements:
- XMPP WebSocket client (Jingle signaling)
- Pion WebRTC (ICE + DTLS + SCTP + DataChannel)
- Muxer framing protocol (ChaCha20-Poly1305)
- SOCKS5 proxy server (tunnel ingress)
- Metrics / health endpoints

### ktalk-panel

Admin web panel. Implements:
- Tunnel lifecycle management (create / start / stop)
- E2E key rotation scheduler (24h)
- SSE-based real-time dashboard
- REST API for programmatic control
- SvelteKit web UI

---

## Data Flow

### Tunnel Establishment

```
1. ktalk-panel calls ktalk-core REST: POST /api/connect
2. ktalk-core fetches ktalk.ru /api/rooms/<room-id>
   → receives: wsUrl, hosts.muc, token
3. ktalk-core opens WebSocket: wss://<domain>/<prefix>/xmpp-websocket?room=<name>
4. SASL ANONYMOUS authentication
5. XEP-0199 Ping negotiation (keepalive)
6. XEP-0198 Stream Management (reconnect)
7. MUC join: <room-prefix>@conference.<prefix>.<domain>
8. Jicofo conference-request IQ
9. Jingle session-initiate from Jicofo
10. Pion: ICE gathering → candidates via transport-info
11. DTLS handshake → SCTP → DataChannel open
12. Muxer session created (ChaCha20-Poly1305 cipher)
13. SOCKS5 server starts listening on 127.0.0.1:<port>
```

### Frame Protocol (Muxer)

```
 0         1         2         3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
├─────────────────────────────────────────────────────────────────┤
│                    Stream ID (32 bits)                          │
├─────────────────────────────────────────────────────────────────┤
│   Cmd (8)  │  Flags (8) │        Length (16)                    │
├─────────────────────────────────────────────────────────────────┤
│                    Payload (variable)                           │
└─────────────────────────────────────────────────────────────────┘

Commands:
  0x01 CmdOpen      — open new stream (payload: target addr:port)
  0x02 CmdData      — data frame
  0x03 CmdClose     — close stream
  0x04 CmdPing      — keepalive ping
  0x05 CmdPong      — keepalive pong
  0x06 CmdError     — error notification
  0x07 CmdKeyRotate — E2E key rotation (payload: 32-byte raw key)

Encryption: ChaCha20-Poly1305 (AEAD)
  - Key: 32 bytes (256-bit)
  - Nonce: 12 bytes, monotonic counter per direction
  - Auth tag: 16 bytes appended to ciphertext
```

### E2E Key Rotation

```
Initiator                          Responder
    │                                   │
    │  1. Generate newKey (32 bytes)     │
    │  2. Seal CmdKeyRotate with         │
    │     OLD cipher                    │
    │─────────────────────────────────►│
    │  3. Swap to newCipher (atomic)     │  4. Receive, decrypt with OLD cipher
    │                                   │  5. Derive newCipher from payload
    │                                   │  6. Swap to newCipher (atomic)
    │◄─────────────────────────────────│
    │  All subsequent frames: newKey    │
```

Key rotation is scheduled every 24h by `KeyRotator` in `ktalk-panel/supervisor`.

---

## Directory Structure

```
/
├── ktalk-core/                   # Core tunnel engine (Go)
│   ├── cmd/ktalk-core/main.go    # CLI: serve | connect | version
│   └── internal/
│       ├── carrier/              # Pion WebRTC + DataChannel wiring
│       │   ├── carrier.go        # Carrier: ICE+DTLS+SCTP orchestration
│       │   └── loopback_test.go  # Integration test (2 Pion PCs in-process)
│       ├── crypto/               # ChaCha20-Poly1305 AEAD
│       │   └── crypto.go
│       ├── jingle/               # XMPP Jingle state machine
│       │   └── jingle.go         # XEP-0166/0167/0176/0320
│       ├── metrics/              # Prometheus + /health endpoint
│       │   └── metrics.go
│       ├── muxer/                # Frame protocol (multiplexing)
│       │   ├── frame.go          # Frame header types
│       │   └── muxer.go          # Session, RotateKey()
│       ├── names/                # Room name generator
│       ├── socks5/               # SOCKS5 proxy server
│       │   └── server.go
│       ├── xmpp/                 # XMPP WebSocket client
│       │   └── client.go         # SASL ANONYMOUS, SM, Ping, Jingle dispatch
│       └── config/               # Config types + YAML loader
│           └── config.go
│
├── ktalk-panel/                  # Admin panel (Go + SvelteKit)
│   ├── cmd/ktalk-panel/main.go   # HTTP server + SSE broker + supervisor
│   └── internal/
│       ├── auth/                 # Session auth middleware
│       ├── config/               # Panel config store (JSON)
│       ├── sse/                  # Server-Sent Events fan-out broker
│       │   └── broker.go         # state | log | ping events
│       └── supervisor/
│           ├── supervisor.go     # Tunnel lifecycle manager
│           └── rotator.go        # 24h key rotation scheduler
│   └── web/                      # SvelteKit UI
│       └── src/
│           ├── routes/
│           │   ├── +page.svelte          # Dashboard (SSE-driven)
│           │   ├── login/+page.svelte    # Auth
│           │   └── setup/+page.svelte    # Initial setup
│           └── lib/api.ts                # SSE client, fmtBytes, fmtTime
│
├── docs/                         # Documentation
│   ├── ARCHITECTURE.md           # This file
│   ├── SETUP.md                  # Installation and configuration
│   └── sprint1-tz-amendment.md   # Sprint 1 ТЗ amendments (RE analysis)
│
├── packaging/                    # OS package scripts
│   ├── ktalk-panel.service       # systemd unit
│   ├── preinstall.sh             # useradd xk
│   ├── postinstall.sh            # enable service
│   ├── preremove.sh              # stop/disable
│   └── config.json.default       # default config
│
├── Dockerfile                    # 3-stage build (node→go→alpine)
├── Makefile                      # build, test, package, docker targets
├── nfpm.yaml                     # .deb / .rpm package spec
└── install.sh                    # Unattended install script
```

---

## Network Topology

```
                          Internet
                             │
    ┌────────────────────────┼──────────────────────────────┐
    │    ktalk.ru cluster     │                              │
    │                         │                              │
    │  ┌──────────────────┐   │   ┌────────────────────────┐│
    │  │  XMPP server     │   │   │  TURN/STUN servers     ││
    │  │  (Prosody/Ejabberd│   │   │  (XEP-0215 extdisco)  ││
    │  │  WebSocket)       │   │   └────────────────────────┘│
    │  └──────────────────┘   │                              │
    │  ┌──────────────────┐   │                              │
    │  │  Jicofo          │   │                              │
    │  │  (focus server)  │   │                              │
    │  └──────────────────┘   │                              │
    │  ┌──────────────────┐   │                              │
    │  │  Videobridge(JVB)│   │                              │
    │  │  (media bridge)  │   │                              │
    │  └──────────────────┘   │                              │
    └─────────────────────────┼──────────────────────────────┘
                              │
               ┌──────────────┴──────────────┐
               │                             │
     ┌─────────▼──────────┐      ┌──────────▼──────────┐
     │  ktalk-core         │      │   ktalk-core        │
     │  (initiator)        │      │   (responder)       │
     │  SOCKS5 :1080       │      │   SOCKS5 :1080      │
     └─────────────────────┘      └────────────────────-┘
               │                             │
     ┌─────────▼──────────┐      ┌──────────▼──────────┐
     │  Local application  │      │  Remote service     │
     │  (browser, curl...) │      │  (HTTP, SSH, etc)   │
     └─────────────────────┘      └─────────────────────┘
```

---

## Security Model

### Threat Model

| Threat | Mitigation |
|---|---|
| Network eavesdropping on DataChannel | ChaCha20-Poly1305 E2E encryption |
| Key compromise | 24h scheduled key rotation |
| Replay attacks | Monotonic nonce counter per-direction |
| Panel unauthorized access | Session auth + bcrypt password |
| XMPP credential leakage | SASL ANONYMOUS (no credentials) |
| Session hijacking | `Session <token>` is short-lived per room |

### Encryption Details

```
Key exchange:   Out-of-band (admin panel generates key, stores in config)
                Key is never sent over the network unencrypted.
                CmdKeyRotate frames are sealed with the CURRENT key
                (attacker must break current key to see the new one).

DTLS:           Peer certificates verified via Jingle fingerprint exchange.
                Man-in-the-middle of DTLS requires compromising Jicofo.

SCTP/DC:        Encrypted by DTLS-SRTP layer (WebRTC mandatory).
                Muxer adds application-layer encryption on top.
```

### Hardening (systemd unit)

```ini
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/etc/ktalk-panel /var/lib/ktalk-panel
CapabilityBoundingSet=
AmbientCapabilities=
```

---

## Metrics and Observability

### Endpoints

| Path | Description |
|---|---|
| `GET /health` | `{"status":"ok","uptime":"..."}` |
| `GET /metrics` | Prometheus text format |
| `GET /api/events` | SSE stream (state/log/ping events) |

### Key Metrics

```
# HELP ktalk_tunnels_active Number of active tunnel sessions
# TYPE ktalk_tunnels_active gauge
ktalk_tunnels_active N

# HELP ktalk_bytes_total Total bytes transferred through tunnels
# TYPE ktalk_bytes_total counter
ktalk_bytes_total{direction="in|out"} N

# HELP ktalk_key_rotations_total Total key rotation events
# TYPE ktalk_key_rotations_total counter
ktalk_key_rotations_total N

# HELP ktalk_reconnects_total Total tunnel reconnect events
# TYPE ktalk_reconnects_total counter
ktalk_reconnects_total{reason="ice_fail|xmpp_disconnect|watchdog|shard_change"} N
```

---

## Known Limitations

1. **Call duration limit (free tier):** ktalk.ru free tier may enforce a per-conference  
   time limit (empirical verification required). The watchdog performs a preventive  
   reconnect every 35 minutes as a conservative default.

2. **Anonymous room TTL:** Session tokens issued by `GET /api/rooms/<id>` have an  
   unknown TTL. The client handles `401` responses by triggering a reconnect.

3. **Shard failover:** If ktalk.ru rebalances to a new shard, the client detects the  
   changed `x-jitsi-shard` header from `/_unlock` and reconnects automatically.

4. **Single room per instance:** Each ktalk-core process connects to one room.  
   Multi-room routing (Sprint 6) multiplexes multiple rooms over a single panel process.
