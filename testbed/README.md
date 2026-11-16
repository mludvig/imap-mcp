# Dovecot integration fixture

Run `go test -tags=integration -v ./tests/integration` with Docker available.
The repository's Makefile environment also applies to this command. Tests fail
if Docker is unavailable, so the explicit integration tag cannot silently skip
the fixture. The image is pinned to the multiarch index for Dovecot 2.4.5:
`dovecot/dovecot:2.4.5@sha256:43c14def08a57f25107593cbcf64a0a6fbae84a7b9de7aa038513c9b21e91914`.

Each test owns a uniquely named container and publishes both STARTTLS (31143)
and implicit TLS (31993) on random ports bound only to `127.0.0.1`. A generated
CA signs a short-lived certificate valid for localhost and 127.0.0.1. Only the
synthetic `fixture` account exists. Its credentials and server key are generated
test files mounted read-only and readable by the image's UID 1000.

The complete `dovecot.conf` replacement avoids the image's index mailbox layout,
FTS plugins, other protocols, and default authentication. Maildir uses UTF-8
mailbox storage and UTF-8 extensions (required by Dovecot to advertise IMAP4rev2);
the two test variants explicitly enable or disable IMAP4rev2.
Configuration is checked with the running image's `doveconf -n`.

Messages are appended through standard IMAP with explicit flags and INTERNALDATE.
Their UIDs are discovered by Message-ID after seeding. Fixtures include decoded
headers, a Unicode mailbox, nested MIME with encoded attachment names and bodies,
Seen/unseen/Flagged messages, date boundaries, a reply, 55 pagination messages,
and an unrelated message already carrying `\Deleted`. Seeding selects each
populated mailbox once to consume rev1's session-specific `\Recent` state
without changing Seen or expunging. The Deleted sentinel must remain present
after application writes; the observer uses EXAMINE and BODY.PEEK[].

Same-package integration tests can call `startDovecot(t, rev2)`, `f.Dial(t, mode)`,
`f.TLSConfig(t)`, `f.ConfigFile(t, mode, readonly)`, and `f.Snapshot(t, mailbox)`.
`Seeds` contains expected bytes/flags/dates, and `UIDs` maps seed IDs to actual
server UIDs. `Snapshot` captures membership, full raw messages, flags and dates
through a fresh independent IMAP connection. Close observer connections directly:
never use CLOSE, EXPUNGE, or automatic cleanup that expunges existing messages.

Cleanup is registered before container startup and removes only that container
and its anonymous mail volume. Testing removes the fixture's temporary directory
after container cleanup. No global Docker pruning or real account access occurs.
