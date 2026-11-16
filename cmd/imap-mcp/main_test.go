package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/config"
	diagnosticlog "github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
	"github.com/mludvig/imap-mcp/internal/server"
	"github.com/mludvig/imap-mcp/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Every CLI invocation must resolve user files inside its test's temporary tree.
func isolateCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	return dir
}

func TestArgumentErrors(t *testing.T) {
	isolateCLI(t)
	for _, args := range [][]string{
		{"--config", ""},
		{"--config="},
		{"--config"},
		{"--config", "/missing.toml", "extra"},
		{"extra"},
		{"--unknown"},
		{"--quiet", "--verbose"},
		{"--quiet", "--debug"},
		{"--verbose", "--debug"},
	} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, &stderr); code != 2 || !strings.Contains(stderr.String(), "Usage:") {
			t.Fatalf("args %v: code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestHelpIsUsefulAndOffline(t *testing.T) {
	dir := isolateCLI(t)
	for _, args := range [][]string{{"-h"}, {"--help"}, {"--quiet", "--help"}, {"--config", "/missing.toml", "--help"}} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, &stderr); code != 0 {
			t.Fatalf("args %v: help code = %d", args, code)
		}
		for _, want := range []string{"Usage:", "--config", "--quiet", "--verbose", "--debug", "--help", "XDG_CONFIG_HOME", "HOME/.config", "relative", "stderr", "connection", "XDG_CACHE_HOME", "50 MiB", "password", "file logging continues"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("help missing %q: %s", want, stderr.String())
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("help created user files")
	}
}

func TestConfigurationErrorsDoNotExposeCredentials(t *testing.T) {
	isolateCLI(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "private-config.toml")
	for _, contents := range []string{
		"[imap]\npassword = \"super-secret",
		"[imap]\nhost = \"example.test\"\nusername = \"test\"\npassword = \"super-secret\"\n[cache]\npath = \"/dev/null/secret-database\"\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"--quiet", "--verbose", "--debug"} {
			var stderr bytes.Buffer
			code := runWithHooks(context.Background(), []string{"--config", path, mode}, &stderr,
				func(context.Context, *config.Config, func(string)) error {
					t.Fatal("invalid configuration/cache reached probe")
					return nil
				}, noServe)
			if code != 1 {
				t.Fatalf("code = %d", code)
			}
			for _, secret := range []string{"super-secret", "private-config", "secret-database", "ready:"} {
				if strings.Contains(stderr.String(), secret) {
					t.Fatalf("error disclosed %q: %s", secret, stderr.String())
				}
			}
			if mode == "--quiet" && strings.Count(stderr.String(), "\n") != 1 {
				t.Fatalf("quiet must retain only the error: %s", stderr.String())
			}
		}
	}
}

const testCredentials = "[imap]\nhost = \"secret-host.example.test\"\nusername = \"private-user\"\npassword = \"super-secret\"\n"

