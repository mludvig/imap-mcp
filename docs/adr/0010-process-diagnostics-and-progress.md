# 0010: Process diagnostics and progress

## Status

Accepted. Supersedes only the diagnostics privacy restriction in [ADR 0006](0006-linux-cli-startup-validation.md); stderr verbosity and stdout protocol purity remain in force.

## Context

Startup-only stderr gives little evidence for slow synchronization or failed tool calls. Operators need persistent timings and progress without coupling the log location to a configured mail cache.

## Decision

Write JSON records to `os.UserCacheDir()/imap-mcp/imap-mcp.log` with local RFC3339 timestamps and offsets, 0600 files, a 0700 directory, and 10 MiB active plus four backups. INFO records cover lifecycle and tool completion in every verbosity mode; `--debug` adds stages, durations, typed inputs, and bounded results, with human-readable stage/timing stderr. Redact the configured password, including escaped forms, while retaining other details; send serialized, increasing stage notifications only to calls that supplied a progress token, without a total.

Processes sharing this directory serialize startup normalization, active-file refresh, rotation, and each complete record append through a stable, empty 0600 `imap-mcp.log.lock` advisory flock. Recheck the active inode and size while locked, open files relative to the directory handle, and reject symlinks, multiple hard links, and files or directories owned by another user before changing content or permissions; during logging, context cancellation and Close interrupt lock waits. Oversized input becomes one bounded omission record, and existing oversized files retain only complete trailing records.

## Consequences

Logs can contain usernames, mailbox names, queries, paths, and message data; quiet mode still writes them, and retained log I/O errors fail shutdown. Stderr-only diagnostics lose postmortem evidence, cache-relative logs obscure discovery, broad redaction removes requested detail, and raw protocol dumps add unnecessary authentication exposure; all were rejected. A stage count is not a percentage, and notification display depends on the MCP client.

A process-local mutex or a cached descriptor/size cannot enforce shared retention: another process can enlarge or rename the active file between writes. The lock file stays in place when logs rotate or sinks close, uses no log retention budget, and must never be deleted while a writer may be running; locking is advisory and requires all writers to cooperate on a filesystem supporting flock.

The CLI uses `NewContext` to bind startup normalization and every logging wait to process cancellation, independently of request contexts (the MCP SDK removes root cancellation from those contexts). The original `New` remains available with a background lifecycle; its startup lock wait requires the holder to release the lock. Cancellation drops interrupted records without retaining a write error.

Client shutdown does no diagnostic I/O, so an external log lock cannot delay socket closure or lease release; normal CLI lifecycle records provide shutdown diagnostics. Idle expiry uses a client-owned logging context canceled by `Client.Close`, so its timer callback cannot stay blocked on diagnostic waits after client shutdown.
