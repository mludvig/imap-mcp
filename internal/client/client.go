// Package client adapts verified TLS IMAP sessions to the shared mail contracts.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/mail"
)

type Client struct{ cfg *config.Config }

func New(cfg *config.Config) *Client { return &Client{cfg: cfg} }

var _ mail.Backend = (*Client)(nil)

// CheckConnection verifies TLS and authentication, then closes the session.
// The startup diagnostic boundary is described in docs/adr/0006-linux-cli-startup-validation.md.
func (c *Client) CheckConnection(ctx context.Context) error {
	s, err := c.open(ctx)
	if err != nil {
		return err
	}
	s.close()
	return nil
}

// Each operation owns a connection. Cancellation closes the raw socket, even
// during TLS/literal I/O; see docs/concepts/imap-safety.md.
type session struct {
	c        *imapclient.Client
	raw      net.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	stop     func() bool
	caps     imap.CapSet
	enabled  imap.CapSet
	noModSeq bool
}

type deadlineConn struct {
	net.Conn
	deadline time.Time
}

func (c *deadlineConn) clamp(t time.Time) time.Time {
	if t.IsZero() || c.deadline.Before(t) {
		return c.deadline
	}
	return t
}
func (c *deadlineConn) SetDeadline(t time.Time) error     { return c.Conn.SetDeadline(c.clamp(t)) }
func (c *deadlineConn) SetReadDeadline(t time.Time) error { return c.Conn.SetReadDeadline(c.clamp(t)) }
func (c *deadlineConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(c.clamp(t))
}

func (c *Client) open(ctx context.Context) (*session, error) {
	cfg := c.cfg.IMAP()
	ctx, cancel := context.WithTimeout(ctx, cfg.OperationTimeout())
	raw, err := (&net.Dialer{Timeout: cfg.ConnectTimeout()}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host(), strconv.Itoa(cfg.Port())))
	if err != nil {
		failure := safeError(ctx, err, "connection_failed", false)
		cancel()
		return nil, failure
	}
	s := &session{raw: raw, ctx: ctx, cancel: cancel, enabled: make(imap.CapSet)}
	s.stop = context.AfterFunc(ctx, func() { _ = raw.Close() })
	deadline, _ := ctx.Deadline()
	conn := &deadlineConn{Conn: raw, deadline: deadline}
	_ = conn.SetDeadline(deadline)
	opts := &imapclient.Options{TLSConfig: cfg.TLSConfig()}
	if cfg.TLS() == "starttls" {
		s.c, err = imapclient.NewStartTLS(conn, opts)
	} else {
		secure := tls.Client(conn, cfg.TLSConfig())
		err = secure.HandshakeContext(ctx)
		if err == nil {
			s.c = imapclient.New(secure, opts)
		}
	}
	if err != nil {
		failure := safeError(ctx, err, "tls_failed", false)
		s.close()
		return nil, failure
	}
	if err = s.c.WaitGreeting(); err != nil {
		failure := safeError(ctx, err, "connection_failed", false)
		s.close()
		return nil, failure
	}
	if s.c.State() == imap.ConnStateNotAuthenticated {
		caps := s.c.Caps()
		if caps.Has(imap.AuthCap("PLAIN")) {
			err = s.c.Authenticate(sasl.NewPlainClient("", cfg.Username(), cfg.Password()))
		} else if !caps.Has(imap.CapLoginDisabled) {
			err = s.c.Login(cfg.Username(), cfg.Password()).Wait()
		} else {
			err = errors.New("no permitted authentication mechanism")
		}
		if err != nil {
			failure := safeError(ctx, err, "authentication_failed", false)
			s.close()
			return nil, failure
		}
	}
	// Authentication can change the advertised capability set.
	s.caps, err = s.c.Capability().Wait()
	if err != nil {
		failure := s.err(err, false)
		s.close()
		return nil, failure
	}
	// beta.9 signals LOGIN completion before updating State. The capability
	// round trip also orders this state check after that update.
	// See docs/concepts/imap-safety.md.
	if s.c.State() != imap.ConnStateAuthenticated {
		s.close()
		return nil, mail.NewError("authentication_failed", "IMAP authentication failed")
	}
	if s.caps.Has(imap.CapEnable) {
		var capability imap.Cap
		if s.caps.Has(imap.CapIMAP4rev2) {
			capability = imap.CapIMAP4rev2
		} else if s.caps.Has(imap.CapUTF8Accept) {
			capability = imap.CapUTF8Accept
		}
		if capability != "" {
			enabled, enableErr := s.c.Enable(capability).Wait()
			if enableErr != nil {
				failure := s.err(enableErr, false)
				s.close()
				return nil, failure
			}
			s.enabled = enabled.Caps
		}
	}
	return s, nil
}

