# OpenClaw Tether Kit

**Status:** Draft
**Date:** 2026-02-25
**Supersedes:** Previous OpenClaw kit specs explored running OpenClaw standalone with its own Telegram channel. This spec takes a different approach — OpenClaw as agent brain, AegisVM tether as transport, aegis-gateway as messenger gateway.

---

## 1. Motivation

AegisVM has its own tether protocol, gateway (Telegram, with image support), and session management. OpenClaw has a rich agent runtime — tools, memory, browser control, canvas, context compaction — but its channels overlap with what AegisVM already provides.

Instead of running two competing channel stacks, we bridge them: a custom OpenClaw channel plugin receives messages via tether and sends responses back through tether. OpenClaw runs inside the VM as a black box, unaware it's inside AegisVM. AegisVM handles isolation, lifecycle, messaging, and image transport.

A critical benefit: **AegisVM's gateway runs on the host**, outside the VM. This means it survives VM pause/stop/restart and enables wake-on-message for all channels. OpenClaw's built-in channels run inside the VM — when the VM pauses, the channels die. By routing all messaging through our gateway and tether, we get proper power management for free: VM pauses when idle, wakes on any inbound message from any channel.

### Workspace is required

The OpenClaw kit requires `--workspace` on instance creation. Without it, nothing works:
- **OpenClaw config and state** live in the workspace (`/workspace/.openclaw/`)
- **npm install cache** persists across restarts via workspace (`/workspace/.npm-global/`)
- **Image blob store** uses workspace (`/workspace/.aegis/blobs/`)
- **Semantic memory** stores vectors in workspace (`/workspace/.openclaw/memory/`)
- **Canvas output** writes to workspace (`/workspace/canvas/`)
- **Session transcripts** persist in workspace

If `instance_start` is called with `kit="openclaw"` but no `--workspace`, the MCP server / CLI should return an error: `"openclaw kit requires --workspace"`.

---

## 2. Architecture

```
Telegram/MCP Host
       │
  aegis-gateway / aegis-mcp (host)
       │
   tether (vsock, JSON frames, tiny blob refs)
       │
  aegis-harness (PID 1, guest)
       │
  POST :7778/v1/tether/recv
       │
  aegis-claw-bridge (guest, Node.js)
       │  implements OpenClaw channel monitor pattern
       │  normalizes tether frames ↔ OpenClaw InboundContext
       │
  OpenClaw gateway (ws://127.0.0.1:18789, same process or child)
       │
  Pi Agent runtime → tools, memory, browser, canvas
```

### What runs where

| Component | Where | Role |
|-----------|-------|------|
| aegis-gateway | Host | Telegram polling, message → tether |
| aegis-mcp | Host | MCP tool interface for Claude Code |
| aegis-harness | Guest (PID 1) | Frame routing, exec, logs |
| aegis-claw-bridge | Guest | Tether ↔ OpenClaw channel bridge |
| openclaw gateway | Guest | WebSocket server, session mgmt, agent runtime |
| Pi Agent | Guest (in-process) | LLM calls, tool execution, workspace I/O |

### What we don't use

OpenClaw's built-in Telegram, WhatsApp, Slack, Discord, Signal channels — all disabled. AegisVM's gateway handles the messenger layer. OpenClaw sees a single "aegis" channel.

### Why the gateway stays on the host

OpenClaw's channels run inside the VM process. When the VM pauses (idle timeout), stops, or crashes — all channels die. Telegram polling stops, webhook listeners go down, messages are lost until the VM restarts.

AegisVM's gateway runs on the host as an instance daemon. It survives all VM state transitions:
- **VM paused** → gateway keeps polling Telegram, buffers messages, wakes VM via tether (wake-on-message)
- **VM stopped** → gateway keeps running, wakes VM on first message
- **VM crashed** → gateway detects, can auto-restart VM or queue messages

This is the fundamental advantage of the split architecture. The messaging layer never goes down, even when the agent runtime is sleeping.

### Bridge is platform-agnostic

The bridge doesn't need per-platform knowledge. It translates one thing: tether frame ↔ OpenClaw message. The tether payload already carries everything OpenClaw needs:
- **Sender identity** → `payload.user` (id, name, username)
- **Chat type** → derivable from `session.channel` + chat ID patterns (groups have negative IDs in Telegram)
- **Media** → `payload.images` with blob refs
- **Channel origin** → `session.channel` ("telegram", "host", etc.)

All platform-specific work (downloading photos, sending typing indicators, editing messages, mention gating in groups) lives in aegis-gateway on the host. The bridge stays thin and stable.

---

## 3. The Tether Channel Extension (`@aegis/openclaw-channel-aegis`)

An OpenClaw channel extension (~300-500 lines of TypeScript) that runs in-process with the OpenClaw gateway. Not a separate process — it's an npm package that OpenClaw auto-discovers and loads at startup.

**Tether side:** HTTP server on `:7778` (same endpoint as aegis-agent today)
- Receives `POST /v1/tether/recv` with TetherFrame JSON
- Routes `user.message` frames to OpenClaw auto-reply system
- Sends `assistant.delta`, `assistant.done`, `status.presence` frames back to harness via `POST :7777/v1/tether/send`

