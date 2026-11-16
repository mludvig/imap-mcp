//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const dovecotImage = "dovecot/dovecot:2.4.5@sha256:43c14def08a57f25107593cbcf64a0a6fbae84a7b9de7aa038513c9b21e91914"

// Fixture helpers deliberately use a separate IMAP observer. See testbed/README.md.
type dovecotFixture struct {
	Container       string
	TLSAddress      string
	STARTTLSAddress string
	CAFile          string
	Username        string
	Password        string
	Rev2            bool
	Seeds           map[string]seedMessage
	UIDs            map[string]imap.UID
}

type seedMessage struct {
	ID           string
	Mailbox      string
	Raw          string
	Flags        []imap.Flag
	InternalDate time.Time
}

type observedMessage struct {
	UID          imap.UID
	Raw          string
	Flags        []imap.Flag
	InternalDate time.Time
}

func startDovecot(t *testing.T, rev2 bool) *dovecotFixture {
	t.Helper()
	return startDovecotFixture(t, rev2, false)
}

func startPlaintextDovecot(t *testing.T) *dovecotFixture {
	t.Helper()
	return startDovecotFixture(t, false, true)
}

func startDovecotFixture(t *testing.T, rev2, allowCleartext bool) *dovecotFixture {
	t.Helper()
	docker(t, "info", "--format", "{{.ServerVersion}}")
	dir := t.TempDir()
	// The image runs as UID 1000; these generated fixture credentials and keys
	// must be readable by that user through a read-only bind mount.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	f := &dovecotFixture{
		Container: "imap-mcp-test-" + filepath.Base(dir) + "-" + randomSuffix(t),
		CAFile:    filepath.Join(dir, "ca.crt"),
		Username:  "fixture",
		Password:  "synthetic-test-password",
		Rev2:      rev2,
		Seeds:     make(map[string]seedMessage),
		UIDs:      make(map[string]imap.UID),
	}
	writeCertificates(t, dir)
	conf, err := os.ReadFile(filepath.Join("..", "..", "testbed", "dovecot.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !rev2 {
		conf = []byte(strings.ReplaceAll(string(conf), "imap4rev2_enable = yes", "imap4rev2_enable = no"))
	}
	if allowCleartext {
		// Only this generated, loopback-bound fixture permits plaintext login.
		for _, setting := range [][2]string{
			{"ssl = required", "ssl = yes"},
			{"auth_allow_cleartext = no", "auth_allow_cleartext = yes"},
		} {
			if strings.Count(string(conf), setting[0]) != 1 {
				t.Fatalf("plaintext fixture requires exactly one %q", setting[0])
			}
			conf = []byte(strings.Replace(string(conf), setting[0], setting[1], 1))
		}
	}
	writeFixtureFile(t, filepath.Join(dir, "dovecot.conf"), conf)
	writeFixtureFile(t, filepath.Join(dir, "users"), []byte(f.Username+":{PLAIN}"+f.Password+":1000:1000::/srv/vmail/fixture::\n"))
	t.Cleanup(func() {
		// Register before starting Docker so partial startup also gets cleaned.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", f.Container).CombinedOutput()
		if err != nil && !strings.Contains(string(output), "No such container") {
			t.Errorf("remove fixture container %s: %v: %s", f.Container, err, output)
		}
	})
	docker(t, "run", "--detach", "--name", f.Container,
		"--publish", "127.0.0.1::31143", "--publish", "127.0.0.1::31993",
		"--mount", "type=bind,src="+dir+",dst=/fixture,readonly",
		"--mount", "type=bind,src="+filepath.Join(dir, "dovecot.conf")+",dst=/etc/dovecot/dovecot.conf,readonly",
		dovecotImage)
	f.STARTTLSAddress = dockerAddress(t, f.Container, "31143/tcp")
	f.TLSAddress = dockerAddress(t, f.Container, "31993/tcp")
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := dialFixture(f.TLSAddress, "tls", f.TLSConfig(t))
		if err == nil {
			err = c.WaitGreeting()
			c.Close()
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dovecot readiness: %v\n%s", err, docker(t, "logs", f.Container))
		}
		time.Sleep(100 * time.Millisecond)
	}
	configuration := docker(t, "exec", f.Container, "doveconf", "-n")
	t.Logf("Dovecot effective configuration:\n%s", configuration)
	f.seed(t)
	return f
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", b)
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func dockerAddress(t *testing.T, container, port string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "port", container, port).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture port %s: %v: %s\n%s", port, err, output, docker(t, "logs", container))
	}
	address := strings.TrimSpace(string(output))
	host, number, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("fixture port must bind only loopback: %q (%v)", address, err)
	}
	if _, err := strconv.Atoi(number); err != nil {
		t.Fatal(err)
	}
	return address
}

func writeFixtureFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func writeCertificates(t *testing.T, dir string) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "IMAP fixture CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	writeFixtureFile(t, filepath.Join(dir, "server.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}))
	writeFixtureFile(t, filepath.Join(dir, "server.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func (f *dovecotFixture) TLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	caPEM, err := os.ReadFile(f.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("invalid fixture CA")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "localhost"}
}

func dialFixture(address, mode string, tlsConfig *tls.Config) (*imapclient.Client, error) {
	options := &imapclient.Options{TLSConfig: tlsConfig, Dialer: &net.Dialer{Timeout: 3 * time.Second}}
	if mode == "starttls" {
		return imapclient.DialStartTLS(address, options)
	}
	return imapclient.DialTLS(address, options)
}

func (f *dovecotFixture) Dial(t *testing.T, mode string) *imapclient.Client {
	t.Helper()
	address := f.TLSAddress
	if mode == "starttls" {
		address = f.STARTTLSAddress
	}
	c, err := dialFixture(address, mode, f.TLSConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login(f.Username, f.Password).Wait(); err != nil {
		t.Fatal(err)
	}
	if f.Rev2 {
		data, err := c.Enable(imap.CapIMAP4rev2).Wait()
		if err != nil || !data.Caps.Has(imap.CapIMAP4rev2) {
			t.Fatalf("enable IMAP4rev2: %v (%v)", data, err)
		}
	}
	return c
}

// ConfigFile supplies the application's real immutable config loader with a
// synthetic account. The file disappears with the test.
func (f *dovecotFixture) ConfigFile(t *testing.T, mode string, readonly bool) string {
	t.Helper()
	address := f.TLSAddress
	if mode == "starttls" {
		address = f.STARTTLSAddress
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("readonly = %t\ntrash_mailbox = \"Trash\"\n[imap]\nhost = %q\nport = %s\nusername = %q\npassword = %q\ntls = %q\nca_file = %q\nconnect_timeout_seconds = 3\noperation_timeout_seconds = 10\n",
		readonly, host, port, f.Username, f.Password, mode, f.CAFile)
	writeFixtureFile(t, name, []byte(body))
	return name
}

func (f *dovecotFixture) seed(t *testing.T) {
	t.Helper()
	c := f.Dial(t, "tls")
	defer c.Close()
	for _, mailbox := range []string{"Trash", "Archive", "旅行"} {
		if err := c.Create(mailbox, nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	date := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	add := func(id, mailbox, subject string, flags []imap.Flag, internalDate time.Time) {
		raw := "From: =?UTF-8?Q?Alice_=C3=89?= <alice@example.test>\r\n" +
			"To: Bob <bob@example.test>\r\nCc: Carol <carol@example.test>\r\nBcc: hidden@example.test\r\n" +
			"Subject: " + subject + "\r\nDate: Wed, 15 Jan 2025 12:00:00 +0000\r\n" +
			"Message-ID: <" + id + "@fixture.test>\r\nX-Fixture-ID: " + id + "\r\n" +
			"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
			"Fixture body for " + id + ".\r\n"
		f.Seeds[id] = seedMessage{ID: id, Mailbox: mailbox, Raw: raw, Flags: flags, InternalDate: internalDate}
	}
	add("unseen", "INBOX", "Unread fixture", nil, date)
	add("seen", "INBOX", "Read fixture", []imap.Flag{imap.FlagSeen}, date)
	add("flagged", "INBOX", "Flagged fixture", []imap.Flag{imap.FlagFlagged, "fixture-keyword"}, date)
	// Only the seed account contains this flag; tool writes must preserve it.
	add("deleted", "INBOX", "Unrelated already Deleted fixture", []imap.Flag{imap.FlagDeleted}, date)
	add("before", "INBOX", "Date boundary before", nil, time.Date(2025, 1, 14, 23, 59, 59, 0, time.UTC))
	add("boundary", "INBOX", "Date boundary at midnight", nil, time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC))
	add("after", "INBOX", "Date boundary next day", nil, time.Date(2025, 1, 16, 0, 0, 0, 0, time.UTC))
	add("unicode", "旅行", "=?UTF-8?Q?R=C3=A9sum=C3=A9_=E6=97=85=E8=A1=8C?=", nil, date)
	add("reply", "INBOX", "Re: Unread fixture", nil, date.Add(time.Minute))
	reply := f.Seeds["reply"]
	reply.Raw = strings.Replace(reply.Raw, "MIME-Version:", "In-Reply-To: <unseen@fixture.test>\r\nReferences: <unseen@fixture.test>\r\nMIME-Version:", 1)
	f.Seeds["reply"] = reply
	f.Seeds["mime"] = seedMessage{ID: "mime", Mailbox: "INBOX", Raw: multipartFixture(), InternalDate: date}
	for i := 0; i < 55; i++ {
		id := fmt.Sprintf("page-%02d", i)
		add(id, "INBOX", fmt.Sprintf("Pagination %02d", i), nil, date.Add(time.Duration(i)*time.Minute))
	}
	ids := make([]string, 0, len(f.Seeds))
	for id := range f.Seeds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		message := f.Seeds[id]
		cmd := c.Append(message.Mailbox, int64(len(message.Raw)), &imap.AppendOptions{Flags: message.Flags, Time: message.InternalDate})
		if _, err := cmd.Write([]byte(message.Raw)); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	// Resolve identities from headers, never assume the server's UID allocation.
	for _, mailbox := range []string{"INBOX", "旅行"} {
		// SELECT consumes the session-specific rev1 Recent state once during
		// seeding. It neither marks Seen nor expunges; observers use EXAMINE.
		if _, err := c.Select(mailbox, nil).Wait(); err != nil {
			t.Fatal(err)
		}
		messages, err := c.Fetch(imap.UIDSet{{Start: 1}}, &imap.FetchOptions{UID: true, Envelope: true}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			id := strings.TrimSuffix(message.Envelope.MessageID, "@fixture.test")
			if _, ok := f.Seeds[id]; !ok {
				t.Fatalf("unexpected fixture identity %q", id)
			}
			f.UIDs[id] = message.UID
		}
	}
	if len(f.UIDs) != len(f.Seeds) {
		t.Fatalf("seed identities: got %d, want %d", len(f.UIDs), len(f.Seeds))
	}
}

func multipartFixture() string {
	return strings.ReplaceAll(`From: =?UTF-8?Q?Alice_=C3=89?= <alice@example.test>
To: bob@example.test
Subject: =?UTF-8?Q?Multipart_r=C3=A9sum=C3=A9?=
Date: Wed, 15 Jan 2025 12:00:00 +0000
Message-ID: <mime@fixture.test>
X-Fixture-ID: mime
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="alternative"

--alternative
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Plain caf=C3=A9 body.
--alternative
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: base64

PHA+SFRNTCBjYWbDqSBib2R5LjwvcD4=
--alternative--
--outer
Content-Type: application/octet-stream
Content-Disposition: attachment; filename*=UTF-8''r%C3%A9sum%C3%A9.txt
Content-Transfer-Encoding: base64

YXR0YWNobWVudCBjYWbDqQo=
--outer
Content-Type: multipart/mixed; boundary="nested"

--nested
Content-Type: text/plain; charset=utf-8
Content-Disposition: attachment; filename="=?UTF-8?Q?notes_=C3=A9.txt?="
Content-Transfer-Encoding: quoted-printable

Nested attachment caf=C3=A9.
--nested--
--outer--
`, "\n", "\r\n")
}

// Snapshot captures exact membership, bytes, flags, and dates over a fresh
// authenticated connection, using EXAMINE and BODY.PEEK[] to preserve Seen.
func (f *dovecotFixture) Snapshot(t *testing.T, mailbox string) map[imap.UID]observedMessage {
	t.Helper()
	c := f.Dial(t, "tls")
	defer c.Close()
	if _, err := c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	body := &imap.FetchItemBodySection{Peek: true}
	messages, err := c.Fetch(imap.UIDSet{{Start: 1}}, &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, BodySection: []*imap.FetchItemBodySection{body},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[imap.UID]observedMessage, len(messages))
	for _, message := range messages {
		flags := append([]imap.Flag(nil), message.Flags...)
		sort.Slice(flags, func(i, j int) bool { return flags[i] < flags[j] })
		result[message.UID] = observedMessage{
			UID: message.UID, Raw: string(message.FindBodySection(body)),
			Flags: flags, InternalDate: message.InternalDate,
		}
	}
	return result
}
