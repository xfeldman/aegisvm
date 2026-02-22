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

  depends_on "slp/krun/libkrun"
  depends_on arch: :arm64
  depends_on :macos

  def install
    # Core binaries
    bin.install "aegis"
    bin.install "aegisd"
    bin.install "aegis-mcp"
    bin.install "aegis-mcp-guest"
    bin.install "aegis-vmm-worker"
    bin.install "aegis-harness"

    # Agent Kit binaries
    bin.install "aegis-gateway"
    bin.install "aegis-agent"

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

      To set up Agent Kit (Telegram bot):
        aegis secret set TELEGRAM_BOT_TOKEN <token>
        aegis secret set OPENAI_API_KEY <key>
        See: https://github.com/xfeldman/aegisvm/blob/main/docs/AGENT_KIT.md
    EOS
  end

  service do
    run [opt_bin/"aegisd"]
    keep_alive true
    log_path var/"log/aegisd.log"
    error_log_path var/"log/aegisd.log"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/aegis version")
    # Verify MCP server responds to initialize
    output = pipe_output(
      "#{bin}/aegis-mcp",
      '{"jsonrpc":"2.0","method":"initialize","params":{"capabilities":{}},"id":1}',
      0
    )
    assert_match '"name":"aegisvm"', output
  end
end