**OpenClaw side:** Registered as a channel monitor via extension metadata
- Implements the channel monitor pattern (normalize, route, deliver)
- Runs in-process — no WebSocket hop, no IPC

### Why channel extension (not WebSocket client)

| | Channel extension (chosen) | WebSocket client (rejected) |
|---|---|---|
| **API surface** | Channel monitor pattern — same interface OpenClaw's own Telegram/Slack/Discord use | Client protocol — designed for human-facing apps (CLI, mobile) |
| **Stability** | OpenClaw can't break it without breaking their own bundled channels | Can change for UX reasons unrelated to us |
| **Coupling** | Documented, versioned via npm, discoverable from `node_modules` | Reverse-engineered from source, undocumented |
| **Process model** | In-process with gateway — no extra process, no WebSocket hop | Separate process + WebSocket connection |
| **Media handling** | Built into channel API (normalized media attachments) | Must re-implement media serialization |

### Package structure

```
@aegis/openclaw-channel-aegis/
  package.json          ← declares as OpenClaw extension (channel type)
  src/
    index.ts            ← channel monitor: tether HTTP ↔ InboundContext
    tether.ts           ← tether frame send/receive helpers
  dist/                 ← compiled JS
```

Installed alongside OpenClaw at first boot. Auto-discovered by OpenClaw gateway at startup via `package.json` metadata — no manual registration needed beyond `channels.aegis.enabled: true` in config.

### Design principle: keep the extension dumb

The bridge is a strict adapter — no platform logic, no rate limiting, no retries, no buffering, minimal session mapping. If policy is needed (throttling, dedupe, buffering), it belongs in aegis-gateway (host) or aegisd tether store, not in the bridge. This keeps the guest side easy to restart and stateless.

### Bridge health contract

On startup, the bridge must signal readiness before accepting tether frames:

1. Bridge boots, starts OpenClaw, waits for `ws://127.0.0.1:18789` to become reachable
2. Bridge opens HTTP server on `:7778`
3. Bridge sends `status.ready` frame to harness via `POST :7777/v1/tether/send`
4. Harness (which already buffers tether frames during agent startup) begins draining queued frames

This is critical for cold boot: the gateway may have queued messages while the VM was starting. Without a readiness signal, frames arrive before OpenClaw is ready and get dropped.

### Message flow: ingress (user → agent)

1. Tether frame arrives at bridge: `{type: "user.message", session: {channel: "telegram", id: "12345"}, payload: {text: "...", images: [...], user: {...}}}`
2. Bridge normalizes to OpenClaw `InboundContext`:
   - `channelId`: `"aegis"`
   - `chatId`: `session.channel + ":" + session.id` (e.g., `"telegram:12345"`)
   - `senderId`: `payload.user.id`
   - `senderName`: `payload.user.name`
   - `text`: `payload.text`
   - `media`: image blobs resolved from `/workspace/.aegis/blobs/` → attached as file refs
3. Bridge dispatches to OpenClaw auto-reply system via gateway API
4. OpenClaw creates/resumes session, runs agent

### Message flow: egress (agent → user)

1. OpenClaw agent produces response (streamed via Pi Agent)
2. Bridge receives response chunks from OpenClaw (streaming API or channel delivery callback)
3. Bridge emits tether frames:
   - Text chunks → `assistant.delta` frames
   - Final text → `assistant.done` frame
   - Tool execution → `status.presence` with tool name
4. Harness forwards frames to aegisd → gateway/MCP

### Image handling

**Ingress:** Bridge reads blob refs from tether payload, reads raw bytes from `/workspace/.aegis/blobs/{key}`, passes to OpenClaw as media attachment.

**Egress:** When OpenClaw produces images (canvas, tool output), bridge writes to blob store via `blobStore.Put()`, includes refs in `assistant.done` frame.

### Session mapping

**The tether session is the single source of truth for session identity.** The OpenClaw session ID is derived from it — never the other way around. The channel extension computes the OpenClaw session key deterministically from the tether session:

```
OpenClaw session = "agent:default:aegis:dm:" + session.channel + "_" + session.id
```

| Tether session (authoritative) | OpenClaw session (derived) |
|-------------------------------|---------------------------|
| `{channel: "telegram", id: "12345"}` | `agent:default:aegis:dm:telegram_12345` |
| `{channel: "host", id: "default"}` | `agent:default:aegis:dm:host_default` |

The gateway and tether store own session lifecycle. OpenClaw just stores conversation history under the derived key. Session history persists in OpenClaw's JSONL transcripts at `/workspace/.openclaw/.openclaw/agents/default/sessions/`.

---

## 4. Opinionated Configuration Map

The kit pre-sets OpenClaw configuration to match AegisVM's architecture. The user doesn't need to touch `openclaw.json` — the bridge generates it at boot.

### 4.1 What the kit opinionates

