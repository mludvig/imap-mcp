# Local go-imap client distribution

Source: https://github.com/emersion/go-imap/tree/v2.0.0-beta.9

- Module: `github.com/emersion/go-imap/v2`, version `v2.0.0-beta.9`.
- Upstream commit: `99eeae5b37d84f322b5f7b9b42bd95c92a0eaab1`.
- Go module archive checksum: `h1:ttFRU4F9A2cpHlhBMpEt5rtSArYxBVU8+lqP0QeUmuc=`.
- License: upstream MIT license, preserved in `LICENSE`.

The files were copied directly from the repository's beta.9 module cache. This
distribution contains all root Go protocol types, production `imapclient`
sources, their required `internal` packages, and upstream `go.mod`/`go.sum`.
Root/internal tests and the self-contained client notification encoder tests
are retained unchanged. Server packages, command examples, and client tests
that use the upstream server test harness or external Dovecot are omitted.

Changed upstream Go source files:

- `imapclient/client.go`: VANISHED dispatch/callback and mailbox wire mode.
- `imapclient/starttls.go`: public same-client upgrade and decoder-shutdown handling.
- `internal/imapwire/encoder.go`: raw UTF-8 mailbox names in active modern mode.
- `internal/imapwire/decoder.go`: mode-aware mailbox decoding.

The VANISHED patch dispatches to the added `imapclient/vanished.go` and exposes
an optional `UnilateralDataHandler.Vanished` callback. The handler parses static UID sets
and optional `(EARLIER)`, counts ranges with `uint64` without expanding UIDs,
and reduces the selected message count only for current removals (clamped at
zero). Callbacks receive the UID set and historical marker outside the client
mutex; no message sequence numbers are invented. `imapclient/vanished_test.go`
contains independent protocol peer regressions.

This patch permits native IMAP4rev2 MOVE completion without downgrading the
connection. It does not implement complete QRESYNC synchronization. See
[`docs/adr/0004-backport-imap4rev2-vanished.md`](../../docs/adr/0004-backport-imap4rev2-vanished.md).

The mailbox patch preserves raw UTF-8 names, including literal ampersands and
modified UTF-7-looking sequences, after ENABLED confirms UTF8=ACCEPT or
IMAP4rev2. Pure IMAP4rev2 servers use UTF-8 from their advertised capabilities;
dual IMAP4rev1/IMAP4rev2 servers retain modified UTF-7 until enabled (RFC 9051,
Appendix A). Both wire directions use the same locked capability state, and
only the reader goroutine updates decoder mode, before each response.
Invalid raw UTF-8 is rejected; INBOX canonicalization and existing string
quoting/literal rules are preserved. The added `internal/imapwire/mailbox_test.go`
and `imapclient/mailbox_utf8_test.go` cover both modes and exact LIST, STATUS,
COPY, APPEND, and MOVE mailbox targets, including responses and commands before
ENABLE's tagged completion and ignored or rejected ENABLE requests. See
[`docs/adr/0005-mode-aware-mailbox-names.md`](../../docs/adr/0005-mode-aware-mailbox-names.md).

The STARTTLS patch exposes `Client.StartTLS(*tls.Config) error` after the initial
greeting, rejecting authenticated clients before sending an upgrade command.
It upgrades the existing connection, buffered reader, and decoder, keeping the
command encoder locked through the TLS handshake and preserving capability
refresh and caller-supplied certificate verification. Capabilities supplied in
the plaintext STARTTLS completion are discarded and fetched again over TLS.
The existing `NewStartTLS` constructor retains its private upgrade flow. A truncated or
malformed tagged OK now returns the decoder error instead of waiting forever
for an upgrade that cannot run. The added `imapclient/starttls_upgrade_test.go` uses
independent loopback peers and fresh certificates to cover discovery and
same-client upgrades, capability invalidation, authentication after TLS,
greeting/state guards, certificate rejection, handshake serialization, and
malformed completions. See
[`docs/adr/0007-imap-transport-modes.md`](../../docs/adr/0007-imap-transport-modes.md).

Run the retained upstream tests and focused regressions from this directory
with `go test ./...`.
