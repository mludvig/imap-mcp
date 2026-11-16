package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/client"
	"github.com/mludvig/imap-mcp/internal/config"
	diagnosticlog "github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
	"github.com/mludvig/imap-mcp/internal/server"
	"github.com/mludvig/imap-mcp/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

func run(ctx context.Context, args []string, stderr io.Writer) int {
	return runWithHooks(ctx, args, stderr,
		func(ctx context.Context, cfg *config.Config, reportTransport func(string)) error {
			return client.New(cfg).CheckConnection(ctx, reportTransport)
		},
		func(ctx context.Context, svc *service.Service) error {
			return server.New(svc).Run(ctx, &mcp.StdioTransport{})
		})
}

// Only network probing and MCP serving are replaced in CLI tests.
func runWithHooks(ctx context.Context, args []string, stderr io.Writer,
	probe func(context.Context, *config.Config, func(string)) error,
	serve func(context.Context, *service.Service) error,
) (code int) {
	stderr = &lockedWriter{writer: stderr}
	flags := flag.NewFlagSet("imap-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "path to TOML configuration (relative paths use the working directory)")
	quiet := flags.Bool("quiet", false, "show only errors on stderr; diagnostic file logging continues")
	verbose := flags.Bool("verbose", false, "show startup and shutdown lifecycle details")
	debug := flags.Bool("debug", false, "show lifecycle details and startup stage timings; no IMAP protocol logging")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: imap-mcp [--config PATH] [--quiet | --verbose | --debug]")
		fmt.Fprintln(stderr, "\nServe IMAP tools over MCP stdio. Startup diagnostics go to stderr.")
		fmt.Fprintln(stderr, "Default configuration: os.UserConfigDir()/imap-mcp/config.toml")
		fmt.Fprintln(stderr, "On Linux: $XDG_CONFIG_HOME/imap-mcp/config.toml, or $HOME/.config/imap-mcp/config.toml.")
		fmt.Fprintln(stderr, "Configuration and cache are validated and the IMAP connection is checked before serving.")
		fmt.Fprintln(stderr, "\nOptions:")
		flags.PrintDefaults()
		fmt.Fprintln(stderr, "\nDiagnostic log: os.UserCacheDir()/imap-mcp/imap-mcp.log, independent of cache.path.")
		fmt.Fprintln(stderr, "On Linux: $XDG_CACHE_HOME/imap-mcp/imap-mcp.log, or $HOME/.cache/imap-mcp/imap-mcp.log.")
		fmt.Fprintln(stderr, "Logs retain at most 50 MiB (10 MiB active plus four backups); only the password is redacted.")
		fmt.Fprintln(stderr, "  -h, --help\n    \tshow this help without loading configuration, creating files or connecting")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	argumentError := func(message string) int {
		fmt.Fprintln(stderr, "imap-mcp:", message)
		flags.Usage()
		return 2
	}
	if flags.NArg() != 0 {
		return argumentError("positional arguments are not supported")
	}
	if (*quiet && *verbose) || (*quiet && *debug) || (*verbose && *debug) {
		return argumentError("--quiet, --verbose and --debug are mutually exclusive")
	}
	explicitConfig := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})
	if explicitConfig && *path == "" {
		return argumentError("--config must not be empty")
	}
	log := diagnostics{ctx: ctx, stderr: stderr, quiet: *quiet, verbose: *verbose || *debug, debug: *debug}
	log.detail("startup started")
	started := time.Now()
	log.stage("resolving configuration path")
	if !explicitConfig {
		dir, err := os.UserConfigDir()
		if err != nil {
			fmt.Fprintln(stderr, "imap-mcp: cannot resolve user configuration directory; set --config explicitly")
			return 1
		}
		*path = filepath.Join(dir, "imap-mcp", "config.toml")
	}
	resolvedPath, err := filepath.Abs(*path)
	if err != nil {
		fmt.Fprintln(stderr, "imap-mcp: cannot resolve configuration path")
		return 1
	}
	log.timing("configuration path resolution", started)
	started = time.Now()
	log.info("loading configuration")
	cfg, err := config.Load(resolvedPath)
	if err != nil {
		fmt.Fprintln(stderr, "imap-mcp: cannot load configuration")
		return 1
	}
	sink, err := diagnosticlog.NewContext(ctx, "", cfg.IMAP().Password(), *debug, debugStageWriter{stderr})
	if err != nil {
		if ctx.Err() != nil {
			log.detail("startup cancelled")
			return 0
		}
		fmt.Fprintln(stderr, "imap-mcp: cannot open diagnostic log")
		return 1
	}
	ctx = diagnosticlog.WithLogger(ctx, sink.Logger())
	log.ctx, log.logger, log.password = ctx, sink.Logger(), cfg.IMAP().Password()
	log.logger.InfoContext(ctx, "startup started", "configuration", resolvedPath, "cache", cfg.CachePath(),
		"host", cfg.IMAP().Host(), "username", cfg.IMAP().Username(), "readonly", cfg.Readonly())
	defer func() {
		log.detail("shutdown complete")
		if err := sink.Close(); err != nil {
			fmt.Fprintln(stderr, "imap-mcp: cannot close diagnostic log")
			code = 1
		}
	}()
	log.detail("configuration loaded")
	log.timing("configuration load", started)
	started = time.Now()
	log.info("opening header cache")
	store, err := cache.OpenContext(ctx, cfg.CachePath())
	if err != nil {
		log.logger.ErrorContext(ctx, "cannot open header cache", "error", err)
		fmt.Fprintln(stderr, "imap-mcp: cannot open header cache")
		return 1
	}
	defer func() {
		log.detail("closing header cache")
		if err := store.Close(); err != nil {
			log.logger.ErrorContext(ctx, "cannot close header cache", "error", err)
			fmt.Fprintln(stderr, "imap-mcp: cannot close header cache")
			code = 1
		}
	}()
	log.detail("header cache opened")
	log.timing("header cache open", started)
	// Delay path disclosure until both inputs succeed; see docs/adr/0006-linux-cli-startup-validation.md.
	log.info("configuration: %q", resolvedPath)
	log.info("header cache: %q", cfg.CachePath())
	log.info("diagnostic log: %q", sink.Path())
	if cfg.Readonly() {
		log.info("mode: readonly")
	} else {
		log.info("mode: write")
	}

	// The bounded probe owns and closes its connection before MCP starts; see docs/adr/0006-linux-cli-startup-validation.md.
	started = time.Now()
	log.info("checking IMAP connection")
	probeCtx, cancel := context.WithTimeout(ctx, cfg.IMAP().OperationTimeout())
	err = probe(probeCtx, cfg, func(transport string) {
		switch transport {
		case "tls":
			log.info("transport: implicit TLS (verified)")
		case "starttls":
			log.info("transport: STARTTLS (verified)")
		case "none":
			log.info("transport: plaintext")
		}
	})
	cancel()
	log.timing("IMAP connection check", started)
	if err != nil {
		log.logger.ErrorContext(ctx, "IMAP connection check failed", "error", err)
		if ctx.Err() != nil {
			log.detail("startup cancelled")
			return 0
		}
		var domain *mail.Error
		if errors.As(err, &domain) {
			fmt.Fprintln(stderr, "imap-mcp:", log.safe(fmt.Sprintf("IMAP connection check failed [%s]: %s", domain.Code, domain.Message)))
		} else {
			fmt.Fprintln(stderr, "imap-mcp: IMAP connection check failed")
		}
		return 1
	}
	if ctx.Err() != nil {
		log.detail("startup cancelled")
		return 0
	}
	log.info("IMAP connection check succeeded; connection closed")
	backend := client.New(cfg)
	closeBackend := sync.OnceValue(backend.Close)
	// Close before SDK handler draining; see docs/adr/0009-serialized-runtime-connection.md.
	stopBackend := context.AfterFunc(ctx, func() { _ = closeBackend() })
	defer func() {
		stopBackend()
		if err := closeBackend(); err != nil {
			log.logger.ErrorContext(ctx, "cannot close IMAP client", "error", err)
			fmt.Fprintln(stderr, "imap-mcp: cannot close IMAP client")
			code = 1
		}
	}()
	svc := service.New(cfg, backend, store)
	log.detail("starting MCP server")
	log.info("ready: serving MCP over stdio")
	if err := serve(ctx, svc); err != nil {
		log.logger.ErrorContext(ctx, "stdio server stopped with error", "error", err)
		if ctx.Err() == nil {
			fmt.Fprintln(stderr, "imap-mcp: stdio server failed")
			return 1
		}
	}
	log.detail("MCP server stopped")
	return 0
}

