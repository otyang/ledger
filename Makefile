# Settings can come from the environment or a local .env file (see .env.example).
-include .env
export

VERSION ?= dev
BINARY  := bin/ledgerd
IMAGE   ?= ledgerd
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the ledgerd binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/ledgerd

.PHONY: run
run: ## Run the service (reads .env)
	go run -ldflags "$(LDFLAGS)" ./cmd/ledgerd

.PHONY: test
test: ## Run all tests (starts an embedded Postgres)
	go test ./...

.PHONY: test-race
test-race: ## Run all tests with the race detector
	go test -race ./...

.PHONY: check
check: ## Check formatting, vet, and run tests with the race detector
	@test -z "$$(gofmt -l .)" || { echo "Not gofmt-formatted:"; gofmt -l .; exit 1; }
	go vet ./...
	go test -race ./...

.PHONY: fmt
fmt: ## Format all Go code
	gofmt -w .

.PHONY: docker-build
docker-build: ## Build the Docker image (IMAGE, VERSION)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

.PHONY: site
site: ## Rebuild docs/index.html (the GitHub Pages home) from README.md
	python3 tools/site/build.py

.PHONY: clean
clean: ## Remove build output
	rm -rf bin
