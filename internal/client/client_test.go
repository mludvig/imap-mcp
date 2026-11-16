package client

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/mail"
)

type testPeer struct {
	conn               net.Conn
	reader             *bufio.Reader
	writer             *bufio.Writer
	tag, command, args string
}

func (p *testPeer) send(format string, args ...any) {
	_, _ = fmt.Fprintf(p.writer, format, args...)
	_ = p.writer.Flush()
}
func (p *testPeer) ok() { p.send("%s OK complete\r\n", p.tag) }
func (p *testPeer) selectBox(epoch uint32, count uint32, modseq uint64) {
	p.send("* FLAGS (\\Seen \\Flagged)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY %d] epoch\r\n* OK [UIDNEXT 100] next\r\n", count, epoch)
	if modseq != 0 {
		p.send("* OK [HIGHESTMODSEQ %d] cursor\r\n", modseq)
	}
	p.send("%s OK [%s] selected\r\n", p.tag, map[bool]string{true: "READ-ONLY", false: "READ-WRITE"}[p.command == "EXAMINE"])
}
func (p *testPeer) status(epoch, count uint32, modseq uint64) {
	extra := ""
	if strings.Contains(p.args, "HIGHESTMODSEQ") {
		extra = fmt.Sprintf(" HIGHESTMODSEQ %d", modseq)
	}
	p.send("* STATUS INBOX (MESSAGES %d UNSEEN 0 UIDNEXT 100 UIDVALIDITY %d%s)\r\n", count, epoch, extra)
	p.ok()
}
func (p *testPeer) fetchLiteral(seq, uid uint32, section, body string) {
	// Intentionally place UID after the literal, a legal server response.
	p.send("* %d FETCH (BODY[%s] {%d}\r\n%s UID %d INTERNALDATE \"16-Nov-2026 12:00:00 +0000\" RFC822.SIZE %d)\r\n", seq, section, len(body), body, uid, len(body))
	p.ok()
}

type peerServer struct {
	listener    net.Listener
	tlsConfig   *tls.Config
	caPath      string
	mu          sync.Mutex
	commands    []string
	connections int
	conns       []net.Conn
	wg          sync.WaitGroup
}

