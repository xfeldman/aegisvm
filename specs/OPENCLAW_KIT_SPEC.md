# OpenClaw Kit for Aegis

**Claw Kit — Telegram bot + on-demand agent workloads**

**Status:** Design draft (informed by hands-on friction testing)
**Date:** 2026-02-21
**Depends on:** Guest Orchestration API (TBD), [IDLE_POWER_STATES.md](IDLE_POWER_STATES.md), [GVPROXY_NETWORKING.md](GVPROXY_NETWORKING.md), [OPENCLAW_CONFIG_MAP.md](OPENCLAW_CONFIG_MAP.md)

---

## 1. Architecture (learned from friction testing)

OpenClaw is a **single Node.js process** that bundles gateway + agent runtime. There is no supported way to separate them. The agent is invoked in-process via `runEmbeddedPiAgent()`.

This means the Claw Kit uses a **two-tier instance model**:

```
claw-bot (always-on, ~200MB idle, polling or webhook)
  │
  │  receives "build me a website"
  │  calls Guest Aegis API → spawn work instance
  │
  ├── work-instance-1 (node:22, 2GB, --expose 8080, workspace)
  │     builds the site, serves it
  │     idles → pauses → stops (proper lifecycle)
  │
  │  receives "show me the logs"
  │  calls Guest Aegis API → exec on work-instance-1
  │
  └── work-instance-2 (python:3.12, 1GB, workspace)
        runs data analysis
        completes → stops
```

### Bot instance (always-on)

- Runs OpenClaw gateway with Telegram polling
- Lightweight: handles message routing, session management, auth
- Does NOT run heavy agent work itself
- Always-on: `idle_policy: "leases_only"` with permanent lease (or heartbeat keeps it alive via polling)
- Spawns work instances via Guest Aegis API when tasks arrive

### Work instances (on-demand)

- Spawned by the bot for heavy tasks (coding, building, serving)
- Full agent runtime with tools, workspace, exposed ports
- Proper idle/pause/stop lifecycle
- Bot monitors progress, reports back to Telegram user
- Can be long-running (hours) or ephemeral (minutes)

### Why this split?

| Concern | Bot instance | Work instance |
|---------|-------------|---------------|
| Lifetime | Always-on | On-demand |
| Memory | ~200MB | 1-2GB |
| Idle behavior | Never pauses | Pauses after 60s idle |
| Wake trigger | N/A (always running) | Inbound connection or bot restarts it |
| Network | Outbound only (Telegram API) | Outbound + exposed ports |
| Workspace | Config + state only | Full project workspace |

---

## 2. Key Dependency: Guest Aegis API

The bot instance needs to call aegisd to spawn/manage work instances. This requires:

- **aegis-guestd**: in-guest broker (PID2 or sidecar), exposes local HTTP/socket API
- **Capability token**: injected at boot, scopes what the guest can do (spawn, max children, allowed images, resource ceilings)
- **Control channel forwarding**: guestd → harness control channel → aegisd

**This is an Aegis core feature, not kit-specific.** Any kit that needs agent-to-infrastructure orchestration uses the same Guest API.

See: Guest Orchestration API spec (TBD)

---

## 3. OpenClaw Configuration (from friction testing)

Full reference: [OPENCLAW_CONFIG_MAP.md](OPENCLAW_CONFIG_MAP.md)

### Bot instance config

```json
{
  "gateway": {
    "port": 18789,
    "mode": "local",
    "bind": "loopback"
  },
  "agents": {
    "defaults": {
      "model": { "primary": "openai/gpt-4o" },
      "maxConcurrent": 4
    }
  },
  "plugins": {
    "entries": {
      "telegram": { "enabled": true }
    }
  }
}
```

### Telegram modes

| Mode | Config | Idle behavior | Wake |
|------|--------|--------------|------|
| Polling (default) | No `webhookUrl` | Never pauses (outbound TCP keeps heartbeat alive) | N/A |
| Webhook | `channels.telegram.webhookUrl` + `webhookSecret` | Pauses after 60s | Inbound POST wakes VM |

**Webhook mode requires:**
- Public URL (cloudflared tunnel or similar)
- Exposed port 8787 (webhook listener, separate from gateway port 18789)
- Patched OpenClaw: `startTelegramWebhook()` returns immediately, causing gateway restart loop → EADDRINUSE. Fix: block on abort signal after webhook server starts.

**Polling mode is simpler but VM never pauses.** Acceptable for always-on bot tier.

### Auth & secrets

OpenClaw stores API keys in `auth-profiles.json`, not env vars. Kit harness must translate Aegis secrets → auth-profiles.json at boot.

### Known quirks

- `OPENCLAW_HOME` double-nesting: set to `/workspace/.openclaw`, config at `/workspace/.openclaw/.openclaw/openclaw.json`
- `NODE_OPTIONS="--max-old-space-size=1536"` required with 2GB VM (default Node.js heap too small)
- npm install to `/workspace/.npm-global` (persists across restarts via workspace)
- Gateway binds to `127.0.0.1` by default — Aegis harness port proxy bridges to guest IP transparently

