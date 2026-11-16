package service

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestThreadRejectsUnsupportedControlsBeforeQueryDefaults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query mail.SearchQuery
	}{
		{"limit", mail.SearchQuery{Limit: 1}},
		{"negative_limit", mail.SearchQuery{Limit: -1}},
		{"before_uid", mail.SearchQuery{BeforeUID: 100}},
		{"sort", mail.SearchQuery{Sort: "uid_desc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{}
			s := newService(t, true, b)
			tc.query.Since = "invalid date"
			_, err := s.ThreadMessages(context.Background(), mail.ThreadRequest{Query: tc.query})
			typed := expectCode(t, err, "invalid_argument")
			if !strings.Contains(typed.Message, "THREAD") || !strings.Contains(typed.Message, strings.TrimPrefix(tc.name, "negative_")) {
				t.Fatalf("unsupported control did not precede query validation: %v", err)
			}
			if len(b.calls) != 0 {
				t.Fatalf("unsupported THREAD control dispatched: %v", b.calls)
			}
		})
	}
}

func TestThreadPreservesCriteriaWithoutPagingDefaults(t *testing.T) {
	b := &fakeBackend{}
	s := newService(t, true, b)
	q := mail.SearchQuery{
		Mailbox: "inbox", From: "sender", To: "recipient", Cc: "copy", Bcc: "blind",
		Subject: "subject", Body: "body", Text: "text", Headers: map[string]string{"X-Test": "value"},
		Since: "2026-01-01", Before: "2026-02-01", SentSince: "2025-12-01", SentBefore: "2026-03-01",
		Flags: []string{"\\Seen"}, NotFlags: []string{"\\Deleted"}, MinSize: 1, MaxSize: 1000,
	}
	_, err := s.ThreadMessages(context.Background(), mail.ThreadRequest{Mailbox: "inbox", Algorithm: "orderedsubject", Query: q})
	if err != nil {
		t.Fatal(err)
	}
	q.Mailbox = "INBOX"
	want := mail.ThreadRequest{Mailbox: "INBOX", Algorithm: "ORDEREDSUBJECT", Query: q}
	if !reflect.DeepEqual(b.threadRequest, want) || !reflect.DeepEqual(b.calls, []string{"thread"}) {
		t.Fatalf("thread=%+v calls=%v, want %+v", b.threadRequest, b.calls, want)
	}
}

func TestThreadInvalidCriteriaNeverReachBackend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request mail.ThreadRequest
	}{
		{"mailbox_mismatch", mail.ThreadRequest{Mailbox: "Archive", Query: mail.SearchQuery{Mailbox: "Other"}}},
		{"date", mail.ThreadRequest{Query: mail.SearchQuery{Since: "2026-02-30"}}},
		{"header", mail.ThreadRequest{Query: mail.SearchQuery{Headers: map[string]string{"X:Test": "value"}}}},
		{"size", mail.ThreadRequest{Query: mail.SearchQuery{MinSize: -1}}},
		{"flag", mail.ThreadRequest{Query: mail.SearchQuery{Flags: []string{"\\Unknown"}}}},
		{"algorithm", mail.ThreadRequest{Algorithm: "unsupported"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{}
			s := newService(t, true, b)
			_, err := s.ThreadMessages(context.Background(), tc.request)
			expectCode(t, err, "invalid_argument")
			if len(b.calls) != 0 {
				t.Fatalf("invalid THREAD request dispatched: %v", b.calls)
			}
		})
	}
}
