package client

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func clientClosedError() error {
	return mail.NewError("connection_failed", "IMAP client is closed")
}

func (s *session) attach(ctx context.Context) {
	s.ctx = ctx
	s.noModSeq = false
	s.stopped = make(chan struct{})
	stopped := s.stopped
	raw := s.raw
	s.stop = context.AfterFunc(ctx, func() {
		_ = raw.Close()
		close(stopped)
	})
	deadline, _ := ctx.Deadline()
	if err := s.conn.operationDeadline(deadline); err != nil {
		s.invalid = true
	}
}

func (s *session) stopCancellation() {
	if s.stop != nil {
		// A callback that has started must finish before another lease can use
		// the socket; see docs/concepts/imap-safety.md.
		if !s.stop() {
			<-s.stopped
		}
		s.stop = nil
	}
}

func (c *Client) open(ctx context.Context) (*session, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.IMAP().OperationTimeout())
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return nil, safeError(ctx, ctx.Err(), "server_error", false)
	case <-c.done:
		cancel()
		return nil, clientClosedError()
	}
	ready := false
	active := false
	defer func() {
		if !ready {
			if active {
				c.mu.Lock()
				c.runtime = nil
				c.active = false
				c.activeCancel = nil
				c.mu.Unlock()
			}
			cancel()
			<-c.gate
		}
	}()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, clientClosedError()
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, safeError(ctx, err, "server_error", false)
	}
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
	c.idleEpoch++
	s := c.runtime
	c.active = true
	c.activeCancel = cancel
	active = true
	c.mu.Unlock()
	if s != nil {
		diagnostics.Stage(ctx, "Checking existing IMAP connection")
		finish := diagnostics.Measure(ctx, "IMAP connection health check")
		healthCtx, healthCancel := context.WithTimeout(ctx, c.cfg.IMAP().ConnectTimeout())
		s.attach(healthCtx)
		err := s.c.Noop().Wait()
		s.stopCancellation()
		if err == nil {
			err = healthCtx.Err()
		}
		healthCancel()
		finish()
		if err != nil || s.invalid {
			s.teardown()
			diagnostics.Logger(ctx).InfoContext(ctx, "IMAP connection closed", "reason", "failed health check", "error", err)
			s = nil
		} else {
			s.attach(ctx)
			diagnostics.Logger(ctx).InfoContext(ctx, "IMAP connection reused", "host", c.cfg.IMAP().Host())
		}
	}
	if err := ctx.Err(); err != nil {
		if s != nil {
			s.teardown()
		}
		return nil, safeError(ctx, err, "server_error", false)
	}
	if s == nil {
		var err error
		s, err = c.dial(ctx, nil, true)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		s.teardown()
		return nil, safeError(ctx, err, "server_error", false)
	}
	s.cancel = cancel
	ready = true
	return s, nil
}

func (s *session) close() {
	if s.owner == nil {
		s.teardown()
		return
	}
	s.stopCancellation()
	discard := s.invalid || s.ctx.Err() != nil
	select {
	case <-s.c.Closed():
		discard = true
	default:
	}
	if !discard {
		discard = s.conn.operationDeadline(time.Time{}) != nil
	}
	s.cancel()
	s.cancel = nil
	c := s.owner
	c.mu.Lock()
	discard = discard || c.closed
	c.active = false
	c.activeCancel = nil
	if discard {
		c.runtime = nil
	} else {
		c.idleEpoch++
		epoch := c.idleEpoch
		c.idleTimer = time.AfterFunc(c.idleTimeout, func() { c.expire(s, epoch) })
	}
	c.mu.Unlock()
	if discard {
		s.teardown()
		diagnostics.Logger(s.ctx).InfoContext(s.ctx, "IMAP connection closed", "reason", "operation ended with unusable connection")
	}
	<-c.gate
}

func (c *Client) expire(s *session, epoch uint64) {
	c.mu.Lock()
	// Generation checks reject queued callbacks from a previous idle period.
	// See docs/concepts/imap-safety.md.
	if c.closed || c.active || c.runtime != s || c.idleEpoch != epoch {
		c.mu.Unlock()
		return
	}
	c.runtime = nil
	c.idleTimer = nil
	// Close before unlocking so a new operation cannot borrow this socket.
	_ = s.raw.Close()
	c.mu.Unlock()
	s.teardown()
	s.logger.InfoContext(c.logCtx, "IMAP connection closed", "reason", "idle timeout")
}

// Close interrupts active I/O immediately and permanently prevents new leases.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	close(c.done)
	c.logCancel()
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
	if c.activeCancel != nil {
		c.activeCancel()
	}
	if s := c.runtime; s != nil {
		c.closeErr = s.raw.Close()
		if errors.Is(c.closeErr, net.ErrClosed) {
			c.closeErr = nil
		}
	}
	// Close must never wait for diagnostic I/O; see docs/adr/0010-process-diagnostics-and-progress.md.
	return c.closeErr
}
