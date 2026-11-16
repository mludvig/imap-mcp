# 0007: IMAP transport modes

## Status

Accepted and verified on 2026-11-17 by build, unit, race, vet, and Dovecot
integration checks. Supersedes the TLS-only policy in
[ADR 0006](0006-linux-cli-startup-validation.md).

## Context

IMAP endpoints can offer implicit TLS, STARTTLS, or plaintext, including on custom
ports. A TLS-only policy cannot serve plaintext endpoints or choose an advertised
STARTTLS upgrade automatically.

## Decision

Accept exactly `auto`, `tls`, `starttls`, and `none` for `imap.tls`, defaulting to
`auto` when omitted. Default ports are 993 for `auto`/`tls` and 143 for
`starttls`/`none`; an explicit port overrides these defaults.

On port 993, `auto` uses verified implicit TLS. On every other port, it reads one
plaintext greeting/capability set, upgrades the same connection and IMAP client
if STARTTLS is advertised, and otherwise remains plaintext; `tls` requires
implicit TLS, `starttls` requires an upgrade before client authentication, and
`none` never attempts an upgrade.

Every TLS attempt verifies certificates and hostname. Reject retrying another
mode or returning to plaintext after an advertised STARTTLS or certificate
failure, because that would turn a failed upgrade into an authenticated plaintext
session; reject probing multiple connections or clients because discovery and
upgrade can use the same session.

Expose the local client's public `StartTLS` upgrade instead of recreating it
after reading its greeting; preserve the pinned dependency and existing decoder
state. Startup reports actual implicit TLS, STARTTLS, or plaintext transport
before client authentication, validates the authenticated session, and closes
it before MCP readiness.

## Consequences

On non-993 ports, `auto` depends on a plaintext STARTTLS advertisement and accepts
plaintext when it is absent; it does not require encryption. Custom implicit-TLS
ports need explicit `tls`, and operators who require an upgrade choose
`starttls`.

Reject plaintext PREAUTH for required STARTTLS and for `auto` when STARTTLS is
advertised, since an authenticated session cannot be upgraded. `none` and `auto`
without advertised STARTTLS accept PREAUTH as already authenticated.

The readonly boundary, stdio-only MCP transport, cache ownership, and live-session
readiness requirements remain unchanged. This adds no QRESYNC or MIME behavior;
see the [IMAP safety](../concepts/imap-safety.md) and
[stdio contract](../concepts/stdio-mcp.md) invariants.
