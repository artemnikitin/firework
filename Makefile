BINARY    := firework-agent
MODULE    := github.com/artemnikitin/firework
BUILD_DIR := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS   := -s -w \
	-X '$(MODULE)/internal/version.Version=$(VERSION)' \
	-X '$(MODULE)/internal/version.Commit=$(COMMIT)' \
	-X '$(MODULE)/internal/version.BuildTime=$(BUILD_TIME)'

.PHONY: all build-all build build-enricher package-enricher build-scheduler package-scheduler build-fc-init build-linux-arm64 clean test test-verbose test-race lint vet fmt tidy run smoke-local help

all: build-all ## Alias for build-all

build-all: build build-fc-init build-linux-arm64 package-enricher package-scheduler ## Build all binaries

build: ## Build the agent binary
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/agent/
	@echo "Built $(BUILD_DIR)/$(BINARY)"

build-fc-init: ## Build the fc-init guest init binary (linux/arm64, statically linked)
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o $(BUILD_DIR)/fc-init ./cmd/fc-init/
	@echo "Built $(BUILD_DIR)/fc-init (ARM64, static)"

build-linux-arm64: ## Build agent and fc-init for linux/arm64 (for Packer AMI builds)
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/firework-agent-linux-arm64 ./cmd/agent/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o $(BUILD_DIR)/fc-init ./cmd/fc-init/
	@echo "Built $(BUILD_DIR)/firework-agent-linux-arm64 and $(BUILD_DIR)/fc-init"

build-enricher: ## Build the enricher Lambda binary (linux/arm64)
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -tags lambda.norpc -o $(BUILD_DIR)/bootstrap ./cmd/enricher/
	@echo "Built $(BUILD_DIR)/enricher (Lambda ARM64)"

package-enricher: build-enricher ## Package the enricher as a Lambda ZIP
	cd $(BUILD_DIR) && zip enricher.zip bootstrap
	@echo "Packaged $(BUILD_DIR)/enricher.zip"

build-scheduler: ## Build the scheduler Lambda binary (linux/arm64)
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -tags lambda.norpc -o $(BUILD_DIR)/bootstrap ./cmd/scheduler/
	@echo "Built $(BUILD_DIR)/scheduler (Lambda ARM64)"

package-scheduler: build-scheduler ## Package the scheduler as a Lambda ZIP
	cd $(BUILD_DIR) && zip scheduler.zip bootstrap
	@echo "Packaged $(BUILD_DIR)/scheduler.zip"

clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR)
	go clean -testcache

test: ## Run tests
	go test ./... -count=1

test-verbose: ## Run tests with verbose output
	go test ./... -v -count=1

test-race: ## Run tests with race detector
	go test ./... -race -count=1

test-cover: ## Run tests with coverage report
	go test ./... -coverprofile=coverage.out -count=1
	go tool cover -func=coverage.out
	@echo "\nTo view HTML report: go tool cover -html=coverage.out"

lint: vet ## Run linters (go vet + staticcheck if available)
	@which staticcheck > /dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed, skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"

vet: ## Run go vet
	go vet ./...

fmt: ## Format code
	gofmt -s -w .

tidy: ## Tidy and verify module dependencies
	go mod tidy
	go mod verify

run: build ## Build and run with example config
	$(BUILD_DIR)/$(BINARY) --config examples/agent.yaml

smoke-local: ## Run local smoke test with fake firecracker
	./scripts/smoke-local.sh

install: build ## Install the binary to $GOPATH/bin
	cp $(BUILD_DIR)/$(BINARY) $(shell go env GOPATH)/bin/$(BINARY)
	@echo "Installed to $(shell go env GOPATH)/bin/$(BINARY)"

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
