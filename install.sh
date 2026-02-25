#!/bin/sh
# AegisVM installer for Linux
# Usage: curl -sSL https://raw.githubusercontent.com/xfeldman/aegisvm/main/install.sh | sh
set -e

REPO="xfeldman/aegisvm"
BASE_URL="https://github.com/${REPO}/releases/latest/download"
TMPDIR=$(mktemp -d)

cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT

echo "==> Installing AegisVM..."

# Download packages
curl -sSL -o "$TMPDIR/aegisvm.deb" "${BASE_URL}/aegisvm.deb"
curl -sSL -o "$TMPDIR/aegisvm-agent-kit.deb" "${BASE_URL}/aegisvm-agent-kit.deb"

# Install
sudo dpkg -i "$TMPDIR/aegisvm.deb" "$TMPDIR/aegisvm-agent-kit.deb" || true
sudo apt-get install -f -y

echo "==> AegisVM installed. Run 'aegis up' to start."
