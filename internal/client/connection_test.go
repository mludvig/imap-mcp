package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func requireDisconnected(t *testing.T, peer *peerServer) {
	t.Helper()
	select {
	case <-peer.disconnected:
	case <-time.After(time.Second):
		t.Fatal("connection check left the server socket open")
	}
}

func upgradePeer(p *testPeer, cfg *tls.Config) {
	p.ok()
	handshakePeer(p, cfg)
}

func handshakePeer(p *testPeer, cfg *tls.Config) {
	secure := tls.Server(p.conn, cfg)
	if secure.Handshake() != nil {
		return
	}
	p.conn = secure
	p.reader = bufio.NewReader(secure)
	p.writer = bufio.NewWriter(secure)
}

func TestCheckConnectionAuthenticatesAndClosesWithoutMailboxCommands(t *testing.T) {
	for _, mode := range []string{"tls", "starttls"} {
		for _, auth := range []string{"LOGIN", "PLAIN"} {
			t.Run(mode+"/"+auth, func(t *testing.T) {
				caps := "IMAP4rev1 ENABLE UTF8=ACCEPT NAMESPACE"
				if auth == "PLAIN" {
					caps += " AUTH=PLAIN LOGINDISABLED"
				}
				if mode == "starttls" {
					caps += " STARTTLS"
				}
				var peer *peerServer
				peer = makePeer(t, mode == "tls", caps, func(p *testPeer) bool {
					switch p.command {
					case "STARTTLS":
						upgradePeer(p, peer.tlsConfig)
					case "ENABLE":
						p.send("* ENABLED UTF8=ACCEPT\r\n")
						p.ok()
					default:
						return false
					}
					return true
				})
				client := newTestClient(t, peer.config(t, false, true, mode))
				if err := client.CheckConnection(context.Background(), nil); err != nil {
					t.Fatal(err)
				}
				requireDisconnected(t, peer)
				commands, connections := peer.snapshot()
				if connections != 1 {
					t.Fatalf("connection count = %d, want 1", connections)
				}
				authCommand := "LOGIN "
				if auth == "PLAIN" {
					authCommand = "AUTHENTICATE PLAIN"
				}
				if !hasCommand(commands, authCommand) || !hasCommand(commands, "CAPABILITY ") || !hasCommand(commands, "ENABLE UTF8=ACCEPT") {
					t.Fatalf("missing authentication or capability negotiation: %v", commands)
				}
				for _, command := range commands {
					switch strings.Fields(command)[0] {
					case "STARTTLS", "CAPABILITY", "LOGIN", "AUTHENTICATE", "ENABLE":
					default:
						t.Fatalf("connection check issued a mailbox command: %s", command)
					}
				}
				if mode == "starttls" && !strings.HasPrefix(commands[0], "STARTTLS ") {
					t.Fatalf("authenticated before STARTTLS: %v", commands)
				}
			})
		}
	}
}

func TestCheckConnectionTLSFailuresCloseWithoutAuthentication(t *testing.T) {
	for _, test := range []struct {
		name, mode, message string
		implicit, trust     bool
	}{
		{"untrusted", "tls", "IMAP server certificate is signed by an unknown authority", true, false},
		{"plaintext", "tls", "IMAP endpoint speaks plaintext; configure STARTTLS or use an implicit-TLS port", false, true},
		{"starttls-refused", "starttls", "Cannot establish verified TLS with the IMAP server", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer := makePeer(t, test.implicit, "IMAP4rev1 STARTTLS", func(p *testPeer) bool {
				if p.command != "STARTTLS" {
					return false
				}
				p.send("%s NO fixture-secret testuser private-host rejected\r\n", p.tag)
				return true
			})
			err := newTestClient(t, peer.config(t, true, test.trust, test.mode)).CheckConnection(context.Background(), nil)
			if typed := requireCode(t, err, "tls_failed"); typed.Message != test.message {
				t.Fatalf("message = %q, want %q", typed.Message, test.message)
			}
			requireDisconnected(t, peer)
			commands, connections := peer.snapshot()
			if connections != 1 || hasCommand(commands, "LOGIN") || hasCommand(commands, "AUTHENTICATE") {
				t.Fatalf("TLS failure retried or authenticated: %v, connections=%d", commands, connections)
			}
		})
	}
}

func TestCheckConnectionAuthenticationFailureIsSanitized(t *testing.T) {
	for _, auth := range []string{"LOGIN", "PLAIN"} {
		t.Run(auth, func(t *testing.T) {
			caps := "IMAP4rev1"
			if auth == "PLAIN" {
				caps += " AUTH=PLAIN LOGINDISABLED"
			}
			peer := makePeerWithGreeting(t, true, caps, "OK", func(p *testPeer) bool {
				switch p.command {
				case "AUTHENTICATE":
					p.send("+ \r\n")
					if _, err := p.reader.ReadString('\n'); err != nil {
						return true
					}
				case "LOGIN":
				default:
					return false
				}
				p.send("%s NO fixture-secret testuser private-host invalid credentials\r\n", p.tag)
				return true
			}, nil)
			err := newTestClient(t, peer.config(t, true, true, "tls")).CheckConnection(context.Background(), nil)
			if typed := requireCode(t, err, "authentication_failed"); typed.Message != "IMAP authentication failed" {
				t.Fatalf("authentication response exposed: %q", typed.Message)
			}
			requireDisconnected(t, peer)
			commands, connections := peer.snapshot()
			if len(commands) != 1 || connections != 1 {
				t.Fatalf("failed authentication continued or retried: %v, connections=%d", commands, connections)
			}
		})
	}
}