---

## 4. Instance Definitions

### Bot instance

```bash
# Via API
curl -s --unix-socket ~/.aegis/aegisd.sock -X POST http://aegis/v1/instances \
  -H 'Content-Type: application/json' -d '{
    "handle": "claw-bot",
    "command": ["sh", "-c", "...startup command..."],
    "image_ref": "node:22",
    "workspace": "/path/to/claw-workspace",
    "exposes": [{"port": 18789}],
    "secrets": ["ANTHROPIC_API_KEY", "OPENAI_API_KEY", "TELEGRAM_BOT_TOKEN"],
    "memory_mb": 512,
    "idle_policy": "leases_only"
  }'
```

Startup command:
```sh
export HOME=/workspace
export OPENCLAW_HOME=/workspace/.openclaw
export npm_config_prefix=/workspace/.npm-global
export PATH=/workspace/.npm-global/bin:$PATH
export NODE_OPTIONS="--max-old-space-size=384"

if [ ! -f /workspace/.npm-global/bin/openclaw ]; then
  npm install -g openclaw@latest
fi

exec openclaw gateway --allow-unconfigured
```

Note: 512MB + 384MB heap for bot-only mode (no heavy agent work).

### Work instance (spawned by bot via Guest API)

```json
{
  "handle": "claw-work-abc123",
  "command": ["sh", "-c", "...agent task command..."],
  "image_ref": "node:22",
  "workspace": "/path/to/project-workspace",
  "exposes": [{"port": 8080}],
  "secrets": ["ANTHROPIC_API_KEY", "OPENAI_API_KEY"],
  "memory_mb": 2048
}
```

Work instances use default idle policy — heartbeat keeps them alive during work, pause after 60s idle.

---

## 5. File Layout

```
/path/to/claw-workspace/               ← bot workspace (Aegis workspace mount)
  .openclaw/
    .openclaw/
      openclaw.json                     ← main config
      credentials/auth-profiles.json    ← API keys
      agents/                           ← agent state
      telegram/                         ← telegram state (update offset)
  .npm-global/
    bin/openclaw                        ← openclaw CLI
    lib/node_modules/openclaw/          ← openclaw package

/path/to/project-workspace/             ← work instance workspace (separate)
  src/
  package.json
  ...
```

---

## 6. Aegis Fixes Made During OpenClaw Testing

| # | Fix | Impact |
|---|-----|--------|
| 1 | gvproxy in-process (virtio-net) | Eliminates TSI 32KB outbound limit, zero CPU during pause |
| 2 | Netlink network setup | Works on any OCI image (no iproute2 dependency) |
| 3 | vsockConn wrapper | AF_VSOCK support in Go harness |
| 4 | Harness port proxy | Bridges guestIP:port → localhost:port for apps binding to 127.0.0.1 |
| 5 | Activity heartbeat | Prevents pausing during active work (TCP, CPU, network) |
| 6 | Keepalive lease | Kit-facing explicit pause prevention with TTL |
| 7 | idle_policy | Per-instance control over heartbeat vs lease-only |
| 8 | Per-instance memory_mb/vcpus | API-level resource overrides |

---

## 7. Open Issues

### Blocking for Claw Kit

1. **Guest Aegis API** — bot instance must be able to spawn/manage work instances from inside the VM
2. **Capability tokens** — scope what guests can do (spawn limits, allowed images, resource ceilings)
3. **aegis-guestd** — in-guest broker that enforces capabilities and proxies to aegisd

### OpenClaw bugs (not Aegis)

4. **Webhook auto-restart EADDRINUSE** — `startTelegramWebhook()` returns immediately, gateway restarts provider. Patched locally in 4 dist files. Needs upstream fix or permanent patch in kit image.
5. **Agent errors not logged** — OpenClaw logs `isError=true` but not the actual error message. Makes debugging inside VMs painful.
6. **Config validation loop** — legacy `agent` key triggers validation errors even after migration.

### Nice to have

7. **CLI --memory flag** — memory_mb exists in API but not CLI
8. **Pre-built OpenClaw image** — avoid 20-min npm install on first boot
9. **Per-instance idle timeout** — different workloads need different idle windows

---

## 8. Historical Note

An earlier version of this spec (v0, 2026-02-17) imagined OpenClaw as a multi-agent swarm system with coordinators, shared workspaces, inter-VM networking, and agent roles. That design was speculative — written before actually running OpenClaw.

Hands-on testing revealed OpenClaw is a single-process gateway+agent, not a multi-agent coordinator. The architecture pivoted to bot + work instances, which is simpler, more aligned with Aegis's instance model, and doesn't require shared workspace volumes or inter-VM networking in core.

The v0 spec is preserved at `specs/OPENCLAW_KIT_SPEC_v0.md` for reference.
