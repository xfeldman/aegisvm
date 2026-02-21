# Aegis Makefile
# Build aegisd, aegis CLI, harness, and base rootfs

SHELL := /bin/bash

# Go settings
GO := go
GOFLAGS := -trimpath

# Output directory
BIN_DIR := bin

# Host platform (for aegisd and aegis CLI)
HOST_OS := $(shell uname -s | tr A-Z a-z)
HOST_ARCH := $(shell uname -m)
ifeq ($(HOST_ARCH),aarch64)
	HOST_ARCH := arm64
endif
ifeq ($(HOST_ARCH),x86_64)
	HOST_ARCH := amd64
endif

# Harness is always built for Linux ARM64 (runs inside the VM)
HARNESS_OS := linux
HARNESS_ARCH := arm64

# libkrun paths (macOS via Homebrew)
ifeq ($(HOST_OS),darwin)
	CGO_CFLAGS := -I/opt/homebrew/include
	CGO_LDFLAGS := -L/opt/homebrew/lib
endif

.PHONY: all aegisd aegis harness vmm-worker mcp base-rootfs clean test test-unit test-m2 test-m3 test-network integration

all: aegisd aegis harness vmm-worker mcp

# aegisd — the daemon (no cgo needed in M0, cgo is in vmm-worker)
aegisd:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegisd ./cmd/aegisd

# aegis — the CLI
aegis:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis ./cmd/aegis

# aegis-harness — guest PID 1 (static Linux ARM64 binary)
harness:
	@mkdir -p $(BIN_DIR)
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-harness ./cmd/aegis-harness

# aegis-vmm-worker — per-VM helper process (cgo for libkrun)
# On macOS, must be signed with com.apple.security.hypervisor entitlement
vmm-worker:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 \
	CGO_CFLAGS="$(CGO_CFLAGS)" \
	CGO_LDFLAGS="$(CGO_LDFLAGS)" \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-vmm-worker ./cmd/aegis-vmm-worker
ifeq ($(HOST_OS),darwin)
	codesign --sign - --entitlements entitlements.plist --force $(BIN_DIR)/aegis-vmm-worker
endif

# aegis-mcp — MCP server for LLM integration
mcp:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-mcp ./cmd/aegis-mcp

# Base rootfs — Alpine ARM64 with harness baked in
# Requires: brew install e2fsprogs (for mkfs.ext4)
base-rootfs: harness
	$(MAKE) -C base

# Run unit tests
test:
	$(GO) test ./...

# Run unit tests only (internal packages)
test-unit:
	$(GO) test ./internal/...

# Run M2 integration tests only
test-m2: all
ifdef SHORT
	$(GO) test -tags integration -v -count=1 -short -timeout 10m -run 'TestRunWithImage|TestApp|TestM1Backward' ./test/integration/
else
	$(GO) test -tags integration -v -count=1 -timeout 10m -run 'TestRunWithImage|TestApp|TestM1Backward' ./test/integration/
endif

# Run M3 integration + conformance tests
test-m3: all
ifdef SHORT
	$(GO) test -tags integration -v -count=1 -short -timeout 15m \
		-run 'TestSecret|TestKit|TestDoctor|TestConformance' ./test/integration/
else
	$(GO) test -tags integration -v -count=1 -timeout 15m \
		-run 'TestSecret|TestKit|TestDoctor|TestConformance' ./test/integration/
endif

# Run network integration tests (gvproxy/TSI egress+ingress, large payloads)
test-network: all
ifdef SHORT
	$(GO) test -tags integration -v -count=1 -short -timeout 15m \
		-run 'TestNetwork' ./test/integration/
else
	$(GO) test -tags integration -v -count=1 -timeout 15m \
		-run 'TestNetwork' ./test/integration/
endif

# Run integration tests (requires built binaries + base rootfs installed)
# Use SHORT=1 to skip the pause/resume test (70s+ wait)
integration: all
ifdef SHORT
	$(GO) test -tags integration -v -count=1 -short -timeout 10m ./test/integration/
else
	$(GO) test -tags integration -v -count=1 -timeout 10m ./test/integration/
endif

# Clean build artifacts
clean:
	rm -rf $(BIN_DIR)
	$(MAKE) -C base clean
