# imap-mcp

A local, single-account IMAP server for MCP clients. Search and read mail without
marking it Seen; optionally enable flags, copy, move, Trash, and APPEND. It uses
Go, TOML configuration, the official Go MCP SDK, and stdio. There is no HTTP
listener, SMTP sending, raw IMAP command tool, or permanent-delete tool.

## Build and configure

Install Go **1.25 or newer**, then run from this checkout:

```sh
make
imap_mcp_config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/imap-mcp"
install -d -m 700 "$imap_mcp_config_dir"
install -m 600 config.example.toml "$imap_mcp_config_dir/config.toml"
```

The binary is `bin/imap-mcp`. Edit the private copy of
[config.example.toml](config.example.toml), setting the IMAP host, username, and
password. Credentials are stored directly in TOML; keep this file private and
outside version control.

```toml
readonly = true
trash_mailbox = "Trash"

[imap]
host = "imap.example.com"
port = 993
username = "user@example.com"
password = "your-password"
tls = "auto"
connect_timeout_seconds = 10
operation_timeout_seconds = 60
# ca_file = "private-ca.pem"

[cache]
path = "headers.sqlite3"
```

`imap.tls` accepts these exact values; an explicit `port` overrides the default:

| Mode | Default port | Connection behavior |
| --- | --- | --- |
| `auto` (also when omitted) | 993 | Verified implicit TLS on port 993; on every other port, read the plaintext greeting/capabilities and upgrade if STARTTLS is advertised, otherwise remain plaintext |
| `tls` | 993 | Require verified implicit TLS on the chosen port |
| `starttls` | 143 | Require a verified STARTTLS upgrade before client authentication |
| `none` | 143 | Use plaintext, even when STARTTLS is advertised |

`auto` depends on the server's plaintext STARTTLS advertisement on non-993
ports; plaintext without that advertisement is normal successful behavior.
Use explicit `tls` for implicit TLS on a custom port, or `starttls` to require
an upgrade. Auto detection uses one connection and upgrades the same IMAP client;
a failed advertised STARTTLS or certificate check fails the connection without
plaintext fallback or retry. Every TLS connection verifies the certificate and
hostname; `ca_file` adds a private CA to the system trust roots.

A plaintext PREAUTH greeting is accepted by `none` and by `auto` without a
STARTTLS advertisement; the session is already authenticated. Required STARTTLS,
or `auto` with advertised STARTTLS, rejects plaintext PREAUTH.

Relative `cache.path` and `imap.ca_file` paths resolve beside the TOML file.
An empty cache path uses the OS user cache directory at
`imap-mcp/headers.sqlite3`. Unknown configuration keys are errors.

Start the stdio server with the default configuration:

```sh
./bin/imap-mcp
```

The default is `os.UserConfigDir()/imap-mcp/config.toml`: on Linux,
`$XDG_CONFIG_HOME/imap-mcp/config.toml` when `XDG_CONFIG_HOME` is nonempty, otherwise
`$HOME/.config/imap-mcp/config.toml`. `--config` overrides discovery and accepts
either an absolute path or a path relative to the process's working directory:

```sh
/absolute/path/to/imap-mcp/bin/imap-mcp --config /absolute/path/to/config.toml
./bin/imap-mcp --config ./private-config.toml
```

Normally your MCP client starts this process. Standard output carries MCP
JSON-RPC; help and diagnostics use standard error. `-h` or `--help` prints the
full usage and options without loading configuration or connecting.

| Verbosity | Diagnostics on stderr |
| --- | --- |
| Default | Resolved config/cache/log paths, readonly mode, actual IMAP transport, connection-check progress, readiness |
| `--quiet` | Errors only |
| `--verbose` | Default output plus lifecycle detail |
| `--debug` | Verbose output plus startup and tool stages and timings |

The enabled `--quiet`, `--verbose`, and `--debug` modes are pairwise mutually
exclusive. Standard output remains MCP JSON-RPC only.

