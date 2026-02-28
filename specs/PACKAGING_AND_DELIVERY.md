# AegisVM Packaging & Delivery Spec

**Status:** Current as of v0.5
**Scope:** All distribution channels, what ships in each, platform matrix, signing/notarization.

---

## 1. Delivery Channels

| Channel | Platform | Who it's for | Install command |
|---------|----------|-------------|-----------------|
| **Homebrew formula** | macOS arm64 | Developers with Homebrew | `brew install xfeldman/aegisvm/aegisvm` |
| **Homebrew cask** | macOS arm64 | Desktop app via Homebrew | `brew install --cask xfeldman/aegisvm/aegisvm-desktop` |
| **DMG (desktop app)** | macOS arm64 | Non-Homebrew Mac users | Download from GitHub Releases |
| **Debian packages** | Linux amd64/arm64 | Server/desktop Linux | `sudo dpkg -i aegisvm-*.deb` |
| **install.sh** | Linux amd64/arm64 | One-liner server setup | `curl -sSL .../install.sh \| sh` |
| **Linux tarball** | Linux amd64/arm64 | Manual / non-Debian distros | Extract + add to PATH |
| **From source** | macOS / Linux | Contributors | `make all` |

---

## 2. Package Contents

### 2.1 Core Runtime (`aegisvm`)

| Binary | OS | Arch | Role |
|--------|----|------|------|
| `aegis` | native | native | CLI |
| `aegisd` | native | native | Daemon (control plane) |
| `aegis-harness` | linux | arm64 | Guest PID 1, injected into every VM |
| `aegis-vmm-worker` | darwin | arm64 | Per-VM libkrun wrapper (macOS only) |
| `aegis-mcp` | native | native | MCP server (stdio, talks to aegisd) |
| `aegis-mcp-guest` | linux | arm64 | Guest MCP server, injected into VMs |
| `cloud-hypervisor` | linux | native | VMM backend (Linux only) |
| `ch-remote` | linux | native | Cloud Hypervisor control (Linux only) |
| `vmlinux` | linux | native | microVM kernel (Linux only) |

### 2.2 Agent Kit (`aegisvm-agent-kit`)

Separate package, depends on core.

| Binary | OS | Arch | Role |
|--------|----|------|------|
| `aegis-gateway` | native | native | Host-side messaging adapter |
| `aegis-agent` | linux | arm64 | Guest agent runtime, injected via kit |
| `agent.json` | — | — | Kit manifest |

### 2.3 Desktop App (`AegisVM.app`)

Self-contained. Ships everything — core + agent kit + dylibs. No Homebrew required.

```
AegisVM.app/Contents/
├── MacOS/aegis-ui                          Wails native app
├── Resources/
│   ├── aegis, aegisd, aegis-mcp            macOS native binaries
│   ├── aegis-vmm-worker                    macOS native, signed with entitlements
│   ├── aegis-harness, aegis-agent          Linux arm64 (unsigned)
│   ├── aegis-mcp-guest                     Linux arm64 (unsigned)
│   ├── aegis-gateway                       macOS native
│   └── kits/agent.json                     Kit manifest (version-stamped)
├── Frameworks/
│   ├── libkrun.1.dylib                     VM runtime (from Homebrew at build time)
│   ├── libkrunfw.dylib                     Firmware (loaded via dlopen)
│   ├── libepoxy.0.dylib                    OpenGL dispatch
│   ├── libvirglrenderer.1.dylib            GPU virtualization
│   └── libMoltenVK.dylib                   Vulkan → Metal
└── Info.plist                              Version-stamped
```

All binaries and dylibs run directly from the .app bundle (like Docker Desktop). Nothing is copied to `~/`. On first launch, `aegis-ui` only:
- Creates symlinks in `~/.aegis/bin/` for CLI access: `aegis` and `aegis-mcp` → `.app/Contents/Resources/`
- Extracts kit manifest (`agent.json`) to `~/.aegis/kits/` (config, not a binary)

Dragging AegisVM.app to Trash cleanly removes all executables and dylibs. Only runtime data (`~/.aegis/data/`) and config remain — standard for macOS apps.

