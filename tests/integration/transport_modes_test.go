//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mludvig/imap-mcp/internal/mail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCLIPlaintextDovecot(t *testing.T) {
	binary := buildMCPBinary(t)
	f := startPlaintextDovecot(t)
	assertFixtureAdvertisesSTARTTLS(t, f)
	for _, test := range []struct {
		name  string
		mode  string
		quiet bool
	}{
		{name: "none-despite-starttls", mode: "none"},
		{name: "auto-upgrades-starttls", mode: "auto"},
		{name: "quiet-none", mode: "none", quiet: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			configPath, cachePath := startupTransportConfig(t, f, test.mode, true)
			args := []string{"--config", configPath}
			if test.quiet {
				args = append(args, "--quiet")
			}
			p := startStartupCLI(t, binary, t.TempDir(), true, args)
			assertStartupConnectionReleased(t, f)
			assertStartupMailboxStatus(t, p, f)
			assertRuntimeConnection(t, f)
			p.close(t)
			assertStartupConnectionReleased(t, f)
			assertStartupProtocolStdout(t, p.stdout.String())
			assertCacheReleased(t, cachePath)
			diagnostics := p.stderr.String()
			if strings.Contains(diagnostics, f.Password) {
				t.Fatal("CLI exposed the configured password on stderr")
			}
			if test.quiet {
				if diagnostics != "" {
					t.Errorf("quiet plaintext startup wrote stderr: %q", diagnostics)
				}
				return
			}
			want := "transport: plaintext"
			if test.mode == "auto" {
				want = "transport: STARTTLS (verified)"
			}
			assertStartupTransport(t, diagnostics, want)
		})
	}
}

func startupTransportConfig(t *testing.T, f *dovecotFixture, mode string, readonly bool) (string, string) {
	t.Helper()
	if mode == "tls" || mode == "starttls" {
		return mcpFixtureConfig(t, f, mode, readonly, "Trash")
	}
	// Auto and none both start on the fixture's dynamically assigned plain port.
	path, cachePath := mcpFixtureConfig(t, f, "starttls", readonly, "Trash")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := ""
	if mode != "" {
		line = fmt.Sprintf("tls = %q\n", mode)
	}
	writeFixtureFile(t, path, []byte(strings.Replace(string(body), "tls = \"starttls\"\n", line, 1)))
	return path, cachePath
}

func assertFixtureAdvertisesSTARTTLS(t *testing.T, f *dovecotFixture) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.STARTTLSAddress, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	c := imapclient.New(conn, nil)
	defer c.Close()
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if !c.Caps().Has(imap.CapStartTLS) {
		t.Fatal("plaintext fixture must advertise STARTTLS to distinguish auto from none")
	}
}

func assertStartupMailboxStatus(t *testing.T, p *startupCLIProcess, f *dovecotFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := p.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "mailbox_status", Arguments: map[string]any{"mailbox": "INBOX"},
	})
	if err != nil {
		t.Fatalf("mailbox_status transport/protocol error: %v", err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data  mail.MailboxStatus `json:"data"`
		Error *mail.Error        `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || result.IsError || envelope.Error != nil {
		t.Fatalf("mailbox_status failed: %s (%v)", data, err)
	}
	wantMessages := 0
	for _, seed := range f.Seeds {
		if seed.Mailbox == "INBOX" {
			wantMessages++
		}
	}
	if envelope.Data.Mailbox != "INBOX" || int(envelope.Data.Messages) != wantMessages || envelope.Data.UIDValidity == 0 {
		t.Fatalf("unexpected backend INBOX status: %+v, want %d messages", envelope.Data, wantMessages)
	}
}

func assertStartupTransport(t *testing.T, diagnostics, want string) {
	t.Helper()
	if strings.Count(diagnostics, "transport: ") != 1 || !strings.Contains(diagnostics, want) {
		t.Errorf("startup transport diagnostic should be %q exactly once: %s", want, diagnostics)
	}
	transportAt := strings.Index(diagnostics, want)
	successAt := strings.Index(diagnostics, "check succeeded")
	readyAt := strings.Index(diagnostics, "ready:")
	if transportAt < 0 || successAt < transportAt || readyAt < successAt {
		t.Errorf("startup transport, success and readiness diagnostics out of order: %s", diagnostics)
	}
}
