package cache

import (
	"bufio"
	"bytes"
	"strings"

	message "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/textproto"
	"github.com/mludvig/imap-mcp/internal/mail"
	mailMIME "github.com/mludvig/imap-mcp/internal/mime"
)

func parseMetadata(ref mail.MessageRef, metadata mail.Metadata) (record, error) {
	if metadata.Size < 0 || metadata.InternalDate.IsZero() || metadata.InternalDate.Year() < 1 || metadata.InternalDate.Year() > 9999 {
		return record{}, mail.NewError("invalid_argument", "metadata requires a nonnegative size and valid internal date")
	}
	if len(metadata.Header) > mail.MaxHeaderBytes {
		return record{}, mail.NewError("too_large", "message header exceeds cache limit")
	}
	// A metadata payload must end at the header/body separator, not include a body.
	for _, separator := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if index := bytes.Index(metadata.Header, separator); index >= 0 && index+len(separator) != len(metadata.Header) {
			return record{}, mail.NewError("invalid_argument", "cached metadata must contain only headers")
		}
	}
	header, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(append(append([]byte{}, metadata.Header...), '\r', '\n', '\r', '\n'))))
	if err != nil {
		return record{}, mail.NewError("invalid_argument", "invalid cached message header")
	}
	summary, err := mailMIME.Summary(ref, metadata, nil, 0)
	if err != nil {
		return record{}, err
	}
	if summary.Date != nil {
		if _, err := summary.Date.MarshalJSON(); err != nil {
			// An invalid optional Date must not prevent caching the other headers.
			summary.Date = nil
		}
	}
	h := message.Header{Header: header}
	item := record{
		headers: make(map[string][]string),
		summary: summary,
	}
	fields := h.Fields()
	for fields.Next() {
		value, _ := fields.Text() // Unknown encodings preserve the raw field value.
		key := strings.ToLower(fields.Key())
		item.headers[key] = append(item.headers[key], value)
	}
	return item, nil
}
