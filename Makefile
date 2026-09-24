SHELL := /bin/bash
.DEFAULT_GOAL := help

# Load .env when present so `make dev` and `make test` pick up local settings.
ifneq (,$(wildcard .env))
include .env
export
endif

VERSION   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
GO        ?= go
PNPM      ?= $(shell command -v pnpm 2>/dev/null || echo "npx --yes pnpm@12")
SQLC      ?= $(shell command -v sqlc 2>/dev/null || echo "$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1")
IMAGE_REG ?= ghcr.io/local
LDFLAGS   := -s -w -X main.version=$(VERSION)

export V2I_TEST_DATABASE_URL ?= postgres://v2i:v2i@localhost:55432/v2i?sslmode=disable
export V2I_TEST_S3_ENDPOINT   ?= http://localhost:59000

.PHONY: help
help: ## Show targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

## ---- local development ----

.PHONY: deps-up deps-down
deps-up: ## Start Postgres + MinIO (docker compose)
	docker compose up -d --wait postgres minio
	docker compose up minio-init

deps-down: ## Stop Postgres + MinIO (and the studio sidecar)
	docker compose --profile '*' down

.PHONY: migrate dev dev-fake web-dev
migrate: ## Run River + goose migrations
	$(GO) run ./cmd/v2i migrate

# Studio backgrounds: `make studio-up`, then V2I_STUDIO_URL=http://127.0.0.1:4100 in .env
# (or V2I_STUDIO_URL=fake for the built-in fake segmenter without the sidecar).
dev: deps-up migrate ## API + embedded worker on :4000 (analyzer from V2I_ANALYZER, studio from V2I_STUDIO_URL)
	$(GO) run ./cmd/v2i serve --embed-worker

dev-fake: deps-up migrate ## Same as dev but with the free, deterministic fake analyzer
	$(GO) run ./cmd/v2i serve --embed-worker --analyzer=fake

web-dev: ## Next.js dev server on :3000 for the cars domain
	cd web && DOMAIN_KEY=cars API_URL=http://localhost:4000 $(PNPM) dev

## ---- studio sidecar (services/studio; optional) ----

.PHONY: studio-models studio-build studio-up studio-down studio-test
studio-models: ## Fetch + verify the sidecar models into services/studio/models (~0.6 GB, not in git)
	./services/studio/fetch-models.sh

studio-build: studio-models ## Build the sidecar image (v2i-studio:dev)
	docker compose --profile studio build studio

studio-up: studio-models ## Sidecar on 127.0.0.1:4100, waits until the models are loaded
	docker compose --profile studio up -d --build --wait studio

studio-down: ## Stop the sidecar
	docker compose --profile studio rm -sf studio

# Python 3.12+ venv outside services/studio, so it never lands in the image build context.
STUDIO_VENV ?= data/studio-venv

$(STUDIO_VENV)/.deps: services/studio/requirements.txt services/studio/requirements-dev.txt
	python3 -m venv $(STUDIO_VENV)
	$(STUDIO_VENV)/bin/python -m pip install -q -r services/studio/requirements-dev.txt
	touch $@

studio-test: $(STUDIO_VENV)/.deps ## Sidecar unit tests (pytest in data/studio-venv; no models needed)
	cd services/studio && $(abspath $(STUDIO_VENV))/bin/python -m pytest -q

## ---- code generation ----

.PHONY: gen gen-sqlc gen-openapi gen-web
gen: gen-sqlc gen-openapi gen-web ## sqlc code, api/openapi.json, web/lib/api.d.ts

gen-sqlc: ## Regenerate internal/store/sqlcdb from migrations + queries
	$(SQLC) generate

gen-openapi: ## Write api/openapi.json from the Go handlers
	$(GO) run ./cmd/v2i openapi -o api/openapi.json

gen-web: gen-openapi ## Regenerate web/lib/api.d.ts from api/openapi.json
	cd web && $(PNPM) gen:api

## ---- quality ----

.PHONY: test test-short lint vet fmt fixtures e2e e2e-prod
test: ## Go tests (Postgres tests need `make deps-up`)
	$(GO) test -race -count=1 ./...

test-short: ## Go tests without Postgres/MinIO
	V2I_TEST_DATABASE_URL= V2I_TEST_S3_ENDPOINT= $(GO) test -count=1 ./...

vet: ## go vet
	$(GO) vet ./...

lint: vet ## golangci-lint (install: https://golangci-lint.run)
	golangci-lint run ./...

fmt: ## gofmt
	gofmt -w $$(git ls-files '*.go' | grep -v sqlcdb)

fixtures: ## Generate synthetic fixture videos into testdata/videos
	./scripts/gen-fixtures.sh

e2e: ## Playwright e2e (next dev) against a running `make dev-fake`
	cd web && $(PNPM) test:e2e

e2e-prod: ## Playwright e2e against the production standalone build
	cd web && $(PNPM) test:e2e:prod

## ---- deploy ----

.PHONY: deploy-home
deploy-home: ## Build, load and apply to the home k3s cluster (deploy/k8s/README.md)
	./deploy/k8s/deploy.sh

## ---- build ----

.PHONY: build image image-v2i image-web
build: ## Build bin/v2i
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/v2i ./cmd/v2i

image: image-v2i image-web ## Build both container images

image-v2i:
	docker build -f deploy/docker/Dockerfile.v2i --build-arg VERSION=$(VERSION) -t $(IMAGE_REG)/v2i:$(VERSION) .

image-web:
	docker build -f deploy/docker/Dockerfile.web -t $(IMAGE_REG)/web:$(VERSION) web
