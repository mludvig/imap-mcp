package cache

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/mludvig/imap-mcp/internal/diagnostics"
)

const cacheApplicationID = 0x494d4348 // IMCH
// Decoded-only persistence and legacy rebuilds: see docs/adr/0008-decoded-header-cache.md.
const cacheSchemaVersion = 1

const mailboxSchema = `CREATE TABLE mailboxes (
 account TEXT NOT NULL, name TEXT NOT NULL,
 uid_validity INTEGER NOT NULL, highest_mod_seq TEXT NOT NULL,
 PRIMARY KEY(account, name)
)`

const messageSchema = `CREATE TABLE messages (
 account TEXT NOT NULL, mailbox TEXT NOT NULL, uid INTEGER NOT NULL,
 summary BLOB NOT NULL, headers BLOB NOT NULL,
 PRIMARY KEY(account, mailbox, uid),
 FOREIGN KEY(account, mailbox) REFERENCES mailboxes(account, name) ON DELETE CASCADE
)`

const legacyMessageSchema = `CREATE TABLE messages (
 account TEXT NOT NULL, mailbox TEXT NOT NULL, uid INTEGER NOT NULL,
 header BLOB NOT NULL, summary BLOB NOT NULL, headers BLOB NOT NULL,
 PRIMARY KEY(account, mailbox, uid),
 FOREIGN KEY(account, mailbox) REFERENCES mailboxes(account, name) ON DELETE CASCADE
)`

func normalizedSchema(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}

func (c *Cache) initialize(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	var applicationID, version int
	if err := c.db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&applicationID); err != nil {
		return err
	}
	if err := c.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT type, name, sql FROM sqlite_schema WHERE substr(name,1,7) <> 'sqlite_'`)
	if err != nil {
		return err
	}
	schema := make(map[string]string)
	for rows.Next() {
		var kind, name, definition string
		if err := rows.Scan(&kind, &name, &definition); err != nil {
			rows.Close()
			return err
		}
		schema[kind+"/"+name] = normalizedSchema(definition)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	matches := func(messages string) bool {
		return len(schema) == 2 && schema["table/mailboxes"] == normalizedSchema(mailboxSchema) &&
			schema["table/messages"] == normalizedSchema(messages)
	}
	switch {
	case applicationID == 0 && version == 0 && len(schema) == 0:
		_, err := c.db.ExecContext(ctx, fmt.Sprintf(`BEGIN;
%s; %s;
PRAGMA application_id=%d; PRAGMA user_version=%d;
COMMIT;`, mailboxSchema, messageSchema, cacheApplicationID, cacheSchemaVersion))
		return err
	case applicationID == cacheApplicationID && version == cacheSchemaVersion && matches(messageSchema):
		return nil
	case applicationID == 0 && version == 0 && matches(legacyMessageSchema):
		// Exact recognition avoids replacing another application's SQLite file.
		// See docs/concepts/cache-coherence.md for decoded-only cache rebuilds.
		diagnostics.Logger(ctx).InfoContext(ctx, "rebuilding header cache", "old_schema", "raw_headers", "schema_version", cacheSchemaVersion)
		// Leaving the version unset makes an interrupted compaction retryable.
		if _, err := c.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA journal_mode=DELETE;
BEGIN;
DROP TABLE messages; DROP TABLE mailboxes;
%s; %s;
PRAGMA application_id=%d;
COMMIT;`, mailboxSchema, messageSchema, cacheApplicationID)); err != nil {
			return err
		}
	case applicationID == cacheApplicationID && version == 0 && matches(messageSchema):
		diagnostics.Logger(ctx).InfoContext(ctx, "resuming header cache rebuild", "schema_version", cacheSchemaVersion)
	default:
		return fmt.Errorf("unrecognized or unsupported cache schema (application_id=%d, version=%d)", applicationID, version)
	}
	// DROP alone leaves raw header pages consuming space; finish under the lock.
	if _, err := c.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, cacheSchemaVersion))
	return err
}