| Area | Setting | Value | Rationale |
|------|---------|-------|-----------|
| **Channels** | All built-in channels | Disabled | AegisVM gateway handles all messaging |
| **Sandbox** | `agents.defaults.sandbox.mode` | `"off"` | The VM is the sandbox. Docker-in-VM is redundant overhead. |
| **Tools** | `agents.defaults.tools.allow` | `["*"]` | No restrictions inside isolated VM. Full bash, file I/O, browser, everything. |
| **Workspace** | `agents.defaults.workspace` | `"/workspace"` | User's actual files, not a nested subdirectory |
| **Canvas** | `agents.defaults.canvas.outputDir` | `"/workspace/canvas"` | Accessible from host via workspace mount |
| **Memory** | `memory.dataDir` | `"/workspace/.openclaw/memory"` | Persists across restarts, doesn't collide with blob store |
| **Memory backend** | `memory.embedding.provider` | Auto-detect from secrets | Uses same LLM provider the user configured (Anthropic or OpenAI) |
| **Compaction** | `agents.defaults.compaction.mode` | `"safeguard"` | OpenClaw's safe default — summarizes old turns when context fills |
| **Concurrency** | `agents.defaults.maxConcurrent` | `4` | Reasonable for a single-user agent |
| **Gateway port** | `gateway.port` | `18789` | Internal only — NOT exposed to host. Bridge handles all communication. |
| **Gateway bind** | `gateway.bind` | `"loopback"` | No external access to OpenClaw gateway |
| **MCP** | aegis-mcp-guest registered | Yes | OpenClaw agent knows it can spawn child VMs via Aegis |

### 4.2 What the kit does NOT opinionate

| Area | Why |
|------|-----|
| **Model choice** | User picks via secrets (ANTHROPIC_API_KEY vs OPENAI_API_KEY) |
| **Allowed chats** | Gateway config (`~/.aegis/kits/{handle}/gateway.json`), not OpenClaw config |
| **Custom skills** | User adds to `/workspace/.openclaw/workspace/skills/`, OpenClaw discovers automatically |
| **AGENTS.md / SOUL.md** | User can customize agent identity by placing files in workspace |

### 4.3 Pre-set identity and MCP integration

Bootstrap writes two additional files (if missing) to give the agent AegisVM awareness:

- **`AGENTS.md`** — default agent identity describing the VM environment, workspace, and available Aegis orchestration tools. User can edit to customize.
- **`mcp.json`** — registers `aegis-mcp-guest` as an MCP tool server, giving the OpenClaw agent access to `instance_spawn`, `instance_list`, `instance_stop`, `expose_port`, etc.

See section 5 (Kit Manifest → Bootstrap) for the exact file contents and generation logic.

### 4.4 Environment

```sh
HOME=/workspace
OPENCLAW_HOME=/workspace/.openclaw
NODE_OPTIONS="--max-old-space-size=384"
npm_config_prefix=/workspace/.npm-global
PATH=/workspace/.npm-global/bin:$PATH
```

---

## 5. Kit Manifest

```json
{
  "name": "openclaw",
  "version": "0.1.0",
  "description": "OpenClaw-powered agent with rich tools, memory, and browser control",
  "required_secrets": [["OPENAI_API_KEY", "ANTHROPIC_API_KEY"]],
  "usage": "Creates a VM running OpenClaw as the agent brain, bridged to AegisVM tether. Supports all AegisVM messaging channels (Telegram via gateway, MCP via tether_send/tether_read). OpenClaw provides rich tools: file I/O, bash, browser control, semantic memory, canvas, context compaction.\n\nQuick start:\n1. secret_list — check available secrets\n2. instance_start with kit=\"openclaw\", name=\"my-claw\", secrets=[\"ANTHROPIC_API_KEY\"], workspace=\"/path/to/project\"\n3. First boot takes ~60s (OpenClaw npm install, cached after)\n4. Send messages via tether_send, read via tether_read\n\nWith Telegram:\n1. Set secrets: ANTHROPIC_API_KEY + TELEGRAM_BOT_TOKEN\n2. instance_start with kit=\"openclaw\", name=\"my-claw\", secrets=[\"ANTHROPIC_API_KEY\"]\n3. Create gateway config: ~/.aegis/kits/my-claw/gateway.json\n4. Send photos and text — OpenClaw agent analyzes with full tool suite\n\nRequired: ANTHROPIC_API_KEY or OPENAI_API_KEY\nOptional: TELEGRAM_BOT_TOKEN (for Telegram gateway)",
  "instance_daemons": ["aegis-gateway"],
  "image": {
    "base": "node:22-alpine",
    "inject": ["aegis-claw-bootstrap"]
  },
  "defaults": {
    "command": ["aegis-claw-bootstrap"],
    "memory_mb": 1024,
    "capabilities": {
      "spawn": true,
      "spawn_depth": 1,
      "max_children": 3,
      "allowed_images": ["*"],
      "max_memory_mb": 2048,
      "max_vcpus": 2,
      "max_expose_ports": 5,
      "allowed_secrets": ["*"]
    }
  }
}
```

### Main process model

The kit manifest's `defaults.command` declares the main process — the harness runs it as the VM's primary workload. Each kit owns this decision:

