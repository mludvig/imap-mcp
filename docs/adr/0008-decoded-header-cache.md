# 0008: Decoded header cache

## Status

Accepted. Supersedes only raw-header persistence in [ADR 0002](0002-live-sqlite-header-cache.md); live refresh and atomic cache publication remain in force.

## Context

Raw headers duplicate the decoded fields used by local search and increase cache size. Listing a bounded page should not decode every header map or summary in a mailbox.

## Decision

Store summaries and every decoded header field, including repeated and empty values, without raw header copies. Rebuild only the exact known legacy schema under the exclusive cache lock, discard its checkpoints, and VACUUM before marking schema version 1; reject foreign, customized, and future schemas rather than guess. Load header maps only for header predicates; use SQL count and descending-UID pagination for unfiltered listings in one read snapshot.

## Consequences

The first lookup after a legacy rebuild refetches the mailbox; an interrupted compaction resumes before the cache opens. Retaining raw copies or only selected headers was rejected because the former duplicates data and the latter breaks arbitrary-header searches; migrating redundant rows was rejected because this cache is recoverable from IMAP. Synchronization remains O(N) in membership, and no live performance improvement is claimed without measurement.
