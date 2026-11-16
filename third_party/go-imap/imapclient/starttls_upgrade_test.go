package imapclient

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func newStartTLSPeer(t *testing.T, serve func(net.Conn) error) *Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		done <- serve(conn)
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	c := New(conn, nil)
	t.Cleanup(func() {
		c.Close()
		listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("STARTTLS peer did not finish")
		}
	})
	return c
}

func readStartTLSCommand(reader *bufio.Reader, want string) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	tag, command, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
	if command != want {
		return "", fmt.Errorf("wire command = %q, want %q", command, want)
	}
	return tag, nil
}

func startTLSConfigs(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		&tls.Config{RootCAs: roots, ServerName: "localhost"}
}

func startTLSPeerUpgrade(conn net.Conn, reader *bufio.Reader, config *tls.Config) (*tls.Conn, error) {
	tag, err := readStartTLSCommand(reader, "STARTTLS")
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(conn, tag+" OK begin TLS\r\n"); err != nil {
		return nil, err
	}
	tlsConn := tls.Server(conn, config)
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}
	return tlsConn, nil
}

func startTLSPeerCapabilities(conn net.Conn, reader *bufio.Reader, caps string) error {
	tag, err := readStartTLSCommand(reader, "CAPABILITY")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK capabilities\r\n", caps, tag)
	return err
}

func TestStartTLSSameClient(t *testing.T) {
	serverConfig, clientConfig := startTLSConfigs(t)
	c := newStartTLSPeer(t, func(conn net.Conn) error {
		// One greeting, with capabilities discovered before deciding to upgrade.
		if _, err := io.WriteString(conn, "* OK ready\r\n"); err != nil {
			return err
		}
		reader := bufio.NewReader(conn)
		if err := startTLSPeerCapabilities(conn, reader, "IMAP4rev1 STARTTLS LOGINDISABLED"); err != nil {
			return err
		}
		tlsConn, err := startTLSPeerUpgrade(conn, reader, serverConfig)
		if err != nil {
			return err
		}
		reader = bufio.NewReader(tlsConn)
		if err := startTLSPeerCapabilities(tlsConn, reader, "IMAP4rev1 AUTH=PLAIN"); err != nil {
			return err
		}
		// Any authentication before the handshake fails the preceding wire steps.
		tag, err := readStartTLSCommand(reader, `LOGIN "user" "password"`)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(tlsConn, tag+" OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] authenticated\r\n"); err != nil {
			return err
		}
		tag, err = readStartTLSCommand(reader, "NOOP")
		if err != nil {
			return err
		}
		if _, err := io.WriteString(tlsConn, tag+" OK ready\r\n"); err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, tlsConn)
		return err
	})
	originalConn, originalReader, originalDecoder := c.conn, c.br, c.dec
	if caps := c.Caps(); !caps.Has(imap.CapStartTLS) || !caps.Has(imap.CapLoginDisabled) {
		t.Fatalf("pre-TLS capabilities = %v", caps)
	}
	if err := c.StartTLS(clientConfig); err != nil {
		t.Fatal(err)
	}
	if c.conn != originalConn || c.br != originalReader || c.dec != originalDecoder {
		t.Fatal("STARTTLS replaced the connection, reader, or decoder")
	}
	if state := c.State(); state != imap.ConnStateNotAuthenticated {
		t.Fatalf("state after TLS = %v", state)
	}
	if caps := c.Caps(); caps.Has(imap.CapStartTLS) || caps.Has(imap.CapLoginDisabled) || !caps.Has(imap.AuthCap("PLAIN")) {
		t.Fatalf("post-TLS capabilities = %v", caps)
	}
	if err := c.Login("user", "password").Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if state := c.State(); state != imap.ConnStateAuthenticated {
		t.Fatalf("state after encrypted login = %v", state)
	}
}

func TestStartTLSWaitsGreeting(t *testing.T) {
	serverConfig, clientConfig := startTLSConfigs(t)
	c := newStartTLSPeer(t, func(conn net.Conn) error {
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		var b [1]byte
		if _, err := conn.Read(b[:]); err == nil {
			return fmt.Errorf("received byte %q before greeting", b[0])
		} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			return fmt.Errorf("waiting for greeting: %w", err)
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n"); err != nil {
			return err
		}
		tlsConn, err := startTLSPeerUpgrade(conn, bufio.NewReader(conn), serverConfig)
		if err != nil {
			return err
		}
		if err := startTLSPeerCapabilities(tlsConn, bufio.NewReader(tlsConn), "IMAP4rev1"); err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, tlsConn)
		return err
	})
	if err := c.StartTLS(clientConfig); err != nil {
		t.Fatal(err)
	}
	if caps := c.Caps(); !caps.Has(imap.CapIMAP4rev1) || caps.Has(imap.CapStartTLS) {
		t.Fatalf("post-TLS capabilities = %v", caps)
	}
}