func writeCLIConfig(t *testing.T, path, policy, cachePath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	content := policy + testCredentials + "\n[cache]\npath = " + fmt.Sprintf("%q", cachePath) + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func noProbe(_ context.Context, _ *config.Config, reportTransport func(string)) error {
	reportTransport("tls")
	return nil
}
func noServe(context.Context, *service.Service) error { return nil }

func assertCacheReleased(t *testing.T, path string) {
	t.Helper()
	store, err := cache.Open(path)
	if err != nil {
		t.Fatalf("cache not released: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDiscovery(t *testing.T) {
	isolateCLI(t)
	if runtime.GOOS != "linux" {
		t.Skip("Linux XDG discovery")
	}
	for _, useXDG := range []bool{true, false} {
		t.Run(fmt.Sprintf("XDG=%v", useXDG), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", filepath.Join(dir, "home"))
			t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
			configDir := filepath.Join(dir, "home", ".config")
			t.Setenv("XDG_CONFIG_HOME", "")
			if useXDG {
				configDir = filepath.Join(dir, "xdg-config")
				t.Setenv("XDG_CONFIG_HOME", configDir)
			}
			path := filepath.Join(configDir, "imap-mcp", "config.toml")
			writeCLIConfig(t, path, "", "")
			wantCache := filepath.Join(dir, "cache", "imap-mcp", "headers.sqlite3")
			probed := false
			var stderr bytes.Buffer
			code := runWithHooks(context.Background(), nil, &stderr,
				func(_ context.Context, cfg *config.Config, reportTransport func(string)) error {
					probed = true
					if cfg.CachePath() != wantCache || cfg.IMAP().Password() != "super-secret" {
						t.Fatalf("unexpected discovered configuration: cache=%q", cfg.CachePath())
					}
					reportTransport("tls")
					return nil
				}, noServe)
			if code != 0 || !probed || !strings.Contains(stderr.String(), fmt.Sprintf("%q", path)) || !strings.Contains(stderr.String(), fmt.Sprintf("%q", wantCache)) {
				t.Fatalf("code=%d probed=%v stderr=%q", code, probed, stderr.String())
			}
			assertCacheReleased(t, wantCache)
		})
	}
}

func TestExplicitConfigOverridesDiscoveryAndUsesWorkingDirectory(t *testing.T) {
	isolateCLI(t)
	dir := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Chdir(dir)
	path := filepath.Join(dir, "relative", "config.toml")
	writeCLIConfig(t, path, "readonly = false\n", "cache/headers.sqlite3")
	wantCache := filepath.Join(dir, "relative", "cache", "headers.sqlite3")
	var stderr bytes.Buffer
	served := false
	code := runWithHooks(context.Background(), []string{"--config", "relative/config.toml"}, &stderr,
		func(_ context.Context, cfg *config.Config, reportTransport func(string)) error {
			if cfg.CachePath() != wantCache || cfg.Readonly() {
				t.Fatalf("explicit configuration not loaded: cache=%q readonly=%v", cfg.CachePath(), cfg.Readonly())
			}
			reportTransport("tls")
			return nil
		},
		func(_ context.Context, svc *service.Service) error {
			served = true
			if svc.Readonly() {
				t.Fatal("server must use loaded write mode")
			}
			return nil
		})
	if code != 0 || !served || !strings.Contains(stderr.String(), fmt.Sprintf("%q", path)) || !strings.Contains(stderr.String(), "mode: write") {
		t.Fatalf("code=%d served=%v stderr=%q", code, served, stderr.String())
	}
	assertCacheReleased(t, wantCache)
}

func TestUnavailableDefaultConfigIsRuntimeError(t *testing.T) {
	isolateCLI(t)
	if runtime.GOOS != "linux" {
		t.Skip("Linux XDG discovery")
	}
	for _, missingEnvironment := range []bool{false, true} {
		t.Run(fmt.Sprintf("missingEnvironment=%v", missingEnvironment), func(t *testing.T) {
			t.Setenv("HOME", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			if !missingEnvironment {
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "private-missing-config"))
			}
			var stderr bytes.Buffer
			if code := run(context.Background(), nil, &stderr); code != 1 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if strings.Contains(stderr.String(), "private-missing-config") {
				t.Fatalf("disclosed failed config path: %s", stderr.String())
			}
		})
	}
}

func TestStartupVerbosityAndStageOrdering(t *testing.T) {
	isolateCLI(t)
	for _, mode := range []string{"", "--quiet", "--verbose", "--debug", "--verbose=false", "--quiet=false"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			cachePath := filepath.Join(dir, "headers.sqlite3")
			writeCLIConfig(t, path, "", cachePath)
			args := []string{"--config", path}
			if mode != "" {
				args = append(args, mode)
			}
			var stderr bytes.Buffer
			probed, served := false, false
			code := runWithHooks(context.Background(), args, &stderr,
				func(ctx context.Context, cfg *config.Config, reportTransport func(string)) error {
					probed = true
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > cfg.IMAP().OperationTimeout() {
						t.Fatal("probe context must be bounded by configured operation timeout")
					}
					if mode != "--quiet" && (!strings.Contains(stderr.String(), "opening header cache") || strings.Contains(stderr.String(), "ready:")) {
						t.Fatalf("probe stage order: %s", stderr.String())
					}
					if store, err := cache.Open(cachePath); err == nil {
						store.Close()
						t.Fatal("cache lock must be held during probe")
					}
					reportTransport("tls")
					return nil
				},
				func(_ context.Context, _ *service.Service) error {
					served = true
					if !probed {
						t.Fatal("server started before connection check")
					}
					if mode != "--quiet" && !strings.Contains(stderr.String(), "connection closed") {
						t.Fatal("connection check result must precede serving")
					}
					return nil
				})
			if code != 0 || !probed || !served {
				t.Fatalf("code=%d probed=%v served=%v stderr=%q", code, probed, served, stderr.String())
			}
			output := stderr.String()
			if mode == "--quiet" {
				if output != "" {
					t.Fatalf("quiet output: %q", output)
				}
			} else {
				previous := -1
				for _, stage := range []string{"loading configuration", "opening header cache", "configuration:", "header cache:", "mode: readonly", "checking IMAP connection", "transport: implicit TLS (verified)", "connection closed", "ready:"} {
					index := strings.Index(output, stage)
					if index <= previous {
						t.Fatalf("wrong order for %q: %s", stage, output)
					}
					previous = index
				}
			}
			lifecycle := mode == "--verbose" || mode == "--debug"
			for _, stage := range []string{"startup started", "configuration loaded", "header cache opened", "starting MCP server", "MCP server stopped", "closing header cache", "shutdown complete"} {
				if strings.Contains(output, stage) != lifecycle {
					t.Fatalf("unexpected lifecycle %q in %s: %s", stage, mode, output)
				}
			}
			if strings.Contains(output, "completed in ") != (mode == "--debug") {
				t.Fatalf("unexpected timings: %s", output)
			}
			for _, secret := range []string{"super-secret"} {
				if strings.Contains(output, secret) {
					t.Fatalf("disclosed %q: %s", secret, output)
				}
			}
			assertCacheReleased(t, cachePath)
		})
	}
}

