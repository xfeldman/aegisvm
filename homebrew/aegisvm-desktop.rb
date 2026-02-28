# Homebrew cask for AegisVM Desktop — native macOS app.
#
# This cask is designed for the xfeldman/homebrew-aegisvm tap.
# Install: brew tap xfeldman/aegisvm && brew install --cask aegisvm-desktop
#
# The release workflow updates the url, sha256, and version automatically.

cask "aegisvm-desktop" do
  version "0.1.0"
  sha256 "PLACEHOLDER"

  url "https://github.com/xfeldman/aegisvm/releases/download/v#{version}/AegisVM-v#{version}.dmg"
  name "AegisVM"
  desc "Desktop app for AegisVM microVM runtime"
  homepage "https://github.com/xfeldman/aegisvm"

  depends_on macos: ">= :ventura"
  depends_on arch: :arm64

  app "AegisVM.app"
end
