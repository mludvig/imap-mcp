// Package mime decodes live messages and cached headers without retaining bodies.
package mime

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	stdmime "mime"
	"mime/multipart"
	stdmail "net/mail"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	messagemail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
	"github.com/mludvig/imap-mcp/internal/mail"
)

// These structural limits bound recursion and work independently of mailbox size.
const (
	maxDepth = 32
	maxParts = 1000
)

// Summary decodes only the cached RFC 822 header and preserves its Date offset.
// Invalid optional fields remain readable; malformed header structure is an error.
func Summary(ref mail.MessageRef, metadata mail.Metadata, flags []string, modseq uint64) (mail.MessageSummary, error) {
	if len(metadata.Header) > mail.MaxHeaderBytes {
		return mail.MessageSummary{}, mail.NewError("message_too_large", "message header exceeds size limit")
	}
	header, err := textproto.ReadHeader(bufio.NewReader(io.MultiReader(
		bytes.NewReader(metadata.Header), strings.NewReader("\r\n\r\n"))))
	if err != nil {
		return mail.MessageSummary{}, invalidMessage()
	}
	return summarize(ref, metadata, flags, modseq, messagemail.Header{Header: message.Header{Header: header}}), nil
}

func summarize(ref mail.MessageRef, metadata mail.Metadata, flags []string, modseq uint64, header messagemail.Header) mail.MessageSummary {
	subject, err := header.Subject()
	if err != nil {
		subject = decodeWord(header.Get("Subject"))
	}
	summary := mail.MessageSummary{
		Ref: ref, Subject: validText(subject), InternalDate: metadata.InternalDate,
		Size: metadata.Size, Flags: append([]string{}, flags...), ModSeq: modseq,
		From: addresses(header, "From"), To: addresses(header, "To"),
		Cc: addresses(header, "Cc"), Bcc: addresses(header, "Bcc"),
	}
	if date, err := header.Date(); err == nil && !date.IsZero() {
		// net/mail accepts offsets such as +2400 that time.Time cannot encode as JSON.
		if _, err := date.MarshalJSON(); err == nil {
			summary.Date = &date
		}
	}
	return summary
}

func addresses(header messagemail.Header, key string) []string {
	result := []string{}
	parsed, err := header.AddressList(key)
	if err != nil {
		// Unknown charsets must not make otherwise valid address lists disappear.
		parser := stdmail.AddressParser{WordDecoder: &stdmime.WordDecoder{CharsetReader: fallbackCharset}}
		parsed, err = parser.ParseList(validText(header.Get(key)))
	}
	if err != nil {
		if value := header.Get(key); value != "" {
			return []string{decodeWord(value)}
		}
		return result
	}
	for _, address := range parsed {
		if address.Name == "" {
			result = append(result, address.Address)
		} else {
			// net/mail.Address.String re-encodes Unicode names as RFC 2047 words.
			name := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(validText(address.Name))
			result = append(result, "\""+name+"\" <"+address.Address+">")
		}
	}
	return result
}

func decodeWord(value string) string {
	decoder := stdmime.WordDecoder{CharsetReader: fallbackCharset}
	decoded, err := decoder.DecodeHeader(value)
	if err != nil {
		return validText(value)
	}
	return validText(decoded)
}

func fallbackCharset(charset string, input io.Reader) (io.Reader, error) {
	if decoded, err := message.CharsetReader(charset, input); err == nil {
		return decoded, nil
	}
	// Unknown encodings retain ASCII and replace invalid UTF-8 deterministically.
	raw, err := readBounded(input, mail.MaxHeaderBytes)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(validText(string(raw))), nil
}

