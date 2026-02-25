# debian/Makefile — Build .deb packages for AegisVM
#
# Called from the top-level Makefile:
#   make deb            → aegisvm_$(VERSION)_$(ARCH).deb
#   make deb-agent-kit  → aegisvm-agent-kit_$(VERSION)_$(ARCH).deb

SHELL := /bin/bash

# Inherited from parent Makefile
VERSION ?= dev
ARCH ?= amd64
BIN_DIR ?= ../bin
KITS_DIR ?= ../kits
KERNEL_PATH ?= $(HOME)/.aegis/kernel/vmlinux

# Debian version: strip leading 'v' (dpkg requires version starts with a digit)
DEB_VERSION := $(patsubst v%,%,$(VERSION))

# Staging directories
STAGE_CORE := _build/aegisvm
STAGE_KIT := _build/aegisvm-agent-kit

# Output
OUT_CORE := aegisvm_$(DEB_VERSION)_$(ARCH).deb
OUT_KIT := aegisvm-agent-kit_$(DEB_VERSION)_$(ARCH).deb

.PHONY: aegisvm aegisvm-agent-kit clean

aegisvm: $(OUT_CORE)
aegisvm-agent-kit: $(OUT_KIT)

$(OUT_CORE):
	@echo "==> Building $(OUT_CORE)"
	rm -rf $(STAGE_CORE)
	# DEBIAN metadata
	mkdir -p $(STAGE_CORE)/DEBIAN
	sed -e 's/__VERSION__/$(DEB_VERSION)/g' -e 's/__ARCH__/$(ARCH)/g' \
		aegisvm/DEBIAN/control > $(STAGE_CORE)/DEBIAN/control
	cp aegisvm/DEBIAN/postinst $(STAGE_CORE)/DEBIAN/postinst
	chmod 755 $(STAGE_CORE)/DEBIAN/postinst
	# /usr/bin/ — user-facing binaries
	mkdir -p $(STAGE_CORE)/usr/bin
	cp $(BIN_DIR)/aegis $(STAGE_CORE)/usr/bin/
	cp $(BIN_DIR)/aegisd $(STAGE_CORE)/usr/bin/
	cp $(BIN_DIR)/aegis-mcp $(STAGE_CORE)/usr/bin/
	# /usr/lib/aegisvm/ — internal binaries
	mkdir -p $(STAGE_CORE)/usr/lib/aegisvm
	cp $(BIN_DIR)/aegis-harness $(STAGE_CORE)/usr/lib/aegisvm/
	cp $(BIN_DIR)/aegis-mcp-guest $(STAGE_CORE)/usr/lib/aegisvm/
	cp $(BIN_DIR)/cloud-hypervisor $(STAGE_CORE)/usr/lib/aegisvm/
	cp $(BIN_DIR)/ch-remote $(STAGE_CORE)/usr/lib/aegisvm/
	# /usr/share/aegisvm/kernel/ — microVM kernel
	mkdir -p $(STAGE_CORE)/usr/share/aegisvm/kernel
	cp $(KERNEL_PATH) $(STAGE_CORE)/usr/share/aegisvm/kernel/vmlinux
	# /etc/aegisvm/ — reserved for future config
	mkdir -p $(STAGE_CORE)/etc/aegisvm
	# Build .deb
	dpkg-deb --build --root-owner-group $(STAGE_CORE) $(OUT_CORE)
	@echo "==> Built $(OUT_CORE)"

$(OUT_KIT):
	@echo "==> Building $(OUT_KIT)"
	rm -rf $(STAGE_KIT)
	# DEBIAN metadata
	mkdir -p $(STAGE_KIT)/DEBIAN
	sed -e 's/__VERSION__/$(DEB_VERSION)/g' -e 's/__ARCH__/$(ARCH)/g' \
		aegisvm-agent-kit/DEBIAN/control > $(STAGE_KIT)/DEBIAN/control
	# /usr/lib/aegisvm/ — kit binaries
	mkdir -p $(STAGE_KIT)/usr/lib/aegisvm
	cp $(BIN_DIR)/aegis-gateway $(STAGE_KIT)/usr/lib/aegisvm/
	cp $(BIN_DIR)/aegis-agent $(STAGE_KIT)/usr/lib/aegisvm/
	# /usr/share/aegisvm/kits/ — kit manifest
	mkdir -p $(STAGE_KIT)/usr/share/aegisvm/kits
	sed 's/"version": *"[^"]*"/"version": "$(VERSION)"/' \
		$(KITS_DIR)/agent.json > $(STAGE_KIT)/usr/share/aegisvm/kits/agent.json
	# Build .deb
	dpkg-deb --build --root-owner-group $(STAGE_KIT) $(OUT_KIT)
	@echo "==> Built $(OUT_KIT)"

clean:
	rm -rf _build *.deb