| Kit | `defaults.command` | Main process | Tether integration |
|-----|-------------------|-------------|-------------------|
| **agent** | `["aegis-agent"]` | Go binary, listens on `:7778` | Built-in (native tether HTTP) |
| **openclaw** | `["aegis-claw-bootstrap"]` | Shell script → `exec openclaw gateway` | Via `@aegis/openclaw-channel-aegis` extension (in-process plugin) |

For the OpenClaw kit, a thin bootstrap script (`aegis-claw-bootstrap`) handles first-boot setup (npm install, config generation) then `exec`s into the OpenClaw gateway process. The tether bridge is an OpenClaw channel extension — an npm package loaded in-process by the gateway, not a separate binary.

**`aegis-claw-bootstrap`** is a small Go binary (injected into rootfs like `aegis-agent`). It handles first-boot setup, generates OpenClaw configuration from Aegis environment, and `exec`s into the OpenClaw gateway process. After exec, it's gone — OpenClaw is the main process.

Go is the right choice here: it's a static binary (no Node.js dependency for bootstrap), it can generate JSON cleanly, and it matches the existing pattern (`aegis-agent`, `aegis-harness` are all Go binaries injected into the rootfs).

### What bootstrap does

```
aegis-claw-bootstrap
  │
  ├─ 1. Set environment
  │     HOME=/workspace
  │     OPENCLAW_HOME=/workspace/.openclaw
  │     NODE_OPTIONS="--max-old-space-size=384"
  │     npm_config_prefix=/workspace/.npm-global
  │     PATH=/workspace/.npm-global/bin:$PATH
  │
  ├─ 2. First-boot install (if /workspace/.npm-global/bin/openclaw missing)
  │     exec: npm install -g openclaw@0.2.19 @aegis/openclaw-channel-aegis
  │
  ├─ 3. Generate config files
  │
  └─ 4. exec openclaw gateway --allow-unconfigured
        (bootstrap process replaced by OpenClaw)
```

### Config generation mechanism

**Input:** Environment variables (Aegis secrets) + embedded templates (`go:embed`)

**Output:** Files in `/workspace/.openclaw/.openclaw/` (OpenClaw config dir)

**How it works:** The bootstrap binary embeds config templates as Go `embed` files. At runtime, it reads env vars, applies simple substitutions (model name, API keys), and writes the results to the workspace. This is the same pattern as `aegis-agent` embedding its `defaultSystemPrompt` — the kit's opinions are compiled into the binary, versioned with the kit release.

```go
//go:embed templates/openclaw.json.tmpl
var openclawConfigTmpl string

//go:embed templates/agents.md
var agentsMdDefault string

//go:embed templates/mcp.json
var mcpConfigDefault string
```

The templates are plain JSON/Markdown with `{{.Model}}`, `{{.EmbeddingProvider}}` style placeholders — Go's `text/template`. Minimal logic, maximum transparency.

**Write rules:**

| File | Rule | Why |
|------|------|-----|
| `openclaw.json` | Write if missing | User may customize after first boot |
| `credentials/auth-profiles.json` | Always rewrite | Secrets may rotate between restarts |
| `AGENTS.md` | Write if missing | User may customize agent identity |
| `mcp.json` | Write if missing | User may add more MCP servers |

If a user pre-populates any of these files in the workspace before first boot, bootstrap respects them — write-if-missing means "don't clobber."

### Config files

#### File 1: `openclaw.json` (write if missing)

Template with two substitutions: `{{.Model}}` and `{{.EmbeddingProvider}}`.

```json
{
  "gateway": {
    "port": 18789,
    "mode": "local",
    "bind": "loopback"
  },
  "agents": {
    "defaults": {
      "model": {"primary": "{{.Model}}"},
      "workspace": "/workspace",
      "maxConcurrent": 4,
      "tools": {"allow": ["*"]},
      "sandbox": {"mode": "off"},
      "compaction": {"mode": "safeguard"},
      "canvas": {"outputDir": "/workspace/canvas"}
    }
  },
  "memory": {
    "dataDir": "/workspace/.openclaw/memory",
    "embedding": {"provider": "{{.EmbeddingProvider}}"}
  },
  "channels": {
    "aegis": {"enabled": true}
  },
  "plugins": {"entries": {}}
}
```

Substitution logic:
- `ANTHROPIC_API_KEY` present → Model `"anthropic/claude-sonnet-4-20250514"`, EmbeddingProvider `"anthropic"`
- `OPENAI_API_KEY` present → Model `"openai/gpt-4o"`, EmbeddingProvider `"openai"`
- Both → prefer Anthropic

Everything else in the template is static — the kit's opinion, baked in.

#### File 2: `credentials/auth-profiles.json` (always rewrite)

Not templated — built programmatically from env. Bootstrap scans for known key env vars and emits matching profiles:

```go
profiles := map[string]interface{}{}
if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
    profiles["anthropic:default"] = map[string]string{
        "type": "api_key", "provider": "anthropic", "key": key,
    }
}
if key := os.Getenv("OPENAI_API_KEY"); key != "" {
    profiles["openai:default"] = map[string]string{
        "type": "api_key", "provider": "openai", "key": key,
    }
}
writeJSON(authProfilesPath, map[string]interface{}{"version": 1, "profiles": profiles})
```

