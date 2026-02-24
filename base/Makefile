# Base rootfs build for Aegis
#
# Creates a minimal Alpine Linux root filesystem with the aegis-harness
# binary baked in. This is the base image that all VMs boot from.
#
# For libkrun (macOS), we use a directory-based rootfs (krun_set_root uses directories).
# For Cloud Hypervisor (Linux), we create ext4 images from the same directory.
#
# Prerequisites:
#   macOS: brew install e2fsprogs (for mkfs.ext4, only needed for ext4 image)
#   Docker must be available for Alpine rootfs extraction

SHELL := /bin/bash

ROOTFS_DIR := rootfs
HARNESS_BIN := ../bin/aegis-harness
BASE_IMAGE := python:3.12-alpine3.21

# Detect host platform for Docker
HOST_OS := $(shell uname -s | tr A-Z a-z)
HOST_ARCH := $(shell uname -m)
ifeq ($(HOST_ARCH),aarch64)
	HOST_ARCH := arm64
endif
ifeq ($(HOST_ARCH),x86_64)
	HOST_ARCH := amd64
endif

# Docker platform flag
DOCKER_PLATFORM := linux/$(HOST_ARCH)

# On Linux, default all includes ext4 image
ifeq ($(HOST_OS),linux)
.PHONY: all rootfs ext4 clean
all: ext4
else
.PHONY: all rootfs ext4 clean
all: rootfs
endif

# Create rootfs directory from Alpine Docker image
rootfs: $(HARNESS_BIN)
	@echo "==> Building base rootfs ($(BASE_IMAGE) $(DOCKER_PLATFORM))"
	@rm -rf $(ROOTFS_DIR)
	@mkdir -p $(ROOTFS_DIR)

	# Extract rootfs using Docker
	@echo "==> Extracting rootfs from $(BASE_IMAGE)..."
	docker create --name aegis-rootfs-tmp --platform $(DOCKER_PLATFORM) $(BASE_IMAGE) /bin/true
	docker export aegis-rootfs-tmp | tar -C $(ROOTFS_DIR) -xf -
	docker rm aegis-rootfs-tmp

	# Add aegis-harness binary
	@echo "==> Installing aegis-harness..."
	@mkdir -p $(ROOTFS_DIR)/usr/bin
	cp $(HARNESS_BIN) $(ROOTFS_DIR)/usr/bin/aegis-harness
	chmod 755 $(ROOTFS_DIR)/usr/bin/aegis-harness

	# Add aegis-mcp-guest binary (MCP server for agents inside VMs)
	@if [ -f ../bin/aegis-mcp-guest ]; then \
		echo "==> Installing aegis-mcp-guest..."; \
		cp ../bin/aegis-mcp-guest $(ROOTFS_DIR)/usr/bin/aegis-mcp-guest; \
		chmod 755 $(ROOTFS_DIR)/usr/bin/aegis-mcp-guest; \
	fi

	# Create required directories
	@mkdir -p $(ROOTFS_DIR)/workspace
	@mkdir -p $(ROOTFS_DIR)/run/secrets
	@mkdir -p $(ROOTFS_DIR)/tmp

	# Minimal /etc/resolv.conf for DNS
	@echo "nameserver 8.8.8.8" > $(ROOTFS_DIR)/etc/resolv.conf

	@echo "==> Base rootfs ready at $(ROOTFS_DIR)/"
	@du -sh $(ROOTFS_DIR)/

# Create ext4 image from rootfs directory
# Needed for Cloud Hypervisor (block device) and Firecracker
ext4: rootfs
	@echo "==> Creating base-rootfs.ext4 from rootfs..."
	# Requires e2fsprogs (brew install e2fsprogs on macOS, apt install e2fsprogs on Linux)
	mkfs.ext4 -d $(ROOTFS_DIR) -L aegis-base base-rootfs.ext4 512M
	@echo "==> base-rootfs.ext4 ready"
	@ls -lh base-rootfs.ext4

$(HARNESS_BIN):
	@echo "==> Building harness first..."
	$(MAKE) -C .. harness

clean:
	rm -rf $(ROOTFS_DIR)
	rm -f base.ext4 base-rootfs.ext4
