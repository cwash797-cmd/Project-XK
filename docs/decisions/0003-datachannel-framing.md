# ADR-0003: DataChannel framing protocol

**Date**: 2026-05-19  
**Status**: Accepted

## Context

We need a multiplexed, encrypted, padding-aware framing protocol over a single WebRTC DataChannel.

## Decision

Custom framing protocol (see `ktalk-core/internal/muxer/frame.go`):

```
[u32 stream_id][u8 cmd][u64 seq][u16 padding_len][u16 payload_len][...padding][...payload]
```

- `seq` is the AEAD nonce input (monotonic send counter).
- `payload` is ChaCha20-Poly1305 encrypted.
- `padding` is random bytes of length 0…1024 — never encrypted.
- Commands: OPEN, DATA, CLOSE, RST, PING, PONG.
- Multiplexing: N logical streams over one DC, identified by `stream_id`.

**Why not smux/yamux?** Those are well-known libraries — a DPI could fingerprint the framing. Our custom protocol + variable padding defeats size-histogram analysis.

**Why ChaCha20-Poly1305?** Fast on ARM (Android), constant-time, audited. AES-GCM is also fine but requires hardware acceleration to match.

## Consequences

- Any change to the frame format breaks existing clients — bump a version bit.
- The panel and client must agree on `shared_key` (64 hex chars = 32 bytes).