func TestDisabledVerbosityFlagsDoNotConflict(t *testing.T) {
	isolateCLI(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	writeCLIConfig(t, path, "", "headers.sqlite3")
	for _, args := range [][]string{
		{"--quiet", "--verbose=false", "--debug=false"},
		{"--quiet=false", "--verbose", "--debug=false"},
		{"--quiet=false", "--verbose=false", "--debug"},
	} {
		var stderr bytes.Buffer
		if code := runWithHooks(context.Background(), append(args, "--config", path), &stderr, noProbe, noServe); code != 0 {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestTransportDiagnosticIsReportedDuringProbe(t *testing.T) {
	isolateCLI(t)
	for _, transport := range []struct {
		name  string
		mode  string
		label string
	}{
		{"tls", "tls", "transport: implicit TLS (verified)"},
		{"starttls", "starttls", "transport: STARTTLS (verified)"},
		{"none", "none", "transport: plaintext"},
		{"unknown", "super-secret private-user AUTH LOGIN (verified)", ""},
	} {
		for _, quiet := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/quiet=%v", transport.name, quiet), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "config.toml")
				cachePath := filepath.Join(dir, "headers.sqlite3")
				writeCLIConfig(t, path, "", cachePath)
				args := []string{"--config", path}
				if quiet {
					args = append(args, "--quiet")
				}
				var stderr bytes.Buffer
				served := false
				code := runWithHooks(context.Background(), args, &stderr,
					func(_ context.Context, _ *config.Config, reportTransport func(string)) error {
						reportTransport(transport.mode)
						output := stderr.String()
						if !quiet && transport.label != "" && !strings.Contains(output, "imap-mcp: "+transport.label+"\n") {
							t.Fatalf("transport must be reported before probe returns: %q", output)
						}
						for _, forbidden := range []string{"connection check succeeded", "ready:"} {
							if strings.Contains(output, forbidden) {
								t.Fatalf("probe reported %q before authentication completed: %q", forbidden, output)
							}
						}
						return nil
					},
					func(context.Context, *service.Service) error {
						served = true
						if !quiet && transport.label != "" {
							output := stderr.String()
							labelIndex := strings.Index(output, transport.label)
							successIndex := strings.Index(output, "connection check succeeded")
							readyIndex := strings.Index(output, "ready:")
							if labelIndex < 0 || successIndex <= labelIndex || readyIndex <= successIndex {
								t.Fatalf("transport, success and readiness must precede serving in order: %q", output)
							}
						}
						return nil
					})
				output := stderr.String()
				if code != 0 || !served {
					t.Fatalf("code=%d served=%v stderr=%q", code, served, output)
				}
				if quiet && output != "" {
					t.Fatalf("quiet output: %q", output)
				}
				wantCount := 0
				if !quiet && transport.label != "" {
					wantCount = 1
				}
				if strings.Count(output, "transport:") != wantCount {
					t.Fatalf("unexpected transport diagnostics: %q", output)
				}
				if transport.mode != "tls" && transport.mode != "starttls" && strings.Contains(output, "(verified)") {
					t.Fatalf("plaintext or unknown transport claimed TLS verification: %q", output)
				}
				for _, forbidden := range []string{"super-secret"} {
					if strings.Contains(output, forbidden) {
						t.Fatalf("transport diagnostic disclosed %q: %q", forbidden, output)
					}
				}
				assertCacheReleased(t, cachePath)
			})
		}
	}
}

func TestFailedProbeReportsOnlyEstablishedTransport(t *testing.T) {
	isolateCLI(t)
	for _, failure := range []struct {
		name      string
		transport string
		label     string
		err       error
	}{
		{"connection", "", "", errors.New("secret-host private-user super-secret server error")},
		{"TLS handshake", "", "", mail.NewError("tls_failed", "IMAP TLS negotiation failed")},
		{"TLS authentication", "tls", "transport: implicit TLS (verified)", mail.NewError("authentication_failed", "IMAP authentication failed")},
		{"STARTTLS authentication", "starttls", "transport: STARTTLS (verified)", mail.NewError("authentication_failed", "IMAP authentication failed")},
		{"plaintext authentication", "none", "transport: plaintext", mail.NewError("authentication_failed", "IMAP authentication failed")},
	} {
		for _, quiet := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/quiet=%v", failure.name, quiet), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "config.toml")
				cachePath := filepath.Join(dir, "headers.sqlite3")
				writeCLIConfig(t, path, "", cachePath)
				args := []string{"--config", path}
				if quiet {
					args = append(args, "--quiet")
				}
				var stderr bytes.Buffer
				code := runWithHooks(context.Background(), args, &stderr,
					func(_ context.Context, _ *config.Config, reportTransport func(string)) error {
						if failure.transport != "" {
							reportTransport(failure.transport)
						}
						return fmt.Errorf("private-user super-secret wrapper: %w", failure.err)
					},
					func(context.Context, *service.Service) error {
						t.Fatal("failed authentication started MCP")
						return nil
					})
				output := stderr.String()
				if code != 1 || !strings.Contains(output, "IMAP connection check failed") {
					t.Fatalf("code=%d stderr=%q", code, output)
				}
				if quiet && strings.Count(output, "\n") != 1 {
					t.Fatalf("quiet must retain only the error: %q", output)
				}
				if wantLabel := !quiet && failure.label != ""; strings.Contains(output, "transport:") != wantLabel {
					t.Fatalf("unexpected transport label: %q", output)
				}
				if !quiet && failure.label != "" {
					if labelIndex := strings.Index(output, failure.label); labelIndex < 0 || labelIndex > strings.Index(output, "IMAP connection check failed") {
						t.Fatalf("established transport must precede failure: %q", output)
					}
				}
				if failure.transport != "tls" && failure.transport != "starttls" && strings.Contains(output, "(verified)") {
					t.Fatalf("failed handshake or plaintext claimed TLS verification: %q", output)
				}
				for _, forbidden := range []string{"ready:", "connection check succeeded", "super-secret"} {
					if strings.Contains(output, forbidden) {
						t.Fatalf("failed startup disclosed %q: %q", forbidden, output)
					}
				}
				assertCacheReleased(t, cachePath)
			})
		}
	}
}

