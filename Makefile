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

# Harness target architecture:
#   macOS: always cross-compile to Linux ARM64 (libkrun runs ARM64 VMs)
#   Linux: match host arch (Cloud Hypervisor runs native)
HARNESS_OS := linux
ifeq ($(HOST_OS),linux)
	HARNESS_ARCH := $(HOST_ARCH)
else
	HARNESS_ARCH := arm64
endif

# libkrun paths (macOS via Homebrew)
ifeq ($(HOST_OS),darwin)
	CGO_CFLAGS := -I/opt/homebrew/include
	CGO_LDFLAGS := -L/opt/homebrew/lib
endif

# Cloud Hypervisor version for download
CH_VERSION := v43.0

# Platform-aware all target: skip vmm-worker on Linux (no libkrun)
ifeq ($(HOST_OS),linux)
ALL_TARGETS := aegisd aegis harness mcp mcp-guest gateway agent
else
ALL_TARGETS := aegisd aegis harness vmm-worker mcp mcp-guest gateway agent
endif

.PHONY: all aegisd aegis harness vmm-worker mcp mcp-guest gateway agent base-rootfs clean test test-unit test-m2 test-m3 test-network integration install-kit release-tarball release-kit-tarball cloud-hypervisor kernel kernel-build deb deb-agent-kit release-linux-tarball ui ui-frontend aegis-ui package-mac

all: $(ALL_TARGETS)

# aegisd — the daemon (no cgo needed in M0, cgo is in vmm-worker)
aegisd:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegisd ./cmd/aegisd

# aegis — the CLI
aegis:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis ./cmd/aegis

# aegis-harness — guest PID 1 (static Linux binary, arch matches VM target)
harness:
	@mkdir -p $(BIN_DIR)
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-harness ./cmd/aegis-harness

# aegis-vmm-worker — per-VM helper process (cgo for libkrun, macOS only)
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

# aegis-mcp-guest — MCP server for agents inside VMs (guest-side, Linux)
mcp-guest:
	@mkdir -p $(BIN_DIR)
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-mcp-guest ./cmd/aegis-mcp-guest

# aegis-gateway — host-side messaging adapter (Telegram + tether)
gateway:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-gateway ./cmd/aegis-gateway

# aegis-agent — guest agent runtime (LLM bridge, Linux)
agent:
	@mkdir -p $(BIN_DIR)
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-agent ./cmd/aegis-agent

# UI frontend — build Svelte app, then rebuild aegis CLI with embedded frontend
ui-frontend:
	cd ui/frontend && npm install && npm run build

ui: ui-frontend aegis

# aegis-ui — native desktop app (Wails v3, requires WebKit/WebKitGTK)
# Not part of `make all` — opt-in build target.
aegis-ui: ui-frontend
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/aegis-ui ./cmd/aegis-ui

# Package macOS .app bundle with all platform binaries.
# Requires: make all aegis-ui (all binaries must be built first)
APP_DIR := $(BIN_DIR)/AegisVM.app
BUNDLED_BINS := aegisd aegis-harness aegis-vmm-worker aegis-gateway aegis-agent aegis-mcp aegis-mcp-guest

package-mac: all aegis-ui
	@mkdir -p $(APP_DIR)/Contents/MacOS $(APP_DIR)/Contents/Resources/kits
	cp cmd/aegis-ui/Info.plist $(APP_DIR)/Contents/
	cp $(BIN_DIR)/aegis-ui $(APP_DIR)/Contents/MacOS/
	@for bin in $(BUNDLED_BINS); do \
		[ -f $(BIN_DIR)/$$bin ] && cp $(BIN_DIR)/$$bin $(APP_DIR)/Contents/Resources/ || true; \
	done
	sed 's/"version": *"[^"]*"/"version": "$(VERSION)"/' kits/agent.json > $(APP_DIR)/Contents/Resources/kits/agent.json
	@if [ -f $(APP_DIR)/Contents/Resources/aegis-vmm-worker ]; then \
		codesign --sign - --entitlements entitlements.plist --force $(APP_DIR)/Contents/Resources/aegis-vmm-worker; \
	fi
	@echo "Built $(APP_DIR)"

# Base rootfs — Alpine with harness baked in
# Requires: brew install e2fsprogs (for mkfs.ext4)
base-rootfs: harness
	$(MAKE) -C base

# Download Cloud Hypervisor static binary (Linux only)
cloud-hypervisor:
ifeq ($(HOST_OS),linux)
	@mkdir -p $(BIN_DIR)
	@echo "==> Downloading Cloud Hypervisor $(CH_VERSION) ($(HOST_ARCH))..."
