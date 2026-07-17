# CloudlyNet Edge Agent — build & deploy
#
# Dev targets run on any host with Go installed. Production install on the edge
# box (Ubuntu 22.04) is handled by scripts/install.sh, which bootstraps Go if
# missing; `make install` / `make uninstall` simply delegate to those scripts.

GO          ?= go
GOAGENT_DIR := goagent
PKG         := ./cmd/agent
BINARY      := cloudlynet-agent
BIN_DIR     := bin
BIN         := $(BIN_DIR)/$(BINARY)

# Pure-Go SQLite (modernc.org/sqlite) => no CGO toolchain required.
GO_BUILD_ENV := CGO_ENABLED=0

# Scenario for `make e2e-scenario` (full list in `make help`).
SCENARIO ?= happy

.DEFAULT_GOAL := help

.PHONY: help build test test-suite vet fmt run clean install uninstall \
        docker-build docker-up docker-down docker-logs sync-manifest \
        e2e e2e-scenario e2e-all

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

build: ## Build the agent binary into bin/ (CGO disabled)
	@mkdir -p $(BIN_DIR)
	cd $(GOAGENT_DIR) && $(GO_BUILD_ENV) $(GO) build -trimpath -o ../$(BIN) $(PKG)
	@echo "built $(BIN)"

test: ## Run agent unit tests (goagent module)
	cd $(GOAGENT_DIR) && $(GO) test ./...

test-suite: ## Run testsuite unit tests (loggen generator)
	cd testsuite && $(GO) test ./...

vet: ## Run go vet
	cd $(GOAGENT_DIR) && $(GO) vet ./...

fmt: ## Format Go sources
	cd $(GOAGENT_DIR) && $(GO) fmt ./...

run: build ## Build and run locally against config/agent.yaml
	./$(BIN) --config config/agent.yaml

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

install: ## Install on this edge box via scripts/install.sh (needs root)
	./scripts/install.sh

uninstall: ## Remove the installed agent via scripts/uninstall.sh (needs root)
	./scripts/uninstall.sh

docker-build: ## Build the local test image
	docker compose build

docker-up: ## Start agent + testsuite mocks (local functional test)
	docker compose up -d --build

docker-down: ## Stop and remove the local test stack
	docker compose down -v

docker-logs: ## Tail edge agent container logs
	docker compose logs -f cloudlynet-edgeagent

sync-manifest: ## Copy the in-repo NanoLink manifest into testsuite's embedded asset (single source of truth)
	cp $(GOAGENT_DIR)/internal/cwmp/assets/nanolink_param_manifest.json testsuite/assets/nanolink_param_manifest.json
	@echo "synced testsuite/assets/nanolink_param_manifest.json from $(GOAGENT_DIR)/internal/cwmp/assets/"

e2e: ## End-to-end test: happy scenario (up -> assert /health ok -> down; add VERBOSE=1 for per-check detail)
	@./scripts/e2e.sh happy $(if $(filter 1,$(VERBOSE)),--verbose)

e2e-scenario: ## Run one scenario e2e (SCENARIO=happy|ftp-path-reject|ftp-auth-fail|ftp-conn-fail|ftp-timeout|atc-fault|reboot; VERBOSE=1 for detail)
	@./scripts/e2e.sh $(SCENARIO) $(if $(filter 1,$(VERBOSE)),--verbose)

e2e-all: ## End-to-end sweep across all 7 scenarios (VERBOSE=1 for per-check detail; non-zero exit if any fail)
	@./scripts/e2e.sh --all $(if $(filter 1,$(VERBOSE)),--verbose)
