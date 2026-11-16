// Package service coordinates live mailbox reads and durable cache invalidation.
// See docs/concepts/cache-coherence.md and docs/concepts/imap-safety.md.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/mail"
)

type Service struct {
	cfg     *config.Config
	backend mail.Backend
	store   *cache.Cache
	account string
	gate    chan struct{}
}

func New(cfg *config.Config, backend mail.Backend, store *cache.Cache) *Service {
	imap := cfg.IMAP()
	// JSON tuple boundaries avoid namespace collisions; passwords do not identify
	// accounts. See docs/concepts/message-identity.md.
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(imap.Host()), "."))
	identity, _ := json.Marshal([]any{host, imap.Port(), imap.TLS(), imap.Username()})
	digest := sha256.Sum256(identity)
	return &Service{cfg: cfg, backend: backend, store: store, account: hex.EncodeToString(digest[:]), gate: make(chan struct{}, 1)}
}

func (s *Service) Readonly() bool { return s.cfg.Readonly() }

func (s *Service) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) unlock() { <-s.gate }

func mailbox(name string) string {
	if name == "" || strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

func (s *Service) key(name string) mail.MailboxKey {
	return mail.MailboxKey{Account: s.account, Name: mailbox(name)}
}

// Unknown database and transport errors must not expose SQL, paths, or responses.
func safeError(err error, code string, writing bool) error {
	if err == nil {
		return nil
	}
	var typed *mail.Error
	if errors.As(err, &typed) {
		return typed
	}
	if !writing && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	message := "The IMAP operation failed"
	if code == "cache_error" {
		message = "The header cache operation failed"
	}
	return &mail.Error{Code: code, Message: message, OutcomeUnknown: writing}
}

func (s *Service) Info(ctx context.Context) (mail.ServerInfo, error) {
	info, err := s.backend.Info(ctx)
	info.Readonly = s.Readonly()
	return info, safeError(err, "server_error", false)
}

func (s *Service) ListMailboxes(ctx context.Context) ([]mail.MailboxInfo, error) {
	result, err := s.backend.ListMailboxes(ctx)
	return result, safeError(err, "server_error", false)
}

func (s *Service) MailboxStatus(ctx context.Context, name string) (mail.MailboxStatus, error) {
	result, err := s.backend.MailboxStatus(ctx, mailbox(name))
	return result, safeError(err, "server_error", false)
}

func (s *Service) ReadMessage(ctx context.Context, req mail.ReadRequest) (mail.Message, error) {
	if err := validateRef(req.Ref); err != nil {
		return mail.Message{}, err
	}
	if req.MaxTextChars < 0 || req.MaxTextChars > mail.MaxTextChars {
		return mail.Message{}, invalid("max_text_chars is outside the allowed range")
	}
	if req.MaxTextChars == 0 {
		req.MaxTextChars = mail.DefaultTextChars
	}
	req.Ref.Mailbox = mailbox(req.Ref.Mailbox)
	result, err := s.backend.ReadMessage(ctx, req)
	return result, safeError(err, "server_error", false)
}

func (s *Service) GetAttachment(ctx context.Context, req mail.AttachmentRequest) (mail.Attachment, error) {
	if err := validateRef(req.Ref); err != nil {
		return mail.Attachment{}, err
	}
	if req.PartID == "" {
		return mail.Attachment{}, invalid("part_id is required")
	}
	req.Ref.Mailbox = mailbox(req.Ref.Mailbox)
	result, err := s.backend.GetAttachment(ctx, req)
	return result, safeError(err, "server_error", false)
}

func (s *Service) ThreadMessages(ctx context.Context, req mail.ThreadRequest) (mail.ThreadResult, error) {
	req.Mailbox = mailbox(req.Mailbox)
	if req.Query.Mailbox != "" && mailbox(req.Query.Mailbox) != req.Mailbox {
		return mail.ThreadResult{}, invalid("Thread query mailbox must match the requested mailbox")
	}
	req.Query.Mailbox = req.Mailbox
	if req.Query.Limit != 0 || req.Query.BeforeUID != 0 || req.Query.Sort != "" {
		return mail.ThreadResult{}, invalid("THREAD returns complete trees and does not support limit, before_uid, or sort")
	}
	q, err := validateQuery(req.Query)
	if err != nil {
		return mail.ThreadResult{}, err
	}
	// Shared criteria validation applies search defaults; THREAD has no pages.
	// See docs/concepts/stdio-mcp.md for the complete-tree contract.
	q.Limit, q.Sort = 0, ""
	req.Query = q
	req.Algorithm = strings.ToUpper(req.Algorithm)
	if req.Algorithm == "" {
		req.Algorithm = "REFERENCES"
	}
	if req.Algorithm != "REFERENCES" && req.Algorithm != "ORDEREDSUBJECT" {
		return mail.ThreadResult{}, invalid("Unsupported THREAD algorithm")
	}
	result, err := s.backend.ThreadMessages(ctx, req)
	return result, safeError(err, "server_error", false)
}
