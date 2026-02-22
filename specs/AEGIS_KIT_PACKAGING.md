# Aegis Kit Packaging Spec

**Status:** Draft
**Scope:** How kits (add-on bundles) are packaged, discovered, and used.

---

## 1. What is a Kit?

A kit is a self-contained add-on that extends AegisVM with a specific capability. Core aegis provides the VM runtime. Kits provide opinionated workloads that run on top.

The first kit is **Agent Kit** — a messaging-driven LLM agent with Telegram integration, built-in tools, and MCP orchestration.

**Principles:**
- Core aegis has zero kit dependencies. No gateway, no agent, no LLM abstractions.
- Kits are optional. `brew install aegisvm` gives you a fully functional VM runtime.
- Kits bring their own binaries, OCI recipe, and CLI surface.
- Multiple kits can coexist. They don't interfere with each other.

---

## 2. Packaging

### Core: `aegisvm`

```
brew tap xfeldman/aegisvm
brew install aegisvm
```

Binaries:
- `aegis` — CLI
- `aegisd` — daemon
- `aegis-harness` — guest PID 1
- `aegis-vmm-worker` — VMM helper
- `aegis-mcp` — host MCP server
- `aegis-mcp-guest` — guest MCP server

### Agent Kit: `aegisvm-agent-kit`

```
brew install aegisvm-agent-kit
```

Depends on `aegisvm`. Installs:
- `aegis-gateway` — host-side messaging adapter (Telegram initially, extensible to Discord/Slack/WhatsApp)
- `aegis-agent` — guest agent runtime (linux/arm64)

Plus a kit manifest at `~/.aegis/kits/agent.json` (created on first use or by post-install hook):

```json
{
  "name": "agent",
  "version": "0.1.0",
  "description": "Messaging-driven LLM agent with Telegram integration",
  "daemons": ["aegis-gateway"],
  "image": {
    "base": "python:3.12-alpine",
    "inject": ["aegis-agent", "aegis-mcp-guest"]
  },
  "defaults": {
    "command": ["aegis-agent"],
    "capabilities": {
      "spawn": true,
      "spawn_depth": 2,
      "max_children": 5,
      "allowed_images": ["*"],
      "max_memory_mb": 1024,
      "max_vcpus": 2,
      "max_expose_ports": 5,
      "allowed_secrets": ["*"]
    }
  }
}
```

---

## 3. CLI Surface

### `aegis kit list`

Lists installed kits. Scans `~/.aegis/kits/*.json`.

```
$ aegis kit list
NAME     VERSION  DESCRIPTION
agent    0.1.0    Messaging-driven LLM agent with Telegram integration
```

No kits installed:

```
$ aegis kit list
No kits installed.
```

### `aegis instance start --kit <name>`

Creates an instance using a kit's defaults. The kit manifest provides command, image, capabilities. The user provides the name, secrets, and workspace.

```bash
aegis instance start --kit agent --name my-agent --secret OPENAI_API_KEY
```

Equivalent to:

```bash
aegis instance start \
  --name my-agent \
  --image python:3.12-alpine \
  --secret OPENAI_API_KEY \
  --workspace my-agent \
  --capabilities '{"spawn":true,"spawn_depth":2,...}' \
  -- aegis-agent
```

Behavior:
- `--kit` is a **preset** — it supplies defaults for command, image, and capabilities from the manifest
- `--name` is required (used as workspace name if `--workspace` not given)
- `--secret` / `--env` / `--workspace` / `--expose` can be specified and override kit defaults
- `-- <command>` can be specified to override the kit's default command (useful for debugging, e.g. `--kit agent -- sh` to get a shell in a kit-configured VM)
- Precedence: explicit flags > kit defaults > global defaults

### `aegis up` with kit daemons

On `aegis up`, the CLI scans `~/.aegis/kits/*.json` for installed kits. Each kit's `daemons` array lists host-side binaries to start alongside aegisd. The CLI starts each daemon that has its config present (e.g., `~/.aegis/gateway.json` for `aegis-gateway`).

```
$ aegis up
aegis v0.4.0
aegisd: started
aegis-gateway: started         (agent kit daemon, config found)
```

When no kits are installed:

```
$ aegis up
aegis v0.4.0
aegisd: started
```

Daemon without config:

```
$ aegis up
aegis v0.4.0
aegisd: started
aegis-gateway: no config (create ~/.aegis/gateway.json to enable)
```