func TestProbeFailureStopsStartupAndReleasesCache(t *testing.T) {
	isolateCLI(t)
	for _, verbosity := range []string{"--quiet", "--debug"} {
		for _, failure := range []error{
			mail.NewError("authentication_failed", "IMAP authentication failed"),
			fmt.Errorf("untrusted super-secret server greeting"),
		} {
			t.Run(verbosity+"/"+fmt.Sprintf("%T", failure), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "config.toml")
				cachePath := filepath.Join(dir, "headers.sqlite3")
				writeCLIConfig(t, path, "", cachePath)
				var stderr bytes.Buffer
				code := runWithHooks(context.Background(), []string{"--config", path, verbosity}, &stderr,
					func(context.Context, *config.Config, func(string)) error {
						return fmt.Errorf("private-user wrapper: %w", failure)
					},
					func(context.Context, *service.Service) error {
						t.Fatal("failed probe started MCP")
						return nil
					})
				output := stderr.String()
				if code != 1 || !strings.Contains(output, "IMAP connection check failed") {
					t.Fatalf("code=%d stderr=%q", code, output)
				}
				if _, ok := failure.(*mail.Error); ok && !strings.Contains(output, "[authentication_failed]: IMAP authentication failed") {
					t.Fatalf("safe typed error missing: %s", output)
				}
				for _, forbidden := range []string{"ready:", "connection closed", "super-secret"} {
					if strings.Contains(output, forbidden) {
						t.Fatalf("unexpected %q: %s", forbidden, output)
					}
				}
				if verbosity == "--quiet" && strings.Count(output, "\n") != 1 {
					t.Fatalf("quiet must print only error: %q", output)
				}
				assertCacheReleased(t, cachePath)
			})
		}
	}
}

