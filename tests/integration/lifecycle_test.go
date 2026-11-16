//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/client"
	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestMCPRuntimeConnectionDovecot(t *testing.T) {
	binary := buildMCPBinary(t)
	for _, rev2 := range []bool{false, true} {
		name := "rev1"
		if rev2 {
			name = "rev2"
		}
		t.Run(name, func(t *testing.T) {
			f := startDovecot(t, rev2)
			before := f.Snapshot(t, "INBOX")
			configFile, _ := mcpFixtureConfig(t, f, "tls", true, "Trash")
			p := startMCP(t, binary, configFile)
			assertStartupConnectionReleased(t, f)
			page := mcpData[mail.MessagePage](t, p, "list_messages", map[string]any{"limit": 200})
			first := assertRuntimeConnection(t, f)
			var unseen mail.MessageRef
			for _, message := range page.Messages {
				if message.Ref.UID == uint32(f.UIDs["unseen"]) {
					unseen = message.Ref
				}
			}
			if unseen.UID == 0 {
				t.Fatal("unseen fixture reference missing")
			}
			for i := 0; i < 3; i++ {
				mcpData[mail.Message](t, p, "read_message", map[string]any{"ref": unseen})
				mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{"body": "Fixture", "limit": 1})
				if current := assertRuntimeConnection(t, f); !reflect.DeepEqual(first, current) {
					t.Fatalf("repeated reads/searches replaced runtime session: before=%s after=%s", first, current)
				}
			}
			// Force only this synthetic account's idle connection stale.
			docker(t, "exec", f.Container, "doveadm", "kick", f.Username)
			assertStartupConnectionReleased(t, f)
			mcpData[mail.Message](t, p, "read_message", map[string]any{"ref": unseen})
			if current := assertRuntimeConnection(t, f); reflect.DeepEqual(first, current) {
				t.Fatal("stale-session recovery did not establish a new runtime connection")
			}
			p.close(t)
			assertStartupConnectionReleased(t, f)
			if after := f.Snapshot(t, "INBOX"); !reflect.DeepEqual(before, after) {
				t.Fatal("runtime reuse or recovery changed membership, bytes, flags, or dates")
			}
		})
	}
}

func assertRuntimeConnection(t *testing.T, f *dovecotFixture) json.RawMessage {
	t.Helper()
	output := docker(t, "exec", f.Container, "doveadm", "-f", "json", "who", "-1", f.Username)
	var sessions []json.RawMessage
	if err := json.Unmarshal([]byte(output), &sessions); err != nil {
		t.Fatalf("decode runtime connections: %v: %s", err, output)
	}
	if len(sessions) != 1 {
		t.Fatalf("runtime must retain exactly one connection: %s", output)
	}
	return sessions[0]
}

func TestDovecotLostAppendResponseIsNotReplayed(t *testing.T) {
	f := startDovecot(t, true)
	address, completions := droppedAppendResponseProxy(t, f)
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFixtureFile(t, path, []byte(fmt.Sprintf("readonly = false\n[imap]\nhost = %q\nport = %s\ntls = \"none\"\nusername = %q\npassword = %q\nconnect_timeout_seconds = 3\noperation_timeout_seconds = 10\n",
		host, port, f.Username, f.Password)))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	backend := client.New(cfg)
	t.Cleanup(func() { _ = backend.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := backend.Info(ctx); err != nil {
		t.Fatal(err)
	}
	raw := "From: loss@example.test\r\nSubject: Response-loss append\r\nMessage-ID: <response-loss@fixture.test>\r\n\r\nWritten once.\r\n"
	_, err = backend.AppendMessage(ctx, mail.AppendRequest{Mailbox: "INBOX", MessageBase64: base64.StdEncoding.EncodeToString([]byte(raw))})
	var domain *mail.Error
	if !errors.As(err, &domain) || !domain.OutcomeUnknown || completions.Load() != 1 {
		t.Fatalf("lost committed APPEND must be uncertain: error=%v completions=%d", err, completions.Load())
	}
	if _, err := backend.Info(ctx); err != nil {
		t.Fatalf("later operation failed to reconnect: %v", err)
	}
	count := 0
	for _, message := range f.Snapshot(t, "INBOX") {
		if strings.Contains(message.Raw, "Message-ID: <response-loss@fixture.test>") {
			count++
			if message.Raw != raw {
				t.Fatal("committed APPEND bytes changed")
			}
		}
	}
	if count != 1 || completions.Load() != 1 {
		t.Fatalf("APPEND replayed or missing: copies=%d completions=%d", count, completions.Load())
	}
}

// The localhost-only proxy loses a successful response after real Dovecot has
// committed the write. Its upstream TLS verifies the fixture's private CA.
func droppedAppendResponseProxy(t *testing.T, f *dovecotFixture) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tlsConfig := f.TLSConfig(t)
	completions := new(atomic.Int32)
	done := make(chan struct{})
	var handlers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-done
		handlers.Wait()
	})
	go func() {
		defer close(done)
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer downstream.Close()
				upstream, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", f.TLSAddress, tlsConfig)
				if err != nil {
					return
				}
				defer upstream.Close()
				stop := context.AfterFunc(ctx, func() {
					_ = downstream.Close()
					_ = upstream.Close()
				})
				defer stop()
				copied := make(chan struct{})
				go func() {
					defer close(copied)
					_, _ = io.Copy(upstream, downstream)
					_ = upstream.Close()
				}()
				reader := bufio.NewReader(upstream)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						break
					}
					if strings.Contains(line, " OK [APPENDUID ") && completions.Add(1) == 1 {
						break
					}
					if _, err := io.WriteString(downstream, line); err != nil {
						break
					}
				}
				_ = downstream.Close()
				_ = upstream.Close()
				<-copied
			}()
		}
	}()
	return listener.Addr().String(), completions
}
