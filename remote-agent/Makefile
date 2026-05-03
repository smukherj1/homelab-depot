.PHONY: help generate proto-lint proto-check test test-integration test-docker test-e2e build clean

BIN_DIR := bin
BUF_CACHE_DIR ?= /tmp/remote-agent-buf-cache

help:
	@printf '%s\n' 'Targets: generate proto-lint proto-check test test-integration test-docker test-e2e build clean'

generate:
	@if command -v buf >/dev/null 2>&1; then BUF_CACHE_DIR=$(BUF_CACHE_DIR) buf generate; else protoc --go_out=gen/go --go_opt=paths=source_relative --go-grpc_out=gen/go --go-grpc_opt=paths=source_relative proto/agent.proto; fi

proto-lint:
	@if command -v buf >/dev/null 2>&1; then BUF_CACHE_DIR=$(BUF_CACHE_DIR) buf lint; else echo 'buf not installed; skipping proto lint'; fi

proto-check:
	@tmp=$$(mktemp -d); cp -R gen/go "$$tmp/go"; $(MAKE) generate >/dev/null; diff -ru "$$tmp/go" gen/go; rm -rf "$$tmp"

test:
	go test ./...

test-integration:
	go test ./... -run Integration

test-docker:
	go test ./internal/docker -run Docker

test-e2e: build
	go test ./... -run E2E

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/agent ./cmd/agent
	go build -o $(BIN_DIR)/agentctl ./cmd/agentctl

clean:
	rm -rf $(BIN_DIR)
