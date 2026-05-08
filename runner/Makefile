.PHONY: help generate proto-lint proto-check test test-unit test-integration test-e2e build clean

BIN_DIR := bin
BUF_CACHE_DIR ?= /tmp/runner-buf-cache
GOCACHE ?= /tmp/runner-go-build-cache
GOMODCACHE ?= /tmp/runner-go-mod-cache
GOPATH ?= /tmp/runner-go
GO_ENV := GOPATH=$(GOPATH) GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE)

help:
	@printf '%s\n' 'Targets: generate proto-lint proto-check test test-unit test-integration test-e2e build clean'

generate:
	BUF_CACHE_DIR=$(BUF_CACHE_DIR) buf generate

proto-lint:
	BUF_CACHE_DIR=$(BUF_CACHE_DIR) buf lint

proto-check:
	@tmp=$$(mktemp -d); \
	if [ -d gen/go ]; then cp -R gen/go "$$tmp/go"; else mkdir -p "$$tmp/go"; fi; \
	$(MAKE) generate >/dev/null; \
	diff -ru "$$tmp/go" gen/go; \
	rm -rf "$$tmp"

test: test-unit

test-unit:
	$(GO_ENV) go test ./...

test-integration:
	$(GO_ENV) go test ./... -run Integration

test-e2e: build
	$(GO_ENV) go test ./... -run E2E

build:
	mkdir -p $(BIN_DIR)
	$(GO_ENV) go build -o $(BIN_DIR)/runner ./cmd/runner

clean:
	rm -rf $(BIN_DIR) gen/go
