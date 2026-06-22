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

.DEFAULT_GOAL := help

.PHONY: help build test vet fmt run clean install uninstall \
        docker-build docker-up docker-down docker-logs

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

build: ## Build the agent binary into bin/ (CGO disabled)
	@mkdir -p $(BIN_DIR)
	cd $(GOAGENT_DIR) && $(GO_BUILD_ENV) $(GO) build -trimpath -o ../$(BIN) $(PKG)
	@echo "built $(BIN)"

test: ## Run unit tests
	cd $(GOAGENT_DIR) && $(GO) test ./...

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