func (s *peerServer) record(command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
}
func (s *peerServer) snapshot() ([]string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.commands...), s.connections
}
func makePeer(t *testing.T, implicit bool, caps string, handler func(*testPeer) bool) *peerServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "IMAP test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &peerServer{listener: listener, tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, caPath: filepath.Join(t.TempDir(), "ca.pem")}
	if err := os.WriteFile(s.caPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.connections++
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				active := conn
				if implicit {
					secure := tls.Server(conn, s.tlsConfig)
					if secure.Handshake() != nil {
						return
					}
					active = secure
				}
				p := &testPeer{conn: active, reader: bufio.NewReader(active), writer: bufio.NewWriter(active)}
				p.send("* OK [CAPABILITY %s] test peer\r\n", caps)
				for {
					line, err := p.reader.ReadString('\n')
					if err != nil {
						return
					}
					parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
					if len(parts) < 2 {
						return
					}
					p.tag = parts[0]
					p.command = parts[1]
					p.args = ""
					if len(parts) == 3 {
						p.args = parts[2]
					}
					s.record(p.command + " " + p.args)
					switch p.command {
					case "CAPABILITY":
						p.send("* CAPABILITY %s\r\n", caps)
						p.ok()
					case "LOGIN":
						s.mu.Lock()
						s.commands[len(s.commands)-1] = "LOGIN [redacted]"
						s.mu.Unlock()
						p.ok()
					case "AUTHENTICATE":
						p.send("+ \r\n")
						if _, err := p.reader.ReadString('\n'); err != nil {
							return
						}
						p.ok()
					default:
						if handler == nil || !handler(p) {
							p.send("%s BAD unexpected command\r\n", p.tag)
						}
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		s.mu.Lock()
		for _, conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}
func (s *peerServer) config(t *testing.T, readonly, trust bool, mode string) *config.Config {
	t.Helper()
	_, portText, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	ca := ""
	if trust {
		ca = fmt.Sprintf("ca_file = %q\n", s.caPath)
	}
	content := fmt.Sprintf("readonly = %t\n[imap]\nhost = \"127.0.0.1\"\nport = %d\nusername = \"testuser\"\npassword = \"fixture-secret\"\ntls = %q\n%soperation_timeout_seconds = 3\n", readonly, port, mode, ca)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
func requireCode(t *testing.T, err error, code string) *mail.Error {
	t.Helper()
	typed, ok := err.(*mail.Error)
	if !ok || typed.Code != code {
		t.Fatalf("error = %#v, want code %s", err, code)
	}
	if strings.Contains(typed.Message, "fixture-secret") {
		t.Fatal("credential exposed in error")
	}
	return typed
}
func hasCommand(commands []string, fragment string) bool {
	for _, command := range commands {
		if strings.Contains(command, fragment) {
			return true
		}
	}
	return false
}

func TestVerifiedTLSRefusesUntrustedCertificate(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", nil)
	_, err := New(peer.config(t, true, false, "tls")).Info(context.Background())
	requireCode(t, err, "tls_failed")
	commands, _ := peer.snapshot()
	if hasCommand(commands, "LOGIN") || hasCommand(commands, "AUTHENTICATE") {
		t.Fatal("authenticated without verified TLS")
	}
}
func TestRequiredSTARTTLSBeforeAuthentication(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(strconv.FormatBool(upgrade), func(t *testing.T) {
			var peer *peerServer
			peer = makePeer(t, false, "IMAP4rev1 STARTTLS", func(p *testPeer) bool {
				if p.command != "STARTTLS" {
					return false
				}
				if !upgrade {
					p.send("%s NO unavailable fixture-secret\r\n", p.tag)
					return true
				}
				p.ok()
				secure := tls.Server(p.conn, peer.tlsConfig)
				if secure.Handshake() != nil {
					return true
				}
				p.conn = secure
				p.reader = bufio.NewReader(secure)
				p.writer = bufio.NewWriter(secure)
				return true
			})
			_, err := New(peer.config(t, true, true, "starttls")).Info(context.Background())
			if upgrade && err != nil {
				t.Fatal(err)
			}
			if !upgrade {
				requireCode(t, err, "tls_failed")
			}
			commands, _ := peer.snapshot()
			if len(commands) == 0 || !strings.HasPrefix(commands[0], "STARTTLS") {
				t.Fatalf("first command = %v", commands)
			}
			if !upgrade && hasCommand(commands, "LOGIN") {
				t.Fatal("authenticated after STARTTLS refusal")
			}
		})
	}
}
func TestReadonlyAndNativeMoveGate(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", nil)
	refs := []mail.MessageRef{{Mailbox: "INBOX", UIDValidity: 7, UID: 5}}
	read := New(peer.config(t, true, true, "tls"))
	calls := []func() error{
		func() error {
			_, err := read.AddFlags(context.Background(), mail.FlagRequest{Refs: refs, Flags: []string{"\\Seen"}})
			return err
		},
		func() error {
			_, err := read.RemoveFlags(context.Background(), mail.FlagRequest{Refs: refs, Flags: []string{"\\Seen"}})
			return err
		},
		func() error {
			_, err := read.CopyMessages(context.Background(), mail.TransferRequest{Refs: refs, Destination: "Trash"})
			return err
		},
		func() error {
			_, err := read.MoveMessages(context.Background(), mail.TransferRequest{Refs: refs, Destination: "Trash"})
			return err
		},
		func() error {
			_, err := read.AppendMessage(context.Background(), mail.AppendRequest{Mailbox: "INBOX", MessageBase64: "WA=="})
			return err
		},
		func() error { return read.CreateMailbox(context.Background(), "Trash") },
	}
	for _, call := range calls {
		requireCode(t, call(), "readonly")
	}
	commands, connections := peer.snapshot()
	if len(commands) != 0 || connections != 0 {
		t.Fatal("readonly guard contacted the server")
	}
	_, err := New(peer.config(t, false, true, "tls")).MoveMessages(context.Background(), mail.TransferRequest{Refs: refs, Destination: "Trash"})
	requireCode(t, err, "unsupported")
	commands, _ = peer.snapshot()
	for _, command := range commands {
		for _, unsafe := range []string{"COPY", "STORE", "EXPUNGE", "CLOSE", "MOVE"} {
			if strings.Contains(command, unsafe) {
				t.Fatalf("unsafe fallback command %s", command)
			}
		}
	}
}
func TestStaleUIDValidityStopsBeforeFetch(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		if p.command == "EXAMINE" {
			p.selectBox(8, 1, 0)
			return true
		}
		return false
	})
	_, err := New(peer.config(t, false, true, "tls")).ReadMessage(context.Background(), mail.ReadRequest{Ref: mail.MessageRef{Mailbox: "INBOX", UIDValidity: 7, UID: 5}})
	requireCode(t, err, "stale_reference")
	commands, _ := peer.snapshot()
	if hasCommand(commands, "FETCH") {
		t.Fatal("stale reference fetched content")
	}
	if !hasCommand(commands, "EXAMINE") {
		t.Fatal("read did not use EXAMINE in write mode")
	}
}

