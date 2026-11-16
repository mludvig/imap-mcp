# 0005: Mode-aware mailbox names

## Status

Accepted; local-module unit, race, and vet checks and the complete Dovecot integration suite passed on 2026-11-16, including independent exact mailbox-target checks.

## Context

The pinned beta.9 encoder escapes ampersands even in UTF-8 mode, and its decoder always applies modified UTF-7. A UTF-8 mailbox such as `R&D` is therefore sent as `R&-D`, potentially selecting or mutating a different mailbox; literal modified UTF-7-looking names can also be decoded into a different identity.

## Decision

Extend the existing [local replacement](0004-backport-imap4rev2-vanished.md) with mode-aware encoding and decoding: preserve raw names in active IMAP4rev2 or enabled UTF8=ACCEPT, and use modified UTF-7 in legacy IMAP4rev1. Dual-revision servers require ENABLE before switching modes; pure rev2 servers already use UTF-8. Reject disabling UTF-8/rev2 because it abandons the selected modern protocol scope, and reject escaping or unescaping application names because literal ampersand sequences and Unicode names become ambiguous.

## Consequences

Application and cache names remain decoded mailbox identities, with only INBOX canonicalized; the wire codec owns the mode transition. Keep the version pin, license, and patch provenance, and verify ampersands, literal modified UTF-7-looking names, Unicode names, and target identity in both wire modes before removing the replacement.
