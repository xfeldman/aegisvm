# OpenClaw Tether Kit

**Status:** Draft
**Date:** 2026-02-25
**Supersedes:** Previous OpenClaw kit specs explored running OpenClaw standalone with its own Telegram channel. This spec takes a different approach — OpenClaw as agent brain, AegisVM tether as transport, aegis-gateway as messenger gateway.

---

## 1. Motivation

AegisVM has its own tether protocol, gateway (Telegram, with image support), and session management. OpenClaw has a rich agent runtime — tools, memory, browser control, canvas, context compaction — but its channels overlap with what AegisVM already provides.

Instead of running two competing channel stacks, we bridge them: a custom OpenClaw channel plugin receives messages via tether and sends responses back through tether. OpenClaw runs inside the VM as a black box, unaware it's inside AegisVM. AegisVM handles isolation, lifecycle, messaging, and image transport.

A critical benefit: **AegisVM's gateway runs on the host**, outside the VM. This means it survives VM pause/stop/restart and enables wake-on-message for all channels. OpenClaw's built-in channels run inside the VM — when the VM pauses, the channels die. By routing all messaging through our gateway and tether, we get proper power management for free: VM pauses when idle, wakes on any inbound message from any channel.

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

### Design principle: keep the bridge dumb

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

| Tether session | OpenClaw session |
|---------------|-----------------|
| `{channel: "telegram", id: "12345"}` | `agent:default:aegis:dm:telegram_12345` |
| `{channel: "host", id: "default"}` | `agent:default:aegis:dm:host_default` |

Each tether session maps to a unique OpenClaw session. Session history persists in OpenClaw's JSONL transcripts at `/workspace/.openclaw/.openclaw/agents/default/sessions/`.

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

### 4.3 Pre-set identity files

The bridge writes default identity files if they don't exist:

**`/workspace/.openclaw/.openclaw/AGENTS.md`:**
```markdown
You are an AI assistant running inside an isolated AegisVM microVM.

Your workspace is at /workspace/ — files here are shared with the host and persist across restarts.

You have access to Aegis MCP tools for infrastructure orchestration:
- Spawn child VMs for isolated workloads (instance_spawn)
- List and manage running instances (instance_list, instance_stop)
- Expose ports from child instances (expose_port)

Use child VMs for heavy or risky tasks — your own VM is the "bot" tier.
```

User can override by editing the file — the bridge only writes if missing.

### 4.4 MCP integration: aegis-mcp-guest

OpenClaw supports MCP tool servers. The kit registers aegis-mcp-guest (already injected by the harness into every VM) so the OpenClaw agent can use Aegis orchestration tools:

**`/workspace/.openclaw/.openclaw/mcp.json`** (or equivalent OpenClaw MCP config):
```json
{
  "mcpServers": {
    "aegis": {
      "command": "aegis-mcp-guest",
      "args": [],
      "description": "AegisVM guest tools — spawn child VMs, manage instances, expose ports"
    }
  }
}
```

This gives the OpenClaw agent access to `instance_spawn`, `instance_list`, `instance_stop`, `expose_port`, `self_info`, etc. — the same tools available to the lightweight agent kit, but now wielded by OpenClaw's richer reasoning engine.

### 4.5 Generated `openclaw.json`

```json
{
  "gateway": {
    "port": 18789,
    "mode": "local",
    "bind": "loopback"
  },
  "agents": {
    "defaults": {
      "model": {"primary": "${MODEL_FROM_SECRETS}"},
      "workspace": "/workspace",
      "maxConcurrent": 4,
      "tools": {
        "allow": ["*"]
      },
      "sandbox": {"mode": "off"},
      "compaction": {"mode": "safeguard"},
      "canvas": {
        "outputDir": "/workspace/canvas"
      }
    }
  },
  "memory": {
    "dataDir": "/workspace/.openclaw/memory",
    "embedding": {
      "provider": "${EMBEDDING_PROVIDER_FROM_SECRETS}"
    }
  },
  "channels": {},
  "plugins": {
    "entries": {}
  }
}
```