func TestCancellationDuringProbeAndServing(t *testing.T) {
	isolateCLI(t)
	for _, stage := range []string{"probe", "serve"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			cachePath := filepath.Join(dir, "headers.sqlite3")
			writeCLIConfig(t, path, "", cachePath)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			code := runWithHooks(ctx, []string{"--config", path}, &stderr,
				func(probeCtx context.Context, _ *config.Config, reportTransport func(string)) error {
					if stage == "probe" {
						cancel()
						<-probeCtx.Done()
						return probeCtx.Err()
					}
					reportTransport("tls")
					return nil
				},
				func(serverCtx context.Context, _ *service.Service) error {
					if stage == "probe" {
						t.Fatal("cancelled probe started MCP")
					}
					cancel()
					<-serverCtx.Done()
					return errors.New("super-secret transport error")
				})
			if code != 0 || strings.Contains(stderr.String(), "failed") || strings.Contains(stderr.String(), "super-secret") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if stage == "probe" && strings.Contains(stderr.String(), "ready:") {
				t.Fatalf("cancelled startup announced readiness: %s", stderr.String())
			}
			assertCacheReleased(t, cachePath)
		})
	}
}

func TestServerFailureIsSafeAndReleasesCache(t *testing.T) {
	isolateCLI(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cachePath := filepath.Join(dir, "headers.sqlite3")
	writeCLIConfig(t, path, "", cachePath)
	var stderr bytes.Buffer
	code := runWithHooks(context.Background(), []string{"--config", path, "--quiet"}, &stderr, noProbe,
		func(context.Context, *service.Service) error { return errors.New("super-secret transport failure") })
	if code != 1 || stderr.String() != "imap-mcp: stdio server failed\n" {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	assertCacheReleased(t, cachePath)
}

func TestStartupDoesNotWriteStdout(t *testing.T) {
	isolateCLI(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeCLIConfig(t, path, "", "headers.sqlite3")
	for _, tt := range []struct {
		args []string
		code int
	}{
		{[]string{"--config", path, "--debug"}, 0},
		{[]string{"--quiet", "--help"}, 0},
		{[]string{"--config="}, 2},
		{[]string{"--config", filepath.Join(dir, "missing.toml")}, 1},
	} {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stdout
		os.Stdout = writer
		var stderr bytes.Buffer
		code := runWithHooks(context.Background(), tt.args, &stderr, noProbe, noServe)
		os.Stdout = original
		if err := writer.Close(); err != nil {
			reader.Close()
			t.Fatal(err)
		}
		output, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if code != tt.code || len(output) != 0 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", tt.args, code, output, stderr.String())
		}
	}
}

func readCLILog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "imap-mcp", "imap-mcp.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("super-secret")) {
		t.Fatal("diagnostic log disclosed the configured password")
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatal("diagnostic log contains an invalid JSON record")
		}
	}
	return string(data)
}

