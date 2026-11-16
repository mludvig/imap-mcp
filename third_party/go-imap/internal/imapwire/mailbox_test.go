package imapwire_test

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func TestMailboxEncoding(t *testing.T) {
	tests := []struct {
		name, legacy, modern string
	}{
		{"R&D", `"R&-D"`, `"R&D"`},
		{"R&-D", `"R&--D"`, `"R&-D"`},
		{"&AOk-", `"&-AOk-"`, `"&AOk-"`},
		{"é & \"\\", `"&AOk- &- \"\\"`, `"é & \"\\"`},
		{"inbox", "INBOX", "INBOX"},
		{"NIL", `"NIL"`, `"NIL"`},
		{"", `""`, `""`},
	}
	for _, tt := range tests {
		for _, modern := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/UTF8=%v", tt.name, modern), func(t *testing.T) {
				var buf bytes.Buffer
				enc := imapwire.NewEncoder(bufio.NewWriter(&buf), imapwire.ConnSideServer)
				enc.QuotedUTF8 = modern
				if err := enc.Mailbox(tt.name).CRLF(); err != nil {
					t.Fatal(err)
				}
				want := tt.legacy
				if modern {
					want = tt.modern
				}
				if got := buf.String(); got != want+"\r\n" {
					t.Fatalf("wire name = %q, want %q", got, want+"\r\n")
				}
			})
		}
	}
}

func TestMailboxEncodingLiteral(t *testing.T) {
	name := strings.Repeat("é&", 1400)
	var buf bytes.Buffer
	enc := imapwire.NewEncoder(bufio.NewWriter(&buf), imapwire.ConnSideServer)
	enc.QuotedUTF8 = true
	if err := enc.Mailbox(name).CRLF(); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("{%d}\r\n%s\r\n", len(name), name); buf.String() != want {
		t.Fatal("UTF-8 literal name or byte count changed")
	}
}

func TestMailboxDecoding(t *testing.T) {
	tests := []struct {
		name, legacy, modern string
	}{
		{"R&D", "R&-D", "R&D"},
		{"R&-D", "R&--D", "R&-D"},
		{"&AOk-", "&-AOk-", "&AOk-"},
		{"é & \"\\", "&AOk- &- \"\\", "é & \"\\"},
		{"INBOX", "inbox", "inbox"},
		{"NIL", "NIL", "NIL"},
		{"", "", ""},
	}
	for _, tt := range tests {
		for _, modern := range []bool{false, true} {
			wireName := tt.legacy
			if modern {
				wireName = tt.modern
			}
			forms := map[string]string{
				"quoted":  `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(wireName) + `"`,
				"literal": fmt.Sprintf("{%d}\r\n%s", len(wireName), wireName),
			}
			if wireName != "" && !strings.ContainsAny(wireName, " \"\\") {
				forms["atom"] = wireName
			}
			for form, wire := range forms {
				t.Run(fmt.Sprintf("%s/UTF8=%v/%s", tt.name, modern, form), func(t *testing.T) {
					dec := newTestDecoder(wire + "\r\n")
					dec.MailboxUTF8 = modern
					var got string
					if !dec.ExpectMailbox(&got) || !dec.ExpectCRLF() {
						t.Fatal(dec.Err())
					}
					if got != tt.name {
						t.Fatalf("name = %q, want %q", got, tt.name)
					}
				})
			}
		}
	}
}

func TestMailboxInvalidUTF8(t *testing.T) {
	for _, name := range []string{"R&\xff", "\xc0\xaf", "\xed\xa0\x80"} {
		t.Run(fmt.Sprintf("%x", name), func(t *testing.T) {
			var buf bytes.Buffer
			enc := imapwire.NewEncoder(bufio.NewWriter(&buf), imapwire.ConnSideServer)
			enc.QuotedUTF8 = true
			if err := enc.Mailbox(name).CRLF(); err == nil {
				t.Fatal("encoder accepted invalid UTF-8")
			}
			for _, wire := range []string{`"` + name + `"`, fmt.Sprintf("{%d}\r\n%s", len(name), name)} {
				dec := newTestDecoder(wire + "\r\n")
				dec.MailboxUTF8 = true
				var got string
				if dec.ExpectMailbox(&got) {
					t.Fatal("decoder accepted invalid UTF-8")
				}
				expectDecoderExpectError(t, dec)
			}
		})
	}
}

func TestStringQuotedControlFallback(t *testing.T) {
	for _, name := range []string{"a\rb", "a\nb", "a\x00b"} {
		var buf bytes.Buffer
		enc := imapwire.NewEncoder(bufio.NewWriter(&buf), imapwire.ConnSideServer)
		enc.QuotedUTF8 = true
		if err := enc.String(name).CRLF(); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("{%d}\r\n%s\r\n", len(name), name); buf.String() != want {
			t.Fatalf("control string = %q, want literal %q", buf.String(), want)
		}
	}
}
