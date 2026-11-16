# 0006: Linux CLI startup validation

## Status

Accepted and verified on 2026-11-17 by build, unit, race, vet, and Dovecot integration checks.

The TLS-only policy is superseded by [ADR 0007](0007-imap-transport-modes.md);
configuration discovery, diagnostics, startup-session closure, and readiness
validation remain in force.

## Context

Requiring an absolute `--config` path adds setup friction, while serving MCP before checking IMAP can report readiness with an unusable TLS mode or account. Operators need startup progress without corrupting stdio JSON-RPC or disclosing credentials and mail.

## Decision

Discover `os.UserConfigDir()/imap-mcp/config.toml` by default (`XDG_CONFIG_HOME`, otherwise `$HOME/.config`, on Linux), with relative or absolute `--config` overrides and complete `-h`/`--help` output. Use stderr for default config/cache paths, readonly mode, check progress, and readiness; enabled `--quiet`, `--verbose`, and `--debug` modes are pairwise mutually exclusive, with errors retained in quiet mode, extra lifecycle detail in verbose mode, and fixed stages/timings in debug mode without raw IMAP traffic, credentials, or mail.

Load config and open the cache, then use `Client.CheckConnection` to establish and close an authenticated, verified TLS session within configured connection/operation timeouts before serving MCP. Reject mandatory absolute paths because launchers can use explicit overrides; reject waiting for the first tool call or reporting offline readiness because configuration mistakes remain hidden; reject automatic TLS-mode fallback and raw protocol logging because they weaken explicit endpoint selection and expose sensitive data.

## Consequences

Startup requires a reachable, authenticated IMAP endpoint; failures exit 1, invalid arguments exit 2, and help, EOF, or SIGINT/SIGTERM exit 0. Verify the server's port and implicit TLS/STARTTLS mode before changing configuration; later tool calls retain their own live sessions, so successful startup does not guarantee future availability.

This extends [ADR 0001](0001-go-stdio-startup-policy.md), whose transport and immutable readonly decisions remain in force; it made no commitment to lazy IMAP connections. Keep the stderr and startup-session invariants in the [stdio contract](../concepts/stdio-mcp.md).
