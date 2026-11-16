package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestInitialTransportPortBoundary(t *testing.T) {
	for _, test := range []struct {
		mode string
		port int
		want string
	}{
		{"auto", 993, "tls"},
		{"auto", 143, "auto"},
		{"auto", 1993, "auto"},
		{"tls", 143, "tls"},
		{"starttls", 993, "starttls"},
		{"none", 993, "none"},
	} {
		if got := initialTransport(test.mode, test.port); got != test.want {
			t.Errorf("initialTransport(%q, %d) = %q, want %q", test.mode, test.port, got, test.want)
		}
	}
}

func TestTransportResolvedBeforeAuthentication(t *testing.T) {
	for _, test := range []struct {
		mode, caps, want string
		implicit         bool
	}{
		{"auto", "IMAP4rev1", "none", false},
		{"auto", "", "none", false},
		{"none", "IMAP4rev1 STARTTLS", "none", false},
		{"tls", "IMAP4rev1", "tls", true},
		{"starttls", "IMAP4rev1 STARTTLS", "starttls", false},
	} {
		t.Run(test.mode, func(t *testing.T) {
			var peer *peerServer
			peer = makePeerWithGreeting(t, test.implicit, test.caps, "OK", func(p *testPeer) bool {
				if test.caps != "" || p.command != "CAPABILITY" {
					return false
				}
				p.send("* CAPABILITY IMAP4rev1\r\n")
				p.ok()
				return true
			}, func(p *testPeer) bool {
				if p.command != "STARTTLS" {
					return false
				}
				upgradePeer(p, peer.tlsConfig)
				return true
			})
			var reported []string
			err := New(peer.config(t, true, true, test.mode)).CheckConnection(context.Background(), func(mode string) {
				reported = append(reported, mode)
				commands, _ := peer.snapshot()
				if hasCommand(commands, "LOGIN") || hasCommand(commands, "AUTHENTICATE") {
					t.Errorf("transport reported after authentication: %v", commands)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			requireDisconnected(t, peer)
			commands, connections := peer.snapshot()
			if connections != 1 || len(reported) != 1 || reported[0] != test.want || !hasCommand(commands, "LOGIN") {
				t.Fatalf("reported=%v commands=%v connections=%d", reported, commands, connections)
			}
			if test.want == "none" && hasCommand(commands, "STARTTLS") {
				t.Fatalf("plaintext mode upgraded: %v", commands)
			}
		})
	}
}

func TestAutoSTARTTLSUsesFreshCapabilitiesBeforeAuthentication(t *testing.T) {
	for _, test := range []struct {
		greetingCaps, acknowledgementCaps bool
	}{
		{false, false},
		{true, false},
		{true, true},
	} {
		t.Run(fmt.Sprintf("greeting-capabilities=%t/acknowledgement-capabilities=%t", test.greetingCaps, test.acknowledgementCaps), func(t *testing.T) {
			preTLSCaps := "IMAP4rev1 STARTTLS LOGINDISABLED"
			caps := ""
			if test.greetingCaps {
				caps = preTLSCaps
			}
			var peer *peerServer
			peer = makePeerWithGreeting(t, false, caps, "OK", func(p *testPeer) bool {
				if p.command != "CAPABILITY" {
					return false
				}
				caps := preTLSCaps
				if _, secure := p.conn.(*tls.Conn); secure {
					caps = "IMAP4rev1 AUTH=PLAIN LOGINDISABLED ENABLE UTF8=ACCEPT"
				}
				p.send("* CAPABILITY %s\r\n", caps)
				p.ok()
				return true
			}, func(p *testPeer) bool {
				switch p.command {
				case "STARTTLS":
					if test.acknowledgementCaps {
						p.send("%s OK [CAPABILITY IMAP4rev1 LOGINDISABLED] upgrade\r\n", p.tag)
						handshakePeer(p, peer.tlsConfig)
					} else {
						upgradePeer(p, peer.tlsConfig)
					}
				case "ENABLE":
					p.send("* ENABLED UTF8=ACCEPT\r\n")
					p.ok()
				default:
					return false
				}
				return true
			})
			var reported []string
			err := New(peer.config(t, true, true, "auto")).CheckConnection(context.Background(), func(mode string) {
				reported = append(reported, mode)
				commands, _ := peer.snapshot()
				if hasCommand(commands, "LOGIN") || hasCommand(commands, "AUTHENTICATE") {
					t.Errorf("transport reported after authentication: %v", commands)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			requireDisconnected(t, peer)
			commands, connections := peer.snapshot()
			if connections != 1 || len(reported) != 1 || reported[0] != "starttls" ||
				!hasCommand(commands, "AUTHENTICATE PLAIN") || hasCommand(commands, "LOGIN") ||
				!hasCommand(commands, "ENABLE UTF8=ACCEPT") {
				t.Fatalf("reported=%v commands=%v connections=%d", reported, commands, connections)
			}
			upgraded := false
			for _, command := range commands {
				verb := strings.Fields(command)[0]
				if !upgraded && verb != "CAPABILITY" && verb != "STARTTLS" {
					t.Fatalf("command before upgrade: %s", command)
				}
				if verb == "STARTTLS" {
					upgraded = true
				}
			}
			if !upgraded {
				t.Fatalf("missing upgrade: %v", commands)
			}
		})
	}
}

func TestCapabilityDiscoveryFailureDoesNotAuthenticateOrChoosePlaintext(t *testing.T) {
	for _, test := range []struct {
		name, mode, response string
		postTLS              bool
	}{
		{"auto-refused", "auto", "NO fixture-secret capability denied", false},
		{"auto-missing", "auto", "OK complete", false},
		{"none-refused", "none", "NO fixture-secret capability denied", false},
		{"posttls-refused", "auto", "NO fixture-secret capability denied", true},
		{"posttls-missing", "auto", "OK complete", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			caps := ""
			if test.postTLS {
				caps = "IMAP4rev1 STARTTLS"
			}
			var peer *peerServer
			peer = makePeerWithGreeting(t, false, caps, "OK", func(p *testPeer) bool {
				if p.command != "CAPABILITY" {
					return false
				}
				p.send("%s %s\r\n", p.tag, test.response)
				return true
			}, func(p *testPeer) bool {
				if p.command != "STARTTLS" {
					return false
				}
				upgradePeer(p, peer.tlsConfig)
				return true
			})
			var reported []string
			err := New(peer.config(t, true, true, test.mode)).CheckConnection(context.Background(), func(mode string) {
				reported = append(reported, mode)
			})
			requireCode(t, err, "server_error")
			requireDisconnected(t, peer)
			commands, connections := peer.snapshot()
			if connections != 1 || len(reported) != 0 || hasCommand(commands, "LOGIN") || hasCommand(commands, "AUTHENTICATE") {
				t.Fatalf("discovery failure continued: reported=%v commands=%v connections=%d", reported, commands, connections)
			}
			if !test.postTLS && hasCommand(commands, "STARTTLS") {
				t.Fatalf("failed discovery attempted upgrade: %v", commands)
			}
		})
	}
}

func TestAutoAdvertisedSTARTTLSFailureDoesNotRetry(t *testing.T) {
	for _, trust := range []bool{false, true} {
		t.Run(fmt.Sprintf("trust=%t", trust), func(t *testing.T) {
			var peer *peerServer
			peer = makePeer(t, false, "IMAP4rev1 STARTTLS", func(p *testPeer) bool {
				if p.command != "STARTTLS" {
					return false
				}
				if trust {
					p.send("%s NO fixture-secret refused\r\n", p.tag)
				} else {
					upgradePeer(p, peer.tlsConfig)
				}
				return true
			})
			var reported []string
			err := New(peer.config(t, true, trust, "auto")).CheckConnection(context.Background(), func(mode string) {
				reported = append(reported, mode)
			})
			typed := requireCode(t, err, "tls_failed")
			if !trust && typed.Message != "IMAP server certificate is signed by an unknown authority" {
				t.Fatalf("certificate failure = %q", typed.Message)
			}
			requireDisconnected(t, peer)
			commands, connections := peer.snapshot()
			if connections != 1 || len(reported) != 0 || len(commands) != 1 || commands[0] != "STARTTLS " {
				t.Fatalf("upgrade failure continued: reported=%v commands=%v connections=%d", reported, commands, connections)
			}
		})
	}
}

func TestAutoCancellationAndDeadlineCloseDiscoveryAndUpgrade(t *testing.T) {
	for _, stage := range []string{"CAPABILITY", "STARTTLS", "handshake"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%t", stage, deadline), func(t *testing.T) {
				entered := make(chan struct{})
				caps := "IMAP4rev1 STARTTLS"
				if stage == "CAPABILITY" {
					caps = ""
				}
				peer := makePeerWithGreeting(t, false, caps, "OK", func(p *testPeer) bool {
					if p.command != stage && !(stage == "handshake" && p.command == "STARTTLS") {
						return false
					}
					if stage == "handshake" {
						p.ok()
					}
					close(entered)
					_, _ = io.Copy(io.Discard, p.reader)
					return true
				}, nil)
				ctx, cancel := context.WithCancel(context.Background())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
				}
				defer cancel()
				result := make(chan error, 1)
				client := New(peer.config(t, true, true, "auto"))
				go func() {
					result <- client.CheckConnection(ctx, func(mode string) {
						t.Errorf("stalled transport reported as %q", mode)
					})
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("did not enter stalled stage")
				}
				code := "timeout"
				if !deadline {
					cancel()
					code = "cancelled"
				}
				select {
				case err := <-result:
					requireCode(t, err, code)
				case <-time.After(time.Second):
					t.Fatal("cancellation/deadline did not unblock")
				}
				requireDisconnected(t, peer)
				commands, connections := peer.snapshot()
				if connections != 1 || hasCommand(commands, "LOGIN") || hasCommand(commands, "AUTHENTICATE") {
					t.Fatalf("stalled transport retried/authenticated: commands=%v connections=%d", commands, connections)
				}
			})
		}
	}
}

func TestTransportPREAUTHMatrix(t *testing.T) {
	for _, test := range []struct {
		mode, caps, want string
		reject           bool
	}{
		{"auto", "IMAP4rev1 STARTTLS", "", true},
		{"auto", "IMAP4rev1", "none", false},
		{"none", "IMAP4rev1 STARTTLS", "none", false},
		{"starttls", "IMAP4rev1 STARTTLS", "", true},
		{"tls", "IMAP4rev1 STARTTLS", "tls", false},
	} {
		t.Run(test.mode+"/"+test.caps, func(t *testing.T) {
			var peer *peerServer
			peer = makePeerWithGreeting(t, test.mode == "tls", test.caps, "PREAUTH", nil, func(p *testPeer) bool {
				if p.command != "STARTTLS" {
					return false
				}
				upgradePeer(p, peer.tlsConfig)
				return true
			})
			var reported []string
			err := New(peer.config(t, true, true, test.mode)).CheckConnection(context.Background(), func(mode string) {
				reported = append(reported, mode)
			})
			if test.reject {
				if typed := requireCode(t, err, "tls_failed"); typed.Message != "Cannot establish verified TLS with the IMAP server" {
					t.Fatalf("PREAUTH rejection = %q", typed.Message)
				}
				if len(reported) != 0 {
					t.Fatalf("rejected PREAUTH reported transport: %v", reported)
				}
			} else if err != nil || len(reported) != 1 || reported[0] != test.want {
				t.Fatalf("accepted PREAUTH err=%v reported=%v", err, reported)
			}
			requireDisconnected(t, peer)
			commands, connections := peer.snapshot()
			if connections != 1 || hasCommand(commands, "LOGIN") || hasCommand(commands, "AUTHENTICATE") {
				t.Fatalf("PREAUTH authenticated or retried: commands=%v connections=%d", commands, connections)
			}
			if test.mode != "starttls" && hasCommand(commands, "STARTTLS") {
				t.Fatalf("PREAUTH attempted upgrade: %v", commands)
			}
		})
	}
}
