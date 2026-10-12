# MCP Doctor — developer task runner.
#
# Every target here is a gate that CI also runs. A command that only exists in CI is a
# command nobody runs locally before pushing, which is how red builds happen.
#
# Run `make help` for the list.

.DEFAULT_GOAL := help
SHELL := /bin/bash

BINARY      := bin/mcpdoctor
MODULE      := github.com/mcpdoctor/mcpdoctor
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X $(MODULE)/internal/cli.Version=$(VERSION)
PLATFORMS   := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- build

.PHONY: build
build: ## Build the binary for this platform into bin/
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/mcpdoctor
	@echo "built $(BINARY) ($(VERSION))"

.PHONY: install
install: ## Install the binary into GOBIN
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/mcpdoctor

.PHONY: cross
cross: ## Cross-compile for every supported platform
	@set -euo pipefail; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o /dev/null ./cmd/mcpdoctor; \
	done
	@echo "all platforms build"

## ---------------------------------------------------------------- verify

.PHONY: check
check: fmt-check vet test ## Run every local quality gate

.PHONY: fmt
fmt: ## Format all source
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any source is not gofmt-clean
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi; \
	echo "formatting ok"

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: test
test: ## Run the test suite
	go test ./... -count=1

.PHONY: race
race: ## Run the test suite with the race detector
	go test -race ./... -count=1

.PHONY: cover
cover: ## Produce a coverage profile and print the total
	go test -coverprofile=coverage.out -covermode=atomic ./internal/... -count=1
	@go tool cover -func=coverage.out | tail -1
	@go tool cover -func=coverage.out > coverage.txt

.PHONY: fuzz
fuzz: ## Run the fuzz targets for 30s each
	go test -run=XXX -fuzz=FuzzParseConfig -fuzztime=30s ./internal/config/
	go test -run=XXX -fuzz=FuzzScanKeyPositions -fuzztime=30s ./internal/config/

.PHONY: vuln
vuln: ## Scan for known vulnerabilities (requires network)
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: verify
verify: fmt-check vet test race vuln cross ## Every gate, in the order CI runs them

## ---------------------------------------------------------------- run

.PHONY: run
run: build ## Build and run `check` against the discovered configuration
	./$(BINARY) check

.PHONY: demo
demo: build ## Run check against the bundled demo configuration
	./$(BINARY) check --config docs/examples/broken.mcp.json

## ---------------------------------------------------------------- housekeeping

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf bin dist coverage.out coverage.txt

.PHONY: tidy
tidy: ## Tidy the module graph
	go mod tidy