---

## 3. Platform Matrix

### macOS (arm64 only, M1–M4)

| Component | Homebrew | Desktop (.dmg) | Source |
|-----------|----------|-----------------|--------|
| Core binaries | `brew install aegisvm` | In .app bundle | `make all` |
| Agent kit | `brew install aegisvm-agent-kit` | In .app bundle | `make all` |
| libkrun + deps | Homebrew dependency | Bundled in Frameworks/ | `brew install libkrun` |
| Signing | Ad-hoc + hypervisor entitlement | Developer ID + hardened runtime | Ad-hoc |
| Notarization | N/A | Yes (xcrun notarytool) | N/A |
| Auto-update | `brew upgrade` | Not yet (Sparkle planned) | `git pull && make all` |

### Linux (amd64 + arm64)

| Component | Debian (.deb) | Tarball | Source |
|-----------|---------------|---------|--------|
| Core binaries | `/usr/bin/`, `/usr/lib/aegisvm/` | `bin/`, `lib/` | `make all` |
| Agent kit | Separate .deb | In same tarball | `make all` |
| Cloud Hypervisor | Bundled | Bundled | `make cloud-hypervisor` |
| Kernel | `/usr/share/aegisvm/kernel/` | `share/kernel/` | `make kernel` |
| Signing | None | None | N/A |

---

## 4. Dylib Bundling (macOS Desktop)

The desktop .app must be self-contained — no Homebrew dependency at runtime.

**Problem:** `aegis-vmm-worker` links to `libkrun.1.dylib` at the Homebrew install path. On a machine without Homebrew, the worker crashes with `Library not loaded`.

**Solution:** Bundle all non-system dylibs, rewrite install names to `@rpath/`.

### Dependency chain

```
aegis-vmm-worker
└─ libkrun.1.dylib            (linked)
   ├─ libepoxy.0.dylib        (linked)
   ├─ libvirglrenderer.1.dylib (linked)
   │  ├─ libMoltenVK.dylib    (linked)
   │  └─ libepoxy.0.dylib     (linked)
   └─ libkrunfw.dylib         (dlopen at runtime)
```

### Install name rewriting

| Binary/Dylib | Original load path | Rewritten to |
|-------------|-------------------|--------------|
| vmm-worker → libkrun | `/opt/homebrew/opt/libkrun/lib/libkrun.1.dylib` | `@rpath/libkrun.1.dylib` |
| libkrun → libepoxy | `/opt/homebrew/opt/libepoxy/lib/libepoxy.0.dylib` | `@rpath/libepoxy.0.dylib` |
| libkrun → virglrenderer | `/opt/homebrew/opt/virglrenderer/lib/libvirglrenderer.1.dylib` | `@rpath/libvirglrenderer.1.dylib` |
| virglrenderer → MoltenVK | `/opt/homebrew/opt/molten-vk/lib/libMoltenVK.dylib` | `@rpath/libMoltenVK.dylib` |
| virglrenderer → libepoxy | `/opt/homebrew/opt/libepoxy/lib/libepoxy.0.dylib` | `@rpath/libepoxy.0.dylib` |

### Rpath resolution

vmm-worker runs from `Contents/Resources/` inside the .app bundle. Rpaths resolve dylibs from the sibling `Contents/Frameworks/` directory.

| Binary/Dylib | Rpath | Resolves to |
|-------------|-------|------------|
| vmm-worker | `@executable_path/../Frameworks` | `Contents/Frameworks/` |
| libkrun | `@loader_path` | `Contents/Frameworks/` (same dir) |
| virglrenderer | `@loader_path` | `Contents/Frameworks/` (same dir) |

The second rpath on vmm-worker (`@executable_path/../lib`) is kept for Homebrew/dev compatibility but unused in desktop mode.

### dlopen (libkrunfw)

libkrun loads libkrunfw via `dlopen("libkrunfw.dylib")` at runtime. Resolution chain:

