//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCLIFileLoggingAndProgressDovecot(t *testing.T) {
	binary := buildMCPBinary(t)
	f := startDovecot(t, false)
	for _, mode := range []string{"--quiet", "--debug"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			source, _ := startupTransportConfig(t, f, "tls", true)
			body, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			writeStartupConfig(t, filepath.Join(dir, "xdg-config", "imap-mcp", "config.toml"), body)
			p := startStartupCLI(t, binary, dir, true, []string{mode})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			params := &mcp.CallToolParams{Name: "search_messages", Arguments: map[string]any{"limit": 1}}
			params.SetProgressToken("stdio-search")
			for _, call := range []*mcp.CallToolParams{
				params,
				{Name: "search_messages", Arguments: map[string]any{"limit": 1}},
			} {
				result, err := p.session.CallTool(ctx, call)
				if err != nil || result.IsError {
					t.Fatalf("search result=%+v error=%v", result, err)
				}
			}
			result, err := p.session.CallTool(ctx, &mcp.CallToolParams{
				Name: "mailbox_status", Arguments: map[string]any{"mailbox": "Missing-fixture-mailbox"},
			})
			if err != nil || !result.IsError {
				t.Fatalf("missing mailbox result=%+v error=%v", result, err)
			}
			p.close(t)
			assertStartupProtocolStdout(t, p.stdout.String())
			progress := 0
			stages := make(map[string]int)
			for _, line := range strings.Split(strings.TrimSpace(p.stdout.String()), "\n") {
				var frame struct {
					Method string `json:"method"`
					Params struct {
						Token    string   `json:"progressToken"`
						Progress float64  `json:"progress"`
						Total    *float64 `json:"total"`
						Message  string   `json:"message"`
					} `json:"params"`
				}
				if err := json.Unmarshal([]byte(line), &frame); err != nil {
					t.Fatal(err)
				}
				if frame.Method != "notifications/progress" {
					continue
				}
				progress++
				if frame.Params.Token != "stdio-search" || frame.Params.Progress != float64(progress) ||
					frame.Params.Total != nil || frame.Params.Message == "" {
					t.Fatalf("invalid progress notification: %s", line)
				}
				stages[frame.Params.Message]++
			}
			for _, stage := range []string{
				"Starting search_messages", "Synchronizing mailbox headers; first synchronization may take longer",
				"Applying refreshed mailbox headers", "Searching local mailbox headers", "Finished search_messages",
			} {
				if stages[stage] != 1 {
					t.Errorf("tokened call must emit %q once, got %d", stage, stages[stage])
				}
			}
			log, err := os.ReadFile(filepath.Join(dir, "xdg-cache", "imap-mcp", "imap-mcp.log"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(log), f.Password) || strings.Contains(p.stderr.String(), f.Password) {
				t.Fatal("diagnostics exposed the configured password")
			}
			completed, failures, debugRecords := 0, 0, 0
			for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				stamp, ok := record["time"].(string)
				if !ok || len(stamp) < 6 || stamp[len(stamp)-6] != '+' && stamp[len(stamp)-6] != '-' {
					t.Fatalf("timestamp lacks numeric local offset: %s", line)
				}
				if _, err := time.Parse(time.RFC3339, stamp); err != nil {
					t.Fatal(err)
				}
				if record["level"] == "DEBUG" {
					debugRecords++
				}
				if record["msg"] == "IMAP command failed" {
					failures++
					cause, ok := record["error"].(string)
					if record["level"] != "WARN" || !ok || !strings.Contains(cause, "Missing-fixture-mailbox") {
						t.Fatalf("underlying command failure was not retained: %s", line)
					}
				}
				if record["msg"] == "tool completed" {
					completed++
					if record["level"] != "INFO" || record["tool"] == nil || record["duration"].(float64) < 0 {
						t.Fatalf("incomplete tool timing: %s", line)
					}
				}
			}
			if completed != 3 || failures != 1 || (mode == "--quiet" && debugRecords != 0) ||
				(mode == "--debug" && debugRecords == 0) {
				t.Fatalf("file logging mode=%s completed=%d failures=%d debug=%d", mode, completed, failures, debugRecords)
			}
			if mode == "--quiet" && p.stderr.String() != "" {
				t.Fatalf("quiet tool diagnostics wrote stderr: %s", p.stderr.String())
			}
			if mode == "--debug" && !strings.Contains(p.stderr.String(), "debug: Synchronizing mailbox headers") {
				t.Fatalf("debug stderr omitted search stages: %s", p.stderr.String())
			}
		})
	}
}
