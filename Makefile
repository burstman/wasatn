# WasaTN developer tasks.
#
# Run `make help` for the list. Generated code (sqlc output, *_templ.go, CSS)
# is committed so a fresh clone builds without a Node or Go toolchain.

SHELL := /bin/bash
.DEFAULT_GOAL := help

GO      ?= go
SQLC    ?= sqlc
TEMPL   ?= templ
NPX     ?= npx

BIN_DIR := bin

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# --- Code generation -------------------------------------------------------

.PHONY: generate
generate: generate-sqlc generate-templ css vendor ## Run every generator

.PHONY: generate-sqlc
generate-sqlc: ## Regenerate sqlc database code
	$(SQLC) generate

.PHONY: generate-templ
generate-templ: ## Regenerate templ components
	$(TEMPL) generate

.PHONY: css
css: ## Build Tailwind + daisyUI CSS
	$(NPX) @tailwindcss/cli -i ./static/css/app.src.css -o ./static/css/app.css --minify

.PHONY: css-watch
css-watch: ## Rebuild CSS on change
	$(NPX) @tailwindcss/cli -i ./static/css/app.src.css -o ./static/css/app.css --watch

.PHONY: templ-watch
templ-watch: ## Regenerate templ on change
	$(TEMPL) generate --watch

.PHONY: vendor
vendor: ## Copy htmx and Alpine into static/vendor
	node scripts/vendor.mjs

# --- Database --------------------------------------------------------------

.PHONY: migrate
migrate: ## Apply all migrations (app schema + River)
	$(GO) run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back one app migration
	$(GO) run ./cmd/migrate down

.PHONY: migrate-new
migrate-new: ## Create a migration: make migrate-new name=add_thing
	@test -n "$(name)" || (echo "usage: make migrate-new name=<description>" && exit 1)
	$(GO) run ./cmd/migrate sql $(name)

.PHONY: migrate-version
migrate-version: ## Print migration versions
	$(GO) run ./cmd/migrate version

# --- Run -------------------------------------------------------------------

.PHONY: run
run: ## Run the web server
	$(GO) run ./cmd/server

.PHONY: worker
worker: ## Run the job worker
	$(GO) run ./cmd/worker

AIR ?= air

.PHONY: dev
dev: ## Run server and worker with hot reload (needs air)
	@command -v $(AIR) >/dev/null || { echo "air is not installed: go install github.com/air-verse/air@latest"; exit 1; }
	@trap 'kill 0' EXIT INT TERM; \
		$(AIR) -c .air.toml & \
		$(AIR) -c .air.worker.toml & \
		wait

.PHONY: dev-server
dev-server: ## Run only the web server with hot reload (needs air)
	$(AIR) -c .air.toml

.PHONY: dev-worker
dev-worker: ## Run only the worker with hot reload (needs air)
	$(AIR) -c .air.worker.toml

# --- Quality ---------------------------------------------------------------

.PHONY: test
test: ## Run tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run tests with the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## Run tests and report coverage
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w .

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: tidy
tidy: ## Tidy go modules
	$(GO) mod tidy

# --- Build -----------------------------------------------------------------

.PHONY: build
build: ## Build the server and worker binaries
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/server ./cmd/server
	$(GO) build -o $(BIN_DIR)/worker ./cmd/worker

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) coverage.out
