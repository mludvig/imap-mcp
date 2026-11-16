# Message identity

## What it is

A message reference is `(mailbox, uid_validity, uid)`, scoped to the configured account. A UID alone is not a durable message identifier.

## How it works

Validate the mailbox epoch before using a reference and discard cached entries when UIDVALIDITY changes. COPY and MOVE may produce new destination UIDs; return destination references only when the server supplies a usable mapping, and expose mapping availability separately.

## Why

The cache and safe-operation choices in [ADR 0002](../adr/0002-live-sqlite-header-cache.md) and [ADR 0003](../adr/0003-safe-imap-operations.md) require explicit epochs and explicit uncertainty.

## Where it lives

`internal/mail/model.go`: `MessageRef`, `MailboxKey`, `MessagePage`, and `MutationResult`; `internal/cache/cache.go`: `canonicalKey`, `Cache.State`, and `Cache.Apply`; `internal/cache/get.go`: `Cache.Get`. `internal/client/client.go`: `checkRef`; `internal/client/fetch.go`: `session.rawMessage` and `session.finalEpoch`; `internal/client/write.go`: `validateRefs`, `session.selectForWrite`, and `copyMapping`; `internal/client/search.go`: `expandUIDs`. Account-key construction and service integration of live-search UID lookup remain planned.

## Invariants & gotchas

Sequence FETCH is used internally for literals because beta.9 misroutes a UID FETCH when UID follows the literal; verify the returned UID against the requested identity and never expose the sequence number as identity. UID ranges are bounded before expansion and iterated with uint64 so an endpoint of 4,294,967,295 terminates; stale epochs and missing destination mappings must never be guessed or rebound. The planned account namespace uses host, port, TLS mode, and username, so password changes preserve it and account changes select a different partition.