1. Caller's (libkrun) LC_RPATH → `@loader_path` → `Contents/Frameworks/` ✓
2. `DYLD_FALLBACK_LIBRARY_PATH` → `Contents/Frameworks/:/opt/homebrew/lib:...` ✓

The `com.apple.security.cs.allow-dyld-environment-variables` entitlement ensures DYLD vars work under hardened runtime (required for notarization).

---

## 5. Code Signing & Notarization

### Entitlements (`entitlements.plist`)

| Entitlement | Why |
|-------------|-----|
| `com.apple.security.hypervisor` | Hypervisor.framework access (Apple HVF via libkrun) |
| `com.apple.security.cs.allow-dyld-environment-variables` | DYLD_FALLBACK_LIBRARY_PATH for dlopen'd libkrunfw |

### Signing hierarchy (release builds)

Bottom-up order required by macOS:

1. `Contents/Frameworks/*.dylib` — Developer ID, hardened runtime
2. `Contents/Resources/*` (Mach-O only) — Developer ID, hardened runtime
   - `aegis-vmm-worker` gets entitlements.plist
   - Linux ELF binaries (harness, agent, mcp-guest) skipped
3. `Contents/MacOS/aegis-ui` — Developer ID, hardened runtime
4. `AegisVM.app` bundle — Developer ID, hardened runtime
5. `AegisVM-{version}.dmg` — Developer ID

### Notarization

```
xcrun notarytool submit → xcrun stapler staple
```

On failure, the workflow fetches detailed Apple log JSON before exiting.

### Dev builds (`make all`, `make package-mac`)

Ad-hoc signing (`--sign -`). No hardened runtime, no notarization. DYLD vars work without the allow-dyld entitlement.

### Homebrew installs

Ad-hoc signing with hypervisor entitlement only. Re-signed locally during `brew install`. libkrun is a Homebrew dependency, so DYLD_FALLBACK_LIBRARY_PATH for libkrunfw works (no hardened runtime restriction).

---

## 6. File Layout After Install

### Homebrew

```
/opt/homebrew/bin/aegis
/opt/homebrew/bin/aegisd
/opt/homebrew/bin/aegis-mcp
/opt/homebrew/bin/aegis-vmm-worker   (re-signed with entitlements)
/opt/homebrew/bin/aegis-harness      (linux arm64)
/opt/homebrew/bin/aegis-mcp-guest    (linux arm64)
/opt/homebrew/opt/libkrun/lib/libkrun.1.dylib  (dependency)
```

Agent kit adds:
```
/opt/homebrew/bin/aegis-gateway
/opt/homebrew/bin/aegis-agent        (linux arm64)
~/.aegis/kits/agent.json             (created on first use)
```

### Desktop App

All executables and dylibs live inside the .app bundle (run in-place, not copied). Only symlinks and config in `~/`.

```
/Applications/AegisVM.app/                                              (signed, notarized)
├── Contents/MacOS/aegis-ui                                             (Wails native app)
├── Contents/Resources/{aegis,aegisd,aegis-vmm-worker,...}              (all binaries)
├── Contents/Frameworks/{libkrun.1.dylib,libkrunfw.dylib,...}           (all dylibs)
└── Contents/Resources/kits/agent.json                                  (kit manifest source)

~/.aegis/bin/aegis → /Applications/AegisVM.app/Contents/Resources/aegis       (symlink)
~/.aegis/bin/aegis-mcp → /Applications/AegisVM.app/Contents/Resources/aegis-mcp  (symlink)
~/.aegis/bin/.version                                                         (setup marker)
~/.aegis/kits/agent.json                                                      (extracted config)
```

Uninstall: drag .app to Trash. All executables and dylibs are removed. Symlinks become dangling (harmless). Runtime data in `~/.aegis/data/` persists (user data, standard for macOS apps).

### Debian

```
/usr/bin/aegis
/usr/bin/aegisd
/usr/bin/aegis-mcp
/usr/lib/aegisvm/aegis-harness
/usr/lib/aegisvm/aegis-mcp-guest
/usr/lib/aegisvm/cloud-hypervisor
/usr/lib/aegisvm/ch-remote
/usr/share/aegisvm/kernel/vmlinux
```

