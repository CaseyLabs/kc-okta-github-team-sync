SHELL := /bin/bash
ENV_FILE ?= .env
ASSERTION_ONLY ?= true
export ASSERTION_ONLY
BINARY := bin/sync
DOCKER ?= docker
DOCKER_COMPOSE ?= $(DOCKER) compose
DOCKER_SERVICE ?= dev
DOCKER_RUN ?= $(DOCKER_COMPOSE) run --rm --no-deps -T

ifneq ("$(wildcard $(ENV_FILE))","")
ENV_FILE_FLAG := --env-file $(ENV_FILE)
endif

.PHONY: build test run run-debug dryrun test-okta diagnose

build: ## Build the sync binary.
	@$(DOCKER_RUN) $(DOCKER_SERVICE) sh -lc 'mkdir -p $(dir $(BINARY)) && go build -o $(BINARY) ./cmd/sync'

test: ## Run Go tests.
	@$(DOCKER_RUN) $(DOCKER_SERVICE) go test ./...

RUN_FLAGS ?=

run: ## Execute the sync with configured environment.
	@$(DOCKER_RUN) $(ENV_FILE_FLAG) $(DOCKER_SERVICE) go run ./cmd/sync $(RUN_FLAGS)

run-debug: ## Execute the sync with HTTP debugging enabled.
	@$(MAKE) run RUN_FLAGS="--log-level=debug --http-debug"

dryrun: ## Execute the sync in dry-run mode.
	@$(DOCKER_RUN) $(ENV_FILE_FLAG) -e DRY_RUN=true $(DOCKER_SERVICE) go run ./cmd/sync

DIAG_FLAGS ?=

diagnose: ## Inspect Okta groups, GitHub teams, and Team Sync mappings.
	@$(DOCKER_RUN) $(ENV_FILE_FLAG) $(DOCKER_SERVICE) go run ./cmd/diagnose $(DIAG_FLAGS)

test-okta: ## Generate a client assertion and request an Okta access token using local env vars.
	@$(DOCKER_RUN) $(ENV_FILE_FLAG) -e ASSERTION_ONLY=$(ASSERTION_ONLY) $(DOCKER_SERVICE) bash -lc '\
		set -euo pipefail; \
		ASSERTION_FLAG="$${ASSERTION_ONLY:-}"; \
		case "$${ASSERTION_FLAG,,}" in \
			1|true|yes) set -- --assertion-only ;; \
			*) set -- ;; \
		esac; \
		exec go run ./cmd/okta-test "$$@"'
