# Stdio MCP contract

## What it is

One local CLI process exposes the configured account through the official Go MCP SDK's stdio transport. The startup inventory is eight read tools, plus six mutation tools only when readonly is false.

## How it works

The CLI resolves the default configuration at `os.UserConfigDir()/imap-mcp/config.toml`, or a relative/absolute `--config` override, loads it once, and opens one persistent cache. Before serving MCP, `Client.CheckConnection` establishes the configured IMAP transport, reports actual implicit TLS, STARTTLS, or plaintext before client authentication, validates an authenticated session within the configured connection/operation timeouts, and closes it; only success permits MCP serving and readiness. Runtime connection, cache, and diagnostic sink close when stdio ends or SIGINT/SIGTERM cancels the process.

Typed tools return `{data: T}` or `{error: mail.Error}` in structured content and the same JSON in a text fallback; domain errors set `isError` and contain no data.

## Why

See [ADR 0001](../adr/0001-go-stdio-startup-policy.md) for the transport and immutable startup-mode decision, [ADR 0006](../adr/0006-linux-cli-startup-validation.md) for config discovery and startup validation, [ADR 0007](../adr/0007-imap-transport-modes.md) for IMAP transport modes, [ADR 0003](../adr/0003-safe-imap-operations.md) for mutation uncertainty, [ADR 0009](../adr/0009-serialized-runtime-connection.md) for runtime ownership, and [ADR 0010](../adr/0010-process-diagnostics-and-progress.md) for logging and progress.

## Where it lives

`cmd/imap-mcp/main.go`: `main`, `run`, `debugStageWriter`, and `lockedWriter`; `internal/client/client.go`: `Client.CheckConnection` and `Client.dial`; `internal/client/lifecycle.go`: runtime connection lifecycle; `internal/diagnostics/sink.go`: `New`, `Sink`, and `teeHandler`; `internal/diagnostics/redaction.go`: `redactor`; `internal/diagnostics/context.go`: `Stage` and `Measure`; `internal/server/server.go`: `New`, `add`, `progressContext`, `threadSchema`, `publicError`, `result`, and `marshalResult`; `internal/server/inputs.go`: `queryInput`, `threadCriteriaInput`, `threadInput`, and defaults; `internal/mail/model.go`: authoritative output/reference/error types. `internal/service/service.go`: `Service.ThreadMessages`; `internal/service/search.go`: synchronization/query stages; `internal/client/search.go`: `Client.ThreadMessages` and `convertThreads`.

## Invariants & gotchas

Only protocol output belongs on stdout. `listChanged` is false (the SDK omits the false JSON field), configuration is never reloaded, optional DTO fields must stay optional in input schemas, and SDK argument errors need not contain the structured domain envelope.

Help (`-h`/`--help`) and diagnostics use stderr; help exits 0 before configuration/cache/network/log work. Default diagnostics report resolved config/cache/log paths, readonly mode, actual IMAP transport, check progress, and readiness; `--quiet` retains errors, `--verbose` adds lifecycle detail, and `--debug` adds stage/timing stderr without raw protocol traffic. Enabled verbosity modes are pairwise mutually exclusive and invalid arguments exit 2.

Startup configuration/cache/log/check failures exit 1; a cache alone does not establish readiness. The startup session is closed before MCP serving and runtime calls require live IMAP access through serialized reuse, so readiness is a startup check rather than a promise of continued availability. EOF and SIGINT/SIGTERM exit 0 unless closing a runtime resource or a retained log I/O error fails; root cancellation closes the runtime socket before SDK handler draining.

The JSON log uses `os.UserCacheDir()/imap-mcp/imap-mcp.log`, independent of `cache.path`, with 0600 active/backup files, a 0700 directory, local RFC3339 offsets, and a 50 MiB cap (10 MiB active plus `.1`–`.4`). INFO lifecycle and typed-handler completion records persist even with `--quiet`; `--debug` adds stages, timings, inputs, and bounded results. Only the configured password is redacted, so other identity, path, query, and mail details can appear; pre-handler SDK validation failures do not pass through typed-handler completion logging.

All sinks sharing the directory lock the stable, empty 0600 `imap-mcp.log.lock` during startup and each append/rotation, then refresh the active inode and size before writing; never remove that lock while any writer can run. Logs and the lock must be owned, single-link regular files, directory-relative opens prevent path escapes, and oversized records become bounded omission records; existing oversized files retain only complete trailing records. During logging, context cancellation and Close interrupt queued lock waits; startup preparation has no cancellation context. The 50 MiB bound depends on cooperating writers and filesystem flock support; see ADR 0010.

Progress is attached only when the requesting call supplies a token, including zero or empty-string tokens. One per-call mutex orders increasing stage counts and notification sends; total is unknown, canceled calls skip sends, and notification failures leave tool results unchanged. Counts are not percentages and client display is not guaranteed; without a token, DEBUG stages remain available in the log and stderr.

Readiness can follow a successful plaintext session in `none`, or `auto` on a non-993 port without advertised STARTTLS. A TLS or advertised STARTTLS failure never falls back to plaintext or retries a different transport; use explicit `tls` for custom implicit-TLS ports and `starttls` to require an upgrade.

SDK automatic schema inference rejects recursive THREAD types, so `threadSchema` explicitly supplies a recursive `$defs/thread_node` while retaining the actual nested result. `marshalResult` serializes the complete SDK `CallToolResult`, counting both structured JSON and escaped text fallback, and `result` rejects sizes above `maxResultBytes` (8 MiB minus 1 KiB).

Native THREAD returns complete server-provided trees for the matching criteria; its nested query excludes `limit`, `sort`, and `before_uid`, and the service rejects these controls when supplied directly. Do not reuse the paged search schema or trim nodes to a page size: that advertises ignored controls or changes tree structure. Narrow the criteria if a complete result exceeds the serialized result budget; list/search retain their separate 50-default, 200-maximum page contract.

This budget applies before JSON-RPC framing; the SDK adds and echoes the request ID outside the typed tool handler, so the 1 KiB allowance does not guarantee a complete response below 8 MiB for arbitrarily large valid IDs. `run` uses SDK stdio directly, with no additional transport wrapper.
