# Cache coherence

## What it is

SQLite stores summaries and complete decoded header maps, not raw headers, bodies, or attachments. A successful live refresh is required before cached data can answer a request.

## How it works

Refresh obtains complete UID membership, headers for uncached UIDs, and flag changes, using CONDSTORE when the selected mailbox supplies a modification sequence and otherwise fetching all flags. It saves the selection's starting cursor, verifies membership and epoch again before returning, and SQLite publishes rows, membership, epoch, and cursor in one transaction. The service holds a cancellable account lock across refresh and lookup, with no SQL transaction held during network I/O, and durably invalidates affected mailboxes before mutation so a crash or lost response cannot leave a reusable pre-write checkpoint.

## Why

See [ADR 0002](../adr/0002-live-sqlite-header-cache.md) and [ADR 0008](../adr/0008-decoded-header-cache.md).

## Where it lives

`internal/cache/cache.go`: `OpenContext`, `Cache.State`, `Cache.Apply`, `Cache.Invalidate`, and `persistentFlags`; `internal/cache/schema.go`: `Cache.initialize`, schema declarations, and version markers; `internal/cache/headers.go`: `parseMetadata`; `internal/cache/query.go`: `Cache.Query`, `listPage`, `folded`, and `dateMatches`; `internal/cache/get.go`: `Cache.Get`. `internal/client/fetch.go`: `session.fetch`, `Client.Refresh`, `session.membership`, and `session.finalEpoch`; `internal/client/search.go`: `searchCriteria`; `internal/mail/model.go`: `MailboxKey`, `CacheState`, and `Refresh`. `internal/service/service.go`: `New`, `Service.key`, and `Service.lock`; `internal/service/validate.go`: `validateQuery`; `internal/service/search.go`: `Service.refresh`, `Service.SearchMessages`, and `Service.livePage`; `internal/service/write.go`: `Service.invalidate`; `cmd/imap-mcp/main.go`: `run` owns the cache lifetime.

## Invariants & gotchas

Never advance to an end-of-refresh high-water mark: concurrent flag changes could be skipped; UIDVALIDITY changes discard the old epoch, and incomplete refreshes must publish nothing or serve no stale rows. NOMODSEQ suppresses MODSEQ and CHANGEDSINCE, and `Cache.State`, `Cache.Query`, and `Cache.Get` read epoch and rows in one snapshot; MODSEQ is decimal TEXT to preserve unsigned 64-bit values. Cache and lock files are private regular files held under an exclusive process lock, account/mailbox keys partition all rows, and only INBOX spelling is canonicalized; concurrent client processes need distinct cache paths, and account serialization assumes one service per process.

`\Recent` is rev1 session state, not a persistent flag tracked by MODSEQ: omit it case-insensitively from fetched and cached summaries. `Cache.Apply` cleans legacy summaries even when CONDSTORE returns no flag changes, updating only the summary rather than rewriting headers; `Cache.Query` and `Cache.Get` also suppress legacy values defensively.

Opening recognizes only an empty unmarked database, the exact old unversioned schema, or the marked decoded schema (`application_id` IMCH, version 1). A legacy rebuild drops all rows and checkpoints under the lock and VACUUMs before publishing version 1; an IMCH/version-0 decoded schema resumes compaction, while unknown schemas fail without replacement.

Decoded maps retain all fields and repeated/empty values for arbitrary-header predicates. Apply and non-header queries read summaries without loading those maps; unfiltered descending-UID listings count and read at most `limit+1` summaries in one transaction, so the cursor does not change `total` and epoch/count/page cannot mix refresh snapshots.

## Search semantics

Decoded headers use literal substring matching after Unicode NFC normalization and case folding, never SQL LIKE; flag and keyword criteria ignore case while preserving stored spelling. SINCE is inclusive and BEFORE exclusive on the original-offset civil date, with sent-date criteria excluding absent or invalid Date values; nonzero size bounds are strict IMAP LARGER/SMALLER comparisons. Only descending UID order supports the exclusive `before_uid` cursor; BODY/TEXT and to/cc sort use live IMAP matching/collation for the whole query, which can differ from local Unicode matching.

`\Recent` in either `flags` or `not_flags` returns `invalid_argument`, including queries that would otherwise run on the server; service, client, and cache enforce this before their network or lookup work. The ordinary keyword `Recent` without a backslash remains a supported persistent flag.

Live results pass through ordered `Cache.Get` for at most 200 UIDs, with live epoch checks before and after lookup and cached-summary epoch validation. A missing UID or epoch change permits one complete refresh/research retry; another change returns `mailbox_changed` without partial data. The cursor is applied after matching so `total` counts every match.
