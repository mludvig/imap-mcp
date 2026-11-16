# Cache coherence

## What it is

SQLite stores a reusable header and metadata view, not an offline mailbox. A successful refresh is required before cached data can answer a request.

## How it works

Refresh obtains complete UID membership, headers for uncached UIDs, and flag changes, using CONDSTORE when the selected mailbox supplies a modification sequence and otherwise fetching all flags. It saves the selection's starting cursor, verifies membership and epoch again before returning, and SQLite publishes rows, membership, epoch, and cursor in one transaction. The planned service must refresh before answering and durably invalidate affected mailboxes before mutation, so a crash or lost response cannot leave a reusable pre-write checkpoint.

## Why

See [ADR 0002](../adr/0002-live-sqlite-header-cache.md).

## Where it lives

`internal/cache/cache.go`: `Open`, `Cache.State`, `Cache.Apply`, and `Cache.Invalidate`; `internal/cache/headers.go`: `parseMetadata`; `internal/cache/query.go`: `Cache.Query`, `folded`, and `dateMatches`; `internal/cache/get.go`: `Cache.Get`. `internal/client/fetch.go`: `Client.Refresh`, `session.membership`, and `session.finalEpoch`; `internal/mail/model.go`: `MailboxKey`, `CacheState`, and `Refresh`. Service refresh orchestration and account-key construction remain planned.

## Invariants & gotchas

Never advance to an end-of-refresh high-water mark: concurrent flag changes could be skipped; UIDVALIDITY changes discard the old epoch, and incomplete refreshes must publish nothing or serve no stale rows. NOMODSEQ suppresses MODSEQ and CHANGEDSINCE, and `Cache.State`, `Cache.Query`, and `Cache.Get` read epoch and rows in one snapshot; MODSEQ is decimal TEXT to preserve unsigned 64-bit values. Cache and lock files are private regular files held under an exclusive process lock, account/mailbox keys partition all rows, and only INBOX spelling is canonicalized.

## Search semantics

Decoded headers use literal substring matching after Unicode NFC normalization and case folding, never SQL LIKE; flag and keyword criteria ignore case while preserving stored spelling. SINCE is inclusive and BEFORE exclusive on the original-offset civil date, with sent-date criteria excluding absent or invalid Date values. Only descending UID order supports the exclusive `before_uid` cursor; BODY and TEXT require live IMAP search, whose bounded `Cache.Get` lookup preserves requested order and fails on missing UIDs.
