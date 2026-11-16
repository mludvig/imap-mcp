# IMAP safety

## What it is

Read-only operation is a startup boundary, and read tools are side-effect-free even when write tools are enabled. Trash is a move to the configured mailbox, not a permanent-delete endpoint.

## How it works

TOML configuration defaults to readonly; service and client reject writes using that immutable value, and MCP registration exposes eight read tools or fourteen total tools at startup, with changes requiring a restart. Reads select with EXAMINE and fetch with BODY.PEEK; Trash uses exactly the configured mailbox, checking native UID MOVE before creating a missing destination. Capability reporting separates advertised, enabled, and implemented features, and lost mutation responses set `outcome_unknown` without replay.

## Why

See [ADR 0001](../adr/0001-go-stdio-startup-policy.md), [ADR 0003](../adr/0003-safe-imap-operations.md), [ADR 0004](../adr/0004-backport-imap4rev2-vanished.md), and [ADR 0007](../adr/0007-imap-transport-modes.md).

## Where it lives

`internal/config/config.go`: `Config`, `Load`, and `IMAPConfig.TLSConfig`; `internal/mail/model.go`: `ServerInfo`, `Error`, and the size limits. `internal/client/client.go`: `Client.open`, `deadlineConn`, and `session.close`; `internal/client/fetch.go`: `session.fetch`, `session.failedLiteral`, and `Client.Refresh`; `internal/client/write.go`: `Client.writeGuard`, `mutationFlags`, and `Client.transfer`; `internal/mime/mime.go`: `Parse`, `Attachment`, `newEntity`, and `scanner.walk`. `internal/service/service.go`: `Service.lock`; `internal/service/write.go`: `Service.writeGuard`, `Service.invalidate`, and `Service.TrashMessages`; `internal/server/server.go`: `New`; `cmd/imap-mcp/main.go`: `run`.

`third_party/go-imap/imapclient/client.go`: VANISHED dispatch and `UnilateralDataHandler.Vanished`; `third_party/go-imap/imapclient/vanished.go`: `Client.handleVanished` and `UnilateralDataVanished`. `go.mod` selects the local replacement; the subtree retains the upstream MIT `LICENSE`.

`third_party/go-imap/imapclient/starttls.go`: the local public `Client.StartTLS` upgrade on the existing client after capability discovery.

## Invariants & gotchas

No read path may set Seen, and no command may add Deleted or send EXPUNGE, UID EXPUNGE, or CLOSE; teardown closes the connection directly. Gate native MOVE before invoking beta.9's unsafe fallback, and do not expose QRESYNC, background IDLE, conditional STORE, BINARY retrieval, partial body fetches, saved-search handles, or mutation replay. Connections belong to individual operations; the service holds one account lock across refresh/lookup and invalidation/mutation, while direct reads own independent connections and epoch checks.

Generic transfers never create destinations; Trash rejects already-in-Trash references and a Noselect destination, and matches mailbox spelling exactly except INBOX. Both affected mailboxes are invalidated before Trash capability/list/create calls, including ambiguous creation failures, and the client independently gates native MOVE immediately before the upstream call.

`auto` is the default, using verified implicit TLS only on numeric port 993; every other port starts with a plaintext greeting/capabilities and upgrades the same client only when STARTTLS is advertised. Absent advertisement means a normal plaintext session; use explicit `tls` for custom implicit-TLS ports, `starttls` to require an upgrade, or `none` to remain plaintext even when STARTTLS is advertised.

Complete any required or advertised upgrade before sending client authentication; a STARTTLS or certificate failure closes the session without plaintext fallback or another connection attempt. Reject plaintext PREAUTH for required STARTTLS or `auto` with advertised STARTTLS; `none` and unadvertised `auto` accept PREAUTH as already authenticated.

## beta.9 workarounds

Cancellation closes the raw socket even during TLS or literal I/O, and deadline setters clamp upstream timeouts to the operation deadline. After a literal read fails, close the socket and wait for decoder shutdown before `Next` retries discarding it, avoiding concurrent access to the buffered reader. A post-authentication CAPABILITY round trip both refreshes capabilities and orders the authenticated-state check after beta.9's delayed LOGIN state update.

The local decoder accepts VANISHED with a static UID set and optional `(EARLIER)` modifier, allowing native MOVE to complete with IMAP4rev2 enabled. Ordinary removals reduce `SelectedMailbox.NumMessages` without underflow; `(EARLIER)` reports historical removals and must not reduce the current count. Count normalized ranges with uint64 arithmetic without enumerating UIDs, copy the mailbox before updating its count, and invoke the optional callback after unlocking; never synthesize sequence-number EXPUNGE events from removed UIDs.

## Message parsing

Attachment IDs are dotted MIME traversal paths, including nested `message/rfc822` parts; transfer decoding preserves attachment bytes and charset conversion applies only to displayed body text. HTML output requires opt-in, and an optional Date that cannot be encoded as JSON must be omitted without losing other fields. Bound processing with a 10 MiB raw-message limit and aggregate transfer-decoded payload budget, 2 MiB attachment retrieval limit, 256 KiB header limits, depth 32, and 1,000 MIME parts.