func TestFileLoggingAndContextPropagation(t *testing.T) {
	for _, mode := range []string{"", "--quiet", "--verbose", "--debug"} {
		t.Run(mode, func(t *testing.T) {
			isolateCLI(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			cachePath := filepath.Join(dir, "database", "headers.sqlite3")
			writeCLIConfig(t, path, "", cachePath)
			args := []string{"--config", path}
			if mode != "" {
				args = append(args, mode)
			}
			var stderr bytes.Buffer
			code := runWithHooks(context.Background(), args, &stderr,
				func(ctx context.Context, _ *config.Config, report func(string)) error {
					diagnosticlog.Logger(ctx).InfoContext(ctx, "probe logger propagated")
					diagnosticlog.Stage(ctx, "probe stage super-secret private-user")
					defer diagnosticlog.Measure(ctx, "probe detail")()
					report("tls")
					return nil
				},
				func(ctx context.Context, _ *service.Service) error {
					diagnosticlog.Logger(ctx).InfoContext(ctx, "tool completed", "mailbox", "INBOX", "query", "subject:test", "duration", time.Millisecond)
					diagnosticlog.Stage(ctx, "serve stage")
					return nil
				})
			if code != 0 {
				t.Fatalf("exit code = %d", code)
			}
			output := stderr.String()
			if strings.Contains(output, "super-secret") {
				t.Fatal("stderr disclosed the configured password")
			}
			log := readCLILog(t)
			for _, want := range []string{"startup started", "configuration loaded", "header cache opened", "probe logger propagated", "tool completed", "MCP server stopped", "closing header cache", "shutdown complete", "private-user", "secret-host.example.test", "INBOX", "subject:test", cachePath} {
				if !strings.Contains(log, want) {
					t.Fatalf("diagnostic log missing %q", want)
				}
			}
			if strings.Contains(log, "probe detail") != (mode == "--debug") {
				t.Fatal("file debug threshold did not match --debug")
			}
			if strings.Contains(log, `"stage":"cache open"`) != (mode == "--debug") {
				t.Fatal("cache opening diagnostics did not inherit the CLI logger")
			}
			if strings.Contains(output, "probe stage") != (mode == "--debug") {
				t.Fatal("shared stage stderr did not match --debug")
			}
			if mode == "--quiet" {
				if output != "" {
					t.Fatal("quiet printed routine diagnostics")
				}
			} else {
				logPath := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "imap-mcp", "imap-mcp.log")
				if !strings.Contains(output, fmt.Sprintf("%q", logPath)) {
					t.Fatal("startup did not print the resolved log path")
				}
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(cachePath), "imap-mcp.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("diagnostic log followed the configured database path")
			}
			assertCacheReleased(t, cachePath)
		})
	}
}

