# 0001: Go, stdio, and startup policy

## Status

Accepted.

## Context

This is a local, single-user IMAP MCP server with one account and a TOML configuration. Go is the selected language; Python and Rust were considered, but introducing either now would add a toolchain without satisfying an unmet requirement.

## Decision

Use the official Go MCP SDK and `go-imap/v2`, pinned in `go.mod` (currently SDK `v1.8.0` and IMAP `v2.0.0-beta.9`), with stdio as the only MCP transport. Load configuration once at startup, default `readonly` to true, and register only read tools in that mode; changing modes requires a restart.

## Consequences

Configuration, service, IMAP adapter, and MCP registration enforce immutable startup readonly; the CLI creates one service and runs the SDK stdio transport. There is no HTTP listener, multi-user session layer, or runtime configuration switch. The IMAP dependency is a prerelease, so upgrades require checks of the workarounds in [IMAP safety](../concepts/imap-safety.md); adopting a different language or transport requires a new decision.

## Integration findings

Native rev2 MOVE exposed beta.9's missing VANISHED decoding; [ADR 0004](0004-backport-imap4rev2-vanished.md) adds a narrow local replacement while retaining the pinned version and modern protocol scope. The serialized SDK tool result has an 8 MiB minus 1 KiB budget, including structured content and escaped text fallback; this is not a bound on the complete JSON-RPC response because its echoed request ID is outside the typed handler. Direct SDK stdio remains the selected transport; no additional transport wrapper is required.