func (s *session) close() {
	if s.raw != nil {
		_ = s.raw.Close()
	}
	if s.c != nil {
		_ = s.c.Close()
	}
	if s.stop != nil {
		s.stop()
	}
	if s.cancel != nil {
		s.cancel()
	}
}

func safeError(ctx context.Context, err error, fallback string, write bool) error {
	if err == nil {
		return nil
	}
	code, msg := fallback, "IMAP operation failed"
	switch fallback {
	case "connection_failed":
		msg = "Cannot connect to the IMAP server"
	case "tls_failed":
		msg = "Cannot establish verified TLS with the IMAP server"
		var recordHeader tls.RecordHeaderError
		var unknownAuthority x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var certificateInvalid x509.CertificateInvalidError
		switch {
		case errors.As(err, &recordHeader) && string(recordHeader.RecordHeader[:]) == "* OK ":
			msg = "IMAP endpoint speaks plaintext; configure STARTTLS or use an implicit-TLS port"
		case errors.As(err, &unknownAuthority):
			msg = "IMAP server certificate is signed by an unknown authority"
		case errors.As(err, &hostname):
			msg = "IMAP server certificate does not match the configured hostname"
		case errors.As(err, &certificateInvalid) && certificateInvalid.Reason == x509.Expired:
			msg = "IMAP server certificate has expired"
			if certificateInvalid.Cert != nil && time.Now().Before(certificateInvalid.Cert.NotBefore) {
				msg = "IMAP server certificate is not yet valid"
			}
		}
	case "authentication_failed":
		msg = "IMAP authentication failed"
	}
	var networkError net.Error
	timedOut := errors.As(err, &networkError) && networkError.Timeout()
	var response *imap.Error
	knownFailure := errors.As(err, &response) && response.Type != imap.StatusResponseTypeBye
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || timedOut {
		code, msg = "timeout", "IMAP operation timed out"
	} else if ctx.Err() != nil {
		code, msg = "cancelled", "IMAP operation was cancelled"
	}
	return &mail.Error{Code: code, Message: msg, OutcomeUnknown: write && !knownFailure}
}
func (s *session) err(err error, write bool) error {
	return safeError(s.ctx, err, "server_error", write)
}