func TestReadPEEKAcceptsUIDAfterLiteral(t *testing.T) {
	raw := "From: Alice <alice@example.org>\r\nSubject: Test\r\nContent-Type: text/plain\r\n\r\nhello"
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		switch p.command {
		case "EXAMINE":
			p.selectBox(7, 1, 0)
		case "UID":
			if strings.HasPrefix(p.args, "FETCH") {
				p.send("* 1 FETCH (UID 5 FLAGS (\\Flagged) INTERNALDATE \"16-Nov-2026 12:00:00 +0000\" RFC822.SIZE %d)\r\n", len(raw))
				p.ok()
			} else {
				return false
			}
		case "FETCH":
			if !strings.Contains(p.args, "BODY.PEEK[]") {
				return false
			}
			p.fetchLiteral(1, 5, "", raw)
		case "STATUS":
			p.status(7, 1, 0)
		default:
			return false
		}
		return true
	})
	message, err := New(peer.config(t, false, true, "tls")).ReadMessage(context.Background(), mail.ReadRequest{Ref: mail.MessageRef{Mailbox: "INBOX", UIDValidity: 7, UID: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if message.Text != "hello" || message.Summary.Subject != "Test" {
		t.Fatalf("unexpected decoded message: %#v", message)
	}
	commands, _ := peer.snapshot()
	if !hasCommand(commands, "BODY.PEEK[]") {
		t.Fatal("missing PEEK fetch")
	}
	if hasCommand(commands, "STORE") || hasCommand(commands, "\\Seen") {
		t.Fatal("read altered Seen")
	}
}

func TestLiteralCancellationAndOversizeCloseRawSocket(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(strconv.FormatBool(oversized), func(t *testing.T) {
			entered := make(chan struct{})
			closed := make(chan struct{})
			peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
				switch p.command {
				case "EXAMINE":
					p.selectBox(7, 1, 0)
				case "UID":
					p.send("* 1 FETCH (UID 5 FLAGS () INTERNALDATE \"16-Nov-2026 12:00:00 +0000\" RFC822.SIZE 100)\r\n")
					p.ok()
				case "FETCH":
					size := 100
					if oversized {
						size = mail.MaxMessageBytes + 1
					}
					p.send("* 1 FETCH (BODY[] {%d}\r\n", size)
					close(entered)
					_, _ = p.reader.ReadByte()
					close(closed)
				default:
					return false
				}
				return true
			})
			client := New(peer.config(t, true, true, "tls"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := client.ReadMessage(ctx, mail.ReadRequest{Ref: mail.MessageRef{Mailbox: "INBOX", UIDValidity: 7, UID: 5}})
				result <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("literal fetch did not begin")
			}
			if !oversized {
				cancel()
			}
			select {
			case err := <-result:
				if oversized {
					requireCode(t, err, "too_large")
				} else {
					requireCode(t, err, "cancelled")
				}
			case <-time.After(time.Second):
				t.Fatal("literal operation did not unblock")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("raw socket was not closed")
			}
		})
	}
}

