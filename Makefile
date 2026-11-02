# Tools are pinned and run with `go run`, so CI and laptops use identical versions.
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GOVULNCHECK   := go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
SQLC          := go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
MOCKERY       := go run github.com/vektra/mockery/v3@v3.9.0
REDOCLY       := npx --yes @redocly/cli@1.34.0

# Local development connection strings (throwaway values from docker-compose.yml).
DEV_OWNER_URL   := postgres://core_owner:dev_owner_password@127.0.0.1:5432/core?sslmode=disable
DEV_APP_URL     := postgres://core_app:dev_app_password@127.0.0.1:5432/core?sslmode=disable
TEST_S3_ENV     := TEST_S3_ENDPOINT=127.0.0.1:9000 TEST_S3_BUCKET=core-dev TEST_S3_ACCESS_KEY=dev_s3_access TEST_S3_SECRET_KEY=dev_s3_secret_key
TEST_ADMIN_URL  := postgres://core_owner:dev_owner_password@127.0.0.1:5432/postgres?sslmode=disable

DEV_ENV := APP_ENV=development HTTP_ADDR=127.0.0.1:8088 DATABASE_URL='$(DEV_APP_URL)' MIGRATION_DATABASE_URL='$(DEV_OWNER_URL)' \
           SECRET_KEY=dev-only-secret-key-not-for-production-use

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: up
up: ## Start Postgres and S3, apply migrations, grant the dev runtime user
	docker compose up -d --wait
	$(DEV_ENV) go run ./cmd/app migrate up
	docker compose exec -T postgres psql -U core_owner -d core -qc 'GRANT app_runtime TO core_app'
	echo 's3.bucket.create -name core-dev' | docker compose exec -T s3 weed shell >/dev/null

.PHONY: down
down: ## Stop containers (keeps data)
	docker compose down

.PHONY: reset
reset: ## Stop containers and delete local data
	docker compose down -v

.PHONY: build
build: ## Build the app binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/app ./cmd/app

.PHONY: run-api
run-api: ## Run the API against local containers
	$(DEV_ENV) go run ./cmd/app api

.PHONY: run-worker
run-worker: ## Run the worker against local containers
	$(DEV_ENV) go run ./cmd/app worker

.PHONY: test
test: ## Run all tests, including database and S3 tests against local containers
	$(TEST_S3_ENV) TEST_DATABASE_URL='$(TEST_ADMIN_URL)' go test -race -count=1 ./...

.PHONY: test-unit
test-unit: ## Run tests that need no database
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Test with coverage report
	TEST_DATABASE_URL='$(TEST_ADMIN_URL)' go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Lint Go (incl. gosec), check vulnerabilities, validate seeds and OpenAPI
	$(GOLANGCI_LINT) run ./...
	$(GOVULNCHECK) ./...
	go run ./cmd/app seed validate configs/seed
	$(REDOCLY) lint --config api/redocly.yaml api/openapi.yaml

.PHONY: fmt
fmt: ## Format code
	$(GOLANGCI_LINT) fmt ./...

# Each module owns internal/<module>/sqlc.yaml (queries in ./queries, generated
# code in ./internal/db), so teams never edit a shared config.
SQLC_CONFIGS := $(wildcard internal/*/sqlc.yaml)

.PHONY: sqlc
sqlc: ## Generate typed queries for every module
	@for f in $(SQLC_CONFIGS); do echo "sqlc $$f"; $(SQLC) generate -f $$f || exit 1; done

.PHONY: sqlc-check
sqlc-check: ## Fail if generated queries are out of date
	@for f in $(SQLC_CONFIGS); do $(SQLC) diff -f $$f || exit 1; done

.PHONY: mocks
mocks: ## Generate mocks for module contracts
	$(MOCKERY)
