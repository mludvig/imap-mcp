package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func (s *Service) logSearchError(ctx context.Context, stage, name string, err error) {
	var typed *mail.Error
	// Typed backend failures already have their underlying cause logged by the client.
	if err == nil || errors.As(err, &typed) {
		return
	}
	cfg := s.cfg.IMAP()
	diagnostics.Logger(ctx).ErrorContext(ctx, "Mailbox search failed", "stage", stage, "host", cfg.Host(), "username", cfg.Username(), "mailbox", name, "error", err)
}

func (s *Service) refresh(ctx context.Context, key mail.MailboxKey) (uint32, error) {
	defer diagnostics.Measure(ctx, "mailbox refresh")()
	diagnostics.Stage(ctx, "Reading cached mailbox state")
	done := diagnostics.Measure(ctx, "cache state")
	state, err := s.store.State(ctx, key)
	done()
	if err != nil {
		s.logSearchError(ctx, "cache state", key.Name, err)
		return 0, safeError(err, "cache_error", false)
	}
	diagnostics.Stage(ctx, "Synchronizing mailbox headers; first synchronization may take longer")
	done = diagnostics.Measure(ctx, "backend refresh")
	updated, err := s.backend.Refresh(ctx, key.Name, state)
	done()
	if err != nil {
		s.logSearchError(ctx, "backend refresh", key.Name, err)
		return 0, safeError(err, "server_error", false)
	}
	diagnostics.Stage(ctx, "Applying refreshed mailbox headers")
	done = diagnostics.Measure(ctx, "refresh cache apply")
	err = s.store.Apply(ctx, key, updated)
	done()
	if err != nil {
		s.logSearchError(ctx, "refresh cache apply", key.Name, err)
		return 0, safeError(err, "cache_error", false)
	}
	return updated.UIDValidity, nil
}

func (s *Service) SearchMessages(ctx context.Context, input mail.SearchQuery) (mail.MessagePage, error) {
	q, err := validateQuery(input)
	if err != nil {
		return mail.MessagePage{}, err
	}
	diagnostics.Stage(ctx, "Waiting for mailbox access")
	done := diagnostics.Measure(ctx, "mailbox access wait")
	err = s.lock(ctx)
	done()
	if err != nil {
		return mail.MessagePage{}, err
	}
	defer s.unlock()
	key := s.key(q.Mailbox)
	field := strings.TrimSuffix(strings.TrimSuffix(q.Sort, "_asc"), "_desc")
	live := q.Body != "" || q.Text != "" || field == "to" || field == "cc"
	for attempt := 0; attempt < 2; attempt++ {
		epoch, err := s.refresh(ctx, key)
		if err != nil {
			return mail.MessagePage{}, err
		}
		if !live {
			diagnostics.Stage(ctx, "Searching local mailbox headers")
			done := diagnostics.Measure(ctx, "local header query")
			page, err := s.store.Query(ctx, key, q)
			done()
			s.logSearchError(ctx, "local header query", key.Name, err)
			return page, safeError(err, "cache_error", false)
		}
		page, err := s.livePage(ctx, key, q, epoch)
		if err == nil {
			return page, nil
		}
		var typed *mail.Error
		if !errors.As(err, &typed) || (typed.Code != "mailbox_changed" && typed.Code != "cache_miss") {
			return mail.MessagePage{}, err
		}
	}
	return mail.MessagePage{}, mail.NewError("mailbox_changed", "Mailbox changed during search; try again")
}

func (s *Service) ListMessages(ctx context.Context, q mail.SearchQuery) (mail.MessagePage, error) {
	if q.From != "" || q.To != "" || q.Cc != "" || q.Bcc != "" || q.Subject != "" || q.Body != "" || q.Text != "" || len(q.Headers) != 0 || q.Since != "" || q.Before != "" || q.SentSince != "" || q.SentBefore != "" || len(q.Flags) != 0 || len(q.NotFlags) != 0 || q.MinSize != 0 || q.MaxSize != 0 {
		return mail.MessagePage{}, invalid("List accepts only mailbox, limit, before_uid, and sort")
	}
	return s.SearchMessages(ctx, q)
}

func (s *Service) sameEpoch(ctx context.Context, name string, epoch uint32) error {
	diagnostics.Stage(ctx, "Checking mailbox UID epoch")
	defer diagnostics.Measure(ctx, "mailbox epoch check")()
	status, err := s.backend.MailboxStatus(ctx, name)
	if err != nil {
		s.logSearchError(ctx, "mailbox epoch check", name, err)
		return safeError(err, "server_error", false)
	}
	if status.UIDValidity != epoch {
		return mail.NewError("mailbox_changed", "Mailbox UIDVALIDITY changed during search")
	}
	return nil
}

func (s *Service) livePage(ctx context.Context, key mail.MailboxKey, q mail.SearchQuery, epoch uint32) (mail.MessagePage, error) {
	defer diagnostics.Measure(ctx, "live search page")()
	live := q
	// The cursor is applied locally so Total counts all matches like Cache.Query.
	live.BeforeUID = 0
	diagnostics.Stage(ctx, "Searching the IMAP server")
	done := diagnostics.Measure(ctx, "backend search")
	uids, err := s.backend.SearchUIDs(ctx, live)
	done()
	if err != nil {
		s.logSearchError(ctx, "backend search", key.Name, err)
		return mail.MessagePage{}, safeError(err, "server_error", false)
	}
	seen := make(map[uint32]bool, len(uids))
	for _, uid := range uids {
		if uid == 0 || seen[uid] {
			return mail.MessagePage{}, mail.NewError("server_error", "IMAP search returned invalid UIDs")
		}
		seen[uid] = true
	}
	if err := s.sameEpoch(ctx, key.Name, epoch); err != nil {
		return mail.MessagePage{}, err
	}
	if q.Sort == "uid_desc" || q.Sort == "uid_asc" {
		uids = append([]uint32(nil), uids...)
		sort.Slice(uids, func(i, j int) bool {
			if q.Sort == "uid_asc" {
				return uids[i] < uids[j]
			}
			return uids[i] > uids[j]
		})
	}
	page := mail.MessagePage{UIDValidity: epoch, Total: len(uids)}
	selected := make([]uint32, 0, q.Limit)
	more := false
	for _, uid := range uids {
		if q.BeforeUID != 0 && uid >= q.BeforeUID {
			continue
		}
		if len(selected) == q.Limit {
			more = true
			break
		}
		selected = append(selected, uid)
	}
	diagnostics.Stage(ctx, "Reading matching cached message headers")
	done = diagnostics.Measure(ctx, "cache result lookup")
	page.Messages, err = s.store.Get(ctx, key, selected)
	done()
	if err != nil {
		s.logSearchError(ctx, "cache result lookup", key.Name, err)
		return mail.MessagePage{}, safeError(err, "cache_error", false)
	}
	for _, message := range page.Messages {
		if message.Ref.UIDValidity != epoch {
			return mail.MessagePage{}, mail.NewError("mailbox_changed", "Cached mailbox UIDVALIDITY changed during search")
		}
	}
	// A second live check covers epoch changes during the bounded SQLite lookup.
	if err := s.sameEpoch(ctx, key.Name, epoch); err != nil {
		return mail.MessagePage{}, err
	}
	if more && q.Sort == "uid_desc" {
		page.NextBeforeUID = selected[len(selected)-1]
	}
	return page, nil
}