func TestUncertainWriteIsNotReplayed(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		switch p.command {
		case "SELECT":
			p.selectBox(7, 1, 0)
		case "UID":
			if strings.HasPrefix(p.args, "FETCH") {
				p.send("* 1 FETCH (UID 5)\r\n")
				p.ok()
			} else if strings.HasPrefix(p.args, "STORE") {
				_ = p.conn.Close()
			} else {
				return false
			}
		default:
			return false
		}
		return true
	})
	_, err := New(peer.config(t, false, true, "tls")).AddFlags(context.Background(), mail.FlagRequest{Refs: []mail.MessageRef{{Mailbox: "INBOX", UIDValidity: 7, UID: 5}}, Flags: []string{"\\Seen"}})
	typed := requireCode(t, err, "server_error")
	if !typed.OutcomeUnknown {
		t.Fatal("uncertain write was reported as known failure")
	}
	commands, connections := peer.snapshot()
	stores := 0
	for _, command := range commands {
		if strings.Contains(command, "STORE") {
			stores++
			if !strings.Contains(command, "+FLAGS.SILENT") {
				t.Fatal("flag change replaced existing flags")
			}
		}
	}
	if stores != 1 || connections != 1 {
		t.Fatalf("write replayed: stores=%d connections=%d", stores, connections)
	}
}

func TestRefreshChangedFlagsExpungesAndHeaderFetchedOnce(t *testing.T) {
	phase := 0
	headers := 0
	changedSince := false
	peer := makePeer(t, true, "IMAP4rev1 CONDSTORE", func(p *testPeer) bool {
		count := uint32(2)
		cursor := uint64(20)
		if phase > 1 {
			count = 1
			cursor = 30
		}
		switch p.command {
		case "EXAMINE":
			phase++
			if phase > 1 {
				count = 1
				cursor = 30
			}
			p.selectBox(7, count, cursor)
		case "STATUS":
			p.status(7, count, cursor+10)
		case "FETCH":
			if strings.Contains(p.args, "BODY.PEEK[HEADER]") {
				headers++
				for seq := uint32(1); seq <= 2; seq++ {
					body := "Subject: cached\r\n\r\n"
					p.send("* %d FETCH (BODY[HEADER] {%d}\r\n%s UID %d INTERNALDATE \"16-Nov-2026 12:00:00 +0000\" RFC822.SIZE 100)\r\n", seq, len(body), body, seq+4)
				}
				p.ok()
			} else {
				if phase == 1 {
					p.send("* 1 FETCH (UID 5)\r\n* 2 FETCH (UID 6)\r\n")
				} else {
					p.send("* 1 FETCH (UID 6)\r\n")
				}
				p.ok()
			}
		case "UID":
			if !strings.HasPrefix(p.args, "FETCH") {
				return false
			}
			if phase == 1 {
				p.send("* 1 FETCH (UID 5 FLAGS () MODSEQ (25))\r\n* 2 FETCH (UID 6 FLAGS () MODSEQ (25))\r\n")
			} else {
				changedSince = strings.Contains(p.args, "CHANGEDSINCE 20")
				p.send("* 1 FETCH (UID 6 FLAGS (\\Flagged) MODSEQ (35))\r\n")
			}
			p.ok()
		default:
			return false
		}
		return true
	})
	client := New(peer.config(t, true, true, "tls"))
	first, err := client.Refresh(context.Background(), "INBOX", mail.CacheState{})
	if err != nil {
		t.Fatal(err)
	}
	if first.HighestModSeq != 20 || len(first.NewMetadata) != 2 || len(first.Members) != 2 {
		t.Fatalf("first refresh = %#v", first)
	}
	second, err := client.Refresh(context.Background(), "INBOX", mail.CacheState{Present: true, UIDValidity: 7, HighestModSeq: first.HighestModSeq, CachedUIDs: first.Members})
	if err != nil {
		t.Fatal(err)
	}
	if second.HighestModSeq != 30 || len(second.NewMetadata) != 0 || len(second.Members) != 1 || second.Members[0] != 6 || len(second.FlagUpdates) != 1 || len(second.FlagUpdates[0].Flags) != 1 || second.FlagUpdates[0].Flags[0] != "\\Flagged" {
		t.Fatalf("second refresh = %#v", second)
	}
	if headers != 1 || !changedSince {
		t.Fatalf("headers=%d changedSince=%t", headers, changedSince)
	}
}