Always rewritten — secrets may have been rotated via `aegis secret set` between restarts.

#### File 3: `AGENTS.md` (write if missing)

Static embedded file, no substitutions:

```markdown
You are an AI assistant running inside an isolated AegisVM microVM.

Your workspace is at /workspace/ — files here are shared with the host and persist across restarts.

You have access to Aegis MCP tools for infrastructure orchestration:
- Spawn child VMs for isolated workloads (instance_spawn)
- List and manage running instances (instance_list, instance_stop)
- Expose ports from child instances (expose_port)

Use child VMs for heavy or risky tasks — your own VM is the "bot" tier.
```

#### File 4: `mcp.json` (write if missing)

Static embedded file, no substitutions:

```json
{
  "mcpServers": {
    "aegis": {
      "command": "aegis-mcp-guest",
      "args": []
    }
  }
}
```

### After exec

```
harness (PID 1) → openclaw gateway (main process)
                     └─ @aegis/openclaw-channel-aegis (in-process plugin, listens :7778)
                     └─ Pi Agent (in-process, spawned per session)
```

No bootstrap process. No wrapper. OpenClaw IS the main process. The tether channel extension runs inside it.

### Key differences from agent kit

| | agent kit | openclaw kit |
|---|-----------|-------------|
| Base image | `python:3.12-alpine` | `node:22-alpine` |
| Main process | `aegis-agent` (Go, 5MB) | `openclaw gateway` (Node.js) |
| Tether integration | Native HTTP server in agent binary | `@aegis/openclaw-channel-aegis` npm extension (in-process) |
| LLM integration | Direct API calls | Pi Agent (streaming, tool loop, context mgmt) |
| Tools | bash, read/write/list files, MCP guest tools | bash, file I/O, edit/patch, browser (CDP), memory search, canvas, cron, web fetch, + MCP guest tools |
| Memory | None | Vector + BM25 hybrid search (SQLite) |
| Context management | Simple window (last N turns, max chars) | Compaction (summarize old history via LLM) |
| Session persistence | JSONL (blob refs) | JSONL (OpenClaw format) |
| RAM | ~64MB idle | ~200-300MB idle |
| First boot | Instant | ~60s (npm install, cached after) |

---

## 6. Boot Sequence & Cold Start

```
aegis-claw-bootstrap starts (PID from harness)
  │
  ├─ 1. Check /workspace/.npm-global/bin/openclaw
  │     If missing: npm install -g openclaw@0.2.19 @aegis/openclaw-channel-aegis (~60s first time)
  │
  ├─ 2. Generate OpenClaw config from env vars (aegis-claw-config helper)
  │     Write openclaw.json (if not exists — includes channels.aegis.enabled: true)
  │     Write auth-profiles.json (always, from Aegis secrets)
  │     Write AGENTS.md, mcp.json (if not exists)
  │
  ├─ 3. exec openclaw gateway --allow-unconfigured
  │     Bootstrap exits, OpenClaw gateway becomes the main process
  │     OpenClaw discovers @aegis/openclaw-channel-aegis extension
  │     Channel extension starts HTTP server on :7778
  │
  ├─ 4. Channel extension sends status.ready frame to harness
  │     Harness begins draining buffered tether frames
  │
  └─ 5. Ready
```

### Cold start timing

Cold start (npm install) only happens on **first-ever boot** of a new instance. Subsequent lifecycle events don't trigger it:

| Event | macOS (libkrun) | Linux (Cloud Hypervisor) | npm install? |
|-------|----------------|--------------------------|-------------|
| First boot | ~60s (npm install) | ~60s (npm install) | Yes |
| Resume from pause | ~100ms | ~100ms | No (workspace persists) |
| Resume from stop | N/A (Mac never stops) | ~2s (memory snapshot restore) | No (workspace persists) |
| VM restart after crash | ~5s (boot) | ~5s (boot) | No (workspace persists) |

The npm install cost is amortized over the lifetime of the instance. On Mac, instances are never fully stopped — they pause and resume. On Linux, memory snapshots restore the full process state.

### Versioning

**v0.1:** Pin exact version with integrity check. Bootstrap verifies the installed version matches and reinstalls if it drifts:

```sh
OPENCLAW_VERSION="0.2.19"
OPENCLAW_SHA="sha512-<integrity-hash>"
npm install -g "openclaw@${OPENCLAW_VERSION}" --integrity="${OPENCLAW_SHA}"
```

Exact pinning prevents silent breakage from upstream changes. The pinned version and hash are compiled into the bootstrap binary and updated explicitly when we test a new OpenClaw release.

**v0.2:** Publish a pre-built OCI image (`aegis-openclaw:0.2.19`) with OpenClaw pre-installed. Eliminates npm install entirely. Kit manifest changes `image.base` from `node:22-alpine` to `aegis-openclaw:0.2.19`.

---

## 7. Image Support Through the Extension

Images flow through the existing blob store — no base64 in tether frames.

### Ingress (user sends photo)

