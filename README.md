# imap-mcp

A local, single-account IMAP server for MCP clients. Search and read mail without
marking it Seen; optionally enable flags, copy, move, Trash, and APPEND. It uses
Go, TOML configuration, the official Go MCP SDK, and stdio. There is no HTTP
listener, SMTP sending, raw IMAP command tool, or permanent-delete tool.

## Build and configure

Install Go **1.25 or newer**, then run from this checkout:

```sh
make build
install -d -m 700 "$HOME/.config/imap-mcp"
install -m 600 config.example.toml "$HOME/.config/imap-mcp/config.toml"
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
tls = "tls"
connect_timeout_seconds = 10
operation_timeout_seconds = 60
# ca_file = "private-ca.pem"

[cache]
path = "headers.sqlite3"
```

`tls = "tls"` uses implicit TLS, default port 993. For required STARTTLS, set
`tls = "starttls"` and port 143, or omit `port` to use that mode's default.
Both modes verify the certificate and hostname; a failed STARTTLS or verification
fails the connection. There is no insecure mode. `ca_file` adds a private CA to
the system trust roots.

Relative `cache.path` and `imap.ca_file` paths resolve beside the TOML file.
An empty cache path uses the OS user cache directory at
`imap-mcp/headers.sqlite3`. Unknown configuration keys are errors.

Start the stdio server with an **absolute** configuration path:

```sh
/absolute/path/to/imap-mcp/bin/imap-mcp --config /absolute/path/to/config.toml
```

Normally your MCP client starts this process. Standard output carries MCP
JSON-RPC; diagnostics use standard error. Configuration is loaded once.
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

SQLite persists raw headers, decoded header metadata, flags, membership, and
mailbox epochs. It stores no bodies or attachments. Files are private (0600),
new directories use 0700, and the cache is **unencrypted**: headers can contain
sensitive information. Existing parent directory permissions are retained.

Every list/search first refreshes against the live IMAP server. The first sync
is O(N) in mailbox membership and headers. Later searches fetch new headers and
use CONDSTORE for flag changes when available, while still reconciling all UIDs;
without CONDSTORE or with NOMODSEQ they fetch all flags. A failed refresh returns
an error, never an offline or stale answer. Writes durably invalidate affected
mailboxes before dispatch. Account partitions use host, port, TLS mode, and
username; changing only a password preserves the partition.

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
native IMAP4rev2 MOVE and mode-aware mailbox names; see [ADR 0004](docs/adr/0004-backport-imap4rev2-vanished.md)
and [ADR 0005](docs/adr/0005-mode-aware-mailbox-names.md). Mailbox inputs and
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
make build
make test
make integration
```

The Makefile prefers `.tools/go/bin/go` when installed, otherwise `go`, and
keeps its Go caches and temporary files inside `.tools/`. To run race and vet
checks with the same environment:

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
Each fixture uses synthetic mail and credentials, random ports bound to
127.0.0.1, an owned container/volume, and registered cleanup. See the
[reproducible testbed](testbed/README.md) for setup and fixture details.

The build, root and local-module unit, race, and vet checks, and complete Dovecot
integration suite passed on 2026-11-16 with Go 1.25.14, including the mailbox
codec and THREAD contract regressions. Independent `doveadm` checks distinguish
`R&D`, `R&-D`, literal `&AOk-`, and Unicode mailbox names, verify COPY/APPEND/MOVE
target contents and exact Trash creation, and detect unexpected mailboxes.
They compare UID membership and raw message content with CRLF/LF normalization;
IMAP observers also compare exact raw bytes, persistent flags, and date instants.
THREAD checks cover the criteria-only input schema, recursive result schema,
rejected paging/sorting controls, complete 201-root results on both protocol
revisions, header narrowing, and explicit rejection of oversized complete trees.

The [architecture decisions](docs/adr/0001-go-stdio-startup-policy.md),
[cache coherence](docs/concepts/cache-coherence.md),
[message identity](docs/concepts/message-identity.md),
[IMAP safety](docs/concepts/imap-safety.md), and
[stdio contract](docs/concepts/stdio-mcp.md) describe the load-bearing invariants.