func TestNOMODSEQUsesFullFlagsWithoutModseq(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1 CONDSTORE", func(p *testPeer) bool {
		switch p.command {
		case "EXAMINE":
			p.send("* OK [NOMODSEQ] unsupported mailbox\r\n")
			p.selectBox(7, 1, 0)
		case "FETCH":
			p.send("* 1 FETCH (UID 5)\r\n")
			p.ok()
		case "UID":
			p.send("* 1 FETCH (UID 5 FLAGS (\\Seen))\r\n")
			p.ok()
		case "STATUS":
			p.status(7, 1, 0)
		default:
			return false
		}
		return true
	})
	refreshed, err := New(peer.config(t, true, true, "tls")).Refresh(context.Background(), "INBOX", mail.CacheState{Present: true, UIDValidity: 7, HighestModSeq: 10, CachedUIDs: []uint32{5}})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.HighestModSeq != 0 || len(refreshed.FlagUpdates) != 1 {
		t.Fatalf("refresh = %#v", refreshed)
	}
	commands, _ := peer.snapshot()
	for _, command := range commands {
		if strings.Contains(command, "FETCH") && (strings.Contains(command, "MODSEQ") || strings.Contains(command, "CHANGEDSINCE")) {
			t.Fatalf("NOMODSEQ mailbox requested modseq: %s", command)
		}
	}
}

func TestESearchMaxUint32AndFullFilters(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1 ESEARCH", func(p *testPeer) bool {
		switch p.command {
		case "EXAMINE":
			p.selectBox(7, 1, 0)
		case "STATUS":
			p.status(7, 1, 0)
		case "UID":
			if !strings.HasPrefix(p.args, "SEARCH") {
				return false
			}
			p.send("* ESEARCH (TAG \"%s\") UID MIN 4294967295 MAX 4294967295 ALL 4294967295 COUNT 1\r\n", p.tag)
			p.ok()
		default:
			return false
		}
		return true
	})
	q := mail.SearchQuery{Mailbox: "INBOX", From: "alice", To: "bob", Cc: "carol", Bcc: "dan", Subject: "subject", Body: "body", Text: "text", Headers: map[string]string{"X-Tag": "tag"}, Since: "2026-01-01", Before: "2026-11-01", SentSince: "2026-02-01", SentBefore: "2026-10-01", Flags: []string{"\\Seen"}, NotFlags: []string{"\\Flagged"}, MinSize: 10, MaxSize: 100}
	result, err := New(peer.config(t, true, true, "tls")).SearchUIDs(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0] != ^uint32(0) {
		t.Fatalf("result = %v", result)
	}
	commands, _ := peer.snapshot()
	for _, fragment := range []string{"MIN", "MAX", "COUNT", "ALL", "BODY", "TEXT", "X-Tag", "SINCE", "BEFORE", "SENTSINCE", "SENTBEFORE", "SEEN", "UNFLAGGED", "LARGER 10", "SMALLER 100", "FROM", "TO", "CC", "BCC", "SUBJECT"} {
		if !hasCommand(commands, fragment) {
			t.Fatalf("search did not encode %s: %v", fragment, commands)
		}
	}
}

