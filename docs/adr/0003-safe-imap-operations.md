# 0003: Safe IMAP operations

## Status

Accepted.

## Context

Reading a message must not change its Seen flag, and moving to configured Trash must not introduce a permanent-delete operation. A server advertising an extension does not mean this implementation can safely expose every operation it enables.

## Decision

Use EXAMINE for read selections and BODY.PEEK for fetched headers, message bodies, and attachments; never issue EXPUNGE, UID EXPUNGE, CLOSE, or a STORE that sets `\Deleted`. Moves, including Trash, require native UID MOVE: reject COPY plus Deleted/EXPUNGE fallback because it violates the deletion policy and can affect other messages. Report advertised, enabled, and implemented capabilities separately, and report response-loss writes as `outcome_unknown` without automatically replaying them.

## Consequences

Servers without native MOVE can support reads and permitted writes but cannot support move or Trash; the capability gate must precede the upstream call because beta.9 otherwise falls back to Deleted plus EXPUNGE. Native MOVE removes source membership as part of that server operation; the client adds no separate deletion step, reports missing UID mapping explicitly, and implements no QRESYNC, conditional STORE, BINARY retrieval, partial body fetches, or mutation replay. Each operation owns a connection rather than sharing mutable IMAP session state; the service serializes cache refresh/lookup and mutations with one account lock and reinforces readonly tool omission with write checks.

## Integration findings

Rev2 native MOVE can report removed source UIDs through VANISHED without any explicit EXPUNGE command from this client. [ADR 0004](0004-backport-imap4rev2-vanished.md) preserves that native operation with a decoder backport; accepting its response does not expose QRESYNC or relax the deletion policy.
