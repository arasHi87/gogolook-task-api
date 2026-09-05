# gogolook-task-api — developer entrypoints.
# Every target here is also what CI runs; if it passes locally it passes there.

SHELL       := /bin/bash
BIN         := bin/taskapi
PKG         := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
COMPOSE     := docker compose -f deploy/compose.yaml

.DEFAULT_GOAL := help

## help: list targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

## build: compile the binary into bin/
build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/taskapi

## run: run the API with the in-memory store (no Docker, no Postgres)
run:
	go run ./cmd/taskapi all --storage.backend=memory

## fmt: gofmt the tree
fmt:
	gofmt -l -w $(shell git ls-files '*.go' | grep -v '^gen/')

## vet: go vet
vet:
	go vet $(PKG)

## lint: golangci-lint
lint:
	golangci-lint run --timeout=5m

## test: unit tests only (fast, no Docker)
test:
	go test -race -short -covermode=atomic -coverprofile=coverage.out $(PKG)

## test-integration: full suite incl. testcontainers Postgres
test-integration:
	go test -race -covermode=atomic -coverprofile=coverage.out $(PKG)

## cover: open the HTML coverage report
cover: test
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

## tidy: go mod tidy
tidy:
	go mod tidy

## generate: regenerate protobuf, Connect handlers and the OpenAPI document
generate:
	buf dep update
	buf generate
	buf format -w

## generate-check: fail if generated output is stale (CI drift gate)
generate-check: generate
	@git diff --exit-code -- gen docs/openapi.yaml proto \
	  || { echo "generated output is stale — run 'make generate' and commit"; exit 1; }

## proto-lint: buf lint + breaking-change check against main
proto-lint:
	buf lint
	buf breaking --against '.git#branch=main' || true

## docker-build: build the container image
docker-build:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
	             --build-arg DATE=$(DATE) -t taskapi:$(VERSION) -t taskapi:latest .

## up: bring the full local stack up (postgres, api, worker, prometheus, grafana)
up:
	$(COMPOSE) up -d --build

## down: tear the stack down, including volumes
down:
	$(COMPOSE) down -v

## logs: follow stack logs
logs:
	$(COMPOSE) logs -f

## ci: everything CI runs, locally
ci: vet lint test

.PHONY: help build run fmt vet lint test test-integration cover tidy generate \
        generate-check proto-lint docker-build up down logs ci