1. aegis-gateway downloads Telegram photo → writes blob
2. Tether frame arrives with `payload.images: [{media_type, blob, size}]`
3. Bridge reads blob from `/workspace/.aegis/blobs/{key}`
4. Passes raw bytes to OpenClaw as media attachment in message

### Egress (agent produces image)

1. OpenClaw tool generates image file in workspace
2. Bridge detects image in response (canvas output, tool result with file path)
3. Bridge writes to blob store: `blobStore.Put(bytes, mediaType)` → key
4. Bridge emits `assistant.done` with `images: [{media_type, blob, size}]`
5. aegis-gateway reads blob → sends Telegram photo
6. aegis-mcp reads blob → returns MCP image content block

---

## 8. Workspace Layout

```
/workspace/                              ← Aegis workspace mount (shared with host, persists)
  .aegis/
    blobs/                               ← image blob store (tether image support)
  .openclaw/
    .openclaw/
      openclaw.json                      ← generated config
      credentials/auth-profiles.json     ← generated from Aegis secrets
      agents/default/sessions/           ← OpenClaw session transcripts
      AGENTS.md                          ← agent identity (kit default, user-editable)
      mcp.json                           ← MCP config with aegis-mcp-guest
    memory/                              ← semantic memory (SQLite + vectors)
  .npm-global/
    bin/openclaw                         ← OpenClaw CLI (cached across restarts)
    lib/node_modules/openclaw/           ← OpenClaw package
  canvas/                                ← canvas output (visible from host)
  [user project files]                   ← whatever the user mounted
```

---

## 9. Guest Orchestration

OpenClaw kit inherits the same guest orchestration as the agent kit. The bridge (or OpenClaw tools) can spawn child VMs via aegis-mcp-guest tools:

- `instance_spawn` — spin up a work instance for heavy tasks
- `instance_list`, `instance_stop` — manage children
- `expose_port` — expose services from child instances

OpenClaw's built-in `exec` and `bash` tools run inside the OpenClaw VM. For isolated workloads, spawn a child.

---

## 10. Lifecycle & Power Model

The gateway-on-host architecture enables proper power management that OpenClaw standalone cannot achieve.

### State transitions

| VM State | Gateway (host) | OpenClaw (guest) | User experience |
|----------|---------------|-----------------|-----------------|
| **Running** | Polling Telegram, forwarding via tether | Processing messages | Normal, instant responses |
| **Paused** | Polling Telegram, queuing in tether store | Frozen (zero CPU, zero RAM pressure) | Message arrives → wake-on-message → ~100ms resume → response |
| **Stopped** (Linux only) | Polling Telegram, queuing | Not running (memory snapshot on disk) | Message arrives → wake → ~2s snapshot restore → response |
| **Crashed** | Polling Telegram, queuing | Dead | aegisd auto-restarts VM → bridge boots → readiness signal → drain queue |

On macOS, instances pause but never fully stop (libkrun limitation — no memory snapshots). Resume from pause is ~100ms. On Linux with Cloud Hypervisor, full stop + memory snapshot restore is ~2s.

### Why the gateway enables this

**Without our gateway (OpenClaw standalone):** VM pauses → Telegram polling stops → no wake trigger → user messages lost until manual restart. The only workaround was `idle_policy: "leases_only"` (never pause), wasting resources.

**With our gateway:** VM safely pauses after idle timeout. Gateway keeps Telegram connection alive on the host. First inbound message enters tether → aegisd wake-on-message → VM resumes → bridge drains queued frames. Zero resource usage during idle, zero missed messages.

This applies to all future channels — any channel adapter added to aegis-gateway automatically gets pause/resume/wake-on-message for free. The agent runtime (OpenClaw, aegis-agent, or anything else) doesn't need to know about power management.

### Message buffering during wake

When the VM is paused/stopped, inbound tether frames accumulate in aegisd's tether store (ring buffer, in-memory on host). The harness buffers up to 100 frames while the agent process starts. Once the bridge sends `status.ready`, the harness drains the buffer in order. No messages are lost.

---

## 11. What Changes in AegisVM Core

**Nothing.** The bootstrap is a guest binary, injected like aegis-agent. The kit manifest uses existing fields. Tether, blob store, gateway, harness — all unchanged.

---

## 12. Deliverables

### 12.1 Source artifacts

| Artifact | Language | Location | Description |
|----------|----------|----------|-------------|
| `aegis-claw-bootstrap` | Go | `cmd/aegis-claw-bootstrap/` | Bootstrap binary: config generation from embedded templates, npm install, exec into OpenClaw |
| `@aegis/openclaw-channel-aegis` | TypeScript | `packages/openclaw-channel-aegis/` | OpenClaw channel extension: tether HTTP ↔ InboundContext adapter |
| `openclaw.json` | JSON | `kits/openclaw.json` | Kit manifest |

### 12.2 Build targets

Follow the existing pattern from the agent kit (`make gateway agent`, `make release-kit-tarball`, `make deb-agent-kit`):

