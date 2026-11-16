# IMAP safety

## What it is

Read-only operation is a startup boundary, and read tools are side-effect-free even when write tools are enabled. Trash is a move to the configured mailbox, not a permanent-delete endpoint.

## How it works

TOML configuration defaults to readonly and the client rejects writes using that immutable value; the planned service and MCP registration must share it, with changes requiring a restart. Reads select with EXAMINE and fetch with BODY.PEEK; native UID MOVE is required, and the planned Trash tool uses the configured mailbox. Capability reporting separates advertised, enabled, and implemented features, and lost mutation responses set `outcome_unknown` without replay.

## Why

See [ADR 0001](../adr/0001-go-stdio-startup-policy.md) and [ADR 0003](../adr/0003-safe-imap-operations.md).

## Where it lives

`internal/config/config.go`: `Config`, `Load`, and `IMAPConfig.TLSConfig`; `internal/mail/model.go`: `ServerInfo`, `Error`, and the size limits. `internal/client/client.go`: `Client.open`, `deadlineConn`, and `session.close`; `internal/client/fetch.go`: `session.fetch`, `session.failedLiteral`, and `Client.Refresh`; `internal/client/write.go`: `Client.writeGuard`, `mutationFlags`, and `Client.transfer`; `internal/mime/mime.go`: `Parse`, `Attachment`, `newEntity`, and `scanner.walk`. Service account locking, refresh/invalidation orchestration, Trash routing, and MCP tool registration remain planned.

## Invariants & gotchas

No read path may set Seen, and no command may add Deleted or send EXPUNGE, UID EXPUNGE, or CLOSE; teardown closes the connection directly. Gate native MOVE before invoking beta.9's unsafe fallback, and do not expose QRESYNC, conditional STORE, BINARY retrieval, partial body fetches, or mutation replay. Connections belong to individual operations; the planned service must hold one account lock across refresh, lookup, invalidation, and mutation and omit readonly write tools while rejecting direct writes.

## beta.9 workarounds

Cancellation closes the raw socket even during TLS or literal I/O, and deadline setters clamp upstream timeouts to the operation deadline. After a literal read fails, close the socket and wait for decoder shutdown before `Next` retries discarding it, avoiding concurrent access to the buffered reader. A post-authentication CAPABILITY round trip both refreshes capabilities and orders the authenticated-state check after beta.9's delayed LOGIN state update.

## Message parsing

Attachment IDs are dotted MIME traversal paths, including nested `message/rfc822` parts; transfer decoding preserves attachment bytes and charset conversion applies only to displayed body text. HTML output requires opt-in, and an optional Date that cannot be encoded as JSON must be omitted without losing other fields. Bound processing with a 10 MiB raw-message limit and aggregate transfer-decoded payload budget, 2 MiB attachment retrieval limit, 256 KiB header limits, depth 32, and 1,000 MIME parts.
