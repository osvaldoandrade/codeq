# codeq build automation.
#
# `make ci` is the single gate for PRs. Everything CI checks is
# reachable from here so contributors can reproduce results locally.

SHELL          := /usr/bin/env bash
GO             := go
GOFLAGS        ?=
PKGS           := ./...
COVER_PROFILE  := coverage.out
COVER_HTML     := coverage.html
BIN_DIR        := bin
LDFLAGS        ?= -s -w
BUILD_TAGS     ?=
BASE_REV       ?= origin/main

GOLANGCI_LINT_VERSION    := v2.12.2
GOFUMPT_VERSION          := v0.7.0
GCI_VERSION              := v0.13.5
GOVULNCHECK_VERSION      := v1.1.4
GOSEC_VERSION            := v2.28.0
GO_TEST_COVERAGE_VERSION := v2.11.4

TOOL_BIN := $(CURDIR)/$(BIN_DIR)/tools
export PATH := $(TOOL_BIN):$(PATH)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Print this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} \
	      /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' \
	      $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: build-server build-cli ## Build all binaries into ./bin

.PHONY: build-server
build-server: ## Build the server binary.
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -tags "$(BUILD_TAGS)" -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/codeq-server ./cmd/server

.PHONY: build-cli
build-cli: ## Build the CLI binary.
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -tags "$(BUILD_TAGS)" -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/codeq ./cmd/codeq

.PHONY: install
install: ## Install both binaries into $$GOBIN.
	$(GO) install $(GOFLAGS) -tags "$(BUILD_TAGS)" -ldflags "$(LDFLAGS)" \
		./cmd/server ./cmd/codeq

##@ Quality

.PHONY: fmt
fmt: tools-fmt ## Format code (gofumpt + gci + goimports).
	@gofumpt -l -w .
	@gci write --skip-generated -s standard -s default \
		-s 'prefix(github.com/osvaldoandrade/codeq)' .

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet $(PKGS)

.PHONY: lint lint-audit
lint: tools-lint ## Reject lint findings introduced after BASE_REV.
	@golangci-lint run --timeout=5m --new-from-rev=$(BASE_REV) $(PKGS)

lint-audit: tools-lint ## Report the complete pre-existing lint inventory.
	@golangci-lint run --timeout=5m --issues-exit-code=0 $(PKGS)

##@ Test

.PHONY: test
test: ## Run unit tests with race detector.
	$(GO) test -short -race -shuffle=on -count=1 -timeout=15m \
		-coverprofile=$(COVER_PROFILE) -covermode=atomic $(PKGS)

.PHONY: test-short tenant-fuzz tenant-mutation
test-short: ## Run short unit tests (no race, faster local loop).
	$(GO) test -short -count=1 -timeout=5m $(PKGS)

tenant-fuzz: ## Fuzz canonical and legacy tenant claim resolution.
	$(GO) test ./internal/authclaims -run '^$$' -fuzz '^FuzzResolveTenantID$$' -fuzztime=10s

tenant-mutation: ## Mutation-test the tenant claim security boundary.
	$(GO) run github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0 unleash ./internal/authclaims --workers=2 --threshold-efficacy=80 --threshold-mcover=80

.PHONY: test-integration
test-integration: ## Run integration tests (-tags=integration).
	$(GO) test -tags=integration -race -count=1 -timeout=20m $(PKGS)

.PHONY: cover cover-check
cover: test cover-check ## Run tests and enforce coverage thresholds.

cover-check: tools-cover ## Enforce thresholds against an existing coverage profile.
	@go-test-coverage --config .testcoverage.yml

.PHONY: cover-html
cover-html: test ## Open the coverage report in a browser.
	$(GO) tool cover -html=$(COVER_PROFILE) -o $(COVER_HTML)
	@echo "wrote $(COVER_HTML)"

.PHONY: bench
bench: ## Run benchmarks.
	$(GO) test -run=^$$ -bench=. -benchmem -timeout=30m $(PKGS)

##@ Security

.PHONY: sec
sec: tools-sec ## Run security scanners (govulncheck + gosec).
	@govulncheck $(PKGS)
	@gosec -quiet -track-suppressions -nosec-require-justification \
		-nosec-require-rules -severity=medium -confidence=medium $(PKGS)

##@ CI

.PHONY: ci
ci: vet lint test cover-check sec ## Full PR gate.

##@ Tooling

.PHONY: tools
tools: tools-fmt tools-lint tools-cover tools-sec ## Install all dev tools.

.PHONY: tools-fmt
tools-fmt: $(TOOL_BIN)/gofumpt $(TOOL_BIN)/gci

.PHONY: tools-lint
tools-lint: $(TOOL_BIN)/golangci-lint

.PHONY: tools-cover
tools-cover: $(TOOL_BIN)/go-test-coverage

.PHONY: tools-sec
tools-sec: $(TOOL_BIN)/govulncheck $(TOOL_BIN)/gosec

$(TOOL_BIN)/golangci-lint:
	@mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) $(GO) install \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(TOOL_BIN)/gofumpt:
	@mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) $(GO) install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)

$(TOOL_BIN)/gci:
	@mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) $(GO) install github.com/daixiang0/gci@$(GCI_VERSION)

$(TOOL_BIN)/govulncheck:
	@mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

$(TOOL_BIN)/gosec:
	@mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) $(GO) install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)

$(TOOL_BIN)/go-test-coverage:
	@mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) $(GO) install \
		github.com/vladopajic/go-test-coverage/v2@$(GO_TEST_COVERAGE_VERSION)

##@ Housekeeping

.PHONY: tidy
tidy: ## go mod tidy.
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build artifacts and coverage outputs.
	rm -rf $(BIN_DIR) $(COVER_PROFILE) $(COVER_HTML)