Agent kit adds:
```
/usr/lib/aegisvm/aegis-gateway
/usr/lib/aegisvm/aegis-agent
/usr/share/aegisvm/kits/agent.json
```

---

## 7. Runtime Data

Shared across all install methods:

```
~/.aegis/
├── aegisd.sock              Unix socket (daemon API)
├── master.key               AES-256 key (secrets encryption)
├── data/
│   ├── aegis.db             SQLite registry (instances, secrets)
│   ├── aegisd.pid           Daemon PID file
│   ├── aegisd.log           Daemon log
│   ├── images/              OCI image cache (digest-keyed)
│   ├── overlays/            Per-instance rootfs overlays
│   ├── workspaces/          Auto-created workspaces
│   ├── sockets/             Per-VM control channel sockets
│   └── logs/                Per-instance log files
├── kits/
│   ├── agent.json           Kit manifest
│   └── {handle}/            Per-instance daemon configs
│       └── gateway.json
├── bin/                     (desktop app only) CLI symlinks → .app bundle
│   └── .version
```

---

## 8. Release Pipeline

**Trigger:** `git tag v* && git push --tags`

### macOS job (macos-14 runner, arm64)

1. Build all binaries (`make all`)
2. Build frontend (`make ui-frontend`)
3. Run tests
4. Create release tarballs:
   - `aegisvm-{ver}-darwin-arm64.tar.gz` (core)
   - `aegisvm-agent-kit-{ver}-darwin-arm64.tar.gz` (kit)
5. Build desktop app (`make package-mac`)
6. Sign .app bundle (bottom-up) with Developer ID
7. Create DMG (`create-dmg`)
8. Sign + notarize + staple DMG
9. Create GitHub Release with all artifacts + SHA256 checksums
10. Update Homebrew tap (formula + cask versions, URLs, checksums)

### Linux job (matrix: ubuntu-latest + ubuntu-24.04-arm)

1. Build all binaries
2. Run tests
3. Download Cloud Hypervisor + kernel
4. Build .deb packages (core + agent kit)
5. Build Linux tarball
6. Upload artifacts to same GitHub Release

### Artifacts per release

| Artifact | Platform |
|----------|----------|
| `aegisvm-{ver}-darwin-arm64.tar.gz` | macOS |
| `aegisvm-agent-kit-{ver}-darwin-arm64.tar.gz` | macOS |
| `AegisVM-{ver}.dmg` | macOS |
| `aegisvm-{ver}-linux-amd64.tar.gz` | Linux x86_64 |
| `aegisvm-{ver}-linux-arm64.tar.gz` | Linux arm64 |
| `aegisvm_{ver}_amd64.deb` | Linux x86_64 |
| `aegisvm_{ver}_arm64.deb` | Linux arm64 |
| `aegisvm-agent-kit_{ver}_amd64.deb` | Linux x86_64 |
| `aegisvm-agent-kit_{ver}_arm64.deb` | Linux arm64 |
| `aegisvm-{arch}.deb` (unversioned, latest) | Linux |
| `aegisvm-agent-kit-{arch}.deb` (unversioned, latest) | Linux |
| `.sha256` checksum for each | All |

---

## 9. Version Stamping

All artifacts get the git tag version stamped at build time:

| Location | Mechanism |
|----------|-----------|
| Go binaries | `-ldflags -X .../version.version=$(VERSION)` |
| Kit manifests | `sed` replacement of version field |
| Info.plist | `sed` replacement of CFBundleShortVersionString |
| Homebrew formulas | CI updates url, sha256, version via sed |
| Debian control | Template substitution at build time |

---

## 10. Dev Mode

`make all && ./bin/aegisd &` — no packaging, no extraction, no signing (beyond ad-hoc for vmm-worker).

Binary discovery uses `executableDir()` — all binaries are siblings in `./bin/`. Kit discovery falls back to `kits/` relative to the binary's parent dir. libkrun found at Homebrew's install path (direct link, no rpath rewriting needed).
