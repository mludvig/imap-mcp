package client

import (
	"context"

	"github.com/mludvig/imap-mcp/internal/mail"
	"github.com/mludvig/imap-mcp/internal/mime"
)

func (c *Client) ReadMessage(ctx context.Context, req mail.ReadRequest) (mail.Message, error) {
	if req.MaxTextChars < 0 || req.MaxTextChars > mail.MaxTextChars {
		return mail.Message{}, mail.NewError("invalid_argument", "max_text_chars is outside the allowed range")
	}
	s, err := c.open(ctx)
	if err != nil {
		return mail.Message{}, err
	}
	defer s.close()
	metadata, flags, modseq, raw, err := s.rawMessage(req.Ref)
	if err != nil {
		return mail.Message{}, err
	}
	message, err := mime.Parse(req.Ref, raw, metadata, flags, req)
	if err == nil {
		message.Summary.ModSeq = modseq
	}
	return message, err
}
func (c *Client) GetAttachment(ctx context.Context, req mail.AttachmentRequest) (mail.Attachment, error) {
	if req.PartID == "" {
		return mail.Attachment{}, mail.NewError("invalid_argument", "part_id is required")
	}
	s, err := c.open(ctx)
	if err != nil {
		return mail.Attachment{}, err
	}
	defer s.close()
	_, _, _, raw, err := s.rawMessage(req.Ref)
	if err != nil {
		return mail.Attachment{}, err
	}
	return mime.Attachment(req.Ref, raw, req.PartID)
}