func TestNativeTransferMappingMaxUint32(t *testing.T) {
	for _, move := range []bool{false, true} {
		t.Run(strconv.FormatBool(move), func(t *testing.T) {
			peer := makePeer(t, true, "IMAP4rev1 UIDPLUS MOVE", func(p *testPeer) bool {
				switch p.command {
				case "SELECT":
					p.selectBox(7, 1, 0)
				case "UID":
					if strings.HasPrefix(p.args, "FETCH") {
						p.send("* 1 FETCH (UID 4294967295)\r\n")
						p.ok()
					} else if strings.HasPrefix(p.args, "COPY") {
						p.send("%s OK [COPYUID 8 4294967295 4294967295] copied\r\n", p.tag)
					} else if strings.HasPrefix(p.args, "MOVE") {
						p.send("* OK [COPYUID 8 4294967295 4294967295] moved\r\n")
						p.ok()
					} else {
						return false
					}
				default:
					return false
				}
				return true
			})
			client := New(peer.config(t, false, true, "tls"))
			request := mail.TransferRequest{Refs: []mail.MessageRef{{Mailbox: "INBOX", UIDValidity: 7, UID: ^uint32(0)}}, Destination: "Trash"}
			var result mail.MutationResult
			var err error
			if move {
				result, err = client.MoveMessages(context.Background(), request)
			} else {
				result, err = client.CopyMessages(context.Background(), request)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !result.MappingAvailable || len(result.DestinationRefs) != 1 || result.DestinationRefs[0].UID != ^uint32(0) || result.DestinationRefs[0].UIDValidity != 8 {
				t.Fatalf("result = %#v", result)
			}
			commands, _ := peer.snapshot()
			if hasCommand(commands, "EXPUNGE") || hasCommand(commands, "\\Deleted") || hasCommand(commands, "CLOSE") {
				t.Fatalf("unsafe transfer: %v", commands)
			}
		})
	}
}

func TestFlagValidationAndRemoveDeleted(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		switch p.command {
		case "SELECT":
			p.selectBox(7, 1, 0)
		case "UID":
			if strings.HasPrefix(p.args, "FETCH") {
				p.send("* 1 FETCH (UID 5)\r\n")
			}
			p.ok()
		default:
			return false
		}
		return true
	})
	client := New(peer.config(t, false, true, "tls"))
	refs := []mail.MessageRef{{Mailbox: "INBOX", UIDValidity: 7, UID: 5}}
	for _, flag := range []string{"\\Deleted", "\\dElEtEd", "\\Recent", "bad flag", "bad)flag", "bad\r\nflag"} {
		_, err := client.AddFlags(context.Background(), mail.FlagRequest{Refs: refs, Flags: []string{flag}})
		requireCode(t, err, "invalid_argument")
	}
	_, connections := peer.snapshot()
	if connections != 0 {
		t.Fatal("invalid flags contacted IMAP")
	}
	_, err := client.RemoveFlags(context.Background(), mail.FlagRequest{Refs: refs, Flags: []string{"\\Deleted"}})
	if err != nil {
		t.Fatal(err)
	}
	commands, _ := peer.snapshot()
	if !hasCommand(commands, "-FLAGS.SILENT (\\Deleted)") {
		t.Fatalf("remove Deleted did not preserve other flags: %v", commands)
	}
}

