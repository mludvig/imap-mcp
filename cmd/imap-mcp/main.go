package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/client"
	"github.com/mludvig/imap-mcp/internal/config"
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
		func(ctx context.Context, cfg *config.Config) error { return client.New(cfg).CheckConnection(ctx) },
		func(ctx context.Context, svc *service.Service) error {
			return server.New(svc).Run(ctx, &mcp.StdioTransport{})
		})
}

// Only network probing and MCP serving are replaced in CLI tests.
func runWithHooks(ctx context.Context, args []string, stderr io.Writer,
	probe func(context.Context, *config.Config) error,
	serve func(context.Context, *service.Service) error,
) (code int) {
	flags := flag.NewFlagSet("imap-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "path to TOML configuration (relative paths use the working directory)")
	quiet := flags.Bool("quiet", false, "show only errors")
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
		fmt.Fprintln(stderr, "  -h, --help\n    \tshow this help without loading configuration or connecting")
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
	log := diagnostics{stderr: stderr, quiet: *quiet, verbose: *verbose || *debug, debug: *debug}
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
	log.detail("configuration loaded")
	log.timing("configuration load", started)
	started = time.Now()
	log.info("opening header cache")
	store, err := cache.Open(cfg.CachePath())
	if err != nil {
		fmt.Fprintln(stderr, "imap-mcp: cannot open header cache")
		return 1
	}
	defer func() {
		log.detail("closing header cache")
		if err := store.Close(); err != nil {
			fmt.Fprintln(stderr, "imap-mcp: cannot close header cache")
			code = 1
		}
		log.detail("shutdown complete")
	}()
	log.detail("header cache opened")
	log.timing("header cache open", started)
	// Delay path disclosure until both inputs succeed; see docs/adr/0006-linux-cli-startup-validation.md.
	log.info("configuration: %q", resolvedPath)
	log.info("header cache: %q", cfg.CachePath())
	if cfg.Readonly() {
		log.info("mode: readonly")
	} else {
		log.info("mode: write")
	}

	// The bounded probe owns and closes its connection before MCP starts; see docs/adr/0006-linux-cli-startup-validation.md.
	started = time.Now()
	log.info("checking IMAP connection")
	probeCtx, cancel := context.WithTimeout(ctx, cfg.IMAP().OperationTimeout())
	err = probe(probeCtx, cfg)
	cancel()
	log.timing("IMAP connection check", started)
	if err != nil {
		if ctx.Err() != nil {
			log.detail("startup cancelled")
			return 0
		}
		var domain *mail.Error
		if errors.As(err, &domain) {
			fmt.Fprintf(stderr, "imap-mcp: IMAP connection check failed [%s]: %s\n", domain.Code, domain.Message)
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
	svc := service.New(cfg, client.New(cfg), store)
	log.detail("starting MCP server")
	log.info("ready: serving MCP over stdio")
	if err := serve(ctx, svc); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "imap-mcp: stdio server failed")
		return 1
	}
	log.detail("MCP server stopped")
	return 0
}

type diagnostics struct {
	stderr  io.Writer
	quiet   bool
	verbose bool
	debug   bool
}

func (d diagnostics) info(format string, args ...any) {
	if !d.quiet {
		fmt.Fprintf(d.stderr, "imap-mcp: "+format+"\n", args...)
	}
}

func (d diagnostics) detail(message string) {
	if d.verbose {
		d.info("%s", message)
	}
}

func (d diagnostics) stage(message string) {
	if d.debug {
		d.info("debug: %s", message)
	}
}

func (d diagnostics) timing(stage string, started time.Time) {
	if d.debug {
		d.info("debug: %s completed in %s", stage, time.Since(started))
	}
}
