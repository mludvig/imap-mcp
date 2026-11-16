# Mail-search lessons

2026-11-17. Findings from reported stdio and native Codex/MCP searches and a
source review. Improvements below are proposals; no runtime behavior changed.

## Observed search coverage

The initial stdio workflow covered one accessible mailbox of about 14,300
messages: 13 successful calls, including 10 searches. Sender-only pagination
returned 21 summaries; sender-constrained topic queries found no relevant
correspondence. No date filter or message reads were used. The folder containing
the later match did not yet exist.

An initial native follow-up was blocked by worker instructions prohibiting
Codex launch, not an MCP runtime failure. A later native Codex/MCP session
completed all 13 calls successfully: one mailbox listing, five searches, and
seven reads. Refreshed discovery included the new folder; candidate reads found
relevant correspondence and separated the author's reply from quoted questions.
Registration applied only to that ephemeral session.

Negatives cover the account, accessible mailboxes, and criteria at search time.
Refresh `list_mailboxes` after folder creation/import to update coverage.

| Reported measurement | Approximate result |
| --- | --- |
| Initial stdio sender/header search | 46 seconds |
| Later stdio local header searches, including live refresh | 2.1 seconds |
| Stdio remote BODY/TEXT searches, including live refresh | 3.1–7.5 seconds |
| Stdio aggregate cache-file growth | 20 KB to 153 MB |
| Native first search / later searches | 11.0 seconds / 2.2–2.8 seconds |
| Native message reads / summed tool callbacks | 0.53–0.77 seconds each / 25.0 seconds |

Cold-cache status is inferred from growth and timing; SQLite was not inspected.
The 46-second and later 2.1-second calls used different queries, so they are not
a controlled benchmark. Native timings measure tool callbacks, excluding model
reasoning and startup. Cached searches still refresh live state and fail if
refresh fails.

## Highest-priority TODO: connection reuse

Keep one authenticated runtime connection; close it after about ten minutes
without an active operation, as requested. No pool or background IDLE is needed.
Today backend operations open/close their sessions. Source-derived counts
without retries are one session for locally evaluated header search with
default/local sort, and four for BODY/TEXT or header-only search sorted by To/Cc:
refresh, remote search, and two epoch checks. STATUS inside refresh uses its
existing session. Reuse removes setup; UID reconciliation and cache work remain.

Serialize every backend operation; reset deadlines and operation/selection-local
NOMODSEQ state for each use. Cancellation must invalidate the connection;
old cancellation callbacks or idle timers must never close a later operation's
connection. Idle expiry applies only while no operation owns it. Close active
or idle connections on shutdown, detect stale connections before reuse, and
bound any read-only recovery. Preserve per-operation mailbox selection, epoch
guards, and fresh capability negotiation after reconnect. Never replay writes;
preserve `outcome_unknown`. The separate startup authentication check still
disconnects before readiness.

Future checks: repeated/concurrent calls and idle expiry; cancellation/deadline/
shutdown ownership; remote disconnects with bounded read-only recovery and no
write replay. Measure setup, refresh, and cache costs separately. Implementation
would need a new ADR superseding
[ADR 0003](../adr/0003-safe-imap-operations.md)'s connection-ownership decision.

## Small guidance improvements

Search criteria combine with AND; use separate queries for alternatives because
there is no general OR input. Add AND semantics, literal local substrings, and
first-sync cost to `search_messages` metadata, as already documented in README.
Explain that `limit` bounds results, not synchronization, and BODY/TEXT matching
is server-defined. Consider first-sync progress feedback; no timeout occurred.

1. Refresh accessible mailboxes and establish sender-only coverage.
2. Page summaries with unchanged criteria and the returned cursor.
3. Search topic variants in BODY/TEXT without a FROM constraint; use TEXT or
   separate To/Cc queries to cover quoted, forwarded, or addressed correspondence.
4. Read candidates using returned references and attribute the author's own
   reply separately from quoted questions. Report negatives with their scope.

Source checks: [tool descriptions](../../internal/server/server.go),
[search flow](../../internal/service/search.go), [sessions](../../internal/client/client.go),
[refresh](../../internal/client/fetch.go), [inputs](../../internal/server/inputs.go).
