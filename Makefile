MODULE  := github.com/onrcanogul/liman
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# -X sets a package-level string variable at link time.
LDFLAGS := -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
           -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
           -X $(MODULE)/internal/buildinfo.Date=$(DATE)

.PHONY: build release test test-integration load-test lint fmt check clean dev-nginx dev-nginx-stop dev-cert lab-server lab-server-stop

build: ## Build the CLI (bin/liman) and the daemon (bin/limand)
	go build -ldflags "$(LDFLAGS)" -o bin/liman ./cmd/liman
	go build -ldflags "$(LDFLAGS)" -o bin/limand ./cmd/limand

# The binaries contain no C code (the SQLite driver is pure Go), so building
# for another operating system and CPU needs no cross-compiler: GOOS and
# GOARCH are all it takes. -trimpath leaves this machine's paths out.
# The daemon runs on Linux servers only; the CLI also on the Macs people
# deploy from.
RELEASE := limand-linux-amd64 limand-linux-arm64 \
           liman-linux-amd64 liman-linux-arm64 \
           liman-darwin-amd64 liman-darwin-arm64

release: ## Build into dist/: limand and liman for Linux, liman for macOS (amd64 and arm64)
	@for f in $(RELEASE); do \
		cmd=$${f%%-*}; rest=$${f#*-}; os=$${rest%-*}; arch=$${rest#*-}; \
		echo "dist/$$f"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$$f ./cmd/$$cmd || exit 1; \
	done

test: ## Run all tests with the race detector
	go test -race ./...

test-integration: ## Run tests that need a running Docker daemon (Pebble, Nginx, Caddy, Traefik)
	go test -race -count=1 -tags integration ./internal/docker ./internal/certs ./internal/proxy

load-test: ## Measure throughput and latency of the daemon (about a minute)
	go test -count=1 -tags load -run 'TestLoad' -v ./internal/server

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

lab-server: ## Boot a throwaway Ubuntu "server" in Docker to practise installing on
	./scripts/lab-server.sh start

lab-server-stop: ## Remove the lab server
	./scripts/lab-server.sh stop

clean:
	rm -rf bin dist
