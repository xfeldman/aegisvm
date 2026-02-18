# Base rootfs build for Aegis
#
# Creates a minimal Alpine Linux ARM64 root filesystem with the aegis-harness
# binary baked in. This is the base image that all VMs boot from.
#
# For M0, we use a directory-based rootfs (libkrun's krun_set_root uses directories).
# For later milestones, we can create ext4 images from the same directory.
#
# Prerequisites:
#   macOS: brew install e2fsprogs (for mkfs.ext4, only needed for ext4 image)
#   Docker must be available for Alpine rootfs extraction

SHELL := /bin/bash

ROOTFS_DIR := rootfs
HARNESS_BIN := ../bin/aegis-harness
BASE_IMAGE := python:3.12-alpine3.21
ALPINE_ARCH := aarch64

.PHONY: all rootfs ext4 clean

all: rootfs

# Create rootfs directory from Alpine Docker image
rootfs: $(HARNESS_BIN)
	@echo "==> Building base rootfs ($(BASE_IMAGE) $(ALPINE_ARCH))"
	@rm -rf $(ROOTFS_DIR)
	@mkdir -p $(ROOTFS_DIR)

	# Extract rootfs using Docker
	@echo "==> Extracting rootfs from $(BASE_IMAGE)..."
	docker create --name aegis-rootfs-tmp --platform linux/arm64 $(BASE_IMAGE) /bin/true
	docker export aegis-rootfs-tmp | tar -C $(ROOTFS_DIR) -xf -
	docker rm aegis-rootfs-tmp

	# Add aegis-harness binary
	@echo "==> Installing aegis-harness..."
	@mkdir -p $(ROOTFS_DIR)/usr/bin
	cp $(HARNESS_BIN) $(ROOTFS_DIR)/usr/bin/aegis-harness
	chmod 755 $(ROOTFS_DIR)/usr/bin/aegis-harness

	# Create required directories
	@mkdir -p $(ROOTFS_DIR)/workspace
	@mkdir -p $(ROOTFS_DIR)/run/secrets
	@mkdir -p $(ROOTFS_DIR)/tmp

	# Minimal /etc/resolv.conf for DNS
	@echo "nameserver 8.8.8.8" > $(ROOTFS_DIR)/etc/resolv.conf

	@echo "==> Base rootfs ready at $(ROOTFS_DIR)/"
	@du -sh $(ROOTFS_DIR)/

# Optional: create ext4 image from rootfs directory
# This is needed for Firecracker (block device) but not for libkrun M0 (directory rootfs)
ext4: rootfs
	@echo "==> Creating base.ext4 from rootfs..."
	# Requires e2fsprogs (brew install e2fsprogs on macOS)
	# Use guestfish or mkfs.ext4 -d
	mkfs.ext4 -d $(ROOTFS_DIR) -L aegis-base base.ext4 256M
	@echo "==> base.ext4 ready"
	@ls -lh base.ext4

$(HARNESS_BIN):
	@echo "==> Building harness first..."
	$(MAKE) -C .. harness

clean:
	rm -rf $(ROOTFS_DIR)
	rm -f base.ext4