// Parse returns decoded text and a MIME attachment catalogue from a live fetch.
func Parse(ref mail.MessageRef, raw []byte, metadata mail.Metadata, flags []string, req mail.ReadRequest) (mail.Message, error) {
	limit := req.MaxTextChars
	if limit == 0 {
		limit = mail.DefaultTextChars
	}
	if limit < 0 || limit > mail.MaxTextChars {
		return mail.Message{}, mail.NewError("invalid_argument", "max_text_chars is outside the allowed range")
	}
	scan, header, err := parse(raw, "")
	if err != nil {
		return mail.Message{}, err
	}
	result := mail.Message{
		Summary:     summarize(ref, metadata, flags, 0, messagemail.Header{Header: header}),
		Attachments: scan.attachments,
	}
	result.Text, result.Truncated = truncate(strings.Join(scan.plain, "\n"), limit)
	if req.IncludeHTML {
		var truncated bool
		result.HTML, truncated = truncate(strings.Join(scan.html, "\n"), limit)
		result.Truncated = result.Truncated || truncated
	}
	return result, nil
}

// Attachment returns transfer-decoded bytes, not a charset-converted text file.
func Attachment(ref mail.MessageRef, raw []byte, partID string) (mail.Attachment, error) {
	if partID == "" {
		return mail.Attachment{}, mail.NewError("invalid_argument", "part_id is required")
	}
	scan, _, err := parse(raw, partID)
	if err != nil {
		return mail.Attachment{}, err
	}
	if scan.selected == nil {
		return mail.Attachment{}, mail.NewError("not_found", "attachment part was not found")
	}
	return mail.Attachment{Info: *scan.selected, DataBase64: base64.StdEncoding.EncodeToString(scan.payload)}, nil
}

type scanner struct {
	parts       int
	decoded     int
	attachments []mail.AttachmentInfo
	plain       []string
	html        []string
	wanted      string
	selected    *mail.AttachmentInfo
	payload     []byte
}

func parse(raw []byte, wanted string) (*scanner, message.Header, error) {
	if len(raw) > mail.MaxMessageBytes {
		return nil, message.Header{}, mail.NewError("message_too_large", "message exceeds size limit")
	}
	entity, err := readEntity(bytes.NewReader(raw))
	if err != nil {
		return nil, message.Header{}, err
	}
	scan := &scanner{wanted: wanted, attachments: []mail.AttachmentInfo{}}
	path := "1"
	if multipartType(entity.Header) {
		path = ""
	}
	if err := scan.walk(entity, path, 0, false); err != nil {
		return nil, message.Header{}, err
	}
	return scan, entity.Header, nil
}

func readEntity(input io.Reader) (*message.Entity, error) {
	limited := &io.LimitedReader{R: input, N: mail.MaxHeaderBytes + 1}
	buffered := bufio.NewReader(limited)
	header, err := textproto.ReadHeader(buffered)
	if mail.MaxHeaderBytes+1-limited.N-int64(buffered.Buffered()) > mail.MaxHeaderBytes {
		return nil, mail.NewError("message_too_large", "message header exceeds size limit")
	}
	if err != nil {
		return nil, invalidMessage()
	}
	limited.N = mail.MaxMessageBytes + 1
	return newEntity(message.Header{Header: header}, buffered)
}

func newEntity(header message.Header, body io.Reader) (*message.Entity, error) {
	contentType, params, err := header.ContentType()
	if err != nil {
		return nil, invalidMessage()
	}
	copyHeader := header.Copy()
	if strings.HasPrefix(contentType, "text/") {
		// Remove charset for transfer decoding; attachment bytes must stay intact.
		delete(params, "charset")
		copyHeader.Set("Content-Type", stdmime.FormatMediaType(contentType, params))
	}
	entity, err := message.New(copyHeader, body)
	if err != nil {
		return nil, invalidMessage()
	}
	entity.Header = header
	return entity, nil
}

func multipartType(header message.Header) bool {
	contentType, _, _ := header.ContentType()
	return strings.HasPrefix(contentType, "multipart/")
}

