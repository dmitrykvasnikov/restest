# restest — development commands.
#
# Tools are pinned here and fetched by `go run` on demand, so a checkout needs
# nothing installed but Go and Docker, and everyone runs the same versions.

GOOSE_VERSION    ?= v3.27.3
SQLC_VERSION     ?= v1.31.1
GOLANGCI_VERSION ?= v2.12.2
TAILWIND_VERSION ?= v4.3.3
HTMX_VERSION     ?= 2.0.10
CODEMIRROR_VERSION ?= 5.65.20

GOOSE    := go run github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION)
SQLC     := go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

# The database as seen from the host: compose publishes it on loopback. Override
# in the environment to point somewhere else.
RESTEST_DATABASE_URL ?= postgres://restest:restest@localhost:5432/restest?sslmode=disable
export RESTEST_DATABASE_URL

# The goose CLI takes its connection from the environment.
export GOOSE_DRIVER    := postgres
export GOOSE_DBSTRING  := $(RESTEST_DATABASE_URL)
export GOOSE_MIGRATION_DIR := migrations

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available commands
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk -F':.*?## ' '{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# --- application --------------------------------------------------------------

.PHONY: run
run: ## Run the server on the host, against the database in compose
	go run ./cmd/restest

.PHONY: build
build: ## Build the binary into bin/
	go build -trimpath -o bin/restest ./cmd/restest

# --- quality ------------------------------------------------------------------

.PHONY: test
test: ## Run the unit tests
	go test -race ./...

.PHONY: test-integration
test-integration: ## Run every test, including those needing Docker
	go test -race -tags=integration -count=1 ./...

.PHONY: lint
lint: ## Vet and lint
	go vet ./...
	$(GOLANGCI) run

.PHONY: fmt
fmt: ## Format the source
	gofmt -s -w .

.PHONY: tidy
tidy: ## Tidy the module
	go mod tidy

# --- front-end assets ---------------------------------------------------------
#
# No npm, ever (DESIGN.md §9.2). Tailwind is one downloaded binary, HTMX is one
# vendored file, and the generated stylesheet is committed so that `go build`
# needs neither of them.

WEB          := internal/web
TAILWIND_BIN := bin/tailwindcss
# Release assets are named for the platform: linux-x64, macos-arm64, and so on.
TAILWIND_OS   := $(shell uname -s | tr '[:upper:]' '[:lower:]' | sed 's/darwin/macos/')
TAILWIND_ARCH := $(shell uname -m | sed 's/x86_64/x64/; s/aarch64/arm64/')
TAILWIND_URL  := https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(TAILWIND_OS)-$(TAILWIND_ARCH)

$(TAILWIND_BIN):
	@mkdir -p $(dir $@)
	curl -fsSL -o $@ $(TAILWIND_URL)
	chmod +x $@

.PHONY: assets
assets: $(TAILWIND_BIN) ## Rebuild the stylesheet from the templates
	@mkdir -p $(WEB)/static/css
	$(TAILWIND_BIN) -i $(WEB)/tailwind.css -o $(WEB)/static/css/app.css --minify

.PHONY: vendor-htmx
vendor-htmx: ## Re-download the vendored HTMX (only when changing versions)
	@mkdir -p $(WEB)/static/js
	curl -fsSL -o $(WEB)/static/js/htmx.min.js \
		https://unpkg.com/htmx.org@$(HTMX_VERSION)/dist/htmx.min.js

# CodeMirror 5 rather than 6, because 6 is published only as ES modules that
# have to be bundled, and a bundler means npm — see DESIGN.md §9.3. These are
# the plain script files, fetched and committed, and nothing builds them.
CODEMIRROR_FILES := lib/codemirror.js lib/codemirror.css mode/javascript/javascript.js \
                    addon/edit/closebrackets.js addon/edit/matchbrackets.js

.PHONY: vendor-codemirror
vendor-codemirror: ## Re-download the vendored CodeMirror (only when changing versions)
	@mkdir -p $(WEB)/static/vendor/codemirror
	@for f in $(CODEMIRROR_FILES); do \
		curl -fsSL -o $(WEB)/static/vendor/codemirror/$$(basename $$f) \
			https://cdn.jsdelivr.net/npm/codemirror@$(CODEMIRROR_VERSION)/$$f || exit 1; \
		echo "  $$f"; \
	done

# --- database -----------------------------------------------------------------
#
# The application applies migrations itself at startup; these targets are for
# working on them — rolling one back, checking what is applied, adding a new one.

.PHONY: migrate
migrate: ## Apply pending migrations
	$(GOOSE) up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	$(GOOSE) down

.PHONY: migrate-status
migrate-status: ## Show which migrations are applied
	$(GOOSE) status

.PHONY: migrate-new
migrate-new: ## Create a migration: make migrate-new NAME=add_widgets
	@test -n "$(NAME)" || { echo "usage: make migrate-new NAME=add_widgets"; exit 1; }
	$(GOOSE) create $(NAME) sql

.PHONY: sqlc
sqlc: ## Regenerate the database layer from migrations/ and internal/core/queries/
	$(SQLC) generate

# --- compose ------------------------------------------------------------------

# The image build cannot see .git, so the commit is passed in and linked into
# the binary, which logs it at startup.
export RESTEST_REVISION := $(shell git rev-parse --short HEAD 2>/dev/null)$(shell test -z "$$(git status --porcelain 2>/dev/null)" || echo -dirty)

.PHONY: up
up: ## Build and start the whole stack
	docker compose up --build -d

.PHONY: down
down: ## Stop the stack, keeping the data volume
	docker compose down

.PHONY: logs
logs: ## Follow the application log
	docker compose logs -f app

# --- backup -------------------------------------------------------------------
#
# pg_dump and pg_restore run inside the database container, so the host needs
# nothing but Docker. See scripts/ for what each one does and the README for the
# procedure, including the drill that proves a backup is restorable.

.PHONY: backup
backup: ## Dump the database into backups/
	@scripts/backup.sh

.PHONY: restore
restore: ## Restore the database: make restore FILE=backups/restest-….dump
	@test -n "$(FILE)" || { echo "usage: make restore FILE=backups/restest-….dump"; exit 1; }
	@scripts/restore.sh $(FILE)
