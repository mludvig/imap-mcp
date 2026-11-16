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

	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/client"
	"github.com/mludvig/imap-mcp/internal/config"
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
	flags := flag.NewFlagSet("imap-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "absolute path to TOML configuration")
	flags.Usage = func() { fmt.Fprintln(stderr, "Usage: imap-mcp --config /absolute/path.toml") }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *path == "" || !filepath.IsAbs(*path) || flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, "imap-mcp:", err)
		return 1
	}
	store, err := cache.Open(cfg.CachePath())
	if err != nil {
		fmt.Fprintln(stderr, "imap-mcp: cannot open header cache")
		return 1
	}
	defer store.Close()
	svc := service.New(cfg, client.New(cfg), store)
	if err := server.New(svc).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "imap-mcp: stdio server failed")
		return 1
	}
	return 0
}
