package service

import (
	"context"
	"testing"

	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestRecentQueriesRejectedBeforeDispatch(t *testing.T) {
	b := &fakeBackend{refresh: func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
		return mail.Refresh{}, mail.NewError("unexpected_dispatch", "Recent query reached backend")
	}}
	s := newService(t, true, b)
	for _, q := range []mail.SearchQuery{
		{Flags: []string{"\\Recent"}},
		{NotFlags: []string{"\\rEcEnT"}},
		{Flags: []string{"\\Recent"}, Body: "live query"},
		{NotFlags: []string{"\\Recent"}, Sort: "to"},
	} {
		_, err := s.SearchMessages(context.Background(), q)
		expectCode(t, err, "invalid_argument")
		_, err = s.ThreadMessages(context.Background(), mail.ThreadRequest{Query: q})
		expectCode(t, err, "invalid_argument")
	}
	if len(b.calls) != 0 {
		t.Fatalf("invalid Recent query dispatched: %v", b.calls)
	}
	if _, err := validateQuery(mail.SearchQuery{Flags: []string{"Recent"}}); err != nil {
		t.Fatalf("ordinary keyword Recent rejected: %v", err)
	}
}
