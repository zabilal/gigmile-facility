.DEFAULT_GOAL := help
SHELL := /bin/bash

DATABASE_URL ?= postgres://facility:facility@localhost:5433/facility?sslmode=disable
SEED_COUNT   ?= 100000
export DATABASE_URL

.PHONY: help up down migrate seed run test test-unit test-integration load fmt tidy lint clean

help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

up: ## Start Postgres and apply migrations
	docker compose up -d --wait
	$(MAKE) migrate

down: ## Stop Postgres and discard its volume
	docker compose down -v

migrate: ## Apply database migrations
	go run ./cmd/migrate up

seed: ## Seed SEED_COUNT deployments (default 100000)
	go run ./cmd/seed -count=$(SEED_COUNT)

run: ## Run the API server
	go run ./cmd/api

test: ## Run all tests (unit + integration; integration needs Docker)
	go test ./... -race -count=1

test-unit: ## Run unit tests only (no Docker required)
	go test ./internal/domain/... -race -count=1

test-integration: ## Run integration tests only (needs Docker)
	go test ./internal/store/... -race -count=1

load: ## Drive the webhook at 100k requests/minute and report latency
	go run ./cmd/loadgen -rate=100000 -duration=60s -customers=$(SEED_COUNT)

fmt: ## Format and vet
	go fmt ./...
	go vet ./...

tidy: ## Tidy module dependencies
	go mod tidy

clean: ## Remove build artefacts
	rm -rf bin/