func TestCheckConnectionCancellationAndDeadlineCloseSession(t *testing.T) {
	for _, stage := range []string{"STARTTLS", "LOGIN"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%t", stage, deadline), func(t *testing.T) {
				entered := make(chan struct{})
				peer := makePeerWithGreeting(t, stage == "LOGIN", "IMAP4rev1 STARTTLS", "OK", func(p *testPeer) bool {
					if p.command != stage {
						return false
					}
					close(entered)
					_, _ = p.reader.ReadByte()
					return true
				}, nil)
				mode := "tls"
				if stage == "STARTTLS" {
					mode = "starttls"
				}
				ctx, cancel := context.WithCancel(context.Background())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
				}
				defer cancel()
				result := make(chan error, 1)
				client := newTestClient(t, peer.config(t, true, true, mode))
				go func() { result <- client.CheckConnection(ctx, nil) }()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("connection check did not enter the stalled command")
				}
				code, message := "cancelled", "IMAP operation was cancelled"
				if deadline {
					code, message = "timeout", "IMAP operation timed out"
				} else {
					cancel()
				}
				select {
				case err := <-result:
					if typed := requireCode(t, err, code); typed.Message != message {
						t.Fatalf("message = %q, want %q", typed.Message, message)
					}
				case <-time.After(time.Second):
					t.Fatal("connection check did not unblock")
				}
				requireDisconnected(t, peer)
			})
		}
	}
}

func TestCheckConnectionRefusesPlaintextPREAUTH(t *testing.T) {
	var peer *peerServer
	peer = makePeerWithGreeting(t, false, "IMAP4rev1 STARTTLS", "PREAUTH", nil, func(p *testPeer) bool {
		if p.command != "STARTTLS" {
			return false
		}
		upgradePeer(p, peer.tlsConfig)
		return true
	})
	err := newTestClient(t, peer.config(t, true, true, "starttls")).CheckConnection(context.Background(), nil)
	if typed := requireCode(t, err, "tls_failed"); typed.Message != "Cannot establish verified TLS with the IMAP server" {
		t.Fatalf("message = %q", typed.Message)
	}
	requireDisconnected(t, peer)
	if commands, connections := peer.snapshot(); len(commands) != 1 || commands[0] != "STARTTLS " || connections != 1 {
		t.Fatalf("plaintext PREAUTH was accepted: %v, connections=%d", commands, connections)
	}
}

func TestSafeErrorTLSMessagesAreTypedAndSanitized(t *testing.T) {
	certificate := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "fixture-secret testuser private-host"},
		DNSNames:  []string{"private-host"},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(-time.Minute),
	}
	future := *certificate
	future.NotBefore = time.Now().Add(time.Hour)
	future.NotAfter = time.Now().Add(2 * time.Hour)
	for _, test := range []struct {
		name, message string
		err           error
	}{
		{"unknown-authority", "IMAP server certificate is signed by an unknown authority", x509.UnknownAuthorityError{Cert: certificate}},
		{"hostname", "IMAP server certificate does not match the configured hostname", x509.HostnameError{Certificate: certificate, Host: "fixture-secret testuser private-host"}},
		{"expired", "IMAP server certificate has expired", x509.CertificateInvalidError{Cert: certificate, Reason: x509.Expired, Detail: "fixture-secret testuser private-host"}},
		{"not-yet-valid", "IMAP server certificate is not yet valid", x509.CertificateInvalidError{Cert: &future, Reason: x509.Expired, Detail: "fixture-secret testuser private-host"}},
		{"other-invalid-certificate", "Cannot establish verified TLS with the IMAP server", x509.CertificateInvalidError{Cert: certificate, Reason: x509.IncompatibleUsage, Detail: "fixture-secret"}},
		{"plaintext", "IMAP endpoint speaks plaintext; configure STARTTLS or use an implicit-TLS port", tls.RecordHeaderError{RecordHeader: [5]byte{'*', ' ', 'O', 'K', ' '}, Msg: "fixture-secret testuser private-host"}},
		{"other-record-header", "Cannot establish verified TLS with the IMAP server", tls.RecordHeaderError{RecordHeader: [5]byte{'H', 'T', 'T', 'P', '/'}, Msg: "fixture-secret"}},
		{"unproven-preauth-prefix", "Cannot establish verified TLS with the IMAP server", tls.RecordHeaderError{RecordHeader: [5]byte{'*', ' ', 'P', 'R', 'E'}, Msg: "fixture-secret"}},
		{"raw-error", "Cannot establish verified TLS with the IMAP server", errors.New("x509: fixture-secret private-host unknown authority")},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &tls.CertificateVerificationError{Err: test.err}
			typed := requireCode(t, safeError(context.Background(), err, "tls_failed", false), "tls_failed")
			if typed.Message != test.message || typed.OutcomeUnknown {
				t.Fatalf("error = %#v, want message %q and a known outcome", typed, test.message)
			}
		})
	}
}

func TestSafeErrorTimeoutAndCancellationOverrideTLSDetails(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			code, message := "cancelled", "IMAP operation was cancelled"
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				code, message = "timeout", "IMAP operation timed out"
			}
			cancel()
			err := x509.UnknownAuthorityError{Cert: &x509.Certificate{Subject: pkix.Name{CommonName: "fixture-secret"}}}
			if typed := requireCode(t, safeError(ctx, err, "tls_failed", false), code); typed.Message != message {
				t.Fatalf("message = %q, want %q", typed.Message, message)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := &net.DNSError{Err: "fixture-secret", Name: "private-host", IsTimeout: true}
	if typed := requireCode(t, safeError(ctx, err, "tls_failed", false), "timeout"); typed.Message != "IMAP operation timed out" {
		t.Fatalf("network timeout precedence changed: %#v", typed)
	}
}
