# Aegis Desktop App Roadmap

**Status:** Draft
**Depends on:** Steps 11-12 (Wails wrapper + system tray) — base desktop app

---

## Phase 1: Base Desktop App (Steps 11-12) — current

Self-contained .app (macOS) / AppImage (Linux) with native webview, system tray, bundled binaries.

---

## Phase 2: Distribution

### Auto-update

- **macOS:** Sparkle framework integration. Check for updates on launch, prompt user.
- **Linux:** AppImageUpdate protocol. Delta updates to minimize download size.
- Update server: GitHub Releases (same as current brew release workflow).
- Version check: compare embedded version with latest release tag via GitHub API.

### macOS DMG Installer

- Drag-to-Applications DMG with background image and Applications symlink.
- Built via `create-dmg` or `hdiutil` in CI.
- Code-signed and notarized for Gatekeeper.
- Homebrew cask as alternative install method: `brew install --cask aegisvm`.

### Linux Packages

- **.deb** for Debian/Ubuntu — includes desktop entry, icon, systemd service for aegisd.
- **.rpm** for Fedora/RHEL — same content, different packaging.
- **Flatpak** — sandboxed distribution via Flathub. Requires careful handling of `/dev/kvm` access.
- AppImage remains the universal fallback.

---

## Phase 3: Native Integration

### Dock / Taskbar Badge

- macOS: `NSDockTile` badge with instance count or status indicator.
- Linux: Unity launcher count (where supported).
- Shows number of running instances or alert state.

### Menu Bar Instance Count

- macOS menu bar extra showing "3 running" next to tray icon.
- Linux: tray tooltip with instance summary.

### Native Notifications

- Instance state changes (started, stopped, crashed).
- Agent responses (for background chat).
- Gateway events (Telegram connected/disconnected).
- macOS: `NSUserNotification` / `UNUserNotificationCenter`.
- Linux: `libnotify` / D-Bus notifications.

---

## Phase 4: Onboarding

### First-Launch Wizard

- Detect no secrets configured → prompt for API key setup.
- Offer kit selection (agent kit pre-selected if installed).
- Create first instance with guided steps.
- Optional Telegram bot token setup with QR code instructions.

### Diagnostics Bundle

- "Report Issue" menu item in tray/help menu.
- Collects: aegisd logs, instance logs, system info, config (secrets redacted).
- Exports as `.tar.gz` for attaching to GitHub issues.

### Crash Reporting

- Capture aegisd panics and gateway crashes.
- Show user-friendly error with "Copy to clipboard" for bug reports.
- Optional opt-in telemetry for crash frequency tracking.

---

## Phase 5: Advanced Features

### Multiple Windows

- Wails v3 supports multiple windows.
- Detach instance detail into separate window (useful for chat + logs side by side).
- Each window independently closeable.

### Keyboard Shortcuts

- Global hotkey to toggle dashboard (e.g., Cmd+Shift+A on macOS).
- Instance-specific shortcuts (Cmd+1 through Cmd+9 for first 9 instances).
- Vim-style navigation in instance list.

### Theming

- Respect system dark/light mode preference.
- Auto-switch on macOS appearance change events.
- Custom accent color option.