```makefile
# aegis-claw-bootstrap — guest bootstrap binary (static Linux ARM64)
claw-bootstrap:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -o bin/aegis-claw-bootstrap ./cmd/aegis-claw-bootstrap

# Channel extension — build TypeScript, pack as npm tarball
claw-channel:
	cd packages/openclaw-channel-aegis && npm run build && npm pack

# macOS release tarball
release-openclaw-kit-tarball: gateway claw-bootstrap claw-channel
	# aegis-gateway (host daemon, reused from agent kit)
	# aegis-claw-bootstrap (guest binary)
	# openclaw-channel-aegis-*.tgz (npm package, installed at first boot)
	# openclaw.json (kit manifest)

# Linux .deb
deb-openclaw-kit: gateway claw-bootstrap claw-channel
	# Same pattern as deb-agent-kit
```

### 12.3 Installation packages

Each kit is a separate installable package. The OpenClaw kit follows the same pattern as the agent kit:

**Homebrew (macOS):**

Separate cask or formula `aegisvm-openclaw-kit` that depends on `aegisvm`:
- Installs `aegis-gateway` to bin dir (shared with agent kit — same binary)
- Installs `aegis-claw-bootstrap` to lib dir
- Installs `openclaw-channel-aegis-*.tgz` to share dir (npm tarball, installed into VM at first boot)
- Installs `openclaw.json` to `share/aegisvm/kits/`

```ruby
# Formula sketch
class AegisvmOpenclawKit < Formula
  depends_on "aegisvm"

  def install
    bin.install "aegis-gateway"         # host daemon (shared)
    lib.install "aegis-claw-bootstrap"  # guest binary
    (share/"aegisvm/kits").install "openclaw.json"
    (share/"aegisvm/packages").install "openclaw-channel-aegis-0.1.0.tgz"
  end
end
```

**Debian/Ubuntu (.deb):**

Package `aegisvm-openclaw-kit` that depends on `aegisvm`:

```
aegisvm-openclaw-kit_0.1.0_arm64.deb
  /usr/lib/aegisvm/aegis-gateway              ← host daemon (shared with agent kit)
  /usr/lib/aegisvm/aegis-claw-bootstrap       ← guest binary
  /usr/share/aegisvm/kits/openclaw.json       ← kit manifest
  /usr/share/aegisvm/packages/openclaw-channel-aegis-0.1.0.tgz  ← npm package
```

**Development (`make install-openclaw-kit`):**

```makefile
install-openclaw-kit:
	@mkdir -p $(HOME)/.aegis/kits
	sed 's/"version": *"[^"]*"/"version": "$(VERSION)"/' kits/openclaw.json \
		> $(HOME)/.aegis/kits/openclaw.json
	@echo "Kit manifest installed: $(HOME)/.aegis/kits/openclaw.json ($(VERSION))"
```

### 12.4 Channel extension distribution

The `@aegis/openclaw-channel-aegis` npm package needs to reach the VM at first boot. Two paths:

**v0.1:** Bootstrap installs from a local tarball shipped with the kit package:
```sh
# Bootstrap checks for pre-shipped tarball in a well-known location
CHANNEL_TGZ=$(find /usr/share/aegisvm/packages -name "openclaw-channel-aegis-*.tgz" 2>/dev/null | head -1)
if [ -n "$CHANNEL_TGZ" ]; then
    npm install -g "$CHANNEL_TGZ"  # local install, no network needed
else
    npm install -g @aegis/openclaw-channel-aegis  # fallback to registry
fi
```

The tarball is baked into the rootfs via `image.inject` or mounted from the host via a well-known path. This avoids requiring npm registry access for the channel extension.

**v0.2:** Pre-built OCI image includes both OpenClaw and the channel extension pre-installed.

---

## 13. Decisions Made

| Question | Decision | Rationale |
|----------|----------|-----------|
| **Bridge approach** | OpenClaw channel extension (Option B) | Same API surface as bundled channels — documented, versioned, can't break without breaking Telegram/Slack/etc. In-process, no WebSocket hop. |
| **Version pinning** | Pin exact version + integrity hash (`openclaw@0.2.19 --integrity=sha512-...`) | Breakage looks like "Aegis is flaky." No silent drift. |
| **Workspace** | Required for openclaw kit | Config, npm cache, blobs, memory, canvas, sessions all need persistent writable storage. |
| **Session authority** | Tether session is source of truth, OpenClaw session derived | Gateway and tether store own session lifecycle; OpenClaw just stores history. |
| **Pre-built image** | v0.1: npm install at first boot. v0.2: publish pinned image. | First boot only happens once per instance lifetime. Acceptable for v0.1. |
| **Channel extension** | TypeScript (npm package) | Natural for OpenClaw ecosystem. Runs in-process, uses channel monitor API. |
| **Bootstrap binary** | Go | Static binary, no Node dependency for config generation. Matches existing Aegis binary pattern. |
| **Auth sync** | Write at boot from env vars | Simple, sufficient. Secrets don't change while VM is running. |
| **Skills** | None pre-installed | User installs from ClawHub on demand. Skills persist in workspace. |

---

## 14. Open Questions

