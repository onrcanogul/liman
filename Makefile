MODULE  := github.com/onrcanogul/liman
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# -X sets a package-level string variable at link time.
LDFLAGS := -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
           -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
           -X $(MODULE)/internal/buildinfo.Date=$(DATE)

.PHONY: build test test-integration lint fmt check clean dev-nginx dev-nginx-stop dev-cert

build: ## Build the CLI into bin/liman
	go build -ldflags "$(LDFLAGS)" -o bin/liman ./cmd/liman

test: ## Run all tests with the race detector
	go test -race ./...

test-integration: ## Run tests that need a running Docker daemon
	go test -race -count=1 -tags integration ./internal/docker

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format all Go files
	gofmt -w .

check: lint test ## Everything that must pass before a commit

dev-nginx: ## Start a local Nginx in Docker that liman can manage
	./scripts/dev-nginx.sh start

dev-nginx-stop: ## Stop the local Nginx
	./scripts/dev-nginx.sh stop

dev-cert: ## Create a self-signed certificate: make dev-cert DOMAIN=whoami.localhost
	./scripts/dev-cert.sh $(DOMAIN)

clean:
	rm -rf bin
