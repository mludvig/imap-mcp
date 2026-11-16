GO ?= $(if $(wildcard $(CURDIR)/.tools/go/bin/go),$(CURDIR)/.tools/go/bin/go,go)

export GOTOOLCHAIN = local
export GOPATH = $(CURDIR)/.tools/cache/gopath
export GOMODCACHE = $(CURDIR)/.tools/cache/mod
export GOCACHE = $(CURDIR)/.tools/cache/build
export TMPDIR = $(CURDIR)/.tools/tmp

.PHONY: build test fmt version prepare

prepare:
	mkdir -p "$(GOPATH)" "$(GOMODCACHE)" "$(GOCACHE)" "$(TMPDIR)"

build: prepare
	$(GO) build ./...

test: prepare
	$(GO) test ./...

fmt: prepare
	$(GO) fmt ./...

version:
	$(GO) version
