# 0001: Go, stdio, and startup policy

## Status

Accepted.

## Context

This is a local, single-user IMAP MCP server with one account and a TOML configuration. Go is the selected language; Python and Rust were considered, but introducing either now would add a toolchain without satisfying an unmet requirement.

## Decision

Use the official Go MCP SDK and `go-imap/v2`, pinned in `go.mod` (currently SDK `v1.8.0` and IMAP `v2.0.0-beta.9`), with stdio as the only MCP transport. Load configuration once at startup, default `readonly` to true, and register only read tools in that mode; changing modes requires a restart.

## Consequences

Configuration and the IMAP adapter enforce immutable startup readonly; MCP registration and the stdio entry point remain planned. There is no HTTP listener, multi-user session layer, or runtime configuration switch. The IMAP dependency is a prerelease, so upgrades require checks of the workarounds in [IMAP safety](../concepts/imap-safety.md); adopting a different language or transport requires a new decision.
