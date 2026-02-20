# Homebrew formula for AegisVM — microVM sandbox runtime for agents.
#
# This formula is designed for the xfeldman/homebrew-aegisvm tap.
# Install: brew tap xfeldman/aegisvm && brew install aegisvm
#
# The release workflow updates the url, sha256, and version automatically.

class Aegisvm < Formula
  desc "Lightweight microVM sandbox runtime for agents"
  homepage "https://github.com/xfeldman/aegisvm"
  url "https://github.com/xfeldman/aegisvm/releases/download/v0.1.0/aegisvm-v0.1.0-darwin-arm64.tar.gz"
  sha256 "PLACEHOLDER"
  version "0.1.0"
  license "Apache-2.0"

  depends_on "libkrun"
  depends_on arch: :arm64
  depends_on :macos

  def install
    bin.install "aegis"
    bin.install "aegisd"
    bin.install "aegis-mcp"
    bin.install "aegis-vmm-worker"
    bin.install "aegis-harness"

    # Re-sign vmm-worker with hypervisor entitlement for this machine
    entitlements = buildpath/"entitlements.plist"
    entitlements.write <<~XML
      <?xml version="1.0" encoding="UTF-8"?>
      <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
      <plist version="1.0">
      <dict>
        <key>com.apple.security.hypervisor</key>
        <true/>
      </dict>
      </plist>
    XML
    system "codesign", "--sign", "-", "--entitlements", entitlements, "--force", bin/"aegis-vmm-worker"
  end

  def caveats
    <<~EOS
      To start the AegisVM daemon:
        aegis up

      To configure as an MCP server for Claude Code:
        aegis mcp install
    EOS
  end

  service do
    run [opt_bin/"aegisd"]
    keep_alive true
    log_path var/"log/aegisd.log"
    error_log_path var/"log/aegisd.log"
  end

  test do
    assert_match "aegis", shell_output("#{bin}/aegis help 2>&1")
    # Verify MCP server responds to initialize
    output = pipe_output(
      "#{bin}/aegis-mcp",
      '{"jsonrpc":"2.0","method":"initialize","params":{"capabilities":{}},"id":1}',
      0
    )
    assert_match '"name":"aegisvm"', output
  end
end
