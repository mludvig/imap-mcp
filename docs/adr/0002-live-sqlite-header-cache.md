# 0002: Live SQLite header cache

## Status

Accepted.

## Context

Repeated mailbox listings and header searches should avoid repeatedly downloading unchanged headers while still consulting the IMAP server. An in-memory cache loses this work on restart; storing bodies or serving an offline snapshot would enlarge both the privacy footprint and the consistency contract.

## Decision

Persist raw headers, metadata, flags, membership, and mailbox epochs in a local SQLite cache, refresh before use, and fail rather than return stale data after refresh failure. Use CONDSTORE where available for changed flags plus a complete UID membership reconciliation; QRESYNC is not implemented because reconciliation already handles removals without its additional protocol state.

## Consequences

Initial synchronization remains O(N) in mailbox membership and headers; later refreshes fetch new headers and flag changes but still reconcile all UIDs, with a full flag refresh when CONDSTORE is unavailable or a mailbox reports NOMODSEQ. Publish each refresh atomically from its starting modification-sequence cursor and read epochs with rows in one SQLite snapshot; the service durably invalidates affected mailboxes before writes and partitions accounts by host, port, TLS mode, and username rather than passwords. Each private, unencrypted cache file has one exclusive process owner; local header searches use Unicode NFC case folding and original-offset civil dates, so they can differ from server searches, and neither bodies nor offline results are cached.

## Integration findings

Rev1 `\Recent` belongs to an IMAP session, and its changes are not tracked by MODSEQ, so incremental refresh cannot make a persisted value reliable. Exclude it from durable flags, clean legacy cached values even when a refresh has no flag delta, and reject `\Recent` search predicates rather than represent session state as a persistent message property.