func (scan *scanner) walk(entity *message.Entity, path string, depth int, suppressText bool) error {
	scan.parts++
	if depth > maxDepth || scan.parts > maxParts {
		return mail.NewError("invalid_message", "MIME structure exceeds nesting or part limit")
	}
	contentType, params, _ := entity.Header.ContentType()
	disposition, dispositionParams, err := entity.Header.ContentDisposition()
	if err != nil && entity.Header.Get("Content-Disposition") != "" {
		return invalidMessage()
	}
	filename := dispositionParams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	isAttachment := disposition == "attachment" || filename != "" ||
		entity.Header.Get("Content-ID") != "" || (!strings.HasPrefix(contentType, "text/") && !multipartType(entity.Header))
	if multipartType(entity.Header) {
		if params["boundary"] == "" {
			return invalidMessage()
		}
		reader := multipart.NewReader(entity.Body, params["boundary"])
		for index := 1; ; index++ {
			part, err := reader.NextRawPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return invalidMessage()
			}
			headerBytes := 0
			for key, values := range part.Header {
				for _, value := range values {
					headerBytes += len(key) + len(value) + 4
				}
			}
			if headerBytes > mail.MaxHeaderBytes {
				return mail.NewError("message_too_large", "MIME part header exceeds size limit")
			}
			child, err := newEntity(message.HeaderFromMap(part.Header), part)
			if err != nil {
				return err
			}
			childPath := strconv.Itoa(index)
			if path != "" {
				childPath = path + "." + childPath
			}
			if err := scan.walk(child, childPath, depth+1, suppressText || isAttachment); err != nil {
				return err
			}
		}
	}
	payload, err := readBounded(entity.Body, mail.MaxMessageBytes-scan.decoded)
	if err != nil {
		return err
	}
	scan.decoded += len(payload)
	if isAttachment {
		info := mail.AttachmentInfo{
			PartID: path, Filename: validText(filename), ContentType: contentType,
			Disposition: disposition, Size: int64(len(payload)),
		}
		if info.Disposition == "" {
			info.Disposition = "inline"
			if contentType == "message/rfc822" {
				info.Disposition = "attachment"
			}
		}
		scan.attachments = append(scan.attachments, info)
		if path == scan.wanted {
			if len(payload) > mail.MaxAttachmentBytes {
				return mail.NewError("attachment_too_large", "attachment exceeds size limit")
			}
			scan.selected, scan.payload = &info, payload
		}
	}
	if contentType == "message/rfc822" {
		nested, err := readEntity(bytes.NewReader(payload))
		if err != nil {
			return err
		}
		nestedPath := path
		if !multipartType(nested.Header) {
			nestedPath += ".1"
		}
		return scan.walk(nested, nestedPath, depth+1, true)
	}
	if !isAttachment && !suppressText && (contentType == "text/plain" || contentType == "text/html") && scan.wanted == "" {
		input := io.Reader(bytes.NewReader(payload))
		if charset := params["charset"]; charset != "" {
			if converted, err := message.CharsetReader(charset, input); err == nil {
				input = converted
			}
		}
		text, err := readBounded(input, mail.MaxMessageBytes)
		if err != nil {
			return err
		}
		if contentType == "text/plain" {
			scan.plain = append(scan.plain, validText(string(text)))
		} else {
			scan.html = append(scan.html, validText(string(text)))
		}
	}
	return nil
}

func readBounded(input io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, int64(limit)+1))
	if err != nil {
		return nil, invalidMessage()
	}
	if len(data) > limit {
		return nil, mail.NewError("message_too_large", "decoded message exceeds size limit")
	}
	return data, nil
}

func validText(text string) string { return strings.ToValidUTF8(text, "\uFFFD") }

func truncate(text string, limit int) (string, bool) {
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	count := 0
	for offset := range text {
		if count == limit {
			return text[:offset], true
		}
		count++
	}
	return text, false
}

func invalidMessage() *mail.Error {
	return mail.NewError("invalid_message", "message contains malformed MIME data")
}
