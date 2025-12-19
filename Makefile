SHELL := /bin/bash

default: help
.PHONY: default

help: ## Display this help screen (default)
	@grep -h "##" $(MAKEFILE_LIST) | grep -vE '^#|grep' | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' | sort
.PHONY: help

lint: ## Run linter against codebase
	@golangci-lint -v run
.PHONY: lint

# GO_VERSION should (generally) match go.mod version
build: export GO_VERSION               ?= 1.25.5
build: export XCADDY_VERSION           ?= 0.4.5
build: export CADDY_VERSION            ?= 2.10.2
build: export FRANKENPHP_CADDY_VERSION ?= 1.9.0
build: export BROTLI_CADDY_VERSION     ?= 1.0.1
build: lint ## Run 'docker composer build' to build caddy with plugin
	@docker compose build --build-arg GO_VERSION=$(GO_VERSION) --build-arg XCADDY_VERSION=$(XCADDY_VERSION) --build-arg CADDY_VERSION=$(CADDY_VERSION) --build-arg FRANKENPHP_CADDY_VERSION=$(FRANKENPHP_CADDY_VERSION) --build-arg BROTLI_CADDY_VERSION=$(BROTLI_CADDY_VERSION)
	@CID=$$(docker create caddy-sidekick-integration-test:latest);          \
		docker cp $$CID:/usr/local/bin/caddy ./bin/caddy >/dev/null 2>&1;   \
		docker rm $$CID >/dev/null
.PHONY: build

test: export TEST       ?= Test[^I]
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

test-integration: export TEST     ?= TestIntegration
test-integration: export TEST_DIR ?= ./integration-test/...
test-integration: test-integration-setup ## Run integration tests with Docker Compose
	@echo "Running integration tests..."
	@go test -v -timeout=90s -run "$(TEST)" $(TEST_DIR) 2>&1 | tee /tmp/sidekick-integration.log
	@echo "Integration test completed, see /tmp/sidekick-integration.log for details"
.PHONY: test-integration

test-integration-setup:
	@docker compose up -d --wait
.PHONY: test-integration-setup

test-integration-cleanup:
	@docker compose down || true
.PHONY: test-integration-cleanup

fmt: ## Run go-fmt against codebase
	@go fmt ./...
.PHONY: fmt

mod-download: ## Download go modules
	@go mod download
.PHONY: mod-download

mod-tidy: ## Make sure go modules are tidy
	@go mod tidy
.PHONY: mod-tidy

mod-update:
	@if [[ -n "${MODULE}" ]] && [[ -n "${MODULE_VERSION}" ]]; then          \
		echo "Running 'go list -m ${MODULE}@${MODULE_VERSION}' ...";        \
		GOPROXY=proxy.golang.org go list -m "${MODULE}@${MODULE_VERSION}";  \
	else                                                                    \
		echo "ERROR: Missing 'MODULE'/'MODULE_VERSION', cannot continue";   \
		exit 1;                                                             \
	fi
.PHONY: mod-update

clean: test-integration-cleanup ## Clean up repo
	@docker rmi -f caddy-sidekick-integration-test:latest 2>/dev/null || true
	@rm -f bin/caddy 2>/dev/null || true
.PHONY: clean

release: export MODULE         ?= github.com/honest-hosting/caddy-sidekick
release: export MODULE_VERSION ?=
release: mod-update ## Run release step(s) for module version: MODULE_VERSION=v0.0.1 make release
.PHONY: release
