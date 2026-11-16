package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/mail"
	"github.com/mludvig/imap-mcp/internal/service"
)

func TestArgumentErrors(t *testing.T) {
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
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	for _, args := range [][]string{{"-h"}, {"--help"}, {"--quiet", "--help"}, {"--config", "/missing.toml", "--help"}} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, &stderr); code != 0 {
			t.Fatalf("args %v: help code = %d", args, code)
		}
		for _, want := range []string{"Usage:", "--config", "--quiet", "--verbose", "--debug", "--help", "XDG_CONFIG_HOME", "HOME/.config", "relative", "stderr", "connection"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("help missing %q: %s", want, stderr.String())
			}
		}
	}
}

func TestConfigurationErrorsDoNotExposeCredentials(t *testing.T) {
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
			for _, secret := range []string{"secret-host", "private-user", "super-secret", "AUTH", "LOGIN"} {
				if strings.Contains(output, secret) {
					t.Fatalf("disclosed %q: %s", secret, output)
				}
			}
			assertCacheReleased(t, cachePath)
		})
	}
}

func TestDisabledVerbosityFlagsDoNotConflict(t *testing.T) {
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
				for _, forbidden := range []string{"secret-host", "private-user", "super-secret", "AUTH", "LOGIN"} {
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
				for _, forbidden := range []string{"ready:", "connection check succeeded", "secret-host", "private-user", "super-secret"} {
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
				for _, forbidden := range []string{"ready:", "connection closed", "super-secret", "private-user"} {
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
