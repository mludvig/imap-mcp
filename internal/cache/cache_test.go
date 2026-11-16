package cache

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/mail"
)

var testContext = context.Background()

func openTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "headers.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func metadata(uid uint32, header string) mail.Metadata {
	return mail.Metadata{UID: uid, Header: []byte(header), Size: 100, InternalDate: time.Date(2026, 11, 16, 0, 30, 0, 0, time.FixedZone("NZDT", 13*60*60))}
}

func snapshot(epoch uint32, modseq uint64, items ...mail.Metadata) mail.Refresh {
	r := mail.Refresh{UIDValidity: epoch, HighestModSeq: modseq, Members: []uint32{}, NewMetadata: items, FlagUpdates: []mail.FlagUpdate{}}
	for _, item := range items {
		r.Members = append(r.Members, item.UID)
		r.FlagUpdates = append(r.FlagUpdates, mail.FlagUpdate{UID: item.UID, Flags: []string{"\\Seen", "Project"}, ModSeq: modseq})
	}
	return r
}

func apply(t *testing.T, c *Cache, key mail.MailboxKey, r mail.Refresh) {
	t.Helper()
	if err := c.Apply(testContext, key, r); err != nil {
		t.Fatal(err)
	}
}

func query(t *testing.T, c *Cache, key mail.MailboxKey, q mail.SearchQuery) mail.MessagePage {
	t.Helper()
	p, err := c.Query(testContext, key, q)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func uids(page mail.MessagePage) []uint32 {
	values := []uint32{}
	for _, summary := range page.Messages {
		values = append(values, summary.Ref.UID)
	}
	return values
}

func TestPersistenceNamespacesEpochAndInvalidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers.sqlite")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	key := mail.MailboxKey{Account: "one", Name: "inbox"}
	apply(t, c, key, snapshot(42, math.MaxUint64, metadata(7, "Subject: persisted\r\n\r\n"), metadata(9, "Subject: later\r\n\r\n")))
	other := mail.MailboxKey{Account: "two", Name: "INBOX"}
	apply(t, c, other, snapshot(99, 4, metadata(7, "Subject: other account\r\n\r\n")))
	for _, name := range []string{"Work", "work"} {
		apply(t, c, mail.MailboxKey{Account: "one", Name: name}, snapshot(1, 1, metadata(1, "Subject: "+name+"\r\n\r\n")))
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key.Name = "INBOX"
	state, err := c.State(testContext, key)
	if err != nil || !state.Present || state.UIDValidity != 42 || state.HighestModSeq != math.MaxUint64 || !reflect.DeepEqual(state.CachedUIDs, []uint32{7, 9}) {
		t.Fatalf("reopened state: %+v, %v", state, err)
	}
	p := query(t, c, key, mail.SearchQuery{})
	if p.UIDValidity != 42 || p.Messages[1].Subject != "persisted" || p.Messages[1].ModSeq != math.MaxUint64 || p.Messages[1].Ref.Mailbox != "INBOX" {
		t.Fatalf("reopened summaries: %+v", p)
	}
	if p := query(t, c, other, mail.SearchQuery{}); len(p.Messages) != 1 || p.Messages[0].Subject != "other account" {
		t.Fatalf("account collision: %+v", p)
	}
	for _, name := range []string{"Work", "work"} {
		if p := query(t, c, mail.MailboxKey{Account: "one", Name: name}, mail.SearchQuery{}); p.Messages[0].Subject != name {
			t.Fatalf("case-sensitive mailbox collision: %+v", p)
		}
	}
	if err := c.Invalidate(testContext, []mail.MailboxKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err = c.State(testContext, key)
	if err != nil || state.HighestModSeq != 0 || state.UIDValidity != 42 || len(state.CachedUIDs) != 2 {
		t.Fatalf("durable invalidation: %+v, %v", state, err)
	}
	apply(t, c, key, snapshot(43, 1, metadata(7, "Subject: new epoch\r\n\r\n")))
	if p := query(t, c, key, mail.SearchQuery{}); p.UIDValidity != 43 || len(p.Messages) != 1 || p.Messages[0].Subject != "new epoch" {
		t.Fatalf("epoch replacement: %+v", p)
	}
	apply(t, c, key, snapshot(44, 0))
	state, err = c.State(testContext, key)
	if err != nil || !state.Present || state.UIDValidity != 44 || len(state.CachedUIDs) != 0 {
		t.Fatalf("empty mailbox is a present state: %+v, %v", state, err)
	}
	missing, err := c.State(testContext, mail.MailboxKey{Account: "missing", Name: "INBOX"})
	if err != nil || missing.Present || len(missing.CachedUIDs) != 0 {
		t.Fatalf("missing state: %+v, %v", missing, err)
	}
}

func TestRollbackAndHeaderReuse(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	original := metadata(1, "Subject: original\r\n\r\n")
	apply(t, c, key, snapshot(1, 5, original, metadata(2, "Subject: removed\r\n\r\n")))
	for _, r := range []mail.Refresh{
		{UIDValidity: 2, HighestModSeq: 6, Members: []uint32{1}},
		{UIDValidity: 1, HighestModSeq: 6, Members: []uint32{1, 2, 3}},
		{UIDValidity: 1, HighestModSeq: 4, Members: []uint32{1, 2}},
		snapshot(2, 6, metadata(1, "Subject: bad\r\n\r\nBODY")),
	} {
		if err := c.Apply(testContext, key, r); err == nil {
			t.Fatal("accepted an incomplete, regressing, or body-bearing refresh")
		}
		if p := query(t, c, key, mail.SearchQuery{}); p.UIDValidity != 1 || len(p.Messages) != 2 || p.Messages[1].Subject != "original" {
			t.Fatalf("failed apply altered rows: %+v", p)
		}
	}
	if _, err := c.db.Exec(`CREATE TRIGGER reject_insert BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(testContext, key, snapshot(2, 6, metadata(1, "Subject: replacement\r\n\r\n"))); err == nil {
		t.Fatal("expected injected SQL failure")
	}
	if p := query(t, c, key, mail.SearchQuery{}); p.UIDValidity != 1 || len(p.Messages) != 2 || p.Messages[1].Subject != "original" {
		t.Fatalf("SQL failure did not roll back epoch reset: %+v", p)
	}
	if _, err := c.db.Exec(`DROP TRIGGER reject_insert; CREATE TRIGGER immutable_header BEFORE UPDATE OF headers ON messages BEGIN SELECT RAISE(ABORT, 'headers rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	apply(t, c, key, mail.Refresh{UIDValidity: 1, HighestModSeq: 7, Members: []uint32{1}, FlagUpdates: []mail.FlagUpdate{{UID: 1, Flags: []string{}, ModSeq: 7}}})
	p := query(t, c, key, mail.SearchQuery{})
	if len(p.Messages) != 1 || p.Messages[0].Subject != "original" || len(p.Messages[0].Flags) != 0 || p.Messages[0].ModSeq != 7 {
		t.Fatalf("incremental removal/flags: %+v", p)
	}
	if got := query(t, c, key, mail.SearchQuery{Headers: map[string]string{"subject": "original"}}); got.Total != 1 {
		t.Fatalf("decoded header changed: %+v", got)
	}
}

func TestDecodedLiteralUnicodeHeaders(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	header := "Subject: =?UTF-8?Q?Stra=C3=9Fe_Caf=C3=A9_100=25=5Fdone?=\r\n" +
		"From: =?UTF-8?Q?Jos=C3=A9?= <sender@example.com>\r\nTo: receiver@example.com\r\nCc: cc@example.com\r\nBcc: hidden@example.com\r\n" +
		"X-Repeat: first\r\nX-Repeat: =?UTF-8?Q?CAF=C3=89?=\r\nX-Empty:\r\n\r\n"
	apply(t, c, key, snapshot(1, 1, metadata(1, header)))
	for _, q := range []mail.SearchQuery{
		{Subject: "STRASSE CAFE\u0301 100%_DONE"},
		{From: "JOSE\u0301", To: "receiver", Cc: "cc@", Bcc: "HIDDEN"},
		{Headers: map[string]string{"x-repeat": "cafe\u0301"}},
		{Headers: map[string]string{"X-Empty": ""}},
		{Subject: "%_"},
		{Flags: []string{"\\seen", "pRoJeCt"}},
		{NotFlags: []string{"\\Answered", "other-project"}},
	} {
		if p := query(t, c, key, q); !reflect.DeepEqual(uids(p), []uint32{1}) {
			t.Fatalf("query %+v failed: %+v", q, p)
		}
	}
	for _, q := range []mail.SearchQuery{
		{Subject: "%missing"}, {Headers: map[string]string{"X-Absent": ""}},
		{Headers: map[string]string{"X-Repeat": "firstcafé"}},
		{Flags: []string{"other-project"}}, {NotFlags: []string{"pRoJeCt"}}, {NotFlags: []string{"\\SEEN"}},
		{From: "sender", Subject: "absent"},
	} {
		if p := query(t, c, key, q); len(p.Messages) != 0 {
			t.Fatalf("query %+v matched unexpectedly: %+v", q, p)
		}
	}
	s := query(t, c, key, mail.SearchQuery{}).Messages[0]
	if s.Subject != "Straße Café 100%_done" || !reflect.DeepEqual(s.From, []string{"\"José\" <sender@example.com>"}) || len(s.Bcc) != 1 {
		t.Fatalf("decoded summary: %+v", s)
	}
}

func TestCivilDatesAndMissingSentDates(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(1, 1,
		metadata(1, "Date: Mon, 16 Nov 2026 00:15:00 +1300\r\n\r\n"),
		metadata(2, "Date: invalid\r\n\r\n"), metadata(3, "Subject: no date\r\n\r\n"),
		metadata(4, "Date: Mon, 16 Nov 2026 00:15:00 +2400\r\n\r\n")))
	for _, test := range []struct {
		query mail.SearchQuery
		want  []uint32
	}{
		{mail.SearchQuery{Since: "2026-11-16", Before: "2026-11-17"}, []uint32{4, 3, 2, 1}},
		{mail.SearchQuery{Before: "2026-11-16"}, []uint32{}},
		{mail.SearchQuery{SentSince: "2026-11-16", SentBefore: "2026-11-17"}, []uint32{1}},
		{mail.SearchQuery{SentBefore: "2026-11-16"}, []uint32{}},
	} {
		if got := uids(query(t, c, key, test.query)); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("query %+v: got %v, want %v", test.query, got, test.want)
		}
	}
}

func TestPaginationSortAndValidation(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	items := make([]mail.Metadata, 205)
	for i := range items {
		items[i] = metadata(uint32(i+1), "Subject: common\r\n\r\n")
		items[i].Size = int64(i + 1)
	}
	apply(t, c, key, snapshot(1, 1, items...))
	p := query(t, c, key, mail.SearchQuery{})
	if len(p.Messages) != 50 || p.NextBeforeUID != 156 || p.Total != 205 {
		t.Fatalf("default page: %+v", p)
	}
	p = query(t, c, key, mail.SearchQuery{BeforeUID: p.NextBeforeUID, Limit: 200})
	if len(p.Messages) != 155 || p.Messages[0].Ref.UID != 155 || p.NextBeforeUID != 0 || p.Total != 205 {
		t.Fatalf("exclusive next page: %+v", p)
	}
	p = query(t, c, key, mail.SearchQuery{MinSize: 202, MaxSize: 205})
	if !reflect.DeepEqual(uids(p), []uint32{204, 203}) {
		t.Fatalf("strict size bounds: %+v", p)
	}
	for _, field := range []string{"date", "arrival", "subject", "from", "size", "uid"} {
		p := query(t, c, key, mail.SearchQuery{Sort: field + "_desc", Limit: 1})
		if p.Messages[0].Ref.UID != 205 || (field != "uid" && p.NextBeforeUID != 0) {
			t.Fatalf("sort %s: %+v", field, p)
		}
	}
	for _, q := range []mail.SearchQuery{
		{Body: "body"}, {Text: "text"}, {Limit: 201}, {Limit: -1},
		{Sort: "unknown"}, {Sort: "date_asc_desc"}, {Sort: "date", BeforeUID: 2}, {Sort: "uid_asc", BeforeUID: 2},
		{Since: "2026-02-30"}, {SentBefore: "2026-2-2"}, {Since: "2026-11-2"},
		{Headers: map[string]string{"bad header": "value"}}, {MinSize: -1}, {Mailbox: "other"},
		{Headers: map[string]string{"X-\x7f": "value"}},
	} {
		if _, err := c.Query(testContext, key, q); err == nil {
			t.Fatalf("accepted invalid/unsupported query: %+v", q)
		}
	}
}

func TestInvalidSnapshotsAndCancelledTransaction(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	for _, r := range []mail.Refresh{
		{UIDValidity: 0}, {UIDValidity: 1, Members: []uint32{0}}, {UIDValidity: 1, Members: []uint32{1, 1}},
		{UIDValidity: 1, NewMetadata: []mail.Metadata{metadata(1, "Subject: absent\r\n\r\n")}},
		{UIDValidity: 1, Members: []uint32{1}, FlagUpdates: []mail.FlagUpdate{{UID: 1}, {UID: 1}}},
		{UIDValidity: 1, Members: []uint32{1}, FlagUpdates: []mail.FlagUpdate{{UID: 1, Flags: []string{"bad flag"}}}},
		snapshot(1, 0, mail.Metadata{UID: 1, Size: -1}),
		snapshot(1, 0, metadata(1, strings.Repeat("x", mail.MaxHeaderBytes+1))),
	} {
		if err := c.Apply(testContext, key, r); err == nil {
			t.Fatalf("accepted invalid snapshot: %+v", r)
		}
	}
	ctx, cancel := context.WithCancel(testContext)
	cancel()
	if err := c.Apply(ctx, key, snapshot(1, 1, metadata(1, "Subject: cancelled\r\n\r\n"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled apply: %v", err)
	}
	if p := query(t, c, key, mail.SearchQuery{}); len(p.Messages) != 0 || p.UIDValidity != 0 {
		t.Fatalf("cancelled transaction wrote rows: %+v", p)
	}
}

func TestSortDirections(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	items := []mail.Metadata{
		metadata(1, "Subject: Zulu\r\nFrom: z@example.com\r\nDate: Mon, 16 Nov 2026 12:00:00 +0000\r\n\r\n"),
		metadata(2, "Subject: Bravo\r\nFrom: b@example.com\r\nDate: Sun, 15 Nov 2026 12:00:00 +0000\r\n\r\n"),
		metadata(3, "Subject: Alpha\r\nFrom: a@example.com\r\nDate: Sat, 14 Nov 2026 12:00:00 +0000\r\n\r\n"),
	}
	for i := range items {
		items[i].Size = int64(30 - i*10)
		items[i].InternalDate = items[i].InternalDate.Add(-time.Duration(i) * 24 * time.Hour)
	}
	apply(t, c, key, snapshot(1, 1, items...))
	for _, field := range []string{"date", "arrival", "subject", "from", "size"} {
		for _, suffix := range []string{"", "_asc", "_desc"} {
			want := []uint32{3, 2, 1}
			if suffix == "_desc" {
				want = []uint32{1, 2, 3}
			}
			if got := uids(query(t, c, key, mail.SearchQuery{Sort: field + suffix})); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s%s got %v, want %v", field, suffix, got, want)
			}
		}
	}
	if got := uids(query(t, c, key, mail.SearchQuery{Sort: "uid_asc"})); !reflect.DeepEqual(got, []uint32{1, 2, 3}) {
		t.Fatalf("UID ascending: %v", got)
	}
	if p := query(t, c, key, mail.SearchQuery{Sort: "UID_DESC", Limit: 2}); !reflect.DeepEqual(uids(p), []uint32{3, 2}) || p.NextBeforeUID != 2 {
		t.Fatalf("UID descending cursor: %+v", p)
	}
	if p := query(t, c, key, mail.SearchQuery{MinSize: 20, MaxSize: 30}); len(p.Messages) != 0 {
		t.Fatalf("size criteria included strict endpoints: %+v", p)
	}
}

func TestSafeRefreshCheckpointAndUnavailableModSeq(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	r := snapshot(1, 10, metadata(1, "Subject: changed during refresh\r\n\r\n"))
	r.FlagUpdates[0].ModSeq = 12
	apply(t, c, key, r)
	state, err := c.State(testContext, key)
	if err != nil || state.HighestModSeq != 10 {
		t.Fatalf("safe checkpoint: %+v, %v", state, err)
	}
	if p := query(t, c, key, mail.SearchQuery{}); p.Messages[0].ModSeq != 12 {
		t.Fatalf("message MODSEQ lost: %+v", p)
	}
	if err := c.Apply(testContext, key, mail.Refresh{UIDValidity: 1, HighestModSeq: 11, Members: []uint32{1}, FlagUpdates: []mail.FlagUpdate{{UID: 1, ModSeq: 9}}}); err == nil {
		t.Fatal("accepted stale per-message MODSEQ")
	}
	// Losing CONDSTORE support forces an ordinary complete flag refresh.
	apply(t, c, key, mail.Refresh{UIDValidity: 1, Members: []uint32{1}, FlagUpdates: []mail.FlagUpdate{{UID: 1, Flags: []string{}}}})
	state, err = c.State(testContext, key)
	if err != nil || state.HighestModSeq != 0 || len(query(t, c, key, mail.SearchQuery{}).Messages[0].Flags) != 0 {
		t.Fatalf("unavailable MODSEQ: %+v, %v", state, err)
	}
}

func TestEmptyHeader(t *testing.T) {
	c := openTestCache(t)
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(1, 0, metadata(1, "")))
	p := query(t, c, key, mail.SearchQuery{})
	if len(p.Messages) != 1 || p.Messages[0].Subject != "" || p.Messages[0].Date != nil || len(p.Messages[0].From) != 0 {
		t.Fatalf("empty header summary: %+v", p)
	}
}

func TestCacheLockProcess(t *testing.T) {
	if path := os.Getenv("IMAP_CACHE_LOCK_TEST"); path != "" {
		c, err := Open(path)
		if err == nil {
			c.Close()
			t.Fatal("second process acquired cache")
		}
		if !strings.Contains(err.Error(), "already owned") {
			t.Fatalf("unclear lock error: %v", err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "headers.sqlite")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestCacheLockProcess$")
	cmd.Env = append(os.Environ(), "IMAP_CACHE_LOCK_TEST="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("second process: %v\n%s", err, output)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("close did not release lock: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivatePermissions(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "new-private", "headers.sqlite")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{parent: 0755, filepath.Dir(path): 0700, path: 0600, path + ".lock": 0600} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions for %s: %v, %v (want %o)", name, info, err, mode)
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("existing cache is not private: %v, %v", info, err)
	}
}

func TestLiteralSQLiteFilename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers ?mode=memory#%.sqlite")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := mail.MailboxKey{Account: "one", Name: "INBOX"}
	apply(t, c, key, snapshot(1, 1, metadata(1, "Subject: persisted\r\n\r\n")))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("literal cache path was not used: %v, %v", info, err)
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if p := query(t, c, key, mail.SearchQuery{}); len(p.Messages) != 1 || p.Messages[0].Subject != "persisted" {
		t.Fatalf("literal path did not persist: %+v", p)
	}
}
