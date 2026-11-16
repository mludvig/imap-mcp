package mime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mludvig/imap-mcp/internal/mail"
)

var testRef = mail.MessageRef{Mailbox: "INBOX", UIDValidity: 5, UID: 9}

func TestSummaryDecodedHeadersAndDate(t *testing.T) {
	internalDate := time.Date(2026, 11, 1, 1, 2, 3, 0, time.UTC)
	header := "Subject: =?UTF-8?B?5pel5pys6Kqe?=\r\n" +
		"From: =?UTF-8?Q?Jos=C3=A9?= <jose@example.com>\r\n" +
		"To: Alice <alice@example.com>, Bob <bob@example.com>\r\n" +
		"Cc: cc@example.com\r\nBcc: bcc@example.com\r\n" +
		"Date: Fri, 20 Nov 1998 09:55:06 +1300\r\n\r\n"
	metadata := mail.Metadata{UID: testRef.UID, Size: 123, InternalDate: internalDate, Header: []byte(header)}
	flags := []string{`\Seen`}
	got, err := Summary(testRef, metadata, flags, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "日本語" || got.From[0] != `"José" <jose@example.com>` || len(got.To) != 2 ||
		got.Cc[0] != "cc@example.com" || got.Bcc[0] != "bcc@example.com" {
		t.Fatalf("unexpected decoded summary: %#v", got)
	}
	if got.Date == nil || got.Date.Format(time.RFC3339) != "1998-11-20T09:55:06+13:00" {
		t.Fatalf("Date offset changed: %v", got.Date)
	}
	flags[0] = "changed"
	if got.Ref != testRef || got.InternalDate != internalDate || got.Size != 123 || got.ModSeq != 7 || got.Flags[0] != `\Seen` {
		t.Fatalf("metadata changed: %#v", got)
	}
}

func TestSummaryMalformedOptionalFields(t *testing.T) {
	for _, test := range []struct {
		name    string
		header  string
		subject string
		from    string
	}{
		{"unknown charset", "Subject: =?X-UNKNOWN?B?Y2Fm6Q==?=\nFrom: =?X-UNKNOWN?B?Sm9z6Q==?= <j@example.com>\nDate: nonsense\n", "caf�", `"Jos�" <j@example.com>`},
		{"malformed encoded word", "Subject: =?UTF-8?B?%%%= \nFrom: broken address\n", "=?UTF-8?B?%%%=", "broken address"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := Summary(testRef, mail.Metadata{Header: []byte(test.header)}, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Subject != test.subject || len(got.From) != 1 || got.From[0] != test.from || got.Date != nil {
				t.Fatalf("unexpected fallback: %#v", got)
			}
		})
	}
	_, err := Summary(testRef, mail.Metadata{Header: []byte("not a header\r\n\r\n")}, nil, 0)
	requireCode(t, err, "invalid_message")
	_, err = Summary(testRef, mail.Metadata{Header: []byte(strings.Repeat("x", mail.MaxHeaderBytes+1))}, nil, 0)
	requireCode(t, err, "message_too_large")
}

func TestSummaryInvalidDateRemainsJSONEncodable(t *testing.T) {
	header := "Subject: readable mail\r\nDate: Mon, 16 Nov 2026 00:15:00 +2400\r\n\r\n"
	got, err := Summary(testRef, mail.Metadata{Header: []byte(header)}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("invalid optional Date prevents JSON encoding: %v", err)
	}
	if got.Subject != "readable mail" || got.Date != nil {
		t.Fatalf("unexpected summary: %#v", got)
	}
	if strings.Contains(string(encoded), `"date":`) {
		t.Fatalf("invalid Date should be omitted: %s", encoded)
	}
}

func TestParseInvalidDateRemainsJSONEncodable(t *testing.T) {
	raw := []byte("Subject: readable mail\r\nDate: Mon, 16 Nov 2026 00:15:00 +2400\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nReadable body")
	got, err := Parse(testRef, raw, mail.Metadata{}, nil, mail.ReadRequest{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("invalid optional Date prevents JSON encoding: %v", err)
	}
	if got.Summary.Subject != "readable mail" || got.Summary.Date != nil || got.Text != "Readable body" {
		t.Fatalf("unexpected message: %#v", got)
	}
	if strings.Contains(string(encoded), `"date":`) {
		t.Fatalf("invalid Date should be omitted: %s", encoded)
	}
}

const mixedMessage = "Subject: =?UTF-8?Q?caf=C3=A9?=\r\n" +
	"Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
	"--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
	"--inner\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"Hello =E4=B8=96=E7=95=8C\r\n" +
	"--inner\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
	"PHA+SGVsbG8g5LiW55WMPC9wPg==\r\n--inner--\r\n" +
	"--outer\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename*=utf-8''caf%C3%A9.bin\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nAAECAw==\r\n" +
	"--outer\r\nContent-Type: image/png\r\nContent-ID: <logo@example.com>\r\nContent-Disposition: inline\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n--outer--\r\n"

func TestMixedAlternativeAttachmentsAndInline(t *testing.T) {
	raw := []byte(mixedMessage)
	got, err := Parse(testRef, raw, mail.Metadata{Size: int64(len(raw))}, nil, mail.ReadRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Subject != "café" || got.Text != "Hello 世界" || got.HTML != "" || got.Truncated || len(got.Attachments) != 2 {
		t.Fatalf("unexpected message: %#v", got)
	}
	first, second := got.Attachments[0], got.Attachments[1]
	if first.PartID != "2" || first.Filename != "café.bin" || first.Size != 4 || first.Disposition != "attachment" ||
		second.PartID != "3" || second.ContentType != "image/png" || second.Disposition != "inline" || second.Size != 4 {
		t.Fatalf("unexpected attachments: %#v", got.Attachments)
	}
	html, err := Parse(testRef, raw, mail.Metadata{}, nil, mail.ReadRequest{IncludeHTML: true})
	if err != nil || html.HTML != "<p>Hello 世界</p>" || html.Text != got.Text {
		t.Fatalf("HTML/plain precedence: %#v, %v", html, err)
	}
	for _, part := range []struct{ id, payload string }{{"2", "AAECAw=="}, {"3", "iVBORw=="}} {
		attachment, err := Attachment(testRef, raw, part.id)
		if err != nil || attachment.DataBase64 != part.payload || attachment.Info.PartID != part.id {
			t.Fatalf("attachment %s: %#v, %v", part.id, attachment, err)
		}
	}
}

func TestNestedMessageAndMultipartPaths(t *testing.T) {
	nested := "Subject: forwarded\r\nContent-Type: multipart/mixed; boundary=nested\r\n\r\n" +
		"--nested\r\nContent-Type: text/plain\r\n\r\nForwarded body\r\n" +
		"--nested\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Disposition: attachment; filename=note.txt\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\ncaf=E9\r\n--nested--\r\n"
	raw := []byte("Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: text/plain\r\n\r\nOuter body\r\n" +
		"--outer\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment; filename=forward.eml\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(nested)) + "\r\n--outer--\r\n")
	got, err := Parse(testRef, raw, mail.Metadata{}, nil, mail.ReadRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Outer body" || len(got.Attachments) != 2 || got.Attachments[0].PartID != "2" || got.Attachments[1].PartID != "2.2" {
		t.Fatalf("nested message: %#v", got)
	}
	file, err := Attachment(testRef, raw, "2.2")
	if err != nil || file.DataBase64 != base64.StdEncoding.EncodeToString([]byte{'c', 'a', 'f', 0xe9}) || file.Info.Size != 4 {
		t.Fatalf("charset changed attachment bytes: %#v, %v", file, err)
	}
	eml, err := Attachment(testRef, raw, "2")
	if err != nil || eml.DataBase64 != base64.StdEncoding.EncodeToString([]byte(nested)) {
		t.Fatalf("nested RFC822 download: %#v, %v", eml, err)
	}
}

func TestSinglePartCharsetAndRuneTruncation(t *testing.T) {
	for _, test := range []struct{ header, body, text string }{
		{"Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable", "caf=E9", "café"},
		{"Content-Type: text/plain; charset=X-UNKNOWN", "caf\xe9", "caf�"},
		{"Content-Type: text/plain; charset=utf-8", "A世界😊Z", "A世界😊Z"},
	} {
		raw := []byte(test.header + "\r\n\r\n" + test.body)
		got, err := Parse(testRef, raw, mail.Metadata{}, nil, mail.ReadRequest{})
		if err != nil || got.Text != test.text || len(got.Attachments) != 0 {
			t.Fatalf("charset: %#v, %v", got, err)
		}
		got, err = Parse(testRef, raw, mail.Metadata{}, nil, mail.ReadRequest{MaxTextChars: 3})
		if err != nil || utf8.RuneCountInString(got.Text) != 3 || !utf8.ValidString(got.Text) || !got.Truncated {
			t.Fatalf("truncation: %#v, %v", got, err)
		}
	}
	raw := []byte("Content-Type: text/plain\r\nContent-Disposition: attachment; filename=single.txt\r\n\r\nsingle")
	got, err := Attachment(testRef, raw, "1")
	if err != nil || got.Info.PartID != "1" || got.Info.Size != 6 {
		t.Fatalf("root part: %#v, %v", got, err)
	}
}

func TestHTMLOnlyAndHTMLTruncation(t *testing.T) {
	raw := []byte("Content-Type: text/html; charset=utf-8\r\n\r\n<p>😊世界</p>")
	got, err := Parse(testRef, raw, mail.Metadata{}, nil, mail.ReadRequest{})
	if err != nil || got.Text != "" || got.HTML != "" || got.Truncated {
		t.Fatalf("HTML omitted: %#v, %v", got, err)
	}
	got, err = Parse(testRef, raw, mail.Metadata{}, nil, mail.ReadRequest{IncludeHTML: true, MaxTextChars: 4})
	if err != nil || got.HTML != "<p>😊" || !got.Truncated || !utf8.ValidString(got.HTML) {
		t.Fatalf("HTML truncation: %#v, %v", got, err)
	}
}

func TestLimitsAndMalformedMessages(t *testing.T) {
	_, err := Parse(testRef, []byte(strings.Repeat("x", mail.MaxMessageBytes+1)), mail.Metadata{}, nil, mail.ReadRequest{})
	requireCode(t, err, "message_too_large")
	for _, raw := range []string{
		"not a header\r\n\r\nbody",
		"Content-Type: multipart/mixed\r\n\r\nbody",
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nBroken header\r\n\r\nbody\r\n--x--",
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n%%%",
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: unknown\r\n\r\nbody",
	} {
		_, err := Parse(testRef, []byte(raw), mail.Metadata{}, nil, mail.ReadRequest{})
		requireCode(t, err, "invalid_message")
	}
	_, err = Parse(testRef, []byte("Subject: x\r\n\r\nbody"), mail.Metadata{}, nil, mail.ReadRequest{MaxTextChars: -1})
	requireCode(t, err, "invalid_argument")
	_, err = Parse(testRef, []byte("Subject: x\r\n\r\nbody"), mail.Metadata{}, nil, mail.ReadRequest{MaxTextChars: mail.MaxTextChars + 1})
	requireCode(t, err, "invalid_argument")
	_, err = Attachment(testRef, []byte(mixedMessage), "missing")
	requireCode(t, err, "not_found")
	_, err = Attachment(testRef, []byte(mixedMessage), "")
	requireCode(t, err, "invalid_argument")
	_, err = Parse(testRef, []byte("Subject: "+strings.Repeat("x", mail.MaxHeaderBytes)+"\r\n\r\nbody"), mail.Metadata{}, nil, mail.ReadRequest{})
	requireCode(t, err, "message_too_large")
	_, err = Parse(testRef, []byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n"+
		"--x\r\nSubject: "+strings.Repeat("x", mail.MaxHeaderBytes)+"\r\n\r\nbody\r\n--x--\r\n"),
		mail.Metadata{}, nil, mail.ReadRequest{})
	requireCode(t, err, "message_too_large")

	for _, size := range []int{mail.MaxAttachmentBytes, mail.MaxAttachmentBytes + 1} {
		raw := []byte("Content-Type: application/octet-stream\r\n\r\n" + strings.Repeat("a", size))
		message, err := Parse(testRef, raw, mail.Metadata{}, nil, mail.ReadRequest{})
		if err != nil || len(message.Attachments) != 1 || message.Attachments[0].Size != int64(size) {
			t.Fatalf("large attachment metadata: %#v, %v", message, err)
		}
		attachment, err := Attachment(testRef, raw, "1")
		if size == mail.MaxAttachmentBytes {
			if err != nil || len(attachment.DataBase64) != base64.StdEncoding.EncodedLen(size) {
				t.Fatalf("attachment boundary: %#v, %v", attachment.Info, err)
			}
		} else {
			requireCode(t, err, "attachment_too_large")
		}
	}
	raw := []byte("Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", mail.MaxAttachmentBytes+1))))
	_, err = Attachment(testRef, raw, "1")
	requireCode(t, err, "attachment_too_large")
}

func TestStructuralLimits(t *testing.T) {
	raw := "Content-Type: text/plain\r\n\r\nbody"
	for i := 0; i < maxDepth+2; i++ {
		raw = "Content-Type: message/rfc822\r\n\r\n" + raw
	}
	_, err := Parse(testRef, []byte(raw), mail.Metadata{}, nil, mail.ReadRequest{})
	requireCode(t, err, "invalid_message")
	raw = "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		strings.Repeat("--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n", maxParts) + "--x--\r\n"
	_, err = Parse(testRef, []byte(raw), mail.Metadata{}, nil, mail.ReadRequest{})
	requireCode(t, err, "invalid_message")
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var mailError *mail.Error
	if !errors.As(err, &mailError) || mailError.Code != code {
		t.Fatalf("wanted error %s, got %v", code, err)
	}
}