Model auto-detection:
- `ANTHROPIC_API_KEY` set → `"anthropic/claude-sonnet-4-20250514"`, embedding provider `"anthropic"`
- `OPENAI_API_KEY` set → `"openai/gpt-4o"`, embedding provider `"openai"`
- Both set → prefer Anthropic (consistent with agent kit behavior)

### 4.6 Auth profiles

Generated at boot from Aegis secrets — always rewritten (secrets may change between restarts):

```json
{
  "version": 1,
  "profiles": {
    "anthropic:default": {
      "type": "api_key",
      "provider": "anthropic",
      "key": "${ANTHROPIC_API_KEY}"
    },
    "openai:default": {
      "type": "api_key",
      "provider": "openai",
      "key": "${OPENAI_API_KEY}"
    }
  }
}
```

Only profiles for available secrets are written. Missing keys → profile omitted.

### 4.7 Environment

```sh
HOME=/workspace
OPENCLAW_HOME=/workspace/.openclaw
NODE_OPTIONS="--max-old-space-size=384"   # conservative for bot tier
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

**`aegis-claw-bootstrap`** (injected shell script):
```sh
#!/bin/sh
set -e

export HOME=/workspace
export OPENCLAW_HOME=/workspace/.openclaw
export NODE_OPTIONS="--max-old-space-size=384"
export npm_config_prefix=/workspace/.npm-global
export PATH=/workspace/.npm-global/bin:$PATH

# First-boot: install OpenClaw + tether channel extension
if [ ! -f /workspace/.npm-global/bin/openclaw ]; then
  npm install -g openclaw@0.2.x @aegis/openclaw-channel-aegis
fi

# Generate config from env vars (idempotent)
aegis-claw-config  # helper that writes openclaw.json, auth-profiles.json, AGENTS.md, mcp.json

# Hand off to OpenClaw — it becomes the main process
exec openclaw gateway --allow-unconfigured
```

After `exec`, the process tree is:
```
harness (PID 1) → openclaw gateway (main process)
                     └─ @aegis/openclaw-channel-aegis (in-process plugin, listens :7778)
                     └─ Pi Agent (in-process, spawned per session)
```

No bridge process. No wrapper. OpenClaw IS the main process. The tether channel extension runs inside it.

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
  │     If missing: npm install -g openclaw@0.2.x @aegis/openclaw-channel-aegis (~60s first time)
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

**v0.1:** Pin `openclaw@0.2.x` (or whatever the current stable semver range is). Bridge checks installed version at boot — if outside pinned range, reinstalls.

**v0.2:** Publish a pre-built OCI image (`aegis-openclaw:0.2`) with OpenClaw pre-installed. Eliminates npm install entirely. Kit manifest changes `image.base` from `node:22-alpine` to `aegis-openclaw:0.2`.

---

## 7. Bridge Implementation: OpenClaw Channel Extension

The bridge is an OpenClaw channel extension (`@aegis/openclaw-channel-aegis`) — an npm package that registers as a custom channel inside the OpenClaw gateway process.

### Why channel extension, not WebSocket client

| | Channel extension (chosen) | WebSocket client (rejected) |
|---|---|---|
| **API surface** | Channel monitor pattern — same interface OpenClaw's own Telegram/Slack/Discord use | Client protocol — designed for human-facing apps (CLI, mobile) |
| **Stability** | OpenClaw can't break it without breaking their own bundled channels | Can change for UX reasons unrelated to us |
| **Coupling** | Documented, versioned via npm, discoverable from `node_modules` | Reverse-engineered from source, undocumented |
| **Process model** | In-process with gateway — no extra process, no WebSocket hop | Separate process + WebSocket connection |
| **Media handling** | Built into channel API (normalized media attachments) | Must re-implement media serialization |
| **Streaming** | Channel delivery callback gives us response chunks natively | Must parse WS frames and reconstruct streaming |

The channel extension API is OpenClaw's core abstraction for messaging. Every bundled channel depends on it. It's the most stable surface to couple to.

### Package structure

```
@aegis/openclaw-channel-aegis/
  package.json          ← declares as OpenClaw extension (channel type)
  src/
    index.ts            ← channel monitor: tether HTTP ↔ InboundContext
    tether.ts           ← tether frame send/receive helpers
  dist/                 ← compiled JS