ifeq ($(HOST_ARCH),amd64)
	curl -sSL -o $(BIN_DIR)/cloud-hypervisor \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/cloud-hypervisor-static"
	curl -sSL -o $(BIN_DIR)/ch-remote \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/ch-remote-static"
else ifeq ($(HOST_ARCH),arm64)
	curl -sSL -o $(BIN_DIR)/cloud-hypervisor \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/cloud-hypervisor-static-aarch64"
	curl -sSL -o $(BIN_DIR)/ch-remote \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/ch-remote-static-aarch64"
endif
	chmod +x $(BIN_DIR)/cloud-hypervisor $(BIN_DIR)/ch-remote
	@echo "==> Cloud Hypervisor $(CH_VERSION) installed to $(BIN_DIR)/"
else
	@echo "cloud-hypervisor target is Linux-only (current: $(HOST_OS))"
endif

# Download prebuilt microVM kernel from Cloud Hypervisor's linux repo (Linux only).
# The CH project maintains kernels with virtiofs, vsock, and virtio-net built-in
# for x86_64 and arm64. Use `make kernel-build` to compile from source instead
# (for unsupported architectures or custom configs).
CH_KERNEL_RELEASE := ch-release-v6.16.9-20251112
kernel:
ifeq ($(HOST_OS),linux)
	@mkdir -p $(HOME)/.aegis/kernel
	@if [ -f $(HOME)/.aegis/kernel/vmlinux ]; then \
		echo "Kernel already exists at $(HOME)/.aegis/kernel/vmlinux"; \
		echo "Delete it and re-run to re-download"; \
	else \
		echo "==> Downloading Cloud Hypervisor kernel $(CH_KERNEL_RELEASE) ($(HOST_ARCH))..." && \
		case $(HOST_ARCH) in \
			amd64) \
				curl -sSL -o $(HOME)/.aegis/kernel/vmlinux \
					"https://github.com/cloud-hypervisor/linux/releases/download/$(CH_KERNEL_RELEASE)/vmlinux-x86_64" ;; \
			arm64) \
				curl -sSL -o $(HOME)/.aegis/kernel/vmlinux \
					"https://github.com/cloud-hypervisor/linux/releases/download/$(CH_KERNEL_RELEASE)/Image-arm64" ;; \
			*) \
				echo "No prebuilt kernel for $(HOST_ARCH). Use 'make kernel-build' to compile from source."; \
				exit 1 ;; \
		esac && \
		echo "==> Kernel installed to $(HOME)/.aegis/kernel/vmlinux"; \
	fi
else
	@echo "kernel target is Linux-only (current: $(HOST_OS))"
endif

# Build microVM kernel from source (Linux only, fallback for unsupported archs).
# Uses Cloud Hypervisor's kernel branch with ch_defconfig (virtiofs + vsock built-in).
# Requires: build-essential flex bison libelf-dev libssl-dev bc
kernel-build:
ifeq ($(HOST_OS),linux)
	@mkdir -p $(HOME)/.aegis/kernel
	@if [ -f $(HOME)/.aegis/kernel/vmlinux ]; then \
		echo "Kernel already exists at $(HOME)/.aegis/kernel/vmlinux"; \
		echo "Delete it and re-run to rebuild"; \
	else \
		TMPDIR=$$(mktemp -d) && \
		echo "==> Cloning Cloud Hypervisor kernel ($(CH_KERNEL_RELEASE))..." && \
		git clone --depth 1 --branch $(CH_KERNEL_RELEASE) \
			https://github.com/cloud-hypervisor/linux.git $$TMPDIR && \
		echo "==> Configuring (ch_defconfig)..." && \
		$(MAKE) -C $$TMPDIR ch_defconfig && \
		echo "==> Building vmlinux (this may take ~10 minutes)..." && \
		$(MAKE) -C $$TMPDIR -j$$(nproc) vmlinux && \
		cp $$TMPDIR/vmlinux $(HOME)/.aegis/kernel/vmlinux && \
		rm -rf $$TMPDIR && \
		echo "==> Kernel installed to $(HOME)/.aegis/kernel/vmlinux"; \
	fi
else
	@echo "kernel-build target is Linux-only (current: $(HOST_OS))"
endif

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

# Run Linux-specific integration tests (Cloud Hypervisor backend)
# Requires: sudo, kernel, cloud-hypervisor, virtiofsd, base-rootfs.ext4
# Use SHORT=1 to skip snapshot/pause tests
test-linux: all
ifdef SHORT
	$(GO) test -tags integration -v -count=1 -short -timeout 15m \
		-run 'TestLinux_' ./test/integration/