func TestUnderlyingFailuresAreLoggedWithoutPassword(t *testing.T) {
	for _, stage := range []string{"cache", "probe", "serve"} {
		t.Run(stage, func(t *testing.T) {
			isolateCLI(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			cachePath := filepath.Join(dir, "headers.sqlite3")
			if stage == "cache" {
				obstacle := filepath.Join(dir, "super-secret-cache-obstacle")
				if err := os.WriteFile(obstacle, nil, 0600); err != nil {
					t.Fatal(err)
				}
				cachePath = filepath.Join(obstacle, "headers.sqlite3")
			}
			writeCLIConfig(t, path, "", cachePath)
			probe, serve := noProbe, noServe
			if stage == "probe" {
				probe = func(context.Context, *config.Config, func(string)) error {
					return errors.New("super-secret private-user probe detail")
				}
			}
			if stage == "serve" {
				serve = func(context.Context, *service.Service) error {
					return errors.New("super-secret private-user serve detail")
				}
			}
			var stderr bytes.Buffer
			if code := runWithHooks(context.Background(), []string{"--config", path, "--quiet"}, &stderr, probe, serve); code != 1 {
				t.Fatalf("exit code = %d", code)
			}
			if strings.Contains(stderr.String(), "super-secret") || strings.Count(stderr.String(), "\n") != 1 {
				t.Fatal("quiet error output did not remain safe and concise")
			}
			log := readCLILog(t)
			if !strings.Contains(log, `"level":"ERROR"`) || !strings.Contains(log, `"error":`) || !strings.Contains(log, "shutdown complete") {
				t.Fatal("underlying error or shutdown was not logged")
			}
			if stage != "cache" && !strings.Contains(log, stage+" detail") {
				t.Fatal("underlying error detail was lost")
			}
		})
	}
}

func TestDiagnosticOpenFailureStopsBeforeCacheAndProbe(t *testing.T) {
	isolateCLI(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cachePath := filepath.Join(dir, "headers.sqlite3")
	writeCLIConfig(t, path, "", cachePath)
	obstacle := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(obstacle, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", obstacle)
	var stderr bytes.Buffer
	code := runWithHooks(context.Background(), []string{"--config", path, "--quiet"}, &stderr,
		func(context.Context, *config.Config, func(string)) error {
			t.Fatal("failed diagnostics reached probe")
			return nil
		}, noServe)
	if code != 1 || stderr.String() != "imap-mcp: cannot open diagnostic log\n" {
		t.Fatalf("unexpected diagnostic open error: code=%d", code)
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed diagnostics opened the database")
	}
}

type failDebugWriter struct{ bytes.Buffer }

func (w *failDebugWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("imap-mcp: debug:")) {
		return 0, errors.New("debug output unavailable")
	}
	return w.Buffer.Write(p)
}

func TestDiagnosticWriteFailureIsSurfacedAtShutdown(t *testing.T) {
	isolateCLI(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeCLIConfig(t, path, "", "headers.sqlite3")
	var stderr failDebugWriter
	code := runWithHooks(context.Background(), []string{"--config", path, "--debug"}, &stderr, noProbe, noServe)
	if code != 1 || !strings.Contains(stderr.String(), "cannot close diagnostic log") {
		t.Fatal("retained diagnostic write failure was not surfaced")
	}
	if !strings.Contains(readCLILog(t), "shutdown complete") {
		t.Fatal("stderr failure stopped file logging")
	}
}

func TestHumanDiagnosticPasswordRedaction(t *testing.T) {
	for _, password := range []string{"super-secret", "line\nquote\"\\&", "RED"} {
		t.Run(fmt.Sprintf("length=%d", len(password)), func(t *testing.T) {
			dir := isolateCLI(t)
			path := filepath.Join(dir, password, "config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			content := "[imap]\nhost = \"example.test\"\nusername = \"private-user\"\npassword = " + strconv.Quote(password) + "\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			code := runWithHooks(context.Background(), []string{"--config", path, "--debug"}, &stderr,
				func(context.Context, *config.Config, func(string)) error {
					return mail.NewError("authentication_failed", "error "+password)
				}, noServe)
			if code != 1 {
				t.Fatalf("exit code = %d", code)
			}
			quoted := strconv.Quote(password)
			for _, secret := range []string{password, quoted[1 : len(quoted)-1]} {
				if strings.Contains(stderr.String(), secret) {
					t.Fatal("human diagnostic disclosed the configured password")
				}
			}
		})
	}
}

func TestCancellationClosesRuntimeConnectionBeforeServingReturns(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%v", active), func(t *testing.T) {
			dir := isolateCLI(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			disconnected := make(chan error, 1)
			listStarted := make(chan struct{})
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					disconnected <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				fmt.Fprint(conn, "* PREAUTH [CAPABILITY IMAP4rev1] mock ready\r\n")
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						disconnected <- err
						return
					}
					fields := strings.Fields(line)
					if len(fields) < 2 {
						disconnected <- errors.New("invalid mock command")
						return
					}
					switch strings.ToUpper(fields[1]) {
					case "CAPABILITY":
						fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK complete\r\n", fields[0])
					case "NOOP":
						fmt.Fprintf(conn, "%s OK complete\r\n", fields[0])
					case "LIST":
						// Hold a command from a handler whose context outlives root cancellation.
						close(listStarted)
					default:
						fmt.Fprintf(conn, "%s BAD unexpected command\r\n", fields[0])
					}
				}
			}()
			path := filepath.Join(dir, "config.toml")
			content := fmt.Sprintf("[imap]\nhost = \"127.0.0.1\"\nport = %d\ntls = \"none\"\nusername = \"private-user\"\npassword = \"super-secret\"\noperation_timeout_seconds = 10\n",
				listener.Addr().(*net.TCPAddr).Port)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			code := runWithHooks(ctx, []string{"--config", path, "--quiet"}, &stderr, noProbe,
				func(serverCtx context.Context, svc *service.Service) error {
					st, ct := mcp.NewInMemoryTransports()
					serverDone := make(chan error, 1)
					requestCancellation := make(chan error, 1)
					s := server.New(svc)
					s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
						return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
							result, err := next(ctx, method, req)
							if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && params.Name == "list_mailboxes" {
								requestCancellation <- ctx.Err()
							}
							return result, err
						}
					})
					go func() { serverDone <- s.Run(serverCtx, st) }()
					requestCtx, requestCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer requestCancel()
					cs, err := mcp.NewClient(&mcp.Implementation{Name: "shutdown-test", Version: "1"}, nil).Connect(requestCtx, ct, nil)
					if err != nil {
						t.Fatal(err)
					}
					var logLock *os.File
					defer func() {
						if t.Failed() && logLock != nil {
							_ = logLock.Close()
						}
						_ = cs.Close()
					}()
					if result, err := cs.CallTool(requestCtx, &mcp.CallToolParams{Name: "server_info"}); err != nil || result.IsError {
						t.Fatalf("runtime connection setup failed: %v", err)
					}
					var operationDone chan error
					if active {
						operationDone = make(chan error, 1)
						go func() {
							_, err := cs.CallTool(requestCtx, &mcp.CallToolParams{Name: "list_mailboxes"})
							operationDone <- err
						}()
						select {
						case <-listStarted:
						case <-time.After(2 * time.Second):
							t.Fatal("runtime operation did not reach the mock")
						}
					}
					// Keep this external descriptor locked through SDK drain and all CLI defers.
					lock, err := os.OpenFile(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "imap-mcp", "imap-mcp.log.lock"), os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					logLock = lock
					t.Cleanup(func() { lock.Close() })
					if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
						t.Fatal(err)
					}
					cancel()
					select {
					case err := <-disconnected:
						if !errors.Is(err, io.EOF) {
							t.Fatalf("runtime was not promptly closed: %v", err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("runtime stayed open while serving drained")
					}
					if active {
						select {
						case err := <-requestCancellation:
							if err != nil {
								t.Fatalf("SDK request inherited root cancellation: %v", err)
							}
						case <-time.After(2 * time.Second):
							t.Fatal("tool completion waited on diagnostic I/O")
						}
						select {
						case <-operationDone:
						case <-time.After(2 * time.Second):
							t.Fatal("runtime operation stayed blocked after cancellation")
						}
					}
					select {
					case err := <-serverDone:
						return err
					case <-time.After(2 * time.Second):
						t.Fatal("SDK drain stayed blocked on a diagnostic lock after root cancellation")
						return nil
					}
				})
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("cancelled serving returned code=%d", code)
			}
			assertCacheReleased(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "imap-mcp", "headers.sqlite3"))
		})
	}
}

func TestCancellationInterruptsDiagnosticStartupLockWait(t *testing.T) {
	dir := isolateCLI(t)
	path := filepath.Join(dir, "config.toml")
	cachePath := filepath.Join(dir, "headers.sqlite3")
	writeCLIConfig(t, path, "", cachePath)
	logDir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "imap-mcp")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(logDir, "imap-mcp.log.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	finished := make(chan int, 1)
	go func() {
		finished <- runWithHooks(ctx, []string{"--config", path, "--quiet"}, &stderr, noProbe, noServe)
	}()
	select {
	case code := <-finished:
		t.Fatalf("startup did not wait on diagnostic lock: code=%d", code)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case code := <-finished:
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("cancelled startup returned code=%d stderr=%q", code, stderr.String())
		}
	case <-time.After(time.Second):
		t.Fatal("root cancellation did not interrupt diagnostic startup")
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled diagnostic startup opened the header cache")
	}
}
