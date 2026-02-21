# OpenClaw on Aegis — Configuration Map

**Reference document for designing the Claw Kit. Maps OpenClaw's configuration, architecture, and operational modes to Aegis primitives.**

**Date:** 2026-02-21

---

## 1. Architecture Overview

OpenClaw is a single Node.js process that bundles:

| Layer | Role | Resource profile |
|-------|------|------------------|
| **Gateway** | WebSocket/HTTP server, channel routing, auth, session management | Lightweight (~50MB RSS idle) |
| **Lane Queue** | Per-session serial message execution, concurrency control | Minimal |
| **Pi Agent** | LLM calls, tool execution, workspace I/O | Heavy (400MB+ during LLM call) |

All three run in **one process**. There is no supported way to run the gateway separately from the agent in the current architecture. Agents are spawned in-process via `runEmbeddedPiAgent()` — not as separate processes.

### Can we split gateway from agent?

**Not without forking.** The gateway and agent share the same Node.js process, event loop, and memory space. The agent is invoked synchronously (per the lane queue) within the gateway's message handler. There is no IPC boundary.

**However:** OpenClaw supports `gateway.mode: "remote"` where a CLI/client connects to a remote gateway over WebSocket. This means:
- A **gateway VM** (always-on, lightweight) runs the full OpenClaw process
- A **client** (CLI, mobile app, Node Host) connects remotely

This doesn't split gateway from agent — both still run on the gateway VM. But it does mean the gateway VM is the only VM needed. The "split" happens at the client level, not the agent level.

### Implication for Aegis Kit

**One VM runs everything.** The Claw Kit can't split gateway and agent into separate VMs without forking OpenClaw. The kit must handle the full lifecycle: gateway + channels + agent in a single VM.

The idle/wake model must account for the full process (2GB RAM) being either running or paused.

---

## 2. Gateway Modes

| Mode | Config | Behavior |
|------|--------|----------|
| `local` | `gateway.mode: "local"` | Run gateway server locally. Default. |
| `remote` | `gateway.mode: "remote"` | Connect to a remote gateway (client-only). Ignores port/bind/auth. |

Remote mode config:
```json
{
  "gateway": {
    "mode": "remote",
    "remote": {
      "url": "wss://gateway.example.com",
      "transport": "direct",
      "token": "..."
    }
  }
}
```

---

## 3. Telegram Configuration

### Polling mode (default)

No `channels.telegram` config needed. Bot token from env var `TELEGRAM_BOT_TOKEN`.

```json
{
  "plugins": {
    "entries": {
      "telegram": { "enabled": true }
    }
  }
}
```

### Webhook mode

Setting `channels.telegram.webhookUrl` switches from polling to webhooks.

```json
{
  "channels": {
    "telegram": {
      "webhookUrl": "https://public-url.example.com/telegram-webhook",
      "webhookSecret": "random-secret-string",
      "webhookHost": "127.0.0.1",
      "webhookPath": "/telegram-webhook"
    }
  }
}
```

| Key | Default | Purpose |
|-----|---------|---------|
| `webhookUrl` | (none — polling mode) | Public URL Telegram POSTs to. Setting this enables webhook mode. |
| `webhookSecret` | (required when webhookUrl set) | Secret token for webhook HMAC verification |
| `webhookHost` | `127.0.0.1` | Bind address for the webhook HTTP server |
| `webhookPath` | `/telegram-webhook` | Local path the webhook server listens on |
| (port) | `8787` | Webhook server port (separate from gateway port 18789) |

**Important:** The webhook listener is a **separate HTTP server** on port 8787, not a handler on the gateway's port 18789. Both ports must be exposed.

### Known bug: auto-restart EADDRINUSE