`aegis up --no-daemons` suppresses all kit daemons. `aegis down` stops all of them.

---

## 4. OCI Image Recipe

Kits don't ship pre-built OCI images. Instead, the kit manifest describes a recipe:

```json
{
  "image": {
    "base": "python:3.12-alpine",
    "inject": ["aegis-agent", "aegis-mcp-guest"]
  }
}
```

When `--kit agent` is used:

1. Pull the base image (`python:3.12-alpine`) — cached by the existing image cache
2. Create an overlay — same as any OCI instance
3. Inject the kit's binaries (`aegis-agent`, `aegis-mcp-guest`) into the overlay — in addition to the standard harness injection
4. Boot the VM

The kit binaries are resolved from the same `BinDir` as the harness. The injection happens in `prepareImageRootfs` when the instance has a `kit` field — aegisd looks up the kit manifest's `image.inject` list.

**No magic in aegisd.** The `--kit` flag is expanded entirely in the CLI:
- CLI reads the manifest
- CLI builds a normal API request with command, image_ref, capabilities, and a `kit` field
- aegisd treats it as a regular instance — the only kit-aware behavior is: if `kit` is set, inject the kit's binaries into the overlay in addition to the standard harness

This means:
- No separate image build step
- No image registry for kits
- Kit binary updates are picked up on next instance creation (new overlay)
- The "image recipe" is just metadata — the machinery already exists
- Capabilities, secrets, env are all standard instance config — nothing "special because kit"

---

## 5. Kit Detection and Lifecycle

Kits are detected by manifest files at `~/.aegis/kits/<name>.json`.

**Install:** The Homebrew formula creates the manifest via `post_install` hook. For development, `make install-kit` creates it.

**Uninstall:** The Homebrew formula removes the manifest via `post_uninstall` hook.

**Stale detection:** `aegis kit list` and `aegis up` validate that each manifest's daemon and inject binaries actually exist on disk. If binaries are missing (e.g., brew removed the kit but manifest lingered), the kit is shown as `(broken)` in `kit list` and its daemons are skipped by `aegis up`. This makes the system self-healing — no orphaned daemons, no crashes from missing binaries.

**Instance safety:** Kit install/uninstall must not break existing instances. If a kit is removed while instances with `kit: "agent"` exist:
- The instances remain in the registry with their stored config (command, image, capabilities)
- `aegis instance info` shows `kit: agent (not installed)` to surface the issue
- Booting the instance fails with a clear error: `kit "agent" binaries not found`
- The instance can still be deleted, disabled, or have its logs read
- Re-installing the kit makes the instance bootable again

---

## 6. Instance Metadata

Instances created with `--kit` store the kit name in the registry:

```json
{
  "id": "inst-...",
  "kit": "agent",
  "command": ["aegis-agent"],
  ...
}
```

This enables:
- `aegis instance list` showing which instances are kit-managed
- `aegis instance info` showing the kit name
- Future: kit-specific instance operations

---

## 7. Future Kits

The kit system is generic. Possible future kits:

- **aegisvm-dev-kit** — development environments (code server, LSP, git)
- **aegisvm-web-kit** — web app hosting (nginx, SSL, deploy)
- **aegisvm-data-kit** — data processing (jupyter, pandas, spark)

Each brings its own binaries, OCI recipe, and `--kit` preset. Core aegis stays minimal.

---

## 8. Files

| File | Purpose |
|------|---------|
| `~/.aegis/kits/agent.json` | Agent Kit manifest |
| `~/.aegis/kits/` | Kit manifest directory |
| Kit binaries next to `aegis` | Resolved via `BinDir` |

---

## 9. Implementation Phases

### Phase 1 (v0.1)
- [ ] Remove `aegis-agent` from `InjectGuestBinaries` (core injection)
- [ ] Add `InjectKitBinaries` for kit-aware overlay injection
- [ ] Add `--kit` flag to `aegis instance start`
- [ ] Add `aegis kit list` command
- [ ] Kit manifest format + loading
- [ ] Instance `kit` field in registry

### Phase 2 (v0.2)
- [ ] Separate Homebrew formula `aegisvm-agent-kit`
- [ ] Separate release tarball for kit
- [ ] CI workflow for kit releases

### Phase 3 (future)
- [ ] Kit plugin API (arbitrary kits, not just agent)
- [ ] `aegis kit install <name>` from a registry
