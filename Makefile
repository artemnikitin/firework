AGENT_BINARY := firework-agent
CONTROLPLANE_BINARY := firework-controlplane
FC_INIT_BINARY := fc-init
AGENT_LINUX_AMD64_BINARY := firework-agent-linux-amd64
AGENT_LINUX_ARM64_BINARY := firework-agent-linux-arm64
CONTROLPLANE_LINUX_AMD64_BINARY := firework-controlplane-linux-amd64
CONTROLPLANE_LINUX_ARM64_BINARY := firework-controlplane-linux-arm64
FC_INIT_LINUX_AMD64_BINARY := fc-init-linux-amd64
FC_INIT_LINUX_ARM64_BINARY := fc-init-linux-arm64
CONTROLPLANE_IMAGE ?= ghcr.io/artemnikitin/firework-controlplane
IMAGE_TAG ?= dev
CONTROLPLANE_PLATFORMS ?= linux/amd64,linux/arm64
MODULE    := github.com/artemnikitin/firework
BUILD_DIR := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS   := -s -w \
	-X '$(MODULE)/internal/version.Version=$(VERSION)' \
	-X '$(MODULE)/internal/version.Commit=$(COMMIT)' \
	-X '$(MODULE)/internal/version.BuildTime=$(BUILD_TIME)'

.PHONY: all build-all build build-controlplane build-fc-init build-linux-amd64 build-linux-arm64 clean test test-verbose test-race lint vet fmt tidy run smoke-local docker-build-controlplane-image docker-push-controlplane-image push-controlplane-image help

all: build-all ## Alias for build-all

build-all: build-agent build-controlplane build-linux-amd64 build-linux-arm64 ## Build all binaries for native + linux amd64/arm64

build-agent: ## Build the agent binary
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(AGENT_BINARY) ./cmd/agent/
	@echo "Built $(BUILD_DIR)/$(AGENT_BINARY)"

build-controlplane: ## Build the control-plane binary
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(CONTROLPLANE_BINARY) ./cmd/controlplane/
	@echo "Built $(BUILD_DIR)/$(CONTROLPLANE_BINARY)"

build-fc-init: ## Build fc-init guest init binary (linux/arm64, static) + compatibility alias
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o $(BUILD_DIR)/$(FC_INIT_LINUX_ARM64_BINARY) ./cmd/fc-init/
	cp $(BUILD_DIR)/$(FC_INIT_LINUX_ARM64_BINARY) $(BUILD_DIR)/$(FC_INIT_BINARY)
	@echo "Built $(BUILD_DIR)/$(FC_INIT_LINUX_ARM64_BINARY) and $(BUILD_DIR)/$(FC_INIT_BINARY) (ARM64, static)"

build-linux-amd64: ## Build all binaries for linux/amd64
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(AGENT_LINUX_AMD64_BINARY) ./cmd/agent/
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(CONTROLPLANE_LINUX_AMD64_BINARY) ./cmd/controlplane/
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o $(BUILD_DIR)/$(FC_INIT_LINUX_AMD64_BINARY) ./cmd/fc-init/
	@echo "Built $(BUILD_DIR)/$(AGENT_LINUX_AMD64_BINARY), $(BUILD_DIR)/$(CONTROLPLANE_LINUX_AMD64_BINARY) and $(BUILD_DIR)/$(FC_INIT_LINUX_AMD64_BINARY)"

build-linux-arm64: build-fc-init ## Build all binaries for linux/arm64 (for Packer AMI builds)
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(AGENT_LINUX_ARM64_BINARY) ./cmd/agent/
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(CONTROLPLANE_LINUX_ARM64_BINARY) ./cmd/controlplane/
	@echo "Built $(BUILD_DIR)/$(AGENT_LINUX_ARM64_BINARY), $(BUILD_DIR)/$(CONTROLPLANE_LINUX_ARM64_BINARY) and $(BUILD_DIR)/$(FC_INIT_LINUX_ARM64_BINARY)"

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
	$(BUILD_DIR)/$(AGENT_BINARY) --config examples/agent.yaml

smoke-local: ## Run local smoke test with fake firecracker
	./scripts/smoke-local.sh

docker-build-controlplane-image: ## Build control-plane image locally (linux/amd64)
	docker buildx build --platform linux/amd64 --file Dockerfile.controlplane \
		--build-arg VERSION="$(VERSION)" \
		--build-arg COMMIT="$(COMMIT)" \
		--build-arg BUILD_TIME="$(BUILD_TIME)" \
		--tag $(CONTROLPLANE_IMAGE):$(IMAGE_TAG) \
		--load .

docker-push-controlplane-image: ## Build and push multi-arch control-plane image
	PLATFORMS="$(CONTROLPLANE_PLATFORMS)" \
	VERSION="$(VERSION)" \
	COMMIT="$(COMMIT)" \
	BUILD_TIME="$(BUILD_TIME)" \
	./scripts/push-controlplane-image.sh "$(CONTROLPLANE_IMAGE):$(IMAGE_TAG)"

push-controlplane-image: ## Push image via helper script (requires image tag vars)
	./scripts/push-controlplane-image.sh "$(CONTROLPLANE_IMAGE):$(IMAGE_TAG)"

install: build ## Install the binary to $GOPATH/bin
	cp $(BUILD_DIR)/$(AGENT_BINARY) $(shell go env GOPATH)/bin/$(AGENT_BINARY)
	@echo "Installed to $(shell go env GOPATH)/bin/$(AGENT_BINARY)"

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