1. **OpenClaw channel extension API surface:** Map the exact TypeScript interfaces for the channel monitor pattern — `InboundContext` shape, delivery callback signature, media attachment format. Read from OpenClaw's bundled Telegram channel source as reference implementation.

2. **OpenClaw MCP config format:** Verify the exact config path and format for registering MCP tool servers. May be `mcp.json` in the config dir or a `tools.mcp` section in `openclaw.json`.

3. **Group chat semantics:** Does the extension need to pass `chatType: "group"` explicitly, or does OpenClaw infer it from the chat ID pattern? Need to test with the auto-reply system's mention gating.

4. **Session persistence across restarts:** When OpenClaw gateway restarts (VM resume), does it pick up existing sessions from disk or create new ones? Need to verify JSONL session files are loaded on startup.

---

## 15. Comparison with Previous Specs

| Aspect | Previous specs (standalone) | This spec (tether bridge) |
|--------|---------------------------|--------------------------|
| Telegram | OpenClaw's built-in grammY | AegisVM's aegis-gateway |
| Image support | OpenClaw's channel media | AegisVM blob store + tether |
| Session mgmt | OpenClaw owns everything | Tether sessions → OpenClaw sessions |
| Gateway | OpenClaw gateway exposed to host | OpenClaw gateway internal only |
| Boot command | `openclaw gateway` | `aegis-claw-bridge` (manages OpenClaw) |
| Channels | Telegram, WhatsApp, etc. | Single "aegis" channel via bridge |
| AegisVM integration | Loose (just a workload) | Tight (tether protocol, blob store, gateway) |
| Multi-messenger | OpenClaw handles each | AegisVM gateway handles each, unified through tether |

The key insight: previous specs tried to use OpenClaw as a complete stack. This spec uses OpenClaw as a brain and AegisVM as the body.

---

## 16. Implementation Friction Log (2026-02-26)

Hands-on implementation attempt revealed significant friction that may make the OpenClaw kit impractical compared to enriching the native agent kit.

### Pain points encountered

| Issue | Severity | Detail |
|-------|----------|--------|
| **OOM on startup** | Blocker | OpenClaw needs ~250MB heap just to boot. Default Node.js heap on 1GB VM is too small. Requires 2GB VM + `NODE_OPTIONS=--max-old-space-size=1536`. |
| **Silent exit on failure** | Blocker | `openclaw gateway` exits with code 1 and produces zero error output to stdout or stderr. No way to diagnose why it fails without wrapping in debug harnesses. |
| **No inbound plugin API** | High | Channel registration (`api.registerChannel`) handles outbound only. Inbound message dispatch requires importing OpenClaw internals (`dispatchInboundMessageWithDispatcher`, `loadConfig`, `resolveAgentRoute`). Every bundled channel does this — there is no plugin-facing dispatch API. |
| **Alpine incompatible** | High | `koffi` native module has no prebuilt binary for `linux-arm64-musl`. Falls back to source build requiring cmake + g++ + python3 (~340MB of build tools). Forces `node:22` full Debian image (~350MB) instead of Alpine. |
| **git required at install** | Medium | One of OpenClaw's transitive deps uses a `git://` URL. `node:22-slim` doesn't include git. Forces full `node:22` image. |
| **Strict config validation** | Medium | `openclaw.json` rejects all unknown keys with no passthrough. Our opinionated config template had to be stripped to bare minimum. Several keys from OpenClaw docs (`tools.allow`, `canvas.outputDir`, `memory.dataDir`, `memory.embedding`) were rejected as invalid. |
| **3 min cold boot** | Medium | `npm install -g openclaw@2026.2.24` takes ~3 min on first boot (698 packages). Cached after, but painful for dev iteration. |
| **~500MB footprint** | Medium | 350MB base image + 150MB npm packages + 2GB RAM requirement. Compared to agent kit: 70MB image + 5MB binary + 64MB RAM. |
| **Config overwrite behavior** | Low | OpenClaw's doctor auto-rewrites `openclaw.json` on startup (adds `commands`, `meta` sections, modifies auth). Makes it hard to maintain a predictable config. |
| **Extension discovery** | Low | Extensions are discovered from `{configDir}/extensions/` directory, not from `node_modules`. Required symlink workaround from npm global install location. |

### What worked

- Bootstrap binary (Go) — config generation from templates works cleanly
- npm install with workspace caching — second boot skips install
- Kit manifest and `aegis kit list` — shows correctly
- Auth profile generation from env vars — detected and wrote OpenAI profile
- Extension symlink discovery — OpenClaw found our channel extension
- `node:22` (full Debian) image — prebuilt native modules work, git available

### Assessment

The architecture (tether bridge, host gateway, OpenClaw as brain) is sound. The implementation friction is entirely on the OpenClaw side — it's a large, opinionated monolith not designed for embedding. The alternative — enriching our native agent kit with the missing tools (file edit, web fetch, memory search, context compaction) — is likely less total effort and produces a lighter, more debuggable result.

**Recommendation:** Park the OpenClaw kit. Invest in the native agent kit's tool ecosystem instead. Revisit OpenClaw integration if/when they ship a proper plugin dispatch API or a lighter agent-only package.
