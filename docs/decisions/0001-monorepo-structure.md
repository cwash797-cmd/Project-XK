# ADR-0001: Monorepo structure — ktalk-core + ktalk-panel in one repo

**Date**: 2026-05-19  
**Status**: Accepted

## Context

The project requires two server-side binaries:
- `ktalk-core` — the Go tunnel process (one instance per client on the Creator VPS)
- `ktalk-panel` — the admin web panel that manages `ktalk-core` processes

The question was whether to keep them in a single repository or split them.

## Decision

**Single repository** (`ktalk`) containing both `ktalk-core/` and `ktalk-panel/` as Go modules.

Rationale:
- Panel embeds the `ktalk-core` binary as a managed child process — co-location simplifies CI/CD (one build pipeline produces both artifacts).
- `install.sh` deploys both binaries; a single release tag covers both.
- Panel and core share no Go code (separate modules avoid circular imports), but can share documentation, CI workflow, and the Makefile.
- `ktalk-client` is a separate repo (Kotlin Multiplatform — completely different toolchain).

## Consequences

- Both modules must keep their `go.mod` independent to avoid forcing Go version coupling.
- The Makefile `build` target builds core first, then panel (panel embeds the web frontend which must be built first).
- Future: if the panel grows large enough to warrant its own CI cache, we can split it without disrupting clients.
