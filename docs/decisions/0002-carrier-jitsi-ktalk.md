# ADR-0002: Carrier — Kontour Talk (ktalk.ru) as the sole WebRTC carrier

**Date**: 2026-05-19  
**Status**: Accepted

## Context

The olcrtc reference codebase supported multiple carriers: `telemost`, `jazz`, `wbstream`.
The project uses only one.

## Decision

**Single carrier: Kontour Talk (ktalk.ru / Jitsi Meet).**

All references to other carriers have been removed:
- No `jazz` package, no `wbstream` package, no `telemost` package.
- No enum values, no dead-code paths, no log strings mentioning those names.
- The `carrier` package is a single implementation, not a registry.

Rationale:
- ktalk.ru is whitelisted by Минцифры РФ → traffic passes ТСПУ without block.
- Removing alternative carriers shrinks the binary and eliminates forensic footprints.
- A binary analysis should reveal no connection to the public fork ecosystem.

## Consequences

- If ktalk.ru becomes unavailable, no fallback exists — a new carrier would need a new sprint.
- The `config.RoomConfig.Subdomain` must be a valid *.ktalk.ru or *.ktalk.host subdomain.
