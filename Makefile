SHELL := /bin/bash

BINARY  := bin/fakeagents
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/jahrulnr/fakeagents/internal/version.Version=$(VERSION) \
	-X github.com/jahrulnr/fakeagents/internal/version.Commit=$(COMMIT) \
	-X github.com/jahrulnr/fakeagents/internal/version.Date=$(DATE)

.PHONY: build test race vet fmt fmt-fix mod-fix lint quality fix fix-check check cover run init docker stress clean

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/fakeagents

test:
	go test ./...
	@if [ "$$(go env GOHOSTOS)" != "windows" ]; then \
		echo "Cross-compiling Windows amd64 tests (not executing them)"; \
		GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o /dev/null ./...; \
	fi

race:
	go test -race ./...

vet:
	go vet ./...
	@if [ "$$(go env GOHOSTOS)" != "windows" ]; then \
		echo "Vetting Windows amd64 packages"; \
		GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...; \
	fi

fmt:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then echo "gofmt required:"; echo "$$files"; exit 1; fi

fmt-fix:
	gofmt -w .

mod-fix:
	go mod tidy

lint: fmt vet

fix-check:
	go fix -diff ./...
	go mod tidy -diff

# fix applies every safe auto-fixer, then the quality gate verifies.
fix:
	@echo "==> go fix"
	go fix ./...
	@echo "==> gofmt"
	gofmt -w .
	@if command -v goimports >/dev/null 2>&1; then \
		echo "==> goimports"; goimports -w .; \
	else \
		echo "==> goimports not installed (skipped)"; \
	fi
	@echo "==> go mod tidy"
	go mod tidy

quality: fix
	$(MAKE) test
	$(MAKE) lint
	$(MAKE) race

check: lint fix-check test race

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

run: build
	./$(BINARY) serve -c configs/fakeagents.json

init: build
	./$(BINARY) init -o fakeagents.json --force

docker:
	docker build -t fakeagents:local .

stress:
	docker compose -f stress/docker-compose.yml up --build --abort-on-container-exit --exit-code-from clis

clean:
	rm -rf bin coverage.out
