SHELL := /bin/bash

default: help
.PHONY: default

help: ## Display this help screen (default)
	@grep -h "##" $(MAKEFILE_LIST) | grep -vE '^#|grep' | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' | sort
.PHONY: help

lint: ## Run linter against codebase
	@golangci-lint -v run
.PHONY: lint

# CADDY_VERSION should match the equivalent go.mod entry
build: export CADDY_VERSION       ?= v2.10.0
build: export WITH_CADDY_SIDEKICK ?= github.com/honest-hosting/caddy-sidekick=.
build: lint build-setup ## Run 'xcaddy' to build vault storage plugin in to caddy binary
	@xcaddy build ${CADDY_VERSION} --output bin/caddy --with ${WITH_CADDY_SIDEKICK}
.PHONY: build

build-setup:
	@if ! command -v xcaddy >/dev/null 2>&1; then                          \
		echo "ERROR: Missing 'xcaddy' binary on \$PATH, cannot continue";  \
	fi
.PHONY: build-setup

test: export TEST       ?= .*
test: export TEST_DIR   ?= ./...
test: export TEST_COUNT ?= 1
test: test-setup ## Run basic unit tests: TEST=.* TEST_DIR=./... TEST_COUNT=1 make test
	@go test -v -race -count=$(TEST_COUNT) -run "$(TEST)" $(TEST_DIR) 2>&1 | tee /tmp/sidekick-test.log
	@echo "Test completed, see /tmp/sidekick-test.log for details"
.PHONY: test

test-bench: export TEST       ?= ^$
test-bench: export TEST_BENCH ?= .
test-bench: export TEST_DIR   ?= ./...
test-bench: test-setup ## Run bench tests: TEST_BENCH="." TEST_DIR=./... make test-benchm
	@go test -run="$(TEST)" -bench="$(TEST_BENCH)" -benchmem -benchtime=10s -timeout=5m $(TEST_DIR) 2>&1 | tee /tmp/sidekick-bench.log
	@echo "Test completed, see /tmp/sidekick-bench.log for details"
.PHONY: test-bench

test-stress: export TEST       ?= TestConcurrent
test-stress: export TEST_DIR   ?= ./...
test-stress: export TEST_COUNT ?= 100
test-stress: test-setup ## Run stress tests: TEST=TestConcurrent TEST_DIR=./... TEST_COUNT=100 make test-stress
	@go test -v -race -count=$(TEST_COUNT) -run "$(TEST)" $(TEST_DIR) 2>&1 | tee /tmp/sidekick-stress.log
	@echo "Test completed, see /tmp/sidekick-stress.log for details"
.PHONY: test-stress

test-setup:
	@go clean -testcache
.PHONY: test-setup

fmt: ## Run go-fmt against codebase
	@go fmt ./...
.PHONY: fmt

mod-download: ## Download go modules
	@go mod download
.PHONY: mod-download

mod-tidy: ## Make sure go modules are tidy
	@go mod tidy
.PHONY: mod-tidy

mod-update: export MODULE ?=
mod-update: ## Update go proxy with latest module version: MODULE=github.com/honest-hosting/caddy-sidekick@v0.0.1 make mod-update
	@if [[ -n "${MODULE}" ]]; then                       \
		GOPROXY=proxy.golang.org go list -m ${MODULE};   \
	else                                                 \
		echo "ERROR: Missing 'MODULE', cannot continue"; \
		exit 1;                                          \
	fi
.PHONY: mod-update

clean: ## Clean up repo
	@rm -f bin/caddy 2>/dev/null || true
.PHONY: clean
