package cache

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func createDatabase(t *testing.T, path, schema string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema, args...); err != nil {
		t.Fatal(err)
	}
}

func requireDecodedSchema(t *testing.T, c *Cache) {
	t.Helper()
	var version, applicationID int
	if err := c.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := c.db.QueryRow(`PRAGMA application_id`).Scan(&applicationID); err != nil {
		t.Fatal(err)
	}
	if version != cacheSchemaVersion || applicationID != cacheApplicationID {
		t.Fatalf("schema markers: version=%d, application_id=%d", version, applicationID)
	}
	rows, err := c.db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var index, required, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&index, &name, &kind, &required, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(columns, ",") != "account,mailbox,uid,summary,headers" {
		t.Fatalf("unexpected persisted columns: %v", columns)
	}
}

func TestLegacySchemaRebuildReclaimsRawPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers.sqlite")
	raw := bytes.Repeat([]byte("raw-header-copy-must-be-reclaimed"), 40000)
	legacySchema := strings.ReplaceAll(mailboxSchema+";"+legacyMessageSchema, "CREATE TABLE", "CREATE TABLE IF NOT EXISTS")
	createDatabase(t, path, legacySchema+`;
INSERT INTO mailboxes VALUES('one','INBOX',42,'99');
INSERT INTO messages VALUES('one','INBOX',1,?,'{}','{}');`, raw)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	progress := []string{}
	ctx := diagnostics.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx = diagnostics.WithProgress(ctx, func(message string) { progress = append(progress, message) })
	c, err := OpenContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0] != "Opening header cache" ||
		!strings.Contains(logs.String(), `"msg":"rebuilding header cache"`) ||
		!strings.Contains(logs.String(), `"stage":"cache open"`) ||
		strings.Contains(logs.String(), "raw-header-copy-must-be-reclaimed") {
		t.Fatalf("missing or inappropriate cache diagnostics: stages=%v", progress)
	}
	requireDecodedSchema(t, c)
	state, err := c.State(testContext, mail.MailboxKey{Account: "one", Name: "INBOX"})
	if err != nil || state.Present || len(state.CachedUIDs) != 0 {
		t.Fatalf("rebuild retained old membership/checkpoint: %+v, %v", state, err)
	}
	if page := query(t, c, mail.MailboxKey{Account: "one", Name: "INBOX"}, mail.SearchQuery{}); page.Messages == nil || page.Total != 0 || page.UIDValidity != 0 {
		t.Fatalf("rebuilt cache is not empty: %+v", page)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || after.Size() >= before.Size() || after.Mode().Perm() != 0600 {
		t.Fatalf("rebuild did not compact a private file: before=%v, after=%v, error=%v", before, after, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("raw-header-copy-must-be-reclaimed")) {
		t.Fatal("raw header bytes remain after compaction")
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	requireDecodedSchema(t, c)
	apply(t, c, mail.MailboxKey{Account: "one", Name: "INBOX"}, snapshot(43, 100, metadata(1, "Subject: refetched\r\n\r\n")))
}

func TestDecodedSchemaReopensWithoutRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers.sqlite")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	requireDecodedSchema(t, c)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	header := "X-Arbitrary: =?UTF-8?Q?Stra=C3=9Fe_Caf=C3=A9?=\r\nX-Arbitrary: literal %_.*[]\r\nX-Empty:\r\nX-Other: retained\r\nSubject: formatted\r\n\r\n"
	apply(t, c, key, snapshot(7, 8, metadata(9, header)))
	want := query(t, c, key, mail.SearchQuery{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	requireDecodedSchema(t, c)
	for _, criteria := range []map[string]string{
		{"x-arbitrary": "STRASSE CAFE\u0301"},
		{"X-Arbitrary": "%_.*[]"},
		{"X-Empty": "", "X-Other": "retained"},
	} {
		got := query(t, c, key, mail.SearchQuery{Headers: criteria})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded headers or summaries lost on reopen: got %+v, want %+v", got, want)
		}
	}
}

func TestUnrecognizedDatabaseIsNotRebuilt(t *testing.T) {
	tests := map[string]string{
		"unrelated":            `CREATE TABLE other(value TEXT); INSERT INTO other VALUES('retained')`,
		"sqlite-like name":     `CREATE TABLE sqlitecustom(value TEXT); INSERT INTO sqlitecustom VALUES('retained')`,
		"colliding table":      `CREATE TABLE messages(value TEXT)`,
		"legacy plus data":     mailboxSchema + ";" + legacyMessageSchema + ";CREATE TABLE other(value TEXT)",
		"foreign application":  mailboxSchema + ";" + legacyMessageSchema + ";PRAGMA application_id=123",
		"future own version":   fmt.Sprintf("%s;%s;PRAGMA application_id=%d;PRAGMA user_version=%d", mailboxSchema, messageSchema, cacheApplicationID, cacheSchemaVersion+1),
		"unmarked new schema":  mailboxSchema + ";" + messageSchema,
		"legacy with triggers": mailboxSchema + ";" + legacyMessageSchema + ";CREATE TRIGGER customized AFTER INSERT ON messages BEGIN SELECT 1; END",
	}
	for name, schema := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "headers.sqlite")
			createDatabase(t, path, schema)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c, err := Open(path)
			if err == nil {
				c.Close()
				t.Fatal("unrecognized database accepted")
			}
			if !strings.Contains(err.Error(), "unrecognized or unsupported cache schema") {
				t.Fatalf("unclear schema error: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("unrecognized database modified: %v", err)
			}
		})
	}
}

func TestLegacyRebuildRequiresExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers.sqlite")
	createDatabase(t, path, mailboxSchema+";"+legacyMessageSchema)
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(path)
	if err == nil {
		c.Close()
		t.Fatal("rebuild bypassed exclusive cache lock")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("locked database modified: %v", err)
	}
}

func TestInterruptedRebuildFinishesCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers.sqlite")
	createDatabase(t, path, fmt.Sprintf(`%s;%s;
PRAGMA application_id=%d;
CREATE TABLE discarded(raw BLOB);
INSERT INTO discarded VALUES(zeroblob(1000000));
DROP TABLE discarded;`, mailboxSchema, messageSchema, cacheApplicationID))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	requireDecodedSchema(t, c)
	after, err := os.Stat(path)
	if err != nil || after.Size() >= before.Size() {
		t.Fatalf("interrupted compaction not completed: before=%v, after=%v, error=%v", before, after, err)
	}
}