```

Installed into the OpenClaw environment at boot:
```sh
npm install -g @aegis/openclaw-channel-aegis
```

Or bundled directly into the pre-built image (v0.2).

### Registration

The extension auto-registers when its npm package is present in `node_modules/@aegis/`. OpenClaw discovers extensions at gateway startup via `package.json` metadata. No manual config needed — the bridge writes a minimal channel config section:

```json
{
  "channels": {
    "aegis": {
      "enabled": true
    }
  }
}
```

### What the channel monitor does

1. **Startup:** Opens HTTP server on `:7778` (tether recv endpoint)
2. **Ingress:** Receives tether `user.message` → normalizes to `InboundContext` → dispatches to OpenClaw auto-reply
3. **Egress:** Receives OpenClaw response via channel delivery callback → emits tether `assistant.delta` / `assistant.done` frames
4. **Media:** Reads image blobs from workspace for ingress, writes blobs for egress
5. **Health:** Sends `status.ready` tether frame once OpenClaw gateway is fully initialized

---

## 8. Image Support Through the Bridge

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

## 9. Workspace Layout

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

## 10. Guest Orchestration

OpenClaw kit inherits the same guest orchestration as the agent kit. The bridge (or OpenClaw tools) can spawn child VMs via aegis-mcp-guest tools:

- `instance_spawn` — spin up a work instance for heavy tasks
- `instance_list`, `instance_stop` — manage children
- `expose_port` — expose services from child instances

OpenClaw's built-in `exec` and `bash` tools run inside the OpenClaw VM. For isolated workloads, spawn a child.

---

## 11. Lifecycle & Power Model

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

## 12. What Changes in AegisVM Core

**Nothing.** The bridge is a guest binary, injected like aegis-agent. The kit manifest uses existing fields. Tether, blob store, gateway, harness — all unchanged.

New artifacts to build:
- `cmd/aegis-claw-bridge/` — the bridge binary (Node.js wrapper or Go+Node hybrid)
- `kits/openclaw.json` — kit manifest

---

## 13. Decisions Made

| Question | Decision | Rationale |
|----------|----------|-----------|
| **Bridge approach** | OpenClaw channel extension (Option B) | Same API surface as bundled channels — documented, versioned, can't break without breaking Telegram/Slack/etc. In-process, no WebSocket hop. |
| **Version pinning** | Pin to semver range (e.g., `openclaw@0.2.x`) | Breakage looks like "Aegis is flaky." Explicit upgrade path later. |
| **Pre-built image** | v0.1: npm install at first boot. v0.2: publish pinned image. | First boot only happens once per instance lifetime. Acceptable for v0.1. |
| **Bridge language** | Node.js (TypeScript) | Natural for OpenClaw ecosystem. Bridge talks WS to OpenClaw, HTTP to harness. |
| **Auth sync** | Write at boot from env vars | Simple, sufficient. Secrets don't change while VM is running. |
| **Skills** | None pre-installed | User installs from ClawHub on demand. Skills persist in workspace. |

---

## 14. Open Questions

1. **OpenClaw WebSocket client protocol:** Need to map the exact message format for Option A. The CLI and mobile apps use it — reverse-engineer from TypeScript source (`src/gateway/`) or find documentation.

2. **OpenClaw MCP config format:** Verify the exact config path and format for registering MCP tool servers. May be `mcp.json` in the config dir or a `tools.mcp` section in `openclaw.json`.

3. **Group chat semantics:** Does the bridge need to pass `chatType: "group"` explicitly, or does OpenClaw infer it from the chat ID pattern? Need to test with the auto-reply system's mention gating.

4. **Session reset on VM restart:** When the bridge reconnects to OpenClaw after a VM resume, does OpenClaw pick up the existing session or create a new one? Need to verify session persistence across gateway restarts.

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
