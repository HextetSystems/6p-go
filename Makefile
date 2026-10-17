GO ?= go
BINARY ?= 6p

.DEFAULT_GOAL := help

.PHONY: help build install test test-race vet fmt check deps clean

help: ## Show available tasks
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <task>\n\nTasks:\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the client binary
	$(GO) build -trimpath -o $(BINARY) ./cmd/6p

install: ## Install the client with go install
	$(GO) install ./cmd/6p

test: ## Run all tests
	$(GO) test ./...

test-race: ## Run all tests with the race detector
	$(GO) test -race ./...

vet: ## Run Go static analysis
	$(GO) vet ./...

fmt: ## Format Go source files
	$(GO) fmt ./...

check: test-race vet ## Run the full local verification suite

deps: ## Download Go module dependencies
	$(GO) mod download

clean: ## Remove generated build and test artifacts
	$(GO) clean -testcache
	rm -f -- $(BINARY) coverage.out
