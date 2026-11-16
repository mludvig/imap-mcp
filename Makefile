GO ?= $(if $(wildcard $(CURDIR)/.tools/go/bin/go),$(CURDIR)/.tools/go/bin/go,go)

.DEFAULT_GOAL := build

export GOTOOLCHAIN = local
export GOPATH = $(CURDIR)/.tools/cache/gopath
export GOMODCACHE = $(CURDIR)/.tools/cache/mod
export GOCACHE = $(CURDIR)/.tools/cache/build
export TMPDIR = $(CURDIR)/.tools/tmp

.PHONY: build test integration fmt version prepare clean

prepare:
	mkdir -p "$(GOPATH)" "$(GOMODCACHE)" "$(GOCACHE)" "$(TMPDIR)"

build: prepare
	mkdir -p bin
	$(GO) build -o bin/imap-mcp ./cmd/imap-mcp

clean:
	rm -rf "$(CURDIR)/bin" "$(CURDIR)/.tools/cache/build" "$(CURDIR)/.tools/tmp"

test: prepare
	$(GO) test ./...

integration: prepare
	$(GO) test -tags=integration -timeout=5m ./tests/integration

fmt: prepare
	$(GO) fmt ./...

version:
	$(GO) version
