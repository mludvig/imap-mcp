package client

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func newTestClient(t *testing.T, cfg *config.Config) *Client {
	t.Helper()
	c := New(cfg)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func commandCount(commands []string, verb string) int {
	count := 0
	for _, command := range commands {
		if strings.HasPrefix(command, verb+" ") {
			count++
		}
	}
	return count
}

func TestRuntimeConnectionReusedAcrossConcurrentOperations(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		switch p.command {
		case "STATUS":
			p.status(7, 1, 0)
		case "LIST":
			p.send("* LIST () \"/\" INBOX\r\n")
			p.ok()
		default:
			return false
		}
		return true
	})
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	const operations = 18
	results := make(chan error, operations)
	var wg sync.WaitGroup
	for i := 0; i < operations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			switch i % 3 {
			case 0:
				_, err = c.Info(context.Background())
			case 1:
				_, err = c.MailboxStatus(context.Background(), "INBOX")
			case 2:
				_, err = c.ListMailboxes(context.Background())
			}
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	commands, connections := peer.snapshot()
	if connections != 1 || commandCount(commands, "LOGIN") != 1 || commandCount(commands, "NOOP") != operations-1 {
		t.Fatalf("connections=%d login=%d noop=%d", connections, commandCount(commands, "LOGIN"), commandCount(commands, "NOOP"))
	}
}

func TestStartupCheckUsesSeparateConnection(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", nil)
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	for _, check := range []bool{true, false, true, false} {
		if check {
			if err := c.CheckConnection(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			requireDisconnected(t, peer)
		} else if _, err := c.Info(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	commands, connections := peer.snapshot()
	if connections != 3 || commandCount(commands, "LOGIN") != 3 || commandCount(commands, "NOOP") != 1 {
		t.Fatalf("connections=%d login=%d noop=%d", connections, commandCount(commands, "LOGIN"), commandCount(commands, "NOOP"))
	}
}

func TestFailedHealthCheckReconnectsWithFreshCapabilities(t *testing.T) {
	for _, failure := range []string{"disconnect", "tagged"} {
		t.Run(failure, func(t *testing.T) {
			var peer *peerServer
			peer = makePeerWithGreeting(t, true, "IMAP4rev1 NAMESPACE ENABLE UTF8=ACCEPT", "OK", func(p *testPeer) bool {
				switch p.command {
				case "NOOP":
					if failure == "disconnect" {
						_ = p.conn.Close()
					} else {
						p.send("%s NO unhealthy\r\n", p.tag)
					}
					return true
				case "CAPABILITY":
					_, connections := peer.snapshot()
					if connections > 1 {
						p.send("* CAPABILITY IMAP4rev1 CONDSTORE\r\n")
						p.ok()
						return true
					}
				}
				return false
			}, func(p *testPeer) bool {
				switch p.command {
				case "NAMESPACE":
					p.send("* NAMESPACE ((\"\" \"/\")) NIL NIL\r\n")
					p.ok()
				case "ENABLE":
					p.send("* ENABLED UTF8=ACCEPT\r\n")
					p.ok()
				default:
					return false
				}
				return true
			})
			c := newTestClient(t, peer.config(t, true, true, "tls"))
			first, err := c.Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			second, err := c.Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !hasCommand(first.Capabilities, "NAMESPACE") || !hasCommand(second.Capabilities, "CONDSTORE") || hasCommand(second.Capabilities, "NAMESPACE") || len(second.EnabledCapabilities) != 0 {
				t.Fatalf("capabilities were not renegotiated: first=%v second=%v enabled=%v", first.Capabilities, second.Capabilities, second.EnabledCapabilities)
			}
			commands, connections := peer.snapshot()
			if connections != 2 || commandCount(commands, "LOGIN") != 2 || commandCount(commands, "NOOP") != 1 || commandCount(commands, "NAMESPACE") != 1 || commandCount(commands, "ENABLE") != 1 {
				t.Fatalf("reconnect commands=%v connections=%d", commands, connections)
			}
		})
	}
}

func TestFailedReconnectIsBounded(t *testing.T) {
	var peer *peerServer
	peer = makePeerWithGreeting(t, true, "IMAP4rev1", "OK", func(p *testPeer) bool {
		if p.command == "NOOP" {
			_ = p.conn.Close()
			return true
		}
		_, connections := peer.snapshot()
		if p.command == "LOGIN" && connections > 1 {
			p.send("%s NO credentials refused\r\n", p.tag)
			return true
		}
		return false
	}, nil)
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Info(context.Background())
	requireCode(t, err, "authentication_failed")
	if _, connections := peer.snapshot(); connections != 2 {
		t.Fatalf("reconnect was retried: connections=%d", connections)
	}
}

func TestIdleExpiryClosesAndReconnects(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", nil)
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	if c.idleTimeout != 10*time.Minute {
		t.Fatalf("production idle timeout=%s", c.idleTimeout)
	}
	c.idleTimeout = 25 * time.Millisecond
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireDisconnected(t, peer)
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, connections := peer.snapshot(); connections != 2 {
		t.Fatalf("idle socket reused: connections=%d", connections)
	}
}

func TestIdleTimerStoppedDuringOperationAndObsoleteTimerIgnored(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		if p.command != "STATUS" {
			return false
		}
		close(entered)
		<-release
		p.status(7, 1, 0)
		return true
	})
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	c.idleTimeout = 100 * time.Millisecond
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	s, epoch := c.runtime, c.idleEpoch
	c.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, err := c.MailboxStatus(context.Background(), "INBOX")
		result <- err
	}()
	<-entered
	c.mu.Lock()
	hasTimer := c.idleTimer != nil
	c.mu.Unlock()
	if hasTimer {
		t.Fatal("active operation retained an idle timer")
	}
	time.Sleep(150 * time.Millisecond)
	select {
	case <-peer.disconnected:
		t.Fatal("idle expiry interrupted an active operation")
	default:
	}
	unblock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	// Model an obsolete timer callback already queued when Stop was called.
	c.expire(s, epoch)
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, connections := peer.snapshot(); connections != 1 {
		t.Fatalf("obsolete timer closed the renewed session: connections=%d", connections)
	}
}