func TestAppendUIDAndUnknownOutcome(t *testing.T) {
	for _, drop := range []bool{false, true} {
		t.Run(strconv.FormatBool(drop), func(t *testing.T) {
			peer := makePeer(t, true, "IMAP4rev1 UIDPLUS", func(p *testPeer) bool {
				if p.command != "APPEND" {
					return false
				}
				start := strings.LastIndex(p.args, "{")
				if start < 0 {
					return false
				}
				size, err := strconv.Atoi(strings.TrimSuffix(p.args[start+1:], "}"))
				if err != nil {
					return false
				}
				p.send("+ send literal\r\n")
				data := make([]byte, size)
				if _, err := io.ReadFull(p.reader, data); err != nil {
					return true
				}
				if _, err := p.reader.ReadString('\n'); err != nil {
					return true
				}
				if drop {
					_ = p.conn.Close()
				} else {
					p.send("%s OK [APPENDUID 9 4294967295] appended\r\n", p.tag)
				}
				return true
			})
			result, err := New(peer.config(t, false, true, "tls")).AppendMessage(context.Background(), mail.AppendRequest{Mailbox: "Drafts", MessageBase64: "U3ViamVjdDogdGVzdA0KDQpib2R5", Flags: []string{"\\Draft"}})
			if drop {
				typed := requireCode(t, err, "server_error")
				if !typed.OutcomeUnknown {
					t.Fatal("append outcome was reported as known")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !result.MappingAvailable || len(result.DestinationRefs) != 1 || result.DestinationRefs[0].UID != ^uint32(0) || result.DestinationRefs[0].UIDValidity != 9 {
					t.Fatalf("append result = %#v", result)
				}
			}
			commands, connections := peer.snapshot()
			if connections != 1 || !hasCommand(commands, "APPEND") {
				t.Fatalf("append replayed or omitted: %v %d", commands, connections)
			}
		})
	}
}

func TestMessageSizeAndSequenceDriftFailSafely(t *testing.T) {
	for _, tooLarge := range []bool{false, true} {
		t.Run(strconv.FormatBool(tooLarge), func(t *testing.T) {
			peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
				switch p.command {
				case "EXAMINE":
					p.selectBox(7, 1, 0)
				case "UID":
					size := 100
					if tooLarge {
						size = mail.MaxMessageBytes + 1
					}
					p.send("* 1 FETCH (UID 5 FLAGS () INTERNALDATE \"16-Nov-2026 12:00:00 +0000\" RFC822.SIZE %d)\r\n", size)
					p.ok()
				case "FETCH":
					p.fetchLiteral(1, 6, "", "Subject: wrong message\r\n\r\nwrong")
				default:
					return false
				}
				return true
			})
			_, err := New(peer.config(t, true, true, "tls")).ReadMessage(context.Background(), mail.ReadRequest{Ref: mail.MessageRef{Mailbox: "INBOX", UIDValidity: 7, UID: 5}})
			if tooLarge {
				requireCode(t, err, "too_large")
			} else {
				requireCode(t, err, "not_found")
			}
			commands, _ := peer.snapshot()
			if tooLarge && hasCommand(commands, "BODY.PEEK") {
				t.Fatal("oversized message fetched body")
			}
		})
	}
}

