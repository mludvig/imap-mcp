# 0009: Serialized runtime connection

## Status

Accepted. Supersedes only per-operation connection ownership in [ADR 0003](0003-safe-imap-operations.md); read safety, epoch guards, and no mutation replay remain in force.

## Context

Repeated operations pay transport and authentication costs, including several backend calls within one live search. Shared IMAP state requires explicit ownership of selection, deadlines, cancellation, and shutdown.

## Decision

Serialize all backend operations on one authenticated runtime connection, with a cancellable gate included in the operation timeout. Before reuse, perform one NOOP bounded by the connection and remaining operation timeouts; a failed health check permits one fresh connection before commands begin, with fresh capabilities and enablement. Release clears deadlines and finishes cancellation callbacks; ten minutes of idle time closes the socket, and terminal `Client.Close` interrupts active or idle I/O before SDK handler draining.

## Consequences

Mailbox selection and UIDVALIDITY checks remain per operation where required; NOMODSEQ is selection-local, obsolete idle callbacks cannot close a later lease, and failures discard the connection without replay. The startup probe remains a separate connection closed before readiness. Per-operation redial repeats setup, pooling adds unused concurrency and state, background IDLE adds protocol coordination, and retries after commands begin risk ambiguous writes; all were rejected.