OpenClaw 2026.2.19-2 has a bug where the Telegram provider's auto-restart logic fires even after a successful webhook setup, attempting to bind port 8787 a second time → `EADDRINUSE` crash. Related issues:
- [#8140](https://github.com/openclaw/openclaw/issues/8140) — Telegram channel lifecycle
- [#8907](https://github.com/openclaw/openclaw/issues/8907) — Webhook timeout for LLM-backed bots

**Workaround needed:** Likely requires upgrading OpenClaw or patching the restart logic.

### Per-account config

Bot tokens and webhook settings can be per-account:
```json
{
  "channels": {
    "telegram": {
      "accounts": {
        "default": {
          "botToken": "123:abc",
          "webhookSecret": "...",
          "webhookHost": "..."
        }
      }
    }
  }
}
```

---

## 4. Auth & Secrets

OpenClaw stores API keys in `auth-profiles.json`, NOT in environment variables. Even when env vars like `ANTHROPIC_API_KEY` are set, the agent runtime reads from the auth profile store.

```json
// ~/.openclaw/credentials/auth-profiles.json
{
  "version": 1,
  "profiles": {
    "openai:default": {
      "type": "api_key",
      "provider": "openai",
      "key": "sk-..."
    },
    "anthropic:default": {
      "type": "api_key",
      "provider": "anthropic",
      "key": "sk-ant-..."
    }
  }
}
```

Config must reference profiles:
```json
{
  "auth": {
    "profiles": {
      "openai:default": { "provider": "openai", "mode": "api_key" },
      "anthropic:default": { "provider": "anthropic", "mode": "api_key" }
    }
  }
}
```

**Kit implication:** The kit harness must translate Aegis secrets → auth-profiles.json at boot.

---

## 5. Gateway Config

```json
{
  "gateway": {
    "port": 18789,
    "mode": "local",
    "bind": "loopback",
    "auth": {
      "mode": "token",
      "token": "random-token"
    }
  }
}
```

| Key | Default | Kit value |
|-----|---------|-----------|
| `port` | 18789 | 18789 (exposed via Aegis) |
| `bind` | `loopback` | `loopback` (port proxy handles ingress bridging) |
| `auth.mode` | `token` | `token` (gateway UI auth) |

---

## 6. Agent Config

```json
{
  "agents": {
    "defaults": {
      "model": { "primary": "openai/gpt-4o" },
      "workspace": "/workspace/.openclaw/.openclaw/workspace",
      "maxConcurrent": 4,
      "subagents": { "maxConcurrent": 8 },
      "heartbeat": { "every": "30m" },
      "contextPruning": { "mode": "cache-ttl", "ttl": "1h" },
      "compaction": { "mode": "safeguard" }
    }
  }
}
```

---

## 7. Environment Variables

| Var | Source | Purpose |
|-----|--------|---------|
| `HOME` | Set in command | Must be `/workspace` (writable) |
| `OPENCLAW_HOME` | Set in command | Parent of `.openclaw/` config dir |
| `NODE_OPTIONS` | Set in command | `--max-old-space-size=1536` (prevent OOM with 2GB VM) |
| `npm_config_prefix` | Set in command | `/workspace/.npm-global` (persist npm installs) |
| `TELEGRAM_BOT_TOKEN` | Aegis secret | Bot token for Telegram |
| `ANTHROPIC_API_KEY` | Aegis secret | Anthropic API key |
| `OPENAI_API_KEY` | Aegis secret | OpenAI API key |

---

## 8. File Layout in VM

```
/workspace/                          ← Aegis workspace mount (writable, persists)
  .openclaw/                         ← OPENCLAW_HOME
    .openclaw/                       ← actual config dir (double-nested, OpenClaw quirk)
      openclaw.json                  ← main config
      agents/                        ← agent workspaces, sessions
      canvas/                        ← canvas UI assets
      credentials/                   ← auth-profiles.json
      telegram/                      ← telegram state (update offset, etc.)
      workspace/                     ← default agent workspace
  .npm-global/                       ← npm prefix (persists across restarts)
    bin/openclaw                     ← openclaw CLI
    lib/node_modules/openclaw/       ← openclaw package
```

---

## 9. Exposed Ports

| Guest port | Purpose | Wake-on-connect? |
|------------|---------|-------------------|
| 18789 | Gateway WebSocket + control UI + canvas | Yes (main entry point) |
| 8787 | Telegram webhook listener (webhook mode only) | Yes (Telegram POSTs here) |

---

## 10. Startup Command

```sh
export HOME=/workspace
export OPENCLAW_HOME=/workspace/.openclaw
export npm_config_prefix=/workspace/.npm-global
export PATH=/workspace/.npm-global/bin:$PATH
export NODE_OPTIONS="--max-old-space-size=1536"

if [ ! -f /workspace/.npm-global/bin/openclaw ]; then
  npm install -g openclaw@latest
fi

exec openclaw gateway --allow-unconfigured
```

---

## 11. Idle / Power Model for Claw Kit

### Polling mode (current)
- Outbound TCP connections from Telegram polling keep heartbeat active → VM never pauses
- To enable pause: switch to webhook mode

### Webhook mode (target)
- No outbound polling → no heartbeat → VM pauses after 60s idle
- Telegram POSTs to webhook URL → hits exposed port 8787 → wake-on-connect → VM resumes
- **Requires:** public URL (cloudflared tunnel or similar), `webhookUrl` + `webhookSecret` in config
- **Blocked by:** OpenClaw auto-restart EADDRINUSE bug (needs upgrade or patch)

### Always-on (fallback)
- Use keepalive lease from kit harness
- Or set `idle_policy: "leases_only"` and hold a permanent lease
- Highest resource usage (2GB RAM always allocated)

---

## 12. Kit Design Implications

1. **Single VM** — gateway + agent are inseparable in current OpenClaw architecture
2. **Config bootstrap** — kit must write `openclaw.json` + `auth-profiles.json` from Aegis secrets at boot
3. **Webhook for idle** — webhook mode is the only way to get proper pause/wake for Telegram bots
4. **Public URL** — webhook mode requires a tunnel (cloudflared). Kit could manage the tunnel as a sidecar.
5. **Double-nested config** — `OPENCLAW_HOME=/workspace/.openclaw` → config at `.openclaw/.openclaw/openclaw.json`
6. **npm persistence** — install to workspace prefix to survive restarts. Pre-built image would be better.
7. **Memory** — 2GB VM RAM + `NODE_OPTIONS=--max-old-space-size=1536` required
8. **Port proxy** — gateway binds to `127.0.0.1`, Aegis harness port proxy bridges automatically

Sources:
- [Telegram - OpenClaw docs](https://docs.openclaw.ai/channels/telegram)
- [Gateway Security - OpenClaw docs](https://docs.openclaw.ai/gateway/security)
- [Gateway Configuration - DeepWiki](https://deepwiki.com/openclaw/openclaw/3.1-gateway-configuration)
- [OpenClaw Architecture Lessons - Agentailor](https://blog.agentailor.com/posts/openclaw-architecture-lessons-for-agent-builders)
- [Every Way to Deploy OpenClaw - FlowZap](https://flowzap.xyz/blog/every-way-to-deploy-openclaw)
- [GitHub #8140 - Telegram Channel Issues](https://github.com/openclaw/openclaw/issues/8140)
- [GitHub #8907 - Webhook timeout](https://github.com/openclaw/openclaw/issues/8907)
