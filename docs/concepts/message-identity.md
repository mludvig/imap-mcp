# Message identity

## What it is

A message reference is `(mailbox, uid_validity, uid)`, scoped to the configured account. A UID alone is not a durable message identifier.

## How it works

Validate the mailbox epoch before using a reference and discard cached entries when UIDVALIDITY changes. COPY and MOVE may produce new destination UIDs; return destination references only when the server supplies a usable mapping, and expose mapping availability separately.

## Why

The cache and safe-operation choices in [ADR 0002](../adr/0002-live-sqlite-header-cache.md) and [ADR 0003](../adr/0003-safe-imap-operations.md) require explicit epochs and explicit uncertainty. [ADR 0005](../adr/0005-mode-aware-mailbox-names.md) preserves mailbox identity across legacy and UTF-8 wire modes.

## Where it lives

`internal/mail/model.go`: `MessageRef`, `MailboxKey`, `MessagePage`, and `MutationResult`; `internal/cache/cache.go`: `canonicalKey`, `Cache.State`, and `Cache.Apply`; `internal/cache/get.go`: `Cache.Get`. `internal/client/client.go`: `checkRef`; `internal/client/fetch.go`: `session.rawMessage` and `session.finalEpoch`; `internal/client/write.go`: `validateRefs`, `session.selectForWrite`, and `copyMapping`; `internal/client/search.go`: `expandUIDs`. `internal/service/service.go`: `New` and `Service.key`; `internal/service/validate.go`: `validateRef` and `validateRefs`; `internal/service/search.go`: `Service.sameEpoch` and `Service.livePage`; `internal/server/inputs.go`: `readInput`.

`third_party/go-imap/internal/imapwire/encoder.go`: `Encoder.Mailbox` and `Encoder.QuotedUTF8`; `third_party/go-imap/internal/imapwire/decoder.go`: `Decoder.ExpectMailbox` and `Decoder.MailboxUTF8`; `third_party/go-imap/imapclient/client.go`: `Client.mailboxUTF8Locked` and `Client.readResponse`.

## Invariants & gotchas

Sequence FETCH is used internally for literals because beta.9 misroutes a UID FETCH when UID follows the literal; verify the returned UID against the requested identity and never expose the sequence number as identity. UID ranges are bounded before expansion and iterated with uint64 so an endpoint of 4,294,967,295 terminates; stale epochs and missing destination mappings must never be guessed or rebound. The account namespace hashes an unambiguous JSON tuple of normalized host (lowercase, trimmed, trailing DNS dot removed), port, TLS mode, and exact username; password changes preserve it, and only INBOX mailbox spelling is canonicalized.

Mailbox names in references, configuration, and cache keys are application identities, never pre-escaped wire strings. Preserve ampersands and Unicode verbatim in UTF-8 mode; apply modified UTF-7 only in legacy mode. For example, UTF-8 `A&-B` and `A&B` are distinct names, so decoding or compensating for ampersands in the application can redirect reads and mutations.

The encoder's `QuotedUTF8` and decoder's `MailboxUTF8` flags follow the same active mode: enabled UTF8=ACCEPT, enabled IMAP4rev2 on a dual-revision server, or a pure rev2 server. Advertising both revisions alone must not switch the codec; only the reader goroutine updates decoder mode before each response, so ENABLED changes take effect before the next mailbox response.