Persistent JSON logs use `os.UserCacheDir()/imap-mcp/imap-mcp.log`, independent
of `cache.path`. On Linux this is `$XDG_CACHE_HOME/imap-mcp/imap-mcp.log`, or
`$HOME/.cache/imap-mcp/imap-mcp.log` when XDG_CACHE_HOME is unset. The directory
is 0700 and files are 0600; retention is at most 50 MiB: 10 MiB active plus
four backups (`imap-mcp.log.1` through `.4`). Timestamps use local RFC3339 time
with a numeric offset, including `+00:00` in UTC. Processes sharing the log
directory serialize appends and rotation through the stable, empty
`imap-mcp.log.lock`; the bounds require cooperating writers and filesystem
flock support. Never delete the lock file while a writer can run.

Normal logs include lifecycle events, errors, and tool completion timings even
with `--quiet`. `--debug` adds connection, synchronization, cache, and query
stages/timings plus tool inputs and bounded results. Only the configured
password is redacted; usernames, mailbox names, paths, queries, and message
data may be logged. No raw IMAP protocol dump is produced.

Startup loads the configuration, opens the cache, and establishes the configured
IMAP transport. Diagnostics report actual implicit TLS, STARTTLS, or plaintext
transport before client authentication. The check validates an authenticated
session within `connect_timeout_seconds` and `operation_timeout_seconds`, closes
it, and only then reports MCP readiness. Runtime backend operations share one
authenticated connection and run serially; their timeout includes waiting for
that connection. A reused session gets a bounded NOOP health check; failure
permits one fresh connection before the operation starts, with no command or
write replay. Operations using message references still select their mailbox
and check UIDVALIDITY.
Ten minutes without an active operation closes the runtime connection, and
shutdown interrupts active or idle I/O.
A failed check exits with status 1 instead of advertising offline readiness.

| Exit status | Meaning |
| --- | --- |
| 0 | Help, normal stdio EOF, or SIGINT/SIGTERM shutdown |
| 1 | Configuration, cache, log, startup connection check, stdio server, or resource-close failure |
| 2 | Invalid arguments, including conflicting verbosity modes |

Configuration is loaded once.
`readonly` defaults to `true`, exposes only the eight read tools, and still
allows local cache writes. Set it to `false` and restart the server process to
enable the six write tools. Editing a running process's config changes neither
its mode nor its tool inventory.

## Connect a client

Replace the absolute paths below. If Codex and Claude Code run simultaneously,
give each process its own TOML file with a distinct `cache.path`: a cache file
has one exclusive process owner, even for the same account.

### Codex

