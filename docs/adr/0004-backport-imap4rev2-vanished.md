# 0004: Backport IMAP4rev2 VANISHED decoding

## Status

Accepted.

## Context

Real Dovecot integration found that native MOVE under enabled IMAP4rev2 returns VANISHED, which the pinned `go-imap/v2` beta.9 client cannot decode. Modern IMAP support and the no-permanent-delete policy require preserving IMAP4rev2 and native MOVE rather than substituting COPY, Deleted, and EXPUNGE.

## Decision

Keep `github.com/emersion/go-imap/v2 v2.0.0-beta.9` pinned and distribute a local replacement module in `third_party/go-imap`, retaining the upstream version, provenance, and MIT license with a narrow VANISHED decoder backport. Reject forcing IMAP4rev1 or replacing IMAP4rev2 enablement with UTF8=ACCEPT because it abandons the selected modern protocol scope, and reject a line-based wire filter because literals and IMAP response grammar make stream rewriting unsafe. Handle VANISHED in the IMAP decoder without adding full QRESYNC or conditional STORE support.

## Consequences

The local client copy becomes part of the source distribution and must be included in builds; record its differences from beta.9 and preserve upstream license notices. Verify native rev2 MOVE, VANISHED parsing, selected-mailbox counts, and the existing deletion-policy checks before declaring the integration failure fixed. Remove the replacement only after a pinned upstream release implements the needed handling and passes those checks.

## Verification

The initial VANISHED backport's recorded checks passed native MOVE and Trash with IMAP4rev2 enabled, both-revision cache refresh, and deletion-policy checks on 2026-11-16, plus local-module unit, race, and vet checks. Those results predate the mailbox codec correction; rerun the explicit nested-module and integration commands in [Development and integration](../../README.md#development-and-integration) for the current distribution, because root `./...` does not cover this nested module.

## Additional finding

The same beta.9 distribution also rewrites ampersands in UTF-8 mailbox names and decodes them as modified UTF-7 regardless of mode. [ADR 0005](0005-mode-aware-mailbox-names.md) expands this replacement's patch scope to the wire mailbox encoder/decoder and client mode propagation; keep the changed-file inventory in the [local-module provenance](../../third_party/go-imap/README.md) current. This adds a focused correctness patch to the existing distribution decision without replacing the dependency or reducing modern protocol support.