func TestOperationTimeoutIncludesGateWaitWithoutCancellingOwner(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		if p.command != "STATUS" {
			return false
		}
		close(entered)
		<-release
		p.status(7, 1, 0)
		return true
	})
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	result := make(chan error, 1)
	go func() {
		_, err := c.MailboxStatus(context.Background(), "INBOX")
		result <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Info(ctx)
	requireCode(t, err, "timeout")
	if time.Since(start) > time.Second {
		t.Fatal("gate wait ignored the operation deadline")
	}
	select {
	case <-peer.disconnected:
		t.Fatal("waiting caller cancelled the active operation")
	default:
	}
	unblock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands, connections := peer.snapshot()
	if connections != 1 || commandCount(commands, "NOOP") != 1 {
		t.Fatalf("waiting timeout contacted IMAP or invalidated session: commands=%v connections=%d", commands, connections)
	}
}

func TestReleasedOperationDeadlineDoesNotExpireIdleConnection(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", nil)
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	if _, err := c.Info(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(180 * time.Millisecond)
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, connections := peer.snapshot(); connections != 1 {
		t.Fatalf("old cancellation or deadline closed idle transport: connections=%d", connections)
	}
}

func TestAlreadyCancelledCallPreservesIdleConnection(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", nil)
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 20 {
		_, err := c.Info(ctx)
		requireCode(t, err, "cancelled")
	}
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands, connections := peer.snapshot()
	if connections != 1 || commandCount(commands, "NOOP") != 1 {
		t.Fatalf("cancelled caller abandoned the idle transport: commands=%v connections=%d", commands, connections)
	}
}

func TestActiveCancellationDiscardsConnection(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			entered := make(chan struct{})
			peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
				if p.command != "STATUS" {
					return false
				}
				close(entered)
				_, _ = p.reader.ReadByte()
				return true
			})
			c := newTestClient(t, peer.config(t, true, true, "tls"))
			if _, err := c.Info(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := c.MailboxStatus(ctx, "INBOX")
				result <- err
			}()
			<-entered
			code := "timeout"
			if !deadline {
				cancel()
				code = "cancelled"
			}
			requireCode(t, <-result, code)
			requireDisconnected(t, peer)
			if _, err := c.Info(context.Background()); err != nil {
				t.Fatal(err)
			}
			commands, connections := peer.snapshot()
			if connections != 2 || commandCount(commands, "STATUS") != 1 {
				t.Fatalf("cancelled operation replayed or reused: commands=%v connections=%d", commands, connections)
			}
		})
	}
}

