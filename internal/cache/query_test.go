package cache

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestUnfilteredSQLPagesEqualHeaderScan(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(42, 9,
		metadata(1, "Subject: tie\r\nX-Match: yes\r\n\r\n"),
		metadata(3, "Subject: tie\r\nX-Match: yes\r\n\r\n"),
		metadata(8, "Subject: tie\r\nX-Match: yes\r\n\r\n"),
		metadata(4294967295, "Subject: tie\r\nX-Match: yes\r\n\r\n")))
	apply(t, c, mail.MailboxKey{Account: "two", Name: "INBOX"}, snapshot(7, 1, metadata(10, "X-Match: yes\r\n\r\n")))
	for _, sort := range []string{"", "uid", "UID_DESC"} {
		for _, limit := range []int{1, 2, 4, 5} {
			for _, before := range []uint32{0, 4294967295, 8, 3, 1} {
				q := mail.SearchQuery{Sort: sort, Limit: limit, BeforeUID: before}
				got := query(t, c, key, q)
				q.Headers = map[string]string{"X-Match": ""}
				want := query(t, c, key, q)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("SQL page differs from scan (sort=%q limit=%d before=%d): got %+v, want %+v", sort, limit, before, got, want)
				}
			}
		}
	}
	for _, key := range []mail.MailboxKey{{Account: "one", Name: "empty"}, {Account: "missing", Name: "INBOX"}} {
		page := query(t, c, key, mail.SearchQuery{})
		if page.Messages == nil || page.Total != 0 || page.NextBeforeUID != 0 {
			t.Fatalf("empty listing shape: %+v", page)
		}
	}
	for _, sort := range []string{"date", "arrival", "subject", "from", "size"} {
		if got := uids(query(t, c, key, mail.SearchQuery{Sort: sort + "_asc"})); !reflect.DeepEqual(got, []uint32{1, 3, 8, 4294967295}) {
			t.Fatalf("ascending tie order changed for %s: %v", sort, got)
		}
	}
}

func TestApplyAndNonHeaderQueriesAvoidDecodedMaps(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(1, 1, metadata(1, "Subject: stored\r\n\r\n")))
	if _, err := c.db.Exec(`UPDATE messages SET headers='invalid JSON';
CREATE TRIGGER immutable_headers BEFORE UPDATE OF headers ON messages BEGIN SELECT RAISE(ABORT, 'headers rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	apply(t, c, key, mail.Refresh{UIDValidity: 1, HighestModSeq: 2, Members: []uint32{1},
		FlagUpdates: []mail.FlagUpdate{{UID: 1, Flags: []string{"keyword"}, ModSeq: 2}}})
	apply(t, c, key, mail.Refresh{UIDValidity: 1, HighestModSeq: 3, Members: []uint32{1}})
	for _, q := range []mail.SearchQuery{
		{}, {Sort: "uid_asc"}, {Sort: "subject"}, {MinSize: 1},
		{Since: "2026-11-16"}, {Flags: []string{"KEYWORD"}}, {NotFlags: []string{"\\Seen"}},
	} {
		page := query(t, c, key, q)
		if page.Total != 1 || page.Messages[0].Subject != "stored" || page.Messages[0].ModSeq != 2 {
			t.Fatalf("nonheader query lost stored metadata: %+v", page)
		}
	}
	if _, err := c.Query(testContext, key, mail.SearchQuery{Subject: "stored"}); err == nil {
		t.Fatal("header search ignored invalid decoded header data")
	}
}

func TestUnfilteredListingReadsOnlyLimitedSummaries(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(9, 1, metadata(1, "Subject: outside page\r\n\r\n"), metadata(2, "Subject: page\r\n\r\n")))
	if _, err := c.db.Exec(`UPDATE messages SET summary='invalid JSON' WHERE uid=1; UPDATE messages SET headers='invalid JSON'`); err != nil {
		t.Fatal(err)
	}
	page := query(t, c, key, mail.SearchQuery{Limit: 1})
	if page.Total != 2 || page.NextBeforeUID != 2 || len(page.Messages) != 1 || page.Messages[0].Subject != "page" || page.UIDValidity != 9 {
		t.Fatalf("limited listing: %+v", page)
	}
}

func TestRefreshFailureRollsBackMembershipFlagsAndCheckpoint(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(4, 5, metadata(1, "Subject: retained\r\n\r\n"), metadata(2, "Subject: membership\r\n\r\n")))
	wantPage := query(t, c, key, mail.SearchQuery{})
	wantState, err := c.State(testContext, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`CREATE TRIGGER reject_update BEFORE UPDATE ON messages BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(testContext, key, mail.Refresh{UIDValidity: 4, HighestModSeq: 6, Members: []uint32{1},
		FlagUpdates: []mail.FlagUpdate{{UID: 1, Flags: []string{"changed"}, ModSeq: 6}}}); err == nil {
		t.Fatal("injected update failure was ignored")
	}
	gotState, err := c.State(testContext, key)
	if err != nil || !reflect.DeepEqual(gotState, wantState) || !reflect.DeepEqual(query(t, c, key, mail.SearchQuery{}), wantPage) {
		t.Fatalf("failed update published partial state: got %+v, want %+v, error=%v", gotState, wantState, err)
	}
}

func TestConcurrentListingEpochAndCountShareSnapshot(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	small := snapshot(1, 1, metadata(1, "Subject: small\r\n\r\n"))
	large := snapshot(2, 1, metadata(2, "Subject: large\r\n\r\n"), metadata(3, "Subject: large\r\n\r\n"), metadata(4, "Subject: large\r\n\r\n"))
	apply(t, c, key, small)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			refresh := small
			if i%2 == 0 {
				refresh = large
			}
			if err := c.Apply(testContext, key, refresh); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			page, err := c.Query(testContext, key, mail.SearchQuery{Limit: 2})
			if err != nil {
				t.Error(err)
				return
			}
			count, length, next := 1, 1, uint32(0)
			if page.UIDValidity == 2 {
				count, length, next = 3, 2, 3
			}
			if page.Total != count || len(page.Messages) != length || page.NextBeforeUID != next {
				t.Errorf("mixed count/epoch snapshot: %+v", page)
				return
			}
			for _, summary := range page.Messages {
				if summary.Ref.UIDValidity != page.UIDValidity {
					t.Errorf("mixed summary/epoch snapshot: %+v", page)
					return
				}
			}
		}
	}()
	workers.Wait()
}

func BenchmarkCacheListing(b *testing.B) {
	for _, size := range []int{100, 5000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			c, err := Open(b.TempDir() + "/headers.sqlite")
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			key := mail.MailboxKey{Account: "benchmark", Name: "INBOX"}
			items := make([]mail.Metadata, size)
			for i := range items {
				items[i] = metadata(uint32(i+1), "Subject: common\r\nX-Match: yes\r\nX-Other: arbitrary metadata\r\n\r\n")
			}
			if err := c.Apply(testContext, key, snapshot(1, 1, items...)); err != nil {
				b.Fatal(err)
			}
			for _, mode := range []struct {
				name  string
				query mail.SearchQuery
			}{{"sql_page", mail.SearchQuery{Limit: 50}}, {"decoded_scan", mail.SearchQuery{Limit: 50, Headers: map[string]string{"X-Match": ""}}}} {
				b.Run(mode.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if _, err := c.Query(testContext, key, mode.query); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
