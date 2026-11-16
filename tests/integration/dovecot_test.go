//go:build integration

package integration

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestDovecotFixture(t *testing.T) {
	for _, rev2 := range []bool{false, true} {
		name := "rev1"
		if rev2 {
			name = "rev2"
		}
		t.Run(name, func(t *testing.T) {
			f := startDovecot(t, rev2)
			t.Logf("fixture %s: TLS=%s STARTTLS=%s; %d seeded identities", f.Container, f.TLSAddress, f.STARTTLSAddress, len(f.Seeds))
			for _, mode := range []string{"tls", "starttls"} {
				t.Run(mode, func(t *testing.T) {
					c := f.Dial(t, mode)
					caps := c.Caps()
					capabilities := make([]string, 0, len(caps))
					for capability := range caps {
						capabilities = append(capabilities, string(capability))
					}
					sort.Strings(capabilities)
					t.Logf("authenticated advertised capabilities: %s", strings.Join(capabilities, " "))
					for _, required := range []imap.Cap{imap.CapIMAP4rev1, imap.CapUIDPlus, imap.CapMove, imap.CapCondStore, imap.CapEnable} {
						if !caps.Has(required) {
							t.Errorf("missing capability %s", required)
						}
					}
					if caps.Has(imap.CapIMAP4rev2) != rev2 {
						t.Errorf("IMAP4rev2 capability = %t, want %t", caps.Has(imap.CapIMAP4rev2), rev2)
					}
					mailboxes, err := c.List("", "*", nil).Collect()
					if err != nil {
						t.Fatal(err)
					}
					names := make(map[string]bool)
					for _, mailbox := range mailboxes {
						names[mailbox.Mailbox] = true
					}
					for _, required := range []string{"INBOX", "Trash", "Archive", "旅行"} {
						if !names[required] {
							t.Errorf("missing mailbox %q: %v", required, names)
						}
					}
					if _, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
						t.Fatal(err)
					}
					boundary := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
					result, err := c.UIDSearch(&imap.SearchCriteria{Since: boundary, Before: boundary.AddDate(0, 0, 1)}, nil).Wait()
					if err != nil {
						t.Fatal(err)
					}
					matches := make(map[imap.UID]bool)
					for _, uid := range result.AllUIDs() {
						matches[uid] = true
					}
					if !matches[f.UIDs["boundary"]] || matches[f.UIDs["before"]] || matches[f.UIDs["after"]] {
						t.Fatalf("unexpected date boundaries: %v", matches)
					}
				})
			}
			t.Run("observer", func(t *testing.T) {
				before := f.Snapshot(t, "INBOX")
				after := f.Snapshot(t, "INBOX")
				if !reflect.DeepEqual(before, after) {
					t.Fatal("observer changed mailbox membership, bytes, flags, or dates")
				}
				if len(before) <= 50 {
					t.Fatalf("pagination requires more than default limit: %d", len(before))
				}
				for id, seed := range f.Seeds {
					if seed.Mailbox != "INBOX" {
						continue
					}
					got, ok := before[f.UIDs[id]]
					if !ok || got.Raw != seed.Raw || !got.InternalDate.Equal(seed.InternalDate) {
						t.Errorf("seed %s differs from independent observer", id)
					}
					flags := append([]imap.Flag(nil), seed.Flags...)
					sort.Slice(flags, func(i, j int) bool { return flags[i] < flags[j] })
					if !reflect.DeepEqual(got.Flags, flags) {
						t.Errorf("seed %s flags = %v, want %v", id, got.Flags, flags)
					}
				}
				deleted := before[f.UIDs["deleted"]]
				if !containsFlag(deleted.Flags, imap.FlagDeleted) {
					t.Fatal("already Deleted sentinel was lost")
				}
				if containsFlag(before[f.UIDs["unseen"]].Flags, imap.FlagSeen) {
					t.Fatal("unseen seed became Seen")
				}
				unicode := f.Snapshot(t, "旅行")
				if len(unicode) != 1 || unicode[f.UIDs["unicode"]].Raw != f.Seeds["unicode"].Raw {
					t.Fatal("Unicode mailbox seed does not match")
				}
				verifyMultipart(t, before[f.UIDs["mime"]].Raw)
			})
			t.Run("tls-rejections", func(t *testing.T) {
				for _, mode := range []string{"tls", "starttls"} {
					address := f.TLSAddress
					if mode == "starttls" {
						address = f.STARTTLSAddress
					}
					t.Run(mode+"/untrusted-ca", func(t *testing.T) {
						_, err := dialFixture(address, mode, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "localhost", RootCAs: x509.NewCertPool()})
						var unknownAuthority x509.UnknownAuthorityError
						if !errors.As(err, &unknownAuthority) {
							t.Fatalf("expected untrusted CA rejection, got %v", err)
						}
					})
					t.Run(mode+"/hostname", func(t *testing.T) {
						config := f.TLSConfig(t)
						config.ServerName = "wrong.fixture.test"
						_, err := dialFixture(address, mode, config)
						var hostname x509.HostnameError
						if !errors.As(err, &hostname) {
							t.Fatalf("expected hostname rejection, got %v", err)
						}
					})
				}
			})
		})
	}
}

func containsFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}

func verifyMultipart(t *testing.T, raw string) {
	t.Helper()
	message, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var attachments = make(map[string]string)
	var plain, html bool
	var walk func(string, io.Reader, string, string)
	walk = func(contentType string, body io.Reader, disposition, transfer string) {
		mediaType, parameters, err := mime.ParseMediaType(contentType)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(mediaType, "multipart/") {
			reader := multipart.NewReader(body, parameters["boundary"])
			for {
				part, err := reader.NextRawPart()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				walk(part.Header.Get("Content-Type"), part, part.Header.Get("Content-Disposition"), part.Header.Get("Content-Transfer-Encoding"))
				part.Close()
			}
		}
		switch transfer {
		case "base64":
			body = base64.NewDecoder(base64.StdEncoding, body)
		case "quoted-printable":
			body = quotedprintable.NewReader(body)
		}
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		if disposition != "" {
			kind, parameters, err := mime.ParseMediaType(disposition)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "attachment" {
				filename, err := new(mime.WordDecoder).DecodeHeader(parameters["filename"])
				if err != nil {
					t.Fatal(err)
				}
				attachments[filename] = string(data)
				return
			}
		}
		if mediaType == "text/plain" && bytes.Contains(data, []byte("Plain café body.")) {
			plain = true
		}
		if mediaType == "text/html" && bytes.Contains(data, []byte("<p>HTML café body.</p>")) {
			html = true
		}
	}
	walk(message.Header.Get("Content-Type"), message.Body, "", "")
	if !plain || !html || strings.TrimSpace(attachments["résumé.txt"]) != "attachment café" || strings.TrimSpace(attachments["notes é.txt"]) != "Nested attachment café." {
		t.Fatalf("MIME fixture decode: plain=%t html=%t attachments=%v", plain, html, attachments)
	}
}