else
	$(GO) test -tags integration -v -count=1 -timeout 15m \
		-run 'TestLinux_' ./test/integration/
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

# Linux .deb packages
# Rebuilds binaries with system shareDir, then packages with debian/Makefile.
# Requires: all binaries built, cloud-hypervisor downloaded, kernel downloaded.
DEB_LDFLAGS := -X $(VERSION_PKG).version=$(VERSION) -X $(KIT_PKG).shareDir=/usr/share/aegisvm/kits
DEB_GOFLAGS := -trimpath -ldflags "$(DEB_LDFLAGS)"

deb: $(ALL_TARGETS)
	@echo "==> Rebuilding binaries with system paths for .deb..."
	CGO_ENABLED=0 $(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis ./cmd/aegis
	CGO_ENABLED=0 $(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegisd ./cmd/aegisd
	CGO_ENABLED=0 $(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-mcp ./cmd/aegis-mcp
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-harness ./cmd/aegis-harness
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-mcp-guest ./cmd/aegis-mcp-guest
	$(MAKE) -C debian aegisvm \
		VERSION=$(VERSION) ARCH=$(HOST_ARCH) BIN_DIR=../$(BIN_DIR) KITS_DIR=../kits

deb-agent-kit: gateway agent
	@echo "==> Rebuilding agent kit binaries with system paths for .deb..."
	CGO_ENABLED=0 $(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-gateway ./cmd/aegis-gateway
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-agent ./cmd/aegis-agent
	$(MAKE) -C debian aegisvm-agent-kit \
		VERSION=$(VERSION) ARCH=$(HOST_ARCH) BIN_DIR=../$(BIN_DIR) KITS_DIR=../kits

# Linux release tarball (alternative to .deb for non-apt users)
release-linux-tarball: $(ALL_TARGETS) gateway agent
	@echo "==> Rebuilding binaries with system paths for tarball..."
	CGO_ENABLED=0 $(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis ./cmd/aegis
	CGO_ENABLED=0 $(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegisd ./cmd/aegisd
	CGO_ENABLED=0 $(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-mcp ./cmd/aegis-mcp
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-harness ./cmd/aegis-harness
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-mcp-guest ./cmd/aegis-mcp-guest
	CGO_ENABLED=0 $(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-gateway ./cmd/aegis-gateway
	GOOS=$(HARNESS_OS) GOARCH=$(HARNESS_ARCH) CGO_ENABLED=0 \
		$(GO) build $(DEB_GOFLAGS) -o $(BIN_DIR)/aegis-agent ./cmd/aegis-agent
	@mkdir -p /tmp/aegisvm-tarball-staging/{bin,lib,share/kernel,share/kits}
	cp $(BIN_DIR)/aegis $(BIN_DIR)/aegisd $(BIN_DIR)/aegis-mcp \
		/tmp/aegisvm-tarball-staging/bin/
	cp $(BIN_DIR)/aegis-harness $(BIN_DIR)/aegis-mcp-guest \
		$(BIN_DIR)/cloud-hypervisor $(BIN_DIR)/ch-remote \
		$(BIN_DIR)/aegis-gateway $(BIN_DIR)/aegis-agent \
		/tmp/aegisvm-tarball-staging/lib/
	cp $(HOME)/.aegis/kernel/vmlinux /tmp/aegisvm-tarball-staging/share/kernel/
	sed 's/"version": *"[^"]*"/"version": "$(VERSION)"/' kits/agent.json \
		> /tmp/aegisvm-tarball-staging/share/kits/agent.json
	tar czf aegisvm-$(VERSION)-linux-$(HOST_ARCH).tar.gz \
		-C /tmp/aegisvm-tarball-staging bin lib share
	@rm -rf /tmp/aegisvm-tarball-staging
	@echo "==> Built aegisvm-$(VERSION)-linux-$(HOST_ARCH).tar.gz"

# Install kit manifests for development (stamps git version into manifest)
install-kit:
	@mkdir -p $(HOME)/.aegis/kits
	sed 's/"version": *"[^"]*"/"version": "$(VERSION)"/' kits/agent.json > $(HOME)/.aegis/kits/agent.json
	@echo "Kit manifest installed: $(HOME)/.aegis/kits/agent.json ($(VERSION))"

# Clean build artifacts
clean:
	rm -rf $(BIN_DIR)
	$(MAKE) -C base clean
