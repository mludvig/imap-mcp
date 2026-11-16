# Mail-search lessons

2026-11-17. Findings from reported stdio and native Codex/MCP searches and a
source review. Timing observations below precede the implemented improvements;
they are not measurements of the new connection or cache behavior.

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

## Implemented: connection reuse

Backend operations now share one authenticated runtime connection, serialize
through a cancellable gate, and close it after ten minutes without an active
operation. Previously, source-derived counts without retries were one session
for local header search with default/local sort and four for BODY/TEXT or
header-only search sorted by To/Cc: refresh, remote search, and two epoch checks.
Reuse removes repeated setup; UID reconciliation and cache work remain.

Operation deadlines are renewed and cleared, NOMODSEQ resets per selection,
and release finishes cancellation callbacks before reuse; idle generations
reject obsolete timers. A bounded pre-operation NOOP permits one reconnect
before commands begin, with fresh capabilities; operation errors discard the
connection without replay. Mailbox selection, epoch guards, `outcome_unknown`,
shutdown interruption, and the separately closed startup probe remain.

Lifecycle regressions cover reuse, concurrent calls, idle expiry, obsolete
timers, deadlines, cancellation, shutdown, selection state, failed health
checks, and no write replay. See
[ADR 0009](../adr/0009-serialized-runtime-connection.md) for the superseding
ownership decision.

## Implemented: cache, diagnostics, and guidance

SQLite now stores summaries and all decoded headers without raw copies,
rebuilds the exact legacy schema with compaction, avoids header-map loading for
non-header work, and uses bounded SQL pages for unfiltered descending-UID lists.
See [ADR 0008](../adr/0008-decoded-header-cache.md). These changes do not remove
live synchronization or establish a speedup for the reported mailbox.

Persistent bounded logs record normal lifecycle/tool timings; `--debug` adds
setup, synchronization, cache, and query detail with password-only redaction.
Token-bearing MCP calls receive increasing stage counts without a total; client
display remains client-dependent. See
[ADR 0010](../adr/0010-process-diagnostics-and-progress.md).

Search criteria combine with AND; use separate queries for alternatives because
there is no general OR input. Tool metadata now explains AND semantics, literal
local substrings, first-sync cost, and that `limit` bounds results rather than
synchronization; BODY/TEXT matching remains server-defined. Guidance also covers
fresh mailbox discovery, unchanged paging criteria, broadened sender coverage,
reply attribution, and scoped negative findings.

1. Refresh accessible mailboxes and establish sender-only coverage.
2. Page summaries with unchanged criteria and the returned cursor.
3. Search topic variants in BODY/TEXT without a FROM constraint; use TEXT or
   separate To/Cc queries to cover quoted, forwarded, or addressed correspondence.
4. Read candidates using returned references and attribute the author's own
   reply separately from quoted questions. Report negatives with their scope.

## Remaining proposals

Repeat representative cold and warm searches with controlled queries, separating
setup, refresh, and cache costs using the new timings. Verify progress display
in token-capable host clients; in-memory and real CLI protocol tests do not
verify their UI.
No live mailbox timing or host-display result is claimed for these changes.

Source checks: [tool descriptions](../../internal/server/server.go),
[search flow](../../internal/service/search.go), [sessions](../../internal/client/lifecycle.go),
[refresh](../../internal/client/fetch.go), [inputs](../../internal/server/inputs.go).