type diagnostics struct {
	ctx      context.Context
	logger   *slog.Logger
	password string
	stderr   io.Writer
	quiet    bool
	verbose  bool
	debug    bool
}

func (d diagnostics) info(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if d.logger != nil {
		d.logger.InfoContext(d.ctx, message)
	}
	if !d.quiet {
		fmt.Fprintln(d.stderr, "imap-mcp:", d.safe(message))
	}
}

func (d diagnostics) detail(message string) {
	if d.logger != nil {
		d.logger.InfoContext(d.ctx, message)
	}
	if d.verbose {
		fmt.Fprintln(d.stderr, "imap-mcp:", d.safe(message))
	}
}

func (d diagnostics) stage(message string) {
	if d.logger != nil {
		diagnosticlog.Stage(d.ctx, message)
	} else if d.debug {
		d.info("debug: %s", message)
	}
}

func (d diagnostics) timing(stage string, started time.Time) {
	if d.logger != nil {
		d.logger.DebugContext(d.ctx, "duration", "stage", stage, "duration", time.Since(started))
	} else if d.debug {
		d.info("debug: %s completed in %s", stage, time.Since(started))
	}
}

func (d diagnostics) safe(message string) string {
	if d.password == "" {
		return message
	}
	quoted := strconv.Quote(d.password)
	encoded, _ := json.Marshal(d.password)
	marker := "[REDACTED]"
	if strings.Contains(marker, d.password) {
		marker = ""
	}
	for _, secret := range []string{quoted[1 : len(quoted)-1], string(encoded[1 : len(encoded)-1]), d.password} {
		message = strings.ReplaceAll(message, secret, marker)
	}
	return message
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

// The shared logger redacts these records before retaining the CLI's human output.
type debugStageWriter struct{ writer io.Writer }

func (w debugStageWriter) Write(p []byte) (int, error) {
	var event struct {
		Message  string        `json:"msg"`
		Stage    string        `json:"stage"`
		Duration time.Duration `json:"duration"`
	}
	if err := json.Unmarshal(p, &event); err != nil {
		return 0, err
	}
	var err error
	switch event.Message {
	case "stage":
		_, err = fmt.Fprintf(w.writer, "imap-mcp: debug: %s\n", event.Stage)
	case "duration":
		_, err = fmt.Fprintf(w.writer, "imap-mcp: debug: %s completed in %s\n", event.Stage, event.Duration)
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
