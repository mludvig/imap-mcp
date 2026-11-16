# Stdio MCP contract

## What it is

One local CLI process exposes the configured account through the official Go MCP SDK's stdio transport. The startup inventory is eight read tools, plus six mutation tools only when readonly is false.

## How it works

The CLI resolves the default configuration at `os.UserConfigDir()/imap-mcp/config.toml`, or a relative/absolute `--config` override, loads it once, and opens one persistent cache. Before serving MCP, `Client.CheckConnection` establishes the configured IMAP transport, reports actual implicit TLS, STARTTLS, or plaintext before client authentication, validates an authenticated session within the configured connection/operation timeouts, and closes it; only success permits MCP serving and readiness. The cache closes when stdio ends or SIGINT/SIGTERM cancels the process.

Typed tools return `{data: T}` or `{error: mail.Error}` in structured content and the same JSON in a text fallback; domain errors set `isError` and contain no data.

## Why

See [ADR 0001](../adr/0001-go-stdio-startup-policy.md) for the transport and immutable startup-mode decision, [ADR 0006](../adr/0006-linux-cli-startup-validation.md) for config discovery, diagnostics, and startup validation, [ADR 0007](../adr/0007-imap-transport-modes.md) for IMAP transport modes, and [ADR 0003](../adr/0003-safe-imap-operations.md) for mutation uncertainty.

## Where it lives

`cmd/imap-mcp/main.go`: `main` and `run`; `internal/client/client.go`: `Client.CheckConnection`, `Client.openWithTransport`, and connection lifecycle; `internal/server/server.go`: `New`, `add`, `threadSchema`, `publicError`, `result`, and `marshalResult`; `internal/server/inputs.go`: `queryInput`, `threadCriteriaInput`, `threadInput`, and defaults; `internal/mail/model.go`: authoritative output/reference/error types. `internal/service/service.go`: `Service.ThreadMessages`; `internal/client/search.go`: `Client.ThreadMessages` and `convertThreads`.

## Invariants & gotchas

Only protocol output belongs on stdout. `listChanged` is false (the SDK omits the false JSON field), configuration is never reloaded, optional DTO fields must stay optional in input schemas, and SDK argument errors need not contain the structured domain envelope.

Help (`-h`/`--help`) and diagnostics use stderr; help exits 0 before configuration/cache/network work. Default diagnostics report resolved config/cache paths, readonly mode, actual IMAP transport, check progress, and readiness; `--quiet` retains errors, `--verbose` adds lifecycle detail, and `--debug` adds fixed stages/timings without raw IMAP traffic, credentials, or message contents. Enabled verbosity modes are pairwise mutually exclusive and invalid arguments exit 2.

Startup configuration/cache/check failures exit 1; a cache alone does not establish readiness. The startup session is closed before MCP serving, and tool calls still require fresh live connections, so readiness is a startup check rather than a promise of continued availability. EOF and SIGINT/SIGTERM exit 0.

Readiness can follow a successful plaintext session in `none`, or `auto` on a non-993 port without advertised STARTTLS. A TLS or advertised STARTTLS failure never falls back to plaintext or retries a different transport; use explicit `tls` for custom implicit-TLS ports and `starttls` to require an upgrade.

SDK automatic schema inference rejects recursive THREAD types, so `threadSchema` explicitly supplies a recursive `$defs/thread_node` while retaining the actual nested result. `marshalResult` serializes the complete SDK `CallToolResult`, counting both structured JSON and escaped text fallback, and `result` rejects sizes above `maxResultBytes` (8 MiB minus 1 KiB).

Native THREAD returns complete server-provided trees for the matching criteria; its nested query excludes `limit`, `sort`, and `before_uid`, and the service rejects these controls when supplied directly. Do not reuse the paged search schema or trim nodes to a page size: that advertises ignored controls or changes tree structure. Narrow the criteria if a complete result exceeds the serialized result budget; list/search retain their separate 50-default, 200-maximum page contract.

This budget applies before JSON-RPC framing; the SDK adds and echoes the request ID outside the typed tool handler, so the 1 KiB allowance does not guarantee a complete response below 8 MiB for arbitrarily large valid IDs. `run` uses SDK stdio directly, with no additional transport wrapper.
