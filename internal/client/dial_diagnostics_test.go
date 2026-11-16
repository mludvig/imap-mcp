package client

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mludvig/imap-mcp/internal/diagnostics"
)

func TestDialLogsUnderlyingFailureBeforeSanitizing(t *testing.T) {
	for _, tc := range []struct {
		name, mode, stage, code, cause string
		implicit, trust                bool
	}{
		{"tcp", "none", "tcp dial", "connection_failed", "dial tcp", false, true},
		{"tls", "tls", "transport setup", "tls_failed", "x509: certificate signed by unknown authority", true, false},
		{"starttls", "starttls", "transport setup", "tls_failed", "transport rejected", false, true},
		{"auto-starttls", "auto", "starttls", "tls_failed", "transport rejected", false, true},
		{"greeting", "none", "greeting", "connection_failed", "greeting rejected", false, true},
		{"login", "tls", "authentication", "authentication_failed", "authentication rejected", true, true},
		{"plain", "tls", "authentication", "authentication_failed", "authentication rejected", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps, greeting := "IMAP4rev1 STARTTLS", "OK"
			if tc.name == "plain" {
				caps += " AUTH=PLAIN LOGINDISABLED"
			}
			if tc.name == "greeting" {
				caps = ""
				greeting = "BYE fixture-secret testuser private-host greeting rejected"
			}
			peer := makePeerWithGreeting(t, tc.implicit, caps, greeting, func(p *testPeer) bool {
				switch p.command {
				case "STARTTLS":
					p.send("%s NO fixture-secret testuser private-host transport rejected\r\n", p.tag)
				case "AUTHENTICATE":
					p.send("+ \r\n")
					if _, err := p.reader.ReadString('\n'); err != nil {
						return true
					}
					p.send("%s NO fixture-secret testuser private-host authentication rejected\r\n", p.tag)
				case "LOGIN":
					p.send("%s NO fixture-secret testuser private-host authentication rejected\r\n", p.tag)
				default:
					return false
				}
				return true
			}, nil)
			cfg := peer.config(t, true, tc.trust, tc.mode)
			if tc.name == "tcp" {
				if err := peer.listener.Close(); err != nil {
					t.Fatal(err)
				}
			}
			sink, err := diagnostics.New(t.TempDir(), cfg.IMAP().Password(), false, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sink.Close() })
			ctx := diagnostics.WithLogger(context.Background(), sink.Logger())
			err = newTestClient(t, cfg).CheckConnection(ctx, nil)
			public := requireCode(t, err, tc.code)
			if strings.Contains(public.Message, "private-host") || strings.Contains(public.Message, "testuser") || strings.Contains(public.Message, "rejected") {
				t.Fatalf("underlying response exposed publicly: %q", public.Message)
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(sink.Path())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), cfg.IMAP().Password()) {
				t.Fatal("password exposed in diagnostics")
			}
			failures := 0
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["level"] != "ERROR" {
					continue
				}
				failures++
				cause, ok := record["error"].(string)
				if !ok || !strings.Contains(cause, tc.cause) {
					t.Fatalf("underlying cause missing: %+v", record)
				}
				if record["stage"] != tc.stage || record["host"] != cfg.IMAP().Host() || record["username"] != cfg.IMAP().Username() || record["port"] != float64(cfg.IMAP().Port()) {
					t.Fatalf("connection context missing: %+v", record)
				}
				if tc.name != "tcp" && tc.name != "tls" && (!strings.Contains(cause, "private-host") || !strings.Contains(cause, "testuser") || !strings.Contains(cause, "[REDACTED]")) {
					t.Fatalf("response context was lost during redaction: %+v", record)
				}
			}
			if failures != 1 {
				t.Fatalf("error records=%d, want one: %s", failures, data)
			}
		})
	}
}
