package cache

import (
	"errors"
	"reflect"
	"testing"

	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestGetPreservesLiveUIDOrderAndStoredSummaries(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "inbox"}
	apply(t, c, key, snapshot(42, 0,
		metadata(1, "Subject: first\r\n\r\n"),
		metadata(2, "Subject: second\r\n\r\n"),
		metadata(3, "Subject: third\r\n\r\n"),
		metadata(4, "Subject: unrelated\r\n\r\n")))
	all := query(t, c, key, mail.SearchQuery{}).Messages
	want := []mail.MessageSummary{all[2], all[3], all[1]}
	// Broken header indexes and unrelated summaries must not affect page lookup.
	if _, err := c.db.Exec(`UPDATE messages SET headers='invalid JSON';
UPDATE messages SET summary='invalid JSON' WHERE uid=4;
CREATE TRIGGER immutable_header BEFORE UPDATE OF headers ON messages BEGIN SELECT RAISE(ABORT, 'headers rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(testContext, key, []uint32{2, 1, 3})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered summary lookup: got %+v, %v; want %+v", got, err, want)
	}
}

func TestGetNamespacesAndEpochReplacement(t *testing.T) {
	c := openTestCache(t)
	tests := []struct {
		key   mail.MailboxKey
		epoch uint32
	}{
		{mail.MailboxKey{Account: "one", Name: "INBOX"}, 42},
		{mail.MailboxKey{Account: "two", Name: "INBOX"}, 99},
		{mail.MailboxKey{Account: "one", Name: "Work"}, 2},
		{mail.MailboxKey{Account: "one", Name: "work"}, 3},
	}
	for _, test := range tests {
		subject := test.key.Account + "/" + test.key.Name
		apply(t, c, test.key, snapshot(test.epoch, 0, metadata(7, "Subject: "+subject+"\r\n\r\n")))
	}
	for _, test := range tests {
		subject := test.key.Account + "/" + test.key.Name
		got, err := c.Get(testContext, test.key, []uint32{7})
		if err != nil || len(got) != 1 || got[0].Subject != subject || got[0].Ref.UIDValidity != test.epoch {
			t.Fatalf("namespace %v: %+v, %v", test.key, got, err)
		}
	}
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(43, 0, metadata(8, "Subject: replacement\r\n\r\n")))
	requireGetError(t, c, key, []uint32{7}, "cache_miss")
	got, err := c.Get(testContext, key, []uint32{8})
	if err != nil || len(got) != 1 || got[0].Ref.UIDValidity != 43 || got[0].Subject != "replacement" {
		t.Fatalf("replacement epoch: %+v, %v", got, err)
	}
}

func TestGetRejectsMissingUIDsWithoutPartialResults(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(1, 1, metadata(1, "Subject: present\r\n\r\n")))
	for _, requested := range [][]uint32{{2}, {1, 2}, {2, 1}} {
		requireGetError(t, c, key, requested, "cache_miss")
	}
	requireGetError(t, c, mail.MailboxKey{Account: "absent", Name: "INBOX"}, []uint32{1}, "cache_miss")
	apply(t, c, key, snapshot(2, 0))
	requireGetError(t, c, key, []uint32{1}, "cache_miss")
	// Apply cannot publish a zero epoch; defensively reject an invalid stored one.
	apply(t, c, key, snapshot(3, 0, metadata(1, "Subject: retained\r\n\r\n")))
	if _, err := c.db.Exec(`UPDATE mailboxes SET uid_validity=0 WHERE account='one'`); err != nil {
		t.Fatal(err)
	}
	requireGetError(t, c, key, []uint32{1}, "cache_miss")
}

func TestGetEmptyPageAndBounds(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	for _, requested := range [][]uint32{nil, {}} {
		got, err := c.Get(testContext, key, requested)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty page: %+v, %v", got, err)
		}
	}
	items := make([]mail.Metadata, mail.MaxLimit)
	requested := make([]uint32, mail.MaxLimit)
	for i := range items {
		items[i] = metadata(uint32(i+1), "Subject: page\r\n\r\n")
		requested[i] = uint32(mail.MaxLimit - i)
	}
	apply(t, c, key, snapshot(1, 1, items...))
	got, err := c.Get(testContext, key, requested)
	if err != nil || !reflect.DeepEqual(uids(mail.MessagePage{Messages: got}), requested) {
		t.Fatalf("maximum page: got %d summaries, %v", len(got), err)
	}
	for _, invalid := range [][]uint32{{0}, {1, 0}, {1, 1}, append(requested, uint32(mail.MaxLimit+1))} {
		requireGetError(t, c, key, invalid, "invalid_argument")
	}
	requireGetError(t, c, mail.MailboxKey{Name: "INBOX"}, nil, "invalid_argument")
	requireGetError(t, c, mail.MailboxKey{Account: "one"}, []uint32{1}, "invalid_argument")
}

func requireGetError(t *testing.T, c *Cache, key mail.MailboxKey, uids []uint32, code string) {
	t.Helper()
	got, err := c.Get(testContext, key, uids)
	var mailErr *mail.Error
	if got != nil || !errors.As(err, &mailErr) || mailErr.Code != code {
		t.Fatalf("Get(%v, %v): got %+v, %v; want no results and %s", key, uids, got, err, code)
	}
}

func TestMixedCaseFlagsAndLivePageSummaries(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	r := snapshot(1, 1,
		metadata(1, "Subject: both\r\n\r\n"),
		metadata(2, "Subject: keyword\r\n\r\n"),
		metadata(3, "Subject: system flag\r\n\r\n"))
	r.FlagUpdates[0].Flags = []string{"\\Seen", "fixture-keyword"}
	r.FlagUpdates[1].Flags = []string{"FIXTURE-KEYWORD"}
	r.FlagUpdates[2].Flags = []string{"\\sEeN"}
	apply(t, c, key, r)
	for _, test := range []struct {
		name  string
		query mail.SearchQuery
		want  []uint32
	}{
		{"keyword case", mail.SearchQuery{Flags: []string{"FiXtUrE-KeYwOrD"}}, []uint32{2, 1}},
		{"positive AND", mail.SearchQuery{Flags: []string{"\\sEeN", "FIXTURE-KEYWORD"}}, []uint32{1}},
		{"negative keyword", mail.SearchQuery{NotFlags: []string{"fIxTuRe-KeYwOrD"}}, []uint32{3}},
		{"negative system flag", mail.SearchQuery{NotFlags: []string{"\\SEEN"}}, []uint32{2}},
		{"negative AND", mail.SearchQuery{NotFlags: []string{"\\SEEN", "fixture-KEYWORD"}}, []uint32{}},
		{"combined", mail.SearchQuery{Flags: []string{"FIXTURE-KEYWORD"}, NotFlags: []string{"\\seen"}}, []uint32{2}},
		{"missing positive", mail.SearchQuery{Flags: []string{"fixture-keyword", "absent"}}, []uint32{}},
		{"contradiction", mail.SearchQuery{Flags: []string{"fixture-keyword"}, NotFlags: []string{"FIXTURE-KEYWORD"}}, []uint32{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := query(t, c, key, test.query)
			if got := uids(page); !reflect.DeepEqual(got, test.want) || page.Total != len(test.want) {
				t.Fatalf("mixed-case query: got %+v, want UIDs %v", page, test.want)
			}
			// Live BODY/TEXT searches hand their selected page UIDs to Get.
			got, err := c.Get(testContext, key, test.want)
			if err != nil || !reflect.DeepEqual(got, page.Messages) {
				t.Fatalf("live-page summary consistency: %+v, %v; want %+v", got, err, page.Messages)
			}
		})
	}
	got, err := c.Get(testContext, key, []uint32{1, 2, 3})
	if err != nil || len(got) != 3 || !reflect.DeepEqual(got[0].Flags, r.FlagUpdates[0].Flags) ||
		!reflect.DeepEqual(got[1].Flags, r.FlagUpdates[1].Flags) || !reflect.DeepEqual(got[2].Flags, r.FlagUpdates[2].Flags) {
		t.Fatalf("stored flag spelling changed: %+v, %v", got, err)
	}
}