func TestStartTLSDiscardsTaggedCapabilities(t *testing.T) {
	serverConfig, clientConfig := startTLSConfigs(t)
	c := newStartTLSPeer(t, func(conn net.Conn) error {
		if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n"); err != nil {
			return err
		}
		tag, err := readStartTLSCommand(bufio.NewReader(conn), "STARTTLS")
		if err != nil {
			return err
		}
		if _, err := io.WriteString(conn, tag+" OK [CAPABILITY IMAP4rev1 LOGINDISABLED] begin TLS\r\n"); err != nil {
			return err
		}
		tlsConn := tls.Server(conn, serverConfig)
		if err := tlsConn.Handshake(); err != nil {
			return err
		}
		reader := bufio.NewReader(tlsConn)
		if err := startTLSPeerCapabilities(tlsConn, reader, "IMAP4rev1 AUTH=PLAIN"); err != nil {
			return err
		}
		if b, err := reader.ReadByte(); err == nil {
			return fmt.Errorf("extra command after capability refresh starts with %q", b)
		} else if err != io.EOF {
			return err
		}
		return nil
	})
	if err := c.StartTLS(clientConfig); err != nil {
		t.Fatal(err)
	}
	if caps := c.Caps(); caps.Has(imap.CapLoginDisabled) || !caps.Has(imap.AuthCap("PLAIN")) {
		t.Fatalf("post-TLS capabilities retained the plaintext tagged response: %v", caps)
	}
}

func TestStartTLSRejectsAuthenticated(t *testing.T) {
	for _, preauth := range []bool{true, false} {
		t.Run(fmt.Sprintf("PREAUTH=%v", preauth), func(t *testing.T) {
			c := newStartTLSPeer(t, func(conn net.Conn) error {
				greeting := "OK"
				if preauth {
					greeting = "PREAUTH"
				}
				if _, err := fmt.Fprintf(conn, "* %s [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n", greeting); err != nil {
					return err
				}
				reader := bufio.NewReader(conn)
				if !preauth {
					tag, err := readStartTLSCommand(reader, `LOGIN "user" "password"`)
					if err != nil {
						return err
					}
					if _, err := io.WriteString(conn, tag+" OK [CAPABILITY IMAP4rev1 STARTTLS] authenticated\r\n"); err != nil {
						return err
					}
					// Order the state check after LOGIN's post-completion update.
					tag, err = readStartTLSCommand(reader, "NOOP")
					if err != nil {
						return err
					}
					if _, err := io.WriteString(conn, tag+" OK ready\r\n"); err != nil {
						return err
					}
				}
				if b, err := reader.ReadByte(); err == nil {
					return fmt.Errorf("authenticated client sent byte %q", b)
				} else if err != io.EOF {
					return err
				}
				return nil
			})
			if err := c.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			if !preauth {
				if err := c.Login("user", "password").Wait(); err != nil {
					t.Fatal(err)
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.StartTLS(&tls.Config{ServerName: "localhost"}); err == nil {
				t.Fatal("STARTTLS accepted an authenticated client")
			}
		})
	}
}

func TestStartTLSInvalidGreeting(t *testing.T) {
	for _, greeting := range []string{
		"* BYE rejected\r\n",
		"* INVALID greeting\r\n",
		"* OK [CAPABILITY IMAP4rev1 STARTTLS",
		"* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\rX",
	} {
		t.Run(strings.TrimSpace(greeting), func(t *testing.T) {
			c := newStartTLSPeer(t, func(conn net.Conn) error {
				_, err := io.WriteString(conn, greeting)
				return err
			})
			done := make(chan error, 1)
			go func() { done <- c.StartTLS(&tls.Config{ServerName: "localhost"}) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("STARTTLS accepted an invalid greeting")
				}
			case <-time.After(time.Second):
				t.Fatal("STARTTLS did not reject an invalid greeting")
			}
		})
	}
}

func TestStartTLSVerifiesCertificate(t *testing.T) {
	for _, wrongName := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrongName=%v", wrongName), func(t *testing.T) {
			serverConfig, clientConfig := startTLSConfigs(t)
			if wrongName {
				clientConfig.ServerName = "other.invalid"
			} else {
				clientConfig.RootCAs = x509.NewCertPool()
			}
			c := newStartTLSPeer(t, func(conn net.Conn) error {
				if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n"); err != nil {
					return err
				}
				_, err := startTLSPeerUpgrade(conn, bufio.NewReader(conn), serverConfig)
				if err == nil {
					return fmt.Errorf("TLS peer accepted a client rejecting its certificate")
				}
				if !strings.Contains(err.Error(), "bad certificate") {
					return fmt.Errorf("TLS peer expected certificate rejection: %w", err)
				}
				return nil
			})
			if err := c.StartTLS(clientConfig); err == nil {
				t.Fatal("STARTTLS accepted an untrusted or mismatched certificate")
			} else if !strings.Contains(err.Error(), "certificate") {
				t.Fatalf("STARTTLS error = %v, want certificate verification failure", err)
			}
		})
	}
}

