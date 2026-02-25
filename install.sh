#!/bin/sh
# AegisVM installer for Linux
# Usage: curl -sSL https://raw.githubusercontent.com/xfeldman/aegisvm/main/install.sh | sh
set -e

REPO="xfeldman/aegisvm"
BASE_URL="https://github.com/${REPO}/releases/latest/download"
TMPDIR=$(mktemp -d)

cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT

# Detect architecture
case "$(uname -m)" in
    x86_64)  ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
    *)
        echo "Unsupported architecture: $(uname -m)"
        exit 1
        ;;
esac

echo "==> Installing AegisVM for linux/${ARCH}..."

# Download packages
curl -sSL -o "$TMPDIR/aegisvm.deb" "${BASE_URL}/aegisvm-${ARCH}.deb"
curl -sSL -o "$TMPDIR/aegisvm-agent-kit.deb" "${BASE_URL}/aegisvm-agent-kit-${ARCH}.deb"

# Install
sudo dpkg -i "$TMPDIR/aegisvm.deb" "$TMPDIR/aegisvm-agent-kit.deb" || true
sudo apt-get install -f -y

echo "==> AegisVM installed. Run 'aegis up' to start."
