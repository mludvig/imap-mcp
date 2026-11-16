//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCLIStartupDovecot(t *testing.T) {
	binary := buildMCPBinary(t)
	t.Run("help-without-config", func(t *testing.T) {
		dir := t.TempDir()
		cmd, cancel := startupCLICommand(t, binary, dir, true, "--help")
		defer cancel()
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("CLI help: %v\n%s", err, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("help polluted MCP stdout: %q", stdout.String())
		}
		for _, flag := range []string{"config", "quiet", "verbose", "debug"} {
			if !strings.Contains(stderr.String(), flag) {
				t.Errorf("help does not describe %s: %s", flag, stderr.String())
			}
		}
	})

	f := startDovecot(t, false)
	for _, test := range []struct {
		name     string
		mode     string
		readonly bool
		home     bool
		relative bool
		flag     string
	}{
		{name: "xdg-default", mode: "tls", readonly: true},
		{name: "home-default", mode: "tls", readonly: true, home: true},
		{name: "explicit-relative-override", mode: "tls", readonly: true, relative: true},
		{name: "verbose-starttls-read-write", mode: "starttls", flag: "--verbose"},
		{name: "debug", mode: "tls", readonly: true, flag: "--debug"},
		{name: "quiet", mode: "tls", readonly: true, flag: "--quiet"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "xdg-config", "imap-mcp", "config.toml")
			if test.home {
				configPath = filepath.Join(dir, "home", ".config", "imap-mcp", "config.toml")
			}
			var args []string
			if test.relative {
				// An explicit path must win even when default discovery finds bad TOML.
				writeStartupConfig(t, configPath, []byte("invalid default configuration"))
				configPath = filepath.Join(dir, "settings", "account.toml")
				args = append(args, "--config", filepath.Join("settings", "account.toml"))
			}
			if test.flag != "" {
				args = append(args, test.flag)
			}
			source, cachePath := mcpFixtureConfig(t, f, test.mode, test.readonly, "Trash")
			body, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			writeStartupConfig(t, configPath, body)
			p := startStartupCLI(t, binary, dir, !test.home, args)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tools, err := p.session.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("initialized CLI tools/list: %v", err)
			}
			wantCount := len(readToolNames)
			if !test.readonly {
				wantCount += len(writeToolNames)
			}
			if len(tools.Tools) != wantCount {
				t.Errorf("tool count = %d, want %d", len(tools.Tools), wantCount)
			}
			// No tool above touches IMAP. The startup probe must already be gone.
			assertStartupConnectionReleased(t, f)
			p.close(t)
			assertStartupProtocolStdout(t, p.stdout.String())
			assertCacheReleased(t, cachePath)
			diagnostics := p.stderr.String()
			if strings.Contains(diagnostics, f.Password) {
				t.Fatal("CLI exposed the configured password on stderr")
			}
			if test.flag == "--quiet" {
				if diagnostics != "" {
					t.Errorf("quiet normal startup wrote stderr: %q", diagnostics)
				}
				return
			}
			for _, want := range []string{configPath, cachePath, "ready"} {
				if !strings.Contains(strings.ToLower(diagnostics), strings.ToLower(want)) {
					t.Errorf("startup stderr missing %q: %s", want, diagnostics)
				}
			}
			mode := "mode: readonly"
			if !test.readonly {
				mode = "mode: write"
			}
			if !strings.Contains(diagnostics, mode) {
				t.Errorf("startup stderr missing %s mode: %s", mode, diagnostics)
			}
			lower := strings.ToLower(diagnostics)
			if !strings.Contains(lower, "check succeeded") {
				t.Errorf("startup stderr missing successful connection check: %s", diagnostics)
			}
			if test.flag == "--verbose" || test.flag == "--debug" {
				for _, want := range []string{"startup started", "shutdown complete"} {
					if !strings.Contains(diagnostics, want) {
						t.Errorf("lifecycle stderr missing %q: %s", want, diagnostics)
					}
				}
			}
			if test.flag == "--debug" && !strings.Contains(diagnostics, "debug: IMAP connection check completed in ") {
				t.Errorf("debug stderr missing startup timing: %s", diagnostics)
			}
		})
	}

	for _, test := range []struct {
		name   string
		mode   string
		quiet  bool
		change func(string) string
		want   string
	}{
		{name: "invalid-auth", mode: "tls", want: "authentication",
			change: func(body string) string {
				return strings.Replace(body, f.Password, "synthetic-invalid-password", 1)
			}},
		{name: "quiet-invalid-auth", mode: "tls", quiet: true, want: "authentication",
			change: func(body string) string {
				return strings.Replace(body, f.Password, "synthetic-invalid-password", 1)
			}},
		{name: "untrusted-tls", mode: "tls", want: "TLS",
			change: func(body string) string {
				return strings.Replace(body, "ca_file = "+quoteStartupPath(f.CAFile)+"\n", "", 1)
			}},
		{name: "untrusted-starttls", mode: "starttls", want: "TLS",
			change: func(body string) string {
				return strings.Replace(body, "ca_file = "+quoteStartupPath(f.CAFile)+"\n", "", 1)
			}},
		{name: "invalid-mode", mode: "tls", want: "configuration",
			change: func(body string) string {
				return strings.Replace(body, `tls = "tls"`, `tls = "plaintext"`, 1)
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			configPath, _ := mcpFixtureConfig(t, f, test.mode, true, "Trash")
			body, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			changed := test.change(string(body))
			if changed == string(body) {
				t.Fatal("failure setup did not change configuration")
			}
			writeFixtureFile(t, configPath, []byte(changed))
			args := []string{"--config", configPath}
			if test.quiet {
				args = append(args, "--quiet")
			}
			assertStartupFailure(t, binary, t.TempDir(), args, test.want, f.Password)
			assertStartupConnectionReleased(t, f)
		})
	}

	t.Run("offline", func(t *testing.T) {
		configPath, _ := mcpFixtureConfig(t, f, "tls", true, "Trash")
		docker(t, "stop", "--time", "1", f.Container)
		assertStartupFailure(t, binary, t.TempDir(), []string{"--config", configPath}, "connect", f.Password)
	})
}