func stringsOf[T ~string](items []T) []string {
	result := make([]string, len(items))
	for i, item := range items {
		result[i] = string(item)
	}
	return result
}
func capStrings(caps imap.CapSet) []string {
	result := make([]string, 0, len(caps))
	for cap := range caps {
		result = append(result, string(cap))
	}
	sort.Strings(result)
	return result
}
func (c *Client) Info(ctx context.Context) (mail.ServerInfo, error) {
	s, err := c.open(ctx)
	if err != nil {
		return mail.ServerInfo{}, err
	}
	defer s.close()
	features := []string{"header_cache", "body_peek"}
	for _, cap := range []imap.Cap{imap.CapESearch, imap.CapCondStore, imap.CapListExtended, imap.CapListStatus, imap.CapSpecialUse, imap.CapSort, imap.CapUIDPlus, imap.CapMove} {
		if s.caps.Has(cap) {
			features = append(features, string(cap))
		}
	}
	if s.caps.Has(imap.CapNamespace) {
		if _, err := s.c.Namespace().Wait(); err != nil {
			return mail.ServerInfo{}, s.err(err, false)
		}
		features = append(features, string(imap.CapNamespace))
	}
	for _, algorithm := range s.caps.ThreadAlgorithms() {
		features = append(features, "THREAD="+string(algorithm))
	}
	features = append(features, capStrings(s.enabled)...)
	sort.Strings(features)
	return mail.ServerInfo{Capabilities: capStrings(s.caps), EnabledCapabilities: capStrings(s.enabled), ImplementedFeatures: features, Readonly: c.cfg.Readonly()}, nil
}

func (c *Client) ListMailboxes(ctx context.Context) ([]mail.MailboxInfo, error) {
	s, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	opts := &imap.ListOptions{}
	if s.caps.Has(imap.CapListExtended) {
		opts.ReturnChildren = true
		opts.ReturnSubscribed = true
	}
	if s.caps.Has(imap.CapSpecialUse) {
		opts.ReturnSpecialUse = true
	}
	if s.caps.Has(imap.CapListStatus) {
		opts.ReturnStatus = &imap.StatusOptions{NumMessages: true, UIDValidity: true}
	}
	items, err := s.c.List("", "*", opts).Collect()
	if err != nil {
		return nil, s.err(err, false)
	}
	result := make([]mail.MailboxInfo, 0, len(items))
	for _, item := range items {
		delim := ""
		if item.Delim != 0 {
			delim = string(item.Delim)
		}
		result = append(result, mail.MailboxInfo{Name: item.Mailbox, Delimiter: delim, Attributes: stringsOf(item.Attrs)})
	}
	return result, nil
}
func (s *session) status(name string) (*imap.StatusData, error) {
	return s.c.Status(name, &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true, HighestModSeq: s.caps.Has(imap.CapCondStore) && !s.noModSeq}).Wait()
}
func (c *Client) MailboxStatus(ctx context.Context, name string) (mail.MailboxStatus, error) {
	s, err := c.open(ctx)
	if err != nil {
		return mail.MailboxStatus{}, err
	}
	defer s.close()
	data, err := s.status(name)
	if err != nil {
		return mail.MailboxStatus{}, s.err(err, false)
	}
	result := mail.MailboxStatus{Mailbox: name, UIDNext: uint32(data.UIDNext), UIDValidity: data.UIDValidity, HighestModSeq: data.HighestModSeq}
	if data.NumMessages != nil {
		result.Messages = *data.NumMessages
	}
	if data.NumUnseen != nil {
		result.Unseen = *data.NumUnseen
	}
	return result, nil
}
func (s *session) selectMailbox(name string, readonly bool) (*imap.SelectData, error) {
	if name == "" {
		return nil, mail.NewError("invalid_argument", "Mailbox is required")
	}
	data, err := s.c.Select(name, &imap.SelectOptions{ReadOnly: readonly, CondStore: s.caps.Has(imap.CapCondStore)}).Wait()
	if err != nil {
		return nil, s.err(err, false)
	}
	if data.UIDValidity == 0 {
		return nil, mail.NewError("server_error", "IMAP server did not provide UIDVALIDITY")
	}
	s.noModSeq = data.HighestModSeq == 0
	return data, nil
}
func checkRef(ref mail.MessageRef, epoch uint32) error {
	if ref.Mailbox == "" || ref.UID == 0 || ref.UIDValidity == 0 {
		return mail.NewError("invalid_argument", "A complete message reference is required")
	}
	if ref.UIDValidity != epoch {
		return mail.NewError("stale_reference", "Message reference has stale UIDVALIDITY")
	}
	return nil
}
