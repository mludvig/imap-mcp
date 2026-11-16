package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/mludvig/imap-mcp/internal/mail"
)

func (s *Service) refresh(ctx context.Context, key mail.MailboxKey) (uint32, error) {
	state, err := s.store.State(ctx, key)
	if err != nil {
		return 0, safeError(err, "cache_error", false)
	}
	updated, err := s.backend.Refresh(ctx, key.Name, state)
	if err != nil {
		return 0, safeError(err, "server_error", false)
	}
	if err := s.store.Apply(ctx, key, updated); err != nil {
		return 0, safeError(err, "cache_error", false)
	}
	return updated.UIDValidity, nil
}

func (s *Service) SearchMessages(ctx context.Context, input mail.SearchQuery) (mail.MessagePage, error) {
	q, err := validateQuery(input)
	if err != nil {
		return mail.MessagePage{}, err
	}
	if err := s.lock(ctx); err != nil {
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
			page, err := s.store.Query(ctx, key, q)
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
	status, err := s.backend.MailboxStatus(ctx, name)
	if err != nil {
		return safeError(err, "server_error", false)
	}
	if status.UIDValidity != epoch {
		return mail.NewError("mailbox_changed", "Mailbox UIDVALIDITY changed during search")
	}
	return nil
}

func (s *Service) livePage(ctx context.Context, key mail.MailboxKey, q mail.SearchQuery, epoch uint32) (mail.MessagePage, error) {
	live := q
	// The cursor is applied locally so Total counts all matches like Cache.Query.
	live.BeforeUID = 0
	uids, err := s.backend.SearchUIDs(ctx, live)
	if err != nil {
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
	page.Messages, err = s.store.Get(ctx, key, selected)
	if err != nil {
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