func writeStartupConfig(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, body)
}

func quoteStartupPath(path string) string {
	// ConfigFile writes paths with Go's %q quoting.
	quoted, _ := json.Marshal(path)
	return string(quoted)
}

func startupCLICommand(t *testing.T, binary, dir string, xdg bool, args ...string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "HOME="+filepath.Join(dir, "home"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "xdg-cache"))
	if xdg {
		cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME="+filepath.Join(dir, "xdg-config"))
	}
	return cmd, cancel
}

type startupOutput struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *startupOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *startupOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

type startupCLIProcess struct {
	cmd     *exec.Cmd
	session *mcp.ClientSession
	cancel  context.CancelFunc
	stdout  startupOutput
	stderr  bytes.Buffer
	closed  bool
}

func startStartupCLI(t *testing.T, binary, dir string, xdg bool, args []string) *startupCLIProcess {
	t.Helper()
	cmd, cancel := startupCLICommand(t, binary, dir, xdg, args...)
	p := &startupCLIProcess{cmd: cmd, cancel: cancel}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { p.close(t) })
	client := mcp.NewClient(&mcp.Implementation{Name: "imap-mcp-startup-test", Version: "test"}, nil)
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	p.session, err = client.Connect(ctx, &mcp.IOTransport{
		Reader: io.NopCloser(io.TeeReader(stdout, &p.stdout)),
		Writer: stdin,
	}, nil)
	if err != nil {
		cancel()
		p.close(t)
		t.Fatalf("initialize real CLI: %v\n%s", err, p.stderr.String())
	}
	return p
}

func (p *startupCLIProcess) close(t *testing.T) {
	t.Helper()
	if p.closed {
		return
	}
	p.closed = true
	defer p.cancel()
	if p.session != nil {
		if err := p.session.Close(); err != nil {
			t.Errorf("CLI MCP shutdown: %v", err)
		}
	}
	if err := p.cmd.Wait(); err != nil {
		t.Errorf("CLI process shutdown: %v\n%s", err, p.stderr.String())
	}
}

func assertStartupProtocolStdout(t *testing.T, output string) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(output))
	lines := 0
	for scanner.Scan() {
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil || frame.JSONRPC != "2.0" {
			t.Fatalf("stdout contains non-MCP JSON-RPC: %q (%v)", scanner.Text(), err)
		}
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines == 0 {
		t.Fatal("CLI did not emit MCP JSON-RPC initialization")
	}
}

func assertStartupFailure(t *testing.T, binary, dir string, args []string, want, password string) {
	t.Helper()
	cmd, cancel := startupCLICommand(t, binary, dir, true, args...)
	defer cancel()
	// Supply initialize immediately: a failed probe must prevent even this response.
	cmd.Stdin = strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"clientInfo\":{\"name\":\"startup-test\",\"version\":\"test\"}}}\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("startup exit = %v, want exit 1\nstderr: %s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("failed startup emitted MCP stdout: %q", stdout.String())
	}
	if !strings.Contains(strings.ToLower(stderr.String()), strings.ToLower(want)) {
		t.Errorf("startup failure missing %q diagnostic: %s", want, stderr.String())
	}
	if strings.Contains(strings.ToLower(stderr.String()), "ready") {
		t.Errorf("failed startup claimed readiness: %s", stderr.String())
	}
	for _, secret := range []string{password, "synthetic-invalid-password"} {
		if strings.Contains(stderr.String(), secret) {
			t.Error("startup failure exposed configured credentials on stderr")
		}
	}
}

func assertStartupConnectionReleased(t *testing.T, f *dovecotFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		output, err := exec.CommandContext(ctx, "docker", "exec", f.Container, "doveadm", "-f", "json", "who", f.Username).CombinedOutput()
		if err != nil {
			t.Fatalf("observe startup connections with doveadm: %v\n%s", err, output)
		}
		var sessions []json.RawMessage
		if err := json.Unmarshal(output, &sessions); err != nil {
			t.Fatalf("decode doveadm who: %v: %s", err, output)
		}
		if len(sessions) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("startup left an authenticated IMAP connection: %s", output)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