func TestStartTLSHoldsEncoderThroughHandshake(t *testing.T) {
	serverConfig, clientConfig := startTLSConfigs(t)
	taggedSent := make(chan struct{})
	allowHandshake := make(chan struct{})
	c := newStartTLSPeer(t, func(conn net.Conn) error {
		if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n"); err != nil {
			return err
		}
		tag, err := readStartTLSCommand(bufio.NewReader(conn), "STARTTLS")
		if err != nil {
			return err
		}
		if _, err := io.WriteString(conn, tag+" OK begin TLS\r\n"); err != nil {
			return err
		}
		close(taggedSent)
		<-allowHandshake
		tlsConn := tls.Server(conn, serverConfig)
		if err := tlsConn.Handshake(); err != nil {
			return err
		}
		reader := bufio.NewReader(tlsConn)
		// The refresh and the queued command can acquire the encoder in either order.
		for i := 0; i < 2; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			tag, command, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
			switch command {
			case "CAPABILITY":
				_, err = fmt.Fprintf(tlsConn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", tag)
			case "NOOP":
				_, err = io.WriteString(tlsConn, tag+" OK ready\r\n")
			default:
				return fmt.Errorf("unexpected encrypted command %q", command)
			}
			if err != nil {
				return err
			}
		}
		_, err = io.Copy(io.Discard, tlsConn)
		return err
	})
	upgradeDone := make(chan error, 1)
	go func() { upgradeDone <- c.StartTLS(clientConfig) }()
	select {
	case <-taggedSent:
	case <-time.After(time.Second):
		close(allowHandshake)
		t.Fatal("STARTTLS did not reach its tagged response")
	}
	commandSent := make(chan *Command, 1)
	go func() { commandSent <- c.Noop() }()
	var noop *Command
	select {
	case noop = <-commandSent:
		t.Error("queued command returned before TLS handshake completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(allowHandshake)
	if err := <-upgradeDone; err != nil {
		t.Fatal(err)
	}
	if noop == nil {
		noop = <-commandSent
	}
	if err := noop.Wait(); err != nil {
		t.Fatal(err)
	}
	if caps := c.Caps(); !caps.Has(imap.CapIMAP4rev1) {
		t.Fatalf("post-TLS capabilities = %v", caps)
	}
}

func TestStartTLSMalformedCompletion(t *testing.T) {
	for _, suffix := range []string{"\r", "\rX"} {
		t.Run(fmt.Sprintf("%q", suffix), func(t *testing.T) {
			c := newStartTLSPeer(t, func(conn net.Conn) error {
				if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n"); err != nil {
					return err
				}
				tag, err := readStartTLSCommand(bufio.NewReader(conn), "STARTTLS")
				if err != nil {
					return err
				}
				_, err = io.WriteString(conn, tag+" OK"+suffix)
				return err
			})
			if err := c.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.startTLS(&tls.Config{ServerName: "localhost"}) }()
			select {
			case <-c.Closed():
			case <-time.After(time.Second):
				t.Fatal("decoder did not exit after malformed STARTTLS completion")
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("malformed STARTTLS completion succeeded")
				} else if !strings.Contains(err.Error(), "in response:") {
					t.Fatalf("STARTTLS error = %v, want the decoder response error", err)
				}
			case <-time.After(time.Second):
				t.Fatal("STARTTLS blocked after its decoder exited")
			}
		})
	}
}
