# Aegis Makefile
# Build aegisd, aegis CLI, harness, and base rootfs

SHELL := /bin/bash

# Go settings
GO := go
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
PREFIX ?= /opt/homebrew
VERSION_PKG := github.com/xfeldman/aegisvm/internal/version
KIT_PKG := github.com/xfeldman/aegisvm/internal/kit
LDFLAGS := -X $(VERSION_PKG).version=$(VERSION) -X $(KIT_PKG).shareDir=$(PREFIX)/share/aegisvm/kits
GOFLAGS := -trimpath -ldflags "$(LDFLAGS)"

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

.PHONY: all aegisd aegis harness vmm-worker mcp mcp-guest gateway agent base-rootfs clean test test-unit test-m2 test-m3 test-network integration install-kit release-tarball release-kit-tarball

all: aegisd aegis harness vmm-worker mcp mcp-guest gateway agent

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

# aegis-mcp — MCP server for LLM integration (host-side)
mcp:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-mcp ./cmd/aegis-mcp

# aegis-mcp-guest — MCP server for agents inside VMs (guest-side, Linux ARM64)
mcp-guest:
	@mkdir -p $(BIN_DIR)
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-mcp-guest ./cmd/aegis-mcp-guest

# aegis-gateway — host-side messaging adapter (Telegram + tether)
gateway:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-gateway ./cmd/aegis-gateway

# aegis-agent — guest agent runtime (LLM bridge, Linux ARM64)
agent:
	@mkdir -p $(BIN_DIR)
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-agent ./cmd/aegis-agent

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

# Run Agent Kit tether integration tests
test-tether: all
	$(GO) test -tags integration -v -count=1 -timeout 5m \
		-run 'TestTether' ./test/integration/

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

# Release tarballs
release-tarball: aegisd aegis harness vmm-worker mcp mcp-guest
	tar czf aegisvm-$(VERSION)-darwin-arm64.tar.gz -C bin \
		aegis aegisd aegis-mcp aegis-mcp-guest aegis-vmm-worker aegis-harness

release-kit-tarball: gateway agent
	@mkdir -p /tmp/agent-kit-staging
	cp $(BIN_DIR)/aegis-gateway $(BIN_DIR)/aegis-agent /tmp/agent-kit-staging/
	sed 's/"version": *"[^"]*"/"version": "$(VERSION)"/' kits/agent.json > /tmp/agent-kit-staging/agent.json
	tar czf aegisvm-agent-kit-$(VERSION)-darwin-arm64.tar.gz -C /tmp/agent-kit-staging \
		aegis-gateway aegis-agent agent.json
	@rm -rf /tmp/agent-kit-staging

# Install kit manifests for development (stamps git version into manifest)
install-kit:
	@mkdir -p $(HOME)/.aegis/kits
	sed 's/"version": *"[^"]*"/"version": "$(VERSION)"/' kits/agent.json > $(HOME)/.aegis/kits/agent.json
	@echo "Kit manifest installed: $(HOME)/.aegis/kits/agent.json ($(VERSION))"

# Clean build artifacts
clean:
	rm -rf $(BIN_DIR)
	$(MAKE) -C base clean