func TestCloseInterruptsActiveSetupOperationAndQueuedCalls(t *testing.T) {
	for _, stage := range []string{"LOGIN", "STATUS", "idle"} {
		t.Run(stage, func(t *testing.T) {
			entered := make(chan struct{})
			peer := makePeerWithGreeting(t, true, "IMAP4rev1", "OK", func(p *testPeer) bool {
				if p.command != stage {
					return false
				}
				close(entered)
				_, _ = p.reader.ReadByte()
				return true
			}, nil)
			c := newTestClient(t, peer.config(t, true, true, "tls"))
			active := make(chan error, 1)
			if stage == "idle" {
				if _, err := c.Info(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				go func() {
					var err error
					if stage == "LOGIN" {
						_, err = c.Info(context.Background())
					} else {
						_, err = c.MailboxStatus(context.Background(), "INBOX")
					}
					active <- err
				}()
				<-entered
			}
			queued := make(chan error, 1)
			if stage != "idle" {
				go func() {
					_, err := c.Info(context.Background())
					queued <- err
				}()
			}
			start := time.Now()
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("Close waited for the operation gate")
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			requireDisconnected(t, peer)
			if stage != "idle" {
				select {
				case err := <-active:
					requireCode(t, err, "cancelled")
				case <-time.After(time.Second):
					t.Fatal("Close did not interrupt the active operation")
				}
				select {
				case err := <-queued:
					requireCode(t, err, "connection_failed")
				case <-time.After(time.Second):
					t.Fatal("Close did not unblock the queued operation")
				}
			}
			_, err := c.Info(context.Background())
			requireCode(t, err, "connection_failed")
			if _, connections := peer.snapshot(); connections != 1 {
				t.Fatalf("closed client opened another socket: connections=%d", connections)
			}
		})
	}
}

func TestCloseReleasesRetainedSessionWithDiagnosticLockHeld(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%v", active), func(t *testing.T) {
			entered := make(chan struct{})
			peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
				if p.command != "STATUS" {
					return false
				}
				close(entered)
				_, _ = p.reader.ReadByte()
				return true
			})
			sink, err := diagnostics.New(t.TempDir(), "", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sink.Close() })
			c := newTestClient(t, peer.config(t, true, true, "tls"))
			ctx := diagnostics.WithLogger(context.Background(), sink.Logger())
			if _, err := c.Info(ctx); err != nil {
				t.Fatal(err)
			}
			operation := make(chan error, 1)
			if active {
				go func() { _, err := c.MailboxStatus(ctx, "INBOX"); operation <- err }()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("operation did not reach peer")
				}
			}
			lock, err := os.OpenFile(sink.Path()+".lock", os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lock.Close() })
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			closed := make(chan error, 1)
			go func() { closed <- c.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Client.Close waited on diagnostic I/O")
			}
			requireDisconnected(t, peer)
			if active {
				select {
				case err := <-operation:
					requireCode(t, err, "cancelled")
				case <-time.After(time.Second):
					t.Fatal("active operation did not release its lease")
				}
			}
			if len(c.gate) != 0 {
				t.Fatal("closed client retained the operation gate")
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloseInterruptsIdleExpiryDiagnosticWait(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", nil)
	sink, err := diagnostics.New(t.TempDir(), "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	if _, err := c.Info(diagnostics.WithLogger(context.Background(), sink.Logger())); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(sink.Path()+".lock", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.idleTimer.Stop()
	s, epoch := c.runtime, c.idleEpoch
	c.mu.Unlock()
	expired := make(chan struct{})
	go func() { c.expire(s, epoch); close(expired) }()
	requireDisconnected(t, peer)
	select {
	case <-expired:
		t.Fatal("idle-expiry log did not wait on the held lock")
	case <-time.After(30 * time.Millisecond):
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("client shutdown left the idle-expiry logger blocked")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMailboxSelectionResetsNOMODSEQAndChecksEpochOnReuse(t *testing.T) {
	var mu sync.Mutex
	var selected string
	var statusCommands []string
	peer := makePeer(t, true, "IMAP4rev1 CONDSTORE", func(p *testPeer) bool {
		switch p.command {
		case "EXAMINE":
			selected = strings.Fields(p.args)[0]
			if selected == "INBOX" {
				p.send("* OK [NOMODSEQ] unsupported\r\n")
				p.selectBox(7, 0, 0)
			} else {
				p.selectBox(8, 0, 40)
			}
		case "STATUS":
			mu.Lock()
			statusCommands = append(statusCommands, p.args)
			mu.Unlock()
			name := strings.Fields(p.args)[0]
			modseq := ""
			if strings.Contains(p.args, "HIGHESTMODSEQ") {
				modseq = " HIGHESTMODSEQ 40"
			}
			epoch := 7
			if selected != "INBOX" {
				epoch = 8
			}
			p.send("* STATUS %s (MESSAGES 0 UNSEEN 0 UIDNEXT 100 UIDVALIDITY %d%s)\r\n", name, epoch, modseq)
			p.ok()
		default:
			return false
		}
		return true
	})
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	for _, name := range []string{"INBOX", "Archive", "INBOX"} {
		if _, err := c.Refresh(context.Background(), name, mail.CacheState{}); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	statuses := append([]string(nil), statusCommands...)
	mu.Unlock()
	if len(statuses) != 6 || strings.Contains(statuses[0], "HIGHESTMODSEQ") || !strings.Contains(statuses[2], "HIGHESTMODSEQ") || strings.Contains(statuses[4], "HIGHESTMODSEQ") {
		t.Fatalf("selection-local NOMODSEQ leaked: %v", statuses)
	}
	_, err := c.ReadMessage(context.Background(), mail.ReadRequest{Ref: mail.MessageRef{Mailbox: "Archive", UIDValidity: 7, UID: 5}})
	requireCode(t, err, "stale_reference")
	commands, connections := peer.snapshot()
	if connections != 1 || commandCount(commands, "EXAMINE") != 4 || hasCommand(commands, "UID FETCH") {
		t.Fatalf("mailbox not reselected or stale reference fetched: commands=%v connections=%d", commands, connections)
	}
}

func TestTaggedWriteFailureDiscardedWithoutReplay(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1", func(p *testPeer) bool {
		switch p.command {
		case "SELECT":
			p.selectBox(7, 1, 0)
		case "UID":
			if strings.HasPrefix(p.args, "FETCH") {
				p.send("* 1 FETCH (UID 5)\r\n")
				p.ok()
			} else {
				p.send("%s NO write refused\r\n", p.tag)
			}
		default:
			return false
		}
		return true
	})
	c := newTestClient(t, peer.config(t, false, true, "tls"))
	_, err := c.AddFlags(context.Background(), mail.FlagRequest{Refs: []mail.MessageRef{{Mailbox: "INBOX", UIDValidity: 7, UID: 5}}, Flags: []string{"\\Seen"}})
	if typed := requireCode(t, err, "server_error"); typed.OutcomeUnknown {
		t.Fatal("tagged rejection reported an unknown write outcome")
	}
	requireDisconnected(t, peer)
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands, connections := peer.snapshot()
	stores := 0
	for _, cmd := range commands {
		if strings.Contains(cmd, "STORE") {
			stores++
		}
	}
	if connections != 2 || stores != 1 {
		t.Fatalf("write replayed or failed transport reused: connections=%d stores=%d", connections, stores)
	}
}

func TestClientDiagnosticsReportSetupReuseFetchAndSynchronization(t *testing.T) {
	peer := makePeer(t, true, "IMAP4rev1 CONDSTORE", func(p *testPeer) bool {
		switch p.command {
		case "EXAMINE":
			p.selectBox(7, 1, 20)
		case "FETCH":
			p.send("* 1 FETCH (UID 5)\r\n")
			p.ok()
		case "UID":
			p.send("* 1 FETCH (UID 5 FLAGS () MODSEQ (20))\r\n")
			p.ok()
		case "STATUS":
			p.status(7, 1, 20)
		default:
			return false
		}
		return true
	})
	sink, err := diagnostics.New(t.TempDir(), "fixture-secret", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	var stages []string
	ctx := diagnostics.WithProgress(diagnostics.WithLogger(context.Background(), sink.Logger()), func(stage string) {
		stages = append(stages, stage)
	})
	c := newTestClient(t, peer.config(t, true, true, "tls"))
	if _, err := c.Refresh(ctx, "INBOX", mail.CacheState{Present: true, UIDValidity: 7, HighestModSeq: 10, CachedUIDs: []uint32{5}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Info(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.idleTimer.Stop()
	s, epoch := c.runtime, c.idleEpoch
	c.mu.Unlock()
	c.expire(s, epoch)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"IMAP connection setup"`, `"IMAP authenticated"`, `"IMAP connection reused"`, `"IMAP connection closed"`, `"IMAP fetch"`, `"IMAP mailbox synchronization"`, `"username":"testuser"`, `"host":"127.0.0.1"`} {
		if !strings.Contains(string(data), fragment) {
			t.Fatalf("diagnostic record missing %s", fragment)
		}
	}
	if strings.Contains(string(data), "fixture-secret") || strings.Contains(string(data), "AUTHENTICATE") || strings.Contains(string(data), "LOGIN") {
		t.Fatal("authentication payload appeared in diagnostics")
	}
	for _, stage := range []string{"Connecting to IMAP server", "Authenticating to IMAP server", "Synchronizing mailbox", "Fetching IMAP messages", "Checking existing IMAP connection"} {
		if !hasCommand(stages, stage) {
			t.Fatalf("progress missing %q", stage)
		}
	}
}
