package imapclient

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"

	"github.com/emersion/go-imap/v2"
)

// StartTLS upgrades this client's connection to TLS.
//
// It waits for the greeting and requires a non-authenticated connection.
// The TLS configuration must provide the server name or another means of
// verifying the server certificate. It blocks until TLS negotiation completes.
// See docs/adr/0007-imap-transport-modes.md.
func (c *Client) StartTLS(config *tls.Config) error {
	if err := c.WaitGreeting(); err != nil {
		return err
	}
	if c.State() != imap.ConnStateNotAuthenticated {
		return fmt.Errorf("imapclient: STARTTLS requires a non-authenticated connection")
	}
	return c.startTLS(config)
}

// startTLS sends a STARTTLS command.
//
// Unlike other commands, this method blocks until the command completes.
func (c *Client) startTLS(config *tls.Config) error {
	upgradeDone := make(chan struct{})
	cmd := &startTLSCommand{
		tlsConfig:   config,
		upgradeDone: upgradeDone,
	}
	enc := c.beginCommand("STARTTLS", cmd)
	enc.flush()
	defer enc.end()

	// Once a client issues a STARTTLS command, it MUST NOT issue further
	// commands until a server response is seen and the TLS negotiation is
	// complete

	if err := cmd.wait(); err != nil {
		return err
	}

	// Tagged completion precedes CRLF validation. If the decoder exits before
	// upgrading, wait for its shutdown before reading the decoder error.
	select {
	case <-upgradeDone:
		if err := cmd.tlsConn.Handshake(); err != nil {
			return err
		}
	case <-c.decCh:
		if c.decErr != nil {
			return c.decErr
		}
		return io.ErrUnexpectedEOF
	}

	// A CAPABILITY code on STARTTLS is still plaintext. Ordinary tagged OK
	// already schedules a refresh; discard any capabilities that survived it.
	c.mutex.Lock()
	hasCaps := c.caps != nil
	c.mutex.Unlock()
	if hasCaps {
		c.setCaps(nil)
	}
	return nil
}

// upgradeStartTLS finishes the STARTTLS upgrade after the server has sent an
// OK response. It runs in the decoder goroutine.
func (c *Client) upgradeStartTLS(startTLS *startTLSCommand) {
	defer close(startTLS.upgradeDone)

	// Drain buffered data from our bufio.Reader
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, c.br, int64(c.br.Buffered())); err != nil {
		panic(err) // unreachable
	}

	var cleartextConn net.Conn
	if buf.Len() > 0 {
		r := io.MultiReader(&buf, c.conn)
		cleartextConn = startTLSConn{c.conn, r}
	} else {
		cleartextConn = c.conn
	}

	tlsConn := tls.Client(cleartextConn, startTLS.tlsConfig)
	rw := c.options.wrapReadWriter(tlsConn)

	c.br.Reset(rw)
	// Unfortunately we can't re-use the bufio.Writer here, it races with
	// Client.StartTLS
	c.bw = bufio.NewWriter(rw)

	startTLS.tlsConn = tlsConn
}

type startTLSCommand struct {
	commandBase
	tlsConfig *tls.Config

	upgradeDone chan<- struct{}
	tlsConn     *tls.Conn
}

type startTLSConn struct {
	net.Conn
	r io.Reader
}

func (conn startTLSConn) Read(b []byte) (int, error) {
	return conn.r.Read(b)
}
