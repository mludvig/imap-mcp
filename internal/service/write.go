package service

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/mludvig/imap-mcp/internal/mail"
)

func (s *Service) writeGuard() error {
	if s.Readonly() {
		return mail.NewError("readonly", "This server is configured read-only; restart with readonly=false to enable writes")
	}
	return nil
}

func (s *Service) invalidate(ctx context.Context, names ...string) error {
	keys := make([]mail.MailboxKey, 0, len(names))
	seen := map[mail.MailboxKey]bool{}
	for _, name := range names {
		key := s.key(name)
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	return safeError(s.store.Invalidate(ctx, keys), "cache_error", false)
}

func (s *Service) flags(ctx context.Context, q mail.FlagRequest, adding bool) (mail.MutationResult, error) {
	if err := s.writeGuard(); err != nil {
		return mail.MutationResult{}, err
	}
	refs, err := validateRefs(q.Refs)
	if err != nil {
		return mail.MutationResult{}, err
	}
	q.Refs = refs
	if len(q.Flags) == 0 {
		return mail.MutationResult{}, invalid("At least one flag is required")
	}
	if err := validateFlags(q.Flags, true, adding); err != nil {
		return mail.MutationResult{}, err
	}
	if err := s.lock(ctx); err != nil {
		return mail.MutationResult{}, err
	}
	defer s.unlock()
	if err := s.invalidate(ctx, q.Refs[0].Mailbox); err != nil {
		return mail.MutationResult{}, err
	}
	var result mail.MutationResult
	if adding {
		result, err = s.backend.AddFlags(ctx, q)
	} else {
		result, err = s.backend.RemoveFlags(ctx, q)
	}
	return result, safeError(err, "server_error", true)
}

func (s *Service) AddFlags(ctx context.Context, q mail.FlagRequest) (mail.MutationResult, error) {
	return s.flags(ctx, q, true)
}
func (s *Service) RemoveFlags(ctx context.Context, q mail.FlagRequest) (mail.MutationResult, error) {
	return s.flags(ctx, q, false)
}

func (s *Service) transfer(ctx context.Context, q mail.TransferRequest, move bool) (mail.MutationResult, error) {
	if err := s.writeGuard(); err != nil {
		return mail.MutationResult{}, err
	}
	refs, err := validateRefs(q.Refs)
	if err != nil {
		return mail.MutationResult{}, err
	}
	q.Refs = refs
	if q.Destination == "" {
		return mail.MutationResult{}, invalid("Destination mailbox is required")
	}
	q.Destination = mailbox(q.Destination)
	if move && q.Refs[0].Mailbox == q.Destination {
		return mail.MutationResult{}, invalid("Move destination must differ from the source mailbox")
	}
	if err := s.lock(ctx); err != nil {
		return mail.MutationResult{}, err
	}
	defer s.unlock()
	names := []string{q.Destination}
	if move {
		names = append(names, q.Refs[0].Mailbox)
	}
	if err := s.invalidate(ctx, names...); err != nil {
		return mail.MutationResult{}, err
	}
	var result mail.MutationResult
	if move {
		result, err = s.backend.MoveMessages(ctx, q)
	} else {
		result, err = s.backend.CopyMessages(ctx, q)
	}
	return result, safeError(err, "server_error", true)
}

func (s *Service) CopyMessages(ctx context.Context, q mail.TransferRequest) (mail.MutationResult, error) {
	return s.transfer(ctx, q, false)
}
func (s *Service) MoveMessages(ctx context.Context, q mail.TransferRequest) (mail.MutationResult, error) {
	return s.transfer(ctx, q, true)
}

func (s *Service) TrashMessages(ctx context.Context, input []mail.MessageRef) (mail.MutationResult, error) {
	if err := s.writeGuard(); err != nil {
		return mail.MutationResult{}, err
	}
	refs, err := validateRefs(input)
	if err != nil {
		return mail.MutationResult{}, err
	}
	destination := mailbox(s.cfg.TrashMailbox())
	if refs[0].Mailbox == destination {
		return mail.MutationResult{}, invalid("Messages are already in the configured Trash mailbox")
	}
	if err := s.lock(ctx); err != nil {
		return mail.MutationResult{}, err
	}
	defer s.unlock()
	if err := s.invalidate(ctx, refs[0].Mailbox, destination); err != nil {
		return mail.MutationResult{}, err
	}
	info, err := s.backend.Info(ctx)
	if err != nil {
		return mail.MutationResult{}, safeError(err, "server_error", false)
	}
	// Gate before CREATE too: Trash must never use MOVE's delete/expunge fallback.
	// See docs/concepts/imap-safety.md.
	supported := false
	for _, feature := range append(append([]string(nil), info.Capabilities...), info.ImplementedFeatures...) {
		if strings.EqualFold(feature, "MOVE") || strings.EqualFold(feature, "IMAP4rev2") {
			supported = true
		}
	}
	if !supported {
		return mail.MutationResult{}, mail.NewError("unsupported", "The IMAP server does not support native MOVE")
	}
	boxes, err := s.backend.ListMailboxes(ctx)
	if err != nil {
		return mail.MutationResult{}, safeError(err, "server_error", false)
	}
	found := false
	for _, box := range boxes {
		if mailbox(box.Name) != destination {
			continue
		}
		found = true
		for _, attr := range box.Attributes {
			if strings.EqualFold(attr, "\\Noselect") {
				return mail.MutationResult{}, invalid("Configured Trash mailbox is not selectable")
			}
		}
	}
	if !found {
		if err := s.backend.CreateMailbox(ctx, destination); err != nil {
			return mail.MutationResult{}, safeError(err, "server_error", true)
		}
	}
	result, err := s.backend.MoveMessages(ctx, mail.TransferRequest{Refs: refs, Destination: destination})
	return result, safeError(err, "server_error", true)
}

func (s *Service) AppendMessage(ctx context.Context, q mail.AppendRequest) (mail.MutationResult, error) {
	if err := s.writeGuard(); err != nil {
		return mail.MutationResult{}, err
	}
	q.Mailbox = mailbox(q.Mailbox)
	if len(q.MessageBase64) > base64.StdEncoding.EncodedLen(mail.MaxMessageBytes) {
		return mail.MutationResult{}, mail.NewError("too_large", "Message exceeds the 10 MiB append limit")
	}
	raw, err := base64.StdEncoding.DecodeString(q.MessageBase64)
	if err != nil || len(raw) == 0 {
		return mail.MutationResult{}, invalid("Message must be nonempty valid base64")
	}
	if len(raw) > mail.MaxMessageBytes {
		return mail.MutationResult{}, mail.NewError("too_large", "Message exceeds the 10 MiB append limit")
	}
	if err := validateFlags(q.Flags, true, true); err != nil {
		return mail.MutationResult{}, err
	}
	if err := s.lock(ctx); err != nil {
		return mail.MutationResult{}, err
	}
	defer s.unlock()
	if err := s.invalidate(ctx, q.Mailbox); err != nil {
		return mail.MutationResult{}, err
	}
	result, err := s.backend.AppendMessage(ctx, q)
	return result, safeError(err, "server_error", true)
}
