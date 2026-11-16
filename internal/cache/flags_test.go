package cache

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestPersistentFlagsAndLegacyRecentCleanup(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	r := snapshot(1, 20, metadata(1, "Subject: draft\r\n\r\n"), metadata(2, "Subject: stable\r\n\r\n"))
	r.FlagUpdates[0].Flags = []string{"\\Draft", "\\Recent", "Recent", "\\rEcEnT"}
	apply(t, c, key, r)
	want := []string{"\\Draft", "Recent"}
	if got := query(t, c, key, mail.SearchQuery{}).Messages[1].Flags; !reflect.DeepEqual(got, want) {
		t.Fatalf("cached flags=%v, want %v", got, want)
	}
	if page := query(t, c, key, mail.SearchQuery{Flags: []string{"Recent"}}); page.Total != 1 || page.Messages[0].Ref.UID != 1 {
		t.Fatalf("ordinary keyword Recent query=%+v", page)
	}
	var encoded []byte
	if err := c.db.QueryRow(`SELECT summary FROM messages WHERE account='one' AND mailbox='INBOX' AND uid=1`).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var summary mail.MessageSummary
	if err := json.Unmarshal(encoded, &summary); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Flags, want) {
		t.Fatalf("persisted flags=%v, want %v", summary.Flags, want)
	}
	// Simulate a database written before Recent was excluded from summaries.
	summary.Flags = append(summary.Flags, "\\rEcEnT")
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE messages SET summary=? WHERE account='one' AND mailbox='INBOX' AND uid=1`, encoded); err != nil {
		t.Fatal(err)
	}
	if got := query(t, c, key, mail.SearchQuery{}).Messages[1].Flags; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy Query flags=%v, want %v", got, want)
	}
	got, err := c.Get(testContext, key, []uint32{1})
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0].Flags, want) {
		t.Fatalf("legacy Get=%+v, %v", got, err)
	}
	if _, err := c.db.Exec(`
CREATE TRIGGER immutable_headers BEFORE UPDATE OF headers ON messages
BEGIN SELECT RAISE(ABORT, 'headers rewritten'); END;
CREATE TRIGGER immutable_stable BEFORE UPDATE ON messages WHEN OLD.uid=2
BEGIN SELECT RAISE(ABORT, 'unrelated summary rewritten'); END;`); err != nil {
		t.Fatal(err)
	}
	// Recent-only changes do not advance MODSEQ or produce a FLAGS delta.
	apply(t, c, key, mail.Refresh{UIDValidity: 1, HighestModSeq: 20, Members: []uint32{1, 2}})
	if err := c.db.QueryRow(`SELECT summary FROM messages WHERE account='one' AND mailbox='INBOX' AND uid=1`).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &summary); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Flags, want) || summary.ModSeq != 20 || summary.Subject != "draft" {
		t.Fatalf("legacy cleanup altered persistent metadata: %+v", summary)
	}
	// A flag update containing only Recent still represents empty persistent flags.
	apply(t, c, key, mail.Refresh{UIDValidity: 1, HighestModSeq: 20, Members: []uint32{1, 2},
		FlagUpdates: []mail.FlagUpdate{{UID: 1, Flags: []string{"\\Recent"}, ModSeq: 20}}})
	if got := query(t, c, key, mail.SearchQuery{}).Messages[1].Flags; len(got) != 0 {
		t.Fatalf("Recent-only update flags=%v", got)
	}
	if page := query(t, c, key, mail.SearchQuery{Flags: []string{"Recent"}}); page.Total != 0 {
		t.Fatalf("keyword flags were retained after removal: %+v", page)
	}
}

func TestRecentQueryRejected(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	for _, q := range []mail.SearchQuery{{Flags: []string{"\\Recent"}}, {NotFlags: []string{"\\rEcEnT"}}} {
		_, err := c.Query(testContext, key, q)
		var typed *mail.Error
		if err == nil {
			t.Fatal("Recent query succeeded")
		}
		if !errors.As(err, &typed) || typed.Code != "invalid_argument" {
			t.Fatalf("Recent query error=%v", err)
		}
	}
}