func TestCapabilityNegotiationListSortThread(t *testing.T) {
	caps := "IMAP4rev1 ENABLE UTF8=ACCEPT ESEARCH CONDSTORE LIST-EXTENDED LIST-STATUS SPECIAL-USE NAMESPACE SORT THREAD=REFERENCES QRESYNC BINARY"
	peer := makePeer(t, true, caps, func(p *testPeer) bool {
		switch p.command {
		case "ENABLE":
			p.send("* ENABLED UTF8=ACCEPT\r\n")
			p.ok()
		case "NAMESPACE":
			p.send("* NAMESPACE ((\"\" \"/\")) NIL NIL\r\n")
			p.ok()
		case "LIST":
			p.send("* LIST (\\HasNoChildren \\Trash) \"/\" Trash\r\n* STATUS Trash (MESSAGES 0 UIDVALIDITY 7)\r\n")
			p.ok()
		case "EXAMINE":
			p.selectBox(7, 4, 20)
		case "STATUS":
			p.status(7, 4, 20)
		case "UID":
			if strings.HasPrefix(p.args, "SORT") {
				p.send("* SORT 6 5\r\n")
				p.ok()
			} else if strings.HasPrefix(p.args, "THREAD") {
				p.send("* THREAD (5 6 (7)(8))\r\n")
				p.ok()
			} else {
				return false
			}
		default:
			return false
		}
		return true
	})
	client := New(peer.config(t, true, true, "tls"))
	info, err := client.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !hasCommand(info.EnabledCapabilities, "UTF8=ACCEPT") {
		t.Fatalf("enabled capabilities = %v", info.EnabledCapabilities)
	}
	for _, unsupported := range []string{"QRESYNC", "BINARY", "conditionalwrites"} {
		for _, feature := range info.ImplementedFeatures {
			if feature == unsupported {
				t.Fatalf("unsupported feature advertised: %s", feature)
			}
		}
	}
	boxes, err := client.ListMailboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 || boxes[0].Name != "Trash" || boxes[0].Delimiter != "/" || !hasCommand(boxes[0].Attributes, "\\Trash") {
		t.Fatalf("mailboxes = %#v", boxes)
	}
	sorted, err := client.SearchUIDs(context.Background(), mail.SearchQuery{Mailbox: "INBOX", Sort: "subject_desc", Body: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sorted) != 2 || sorted[0] != 6 {
		t.Fatalf("SORT result = %v", sorted)
	}
	threaded, err := client.ThreadMessages(context.Background(), mail.ThreadRequest{Mailbox: "INBOX", Algorithm: "references", Query: mail.SearchQuery{Subject: "needle"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(threaded.Threads) != 1 || threaded.Threads[0].Ref.UID != 5 || threaded.Threads[0].Ref.UIDValidity != 7 || threaded.Threads[0].Children[0].Ref.UID != 6 || len(threaded.Threads[0].Children[0].Children) != 2 {
		t.Fatalf("threads = %#v", threaded)
	}
	commands, _ := peer.snapshot()
	for _, fragment := range []string{"ENABLE UTF8=ACCEPT", "NAMESPACE", "SPECIAL-USE", "STATUS", "SORT (REVERSE SUBJECT) UTF-8 BODY", "THREAD REFERENCES UTF-8 SUBJECT"} {
		if !hasCommand(commands, fragment) {
			t.Fatalf("missing native extension command %s: %v", fragment, commands)
		}
	}
}

func TestOperationDeadlineDuringCommand(t *testing.T) {
	entered := make(chan struct{})
	closed := make(chan struct{})
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		switch p.command {
		case "EXAMINE":
			p.selectBox(7, 1, 0)
		case "UID":
			close(entered)
			_, _ = p.reader.ReadByte()
			close(closed)
		default:
			return false
		}
		return true
	})
	client := New(peer.config(t, true, true, "tls"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := client.SearchUIDs(ctx, mail.SearchQuery{Mailbox: "INBOX"})
	requireCode(t, err, "timeout")
	if time.Since(started) > time.Second {
		t.Fatal("library timeout overrode caller deadline")
	}
	select {
	case <-entered:
	default:
		t.Fatal("search command was not sent")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("timed out command left raw socket open")
	}
}

func TestAdvertisedPLAINPreferredToLogin(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1 AUTH=PLAIN LOGINDISABLED", nil)
	_, err := New(peer.config(t, true, true, "tls")).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	commands, _ := peer.snapshot()
	if !hasCommand(commands, "AUTHENTICATE PLAIN") || hasCommand(commands, "LOGIN") {
		t.Fatalf("authentication commands = %v", commands)
	}
}

func TestRefreshReconcilesFinalMembershipAndEpoch(t *testing.T) {
	for _, change := range []string{"expunge", "arrival", "epoch"} {
		t.Run(change, func(t *testing.T) {
			statusCalls := 0
			finalMembership := false
			peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
				switch p.command {
				case "EXAMINE":
					p.selectBox(7, 1, 0)
				case "STATUS":
					statusCalls++
					finalMembership = true
					epoch, count := uint32(7), uint32(1)
					switch change {
					case "expunge":
						count = 0
					case "arrival":
						count = 2
					case "epoch":
						epoch = 8
					}
					p.status(epoch, count, 0)
				case "UID":
					p.send("* 1 FETCH (UID 5 FLAGS ())\r\n")
					p.ok()
				case "FETCH":
					if strings.Contains(p.args, "BODY.PEEK[HEADER]") {
						p.fetchLiteral(1, 5, "HEADER", "Subject: fetched\r\n\r\n")
					} else {
						p.send("* 1 FETCH (UID 5)\r\n")
						if finalMembership && change == "arrival" {
							p.send("* 2 FETCH (UID 6)\r\n")
						}
						p.ok()
					}
				default:
					return false
				}
				return true
			})
			result, err := New(peer.config(t, true, true, "tls")).Refresh(context.Background(), "INBOX", mail.CacheState{})
			switch change {
			case "expunge":
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Members) != 0 || len(result.NewMetadata) != 0 || len(result.FlagUpdates) != 0 {
					t.Fatalf("expunged entries published: %#v", result)
				}
			case "arrival":
				requireCode(t, err, "server_error")
			case "epoch":
				requireCode(t, err, "stale_reference")
			}
		})
	}
}