Following the [official Codex MCP instructions](https://developers.openai.com/codex/mcp),
add this entry to `~/.codex/config.toml` or the trusted project's
`.codex/config.toml`:

```toml
[mcp_servers.imap]
command = "/absolute/path/to/imap-mcp/bin/imap-mcp"
args = ["--config", "/absolute/path/to/codex-imap.toml"]
```

Use `/mcp` in the Codex TUI to inspect connected servers.

### Claude Code

The [official Claude Code MCP instructions](https://code.claude.com/docs/en/mcp#option-3-add-a-local-stdio-server)
support registering the binary as a local stdio process:

```sh
claude mcp add --transport stdio --scope local imap -- \
  /absolute/path/to/imap-mcp/bin/imap-mcp \
  --config /absolute/path/to/claude-imap.toml
claude mcp list
```

Local scope is private to the current project and stored in `~/.claude.json`.
Alternatively, merge this configuration into a project `.mcp.json`:

```json
{
  "mcpServers": {
    "imap": {
      "type": "stdio",
      "command": "/absolute/path/to/imap-mcp/bin/imap-mcp",
      "args": ["--config", "/absolute/path/to/claude-imap.toml"]
    }
  }
}
```

Project scope is shared through that file and Claude Code asks to approve its
servers. Use `/mcp` within Claude Code to inspect status.

## Tools and results

Read tools always use EXAMINE and BODY.PEEK for message reads, including when
write tools are enabled.

| Read tool | Input and result |
| --- | --- |
| `server_info` | `{}` → advertised capabilities, enabled capabilities, implemented features, startup readonly mode |
| `list_mailboxes` | `{}` → names, delimiters, attributes |
| `mailbox_status` | Optional `mailbox` → counts, UIDNEXT, UIDVALIDITY, MODSEQ |
| `list_messages` | Optional `mailbox`, `limit`, `before_uid`, `sort` → header summary page |
| `search_messages` | Optional criteria below → refreshed summary page |
| `read_message` | Required `ref`; optional `include_html`, `max_text_chars` → text, opt-in HTML, attachment metadata |
| `get_attachment` | Required `ref`, `part_id` → metadata and `data_base64` |
| `thread_messages` | Optional `mailbox`, `algorithm`, `query` → server THREAD tree; algorithm defaults to `REFERENCES`, also accepts `ORDEREDSUBJECT` |

Mailbox inputs default to `INBOX`. A nested thread query defaults to its
enclosing mailbox. THREAD requires the requested server-supported algorithm;
its result contains complete server-provided trees for the matching criteria.
The nested query accepts search criteria but excludes `limit`, `before_uid`,
and `sort`; narrow the criteria if the complete result exceeds the response
budget.

Search accepts `mailbox`, `from`, `to`, `cc`, `bcc`, `subject`, `body`, `text`,
`headers` (a field-name-to-string object), `since`, `before`, `sent_since`,
`sent_before`, `flags`, `not_flags`, `min_size`, `max_size`, `limit`,
`before_uid`, and `sort`. Criteria combine with AND. Examples of tool arguments:

```json
{"subject":"invoice","from":"supplier@example.com","since":"2026-01-01"}
```

```json
{"headers":{"X-Project":"Orion"},"not_flags":["\\Seen"],"limit":50}
```

Use separate queries for topic or sender alternatives. After folder creation or
mail import, refresh `list_mailboxes` before claiming coverage. Keep criteria
unchanged while paging; broaden FROM for forwarded or quoted correspondence,
read candidate replies separately from quoted questions, and state the scope
of negative findings.

A successful tool's `structuredContent` is `{"data":...}`; the same JSON appears
in its text fallback. A search/list page contains `messages`, `uid_validity`,
`total`, and an optional `next_before_uid`. Each summary includes a complete
`ref`, decoded subject and addresses, dates, size, flags, and optional MODSEQ.
Use a returned reference verbatim, for example:

```json
{"ref":{"mailbox":"INBOX","uid_validity":12345,"uid":42},"max_text_chars":20000}
```

Use `part_id` from that read's attachment metadata for `get_attachment`.
References are scoped to the configured account; a UID alone is insufficient.
An epoch change returns `stale_reference`, requiring a fresh list/search.

Domain failures set MCP `isError: true`, omit `data`, and return:

```json
{"error":{"code":"stale_reference","message":"Message reference belongs to an old mailbox epoch","outcome_unknown":false}}
```

The message above is illustrative; handle the `code`, not the wording.
SDK argument/schema errors may use only MCP error text. An ambiguous write
returns `outcome_unknown: true`: inspect current server state before deciding
what to do, because mutations are never automatically retried.

### Write tools

These appear only after startup with `readonly = false`.

| Write tool | Required input |
| --- | --- |
| `add_flags`, `remove_flags` | `refs`, `flags` |
| `copy_messages`, `move_messages` | `refs`, `destination` |
| `trash_messages` | `refs` |
| `append_message` | `message_base64`; optional `mailbox`, `flags`, RFC3339 `internal_date` |

Example arguments for `copy_messages`:

```json
{"refs":[{"mailbox":"INBOX","uid_validity":12345,"uid":42}],"destination":"Archive"}
```

Use 1–200 distinct references from one mailbox and UIDVALIDITY per mutation.
Mutation results contain `affected`, `destination_refs`, `mapping_available`,
and `outcome_unknown`. Destination references are returned only when the server
provides a usable UIDPLUS mapping; missing mappings are never guessed.

Copy and move require an existing destination. Trash moves to exactly
`trash_mailbox` (default `Trash`) and creates that mailbox if missing, only after
checking native MOVE support. It does not discover or choose another special-use
folder. Moving messages already in the configured Trash is rejected.
Native MOVE is required: there is no COPY/Deleted/EXPUNGE fallback. No operation
adds `\Deleted`, sends EXPUNGE/UID EXPUNGE/CLOSE, or permanently deletes mail.
Removing an existing `\Deleted` flag is permitted; changing `\Recent` is not.
APPEND stores an RFC 5322 message in IMAP and does not send it through SMTP.

## Search and cache behavior

SQLite persists summaries, all decoded header fields (including repeated and
empty values), flags, membership, and mailbox epochs. It stores no raw headers,
bodies, or attachments. Files are private (0600),
new directories use 0700, and the cache is **unencrypted**: headers can contain
sensitive information. Existing parent directory permissions are retained.

The exact old raw-header cache schema rebuilds automatically under the exclusive
lock: all rows and checkpoints are discarded and SQLite compacts the file.
The next request refetches headers. Current caches reopen without rebuilding;
unknown, customized, foreign, or future schemas fail without replacement.

Every list/search first refreshes against the live IMAP server. The first sync
is O(N) in mailbox membership and headers. Later searches fetch new headers and
use CONDSTORE for flag changes when available, while still reconciling all UIDs;
without CONDSTORE or with NOMODSEQ they fetch all flags. A failed refresh returns
an error, never an offline or stale answer. Writes durably invalidate affected
mailboxes before dispatch. Account partitions use host, port, TLS mode, and
username; changing only a password preserves the partition.

`limit` bounds the returned page, not header synchronization. Unfiltered
descending-UID listings count matches and fetch a bounded SQL page in one
snapshot; non-header queries avoid loading decoded header maps.

Calls that supply an MCP progress token receive synchronization, fetch, cache,
and query stage messages with increasing counts and no total. These counts are
not percentages, and display depends on the client. Calls without a token can
use `--debug` stages in stderr and the log.

Header matching is a literal substring after Unicode NFC normalization and case
folding. Flags and keywords compare without case sensitivity. Session-specific
`\Recent` is omitted from summaries and persistent cache flags because its
changes are not tracked by MODSEQ; searching for it in `flags` or `not_flags`
returns `invalid_argument`. The keyword `Recent` without a backslash remains
supported. `body`/`text`
criteria run on the server, as do `to`/`cc` sorts; when a query takes that route,
the server evaluates its other criteria too. Its matching and SORT collation can
differ from local header matching.

Dates use `YYYY-MM-DD` civil dates with their original timezone offset, not a
UTC day conversion. `since` is inclusive and `before` exclusive, using IMAP
INTERNALDATE (received/stored date); `sent_since`/`sent_before` use the message
Date header and exclude missing or invalid dates. Nonzero `min_size` and
`max_size` are strict byte bounds: `size > min_size`, `size < max_size`; zero
omits the bound.

Pages default to 50 messages, maximum 200, ordered by descending UID. Pass
`next_before_uid` as the next request's exclusive `before_uid` with the same
criteria. `total` counts all matches before the cursor. Only descending UID
supports pagination; other sorts return the first bounded page. Sort keys are
`uid`, `arrival`, `date`, `from`, `to`, `cc`, `subject`, and `size`, with `_asc`
or `_desc` suffixes. Bare non-UID keys are ascending. Cached header queries can
sort locally except `to`/`cc`; live non-UID sorting requires server SORT.

## Protocol support and limits

`server_info` separates server-advertised capabilities, capabilities enabled
on the current connection, and implementation features. Advertising a capability
does not expose every related command.

| Feature | Implemented use |
| --- | --- |
| IMAP4rev1 / IMAP4rev2, UTF8=ACCEPT | Capability-aware enablement and Unicode mailbox handling |
| CONDSTORE | Incremental read refresh; complete UID reconciliation still runs |
| ESEARCH | UID SEARCH with MIN/MAX/ALL/COUNT when available |
| LIST-EXTENDED, LIST-STATUS, SPECIAL-USE | Richer mailbox listing when supported |
| NAMESPACE | Queried for feature discovery; no namespace-selection tool |
| SORT | Native UID SORT for live non-UID sorts |
| THREAD | Native UID THREAD for supported REFERENCES/ORDEREDSUBJECT |
| UIDPLUS, MOVE | Authoritative destination mappings and guarded native moves |

The pinned IMAP client includes local corrections for VANISHED responses during
native IMAP4rev2 MOVE, mode-aware mailbox names, and a public STARTTLS upgrade on
the same client; see [ADR 0004](docs/adr/0004-backport-imap4rev2-vanished.md),
[ADR 0005](docs/adr/0005-mode-aware-mailbox-names.md), and
[ADR 0007](docs/adr/0007-imap-transport-modes.md). Mailbox inputs and
returned names preserve Unicode and literal ampersands: the wire codec uses
raw UTF-8 in modern mode and modified UTF-7 in legacy IMAP4rev1.
This preserves mailbox identity without adding full Net-Unicode normalization
or repertoire validation.
Include [the local module](third_party/go-imap/README.md) and its preserved
[upstream MIT license](third_party/go-imap/LICENSE) when distributing or building
this source tree.

QRESYNC, background IDLE, conditional STORE, BINARY retrieval, partial body
fetches, and saved-search/SAVE handles are not exposed. There is no general
raw-command escape hatch.

| Limit | Behavior |
| --- | --- |
| Raw message / decoded APPEND | 10 MiB |
| Aggregate transfer-decoded MIME payload | 10 MiB |
| Headers | 256 KiB |
| Retrieved decoded attachment | 2 MiB, returned as base64 |
| Displayed plain text / opt-in HTML | 20,000 characters each by default; `max_text_chars` up to 200,000; reports truncation |
| MIME structure | Depth 32, maximum 1,000 parts |
| Serialized MCP tool result | At most 8 MiB minus 1 KiB, counting structured JSON and escaped text fallback |

Oversized data fails explicitly (`too_large`, `message_too_large`,
`attachment_too_large`, or `response_too_large`), rather than returning a
partial attachment or page.
The budget applies to the serialized SDK tool result before JSON-RPC framing.
The SDK echoes request IDs outside the typed tool handler, so the 1 KiB
allowance does not guarantee a complete response below 8 MiB for arbitrarily
large valid IDs. HTML, headers, filenames, and mail contents are untrusted data.

## Development and integration

```sh
make
make test
make integration
```

The Makefile prefers `.tools/go/bin/go` when installed, otherwise `go`, and
keeps its Go caches and temporary files inside `.tools/`. Plain `make` builds
`bin/imap-mcp`; `make build` remains available. `make clean` removes `bin/`,
the repo-local Go build cache (`.tools/cache/build/`), and build/test scratch
(`.tools/tmp/`), retaining the installed toolchain, downloaded dependencies, and
mail cache. To run race and vet checks with the same environment:

```sh
export GOTOOLCHAIN=local
export GOPATH="$PWD/.tools/cache/gopath"
export GOMODCACHE="$PWD/.tools/cache/mod"
export GOCACHE="$PWD/.tools/cache/build"
export TMPDIR="$PWD/.tools/tmp"
go test -race ./...
go vet ./...
```

Run `make prepare` first if the local directories do not exist. If the Makefile
selected `.tools/go/bin/go`, replace `go` with that path in the direct commands.
Dependencies are pinned in [go.mod](go.mod).

Root `./...` checks do not include the replacement's nested module. From the
repository root, reuse the exports above and run its retained upstream tests,
VANISHED and mailbox codec regressions, race checks, and vet explicitly:

```sh
(
  cd third_party/go-imap
  ../../.tools/go/bin/go test -mod=readonly -count=1 -timeout=30s ./...
  ../../.tools/go/bin/go test -mod=readonly -race -count=1 -timeout=45s ./...
  ../../.tools/go/bin/go vet -mod=readonly ./...
)
```

If using Go from PATH, substitute `go` for `../../.tools/go/bin/go`.

`make integration` requires Docker and uses the pinned Dovecot 2.4.5 fixture.
It exercises the real CLI over SDK MCP stdio, IMAP4rev1/rev2, implicit TLS and
STARTTLS, Unicode headers/mailboxes, MIME attachments, pagination, flag/date/byte
preservation, startup tool inventories, mutations, and persistent refresh.
Runtime lifecycle checks verify session reuse, forced-disconnect recovery,
shutdown release, and Seen preservation; a dropped committed APPEND response
verifies `outcome_unknown` and exactly one stored copy after a later reconnect.
Each fixture uses synthetic mail and credentials, random ports bound to
127.0.0.1, an owned container/volume, and registered cleanup. See the
[reproducible testbed](testbed/README.md) for setup and fixture details.

Before the CLI startup changes, the build, root and local-module unit, race, and
vet checks, and complete Dovecot integration suite passed on 2026-11-16 with
Go 1.25.14, including the mailbox
codec and THREAD contract regressions. Independent `doveadm` checks distinguish
`R&D`, `R&-D`, literal `&AOk-`, and Unicode mailbox names, verify COPY/APPEND/MOVE
target contents and exact Trash creation, and detect unexpected mailboxes.
They compare UID membership and raw message content with CRLF/LF normalization;
IMAP observers also compare exact raw bytes, persistent flags, and date instants.
THREAD checks cover the criteria-only input schema, recursive result schema,
rejected paging/sorting controls, complete 201-root results on both protocol
revisions, header narrowing, and explicit rejection of oversized complete trees.

The CLI changes passed build, unit, race, vet, and the complete Dovecot
integration suite on 2026-11-17 with Go 1.25.14. Regressions cover configuration
discovery, verbosity, startup authentication/TLS failures, connection release,
timeouts, cancellation, and stdout JSON-RPC purity.

The transport-mode changes passed root and local-module unit, race, and vet
checks, plus the complete Dovecot integration suite, on 2026-11-17. An actual
`make clean` followed by plain `make` removed and rebuilt the compilation
artifacts while preserving source, the toolchain, and dependency downloads.

The connection/cache/diagnostics changes passed build, root and local-module
unit, race, and vet checks, plus the complete Dovecot suite, on 2026-11-17 with
Go 1.25.14. Progress routing is tested through SDK in-memory transports and the
real CLI stdout boundary together with quiet/debug file logging; display in
host clients and live-mailbox speedups remain unmeasured.

The [architecture decisions](docs/adr/0001-go-stdio-startup-policy.md),
[Linux CLI startup decision](docs/adr/0006-linux-cli-startup-validation.md),
[IMAP transport modes](docs/adr/0007-imap-transport-modes.md),
[decoded-header cache](docs/adr/0008-decoded-header-cache.md),
[runtime connection ownership](docs/adr/0009-serialized-runtime-connection.md),
[diagnostics and progress](docs/adr/0010-process-diagnostics-and-progress.md),
[cache coherence](docs/concepts/cache-coherence.md),
[message identity](docs/concepts/message-identity.md),
[IMAP safety](docs/concepts/imap-safety.md), and
[stdio contract](docs/concepts/stdio-mcp.md) describe the load-bearing invariants.

See [mail-search lessons](docs/notes/mail-search-lessons.md) for historical
observations, implemented improvements, and remaining proposals from actual
stdio and native Codex/MCP searches.
