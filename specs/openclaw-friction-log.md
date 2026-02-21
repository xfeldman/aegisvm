# OpenClaw on Aegis — Friction Log

## Goal
Run OpenClaw (gateway + embedded agent) inside an Aegis VM with Telegram integration.

## Final Architecture
- Full OpenClaw process in Aegis VM (node:22 Debian, 2GB RAM)
- Workspace: ~/openclaw-workspace → /workspace/ in VM
- OpenClaw home: /workspace/.openclaw (OPENCLAW_HOME)
- Actual config at: /workspace/.openclaw/.openclaw/openclaw.json (double-nested)
- npm install persisted to /workspace/.npm-global (survives restarts)
- Secrets: ANTHROPIC_API_KEY, TELEGRAM_BOT_TOKEN, OPENAI_API_KEY via Aegis secrets
- Exposed: port 18789 (gateway control UI/WebSocket)
- Outbound: LLM APIs + Telegram API via gvproxy (virtio-net)
- Ingress: harness port proxy bridges guestIP → localhost (transparent to app)

## Current Status

**OpenClaw is fully working on Aegis.** Gateway, Telegram bot, and LLM agent conversations all function end-to-end.

### Working
- VM boots with node:22 Debian, 2GB RAM ✓
- OpenClaw installs and persists across restarts ✓
- Gateway starts, serves control UI on port 18789 ✓
- Telegram bot connects, receives messages, pairing works ✓
- Agent conversations work end-to-end (46KB+ API payloads via gvproxy) ✓
- DNS resolution works (gvproxy built-in DNS at gateway) ✓
- Wake-on-connect works for gateway port ✓
- Ingress via gvproxy port forwarding + harness port proxy ✓
- Network setup via netlink (works on any OCI image) ✓

---

## Open Aegis Issues

### #1 — Idle timer pauses VM during setup [HIGH]
- **Problem:** npm install runs silently → Aegis idle timer (60s, connection-based only) pauses VM mid-install
- **Root cause:** idle timer only tracks inbound network connections, not process activity
- **Workaround:** keepalive curl pokes from host every 30s to exposed port
- **Proper fix:** per-instance idle timeout override, or process-aware idle detection
- **Impact:** affects ANY app with a setup/build phase before serving

### #7 — npm reinstalls on every restart [MEDIUM]
- **Problem:** global npm install to /usr/local is on rootfs overlay, lost on restart
- **Workaround:** install to workspace via npm_config_prefix=/workspace/.npm-global
- **Proper fix:** kit system with pre-built images or persistent install layer

### CLI `--memory` flag missing [LOW]
- memory_mb and vcpus exist in the API but the CLI doesn't expose them
- Must use the API directly for now

---

## Open OpenClaw Issues (not Aegis bugs)

### #8 — Secrets don't reach OpenClaw auth store
- OpenClaw doesn't read API keys from env vars for agent runtime
- Wants keys in auth-profiles.json per agent
- **Workaround:** write auth-profiles.json manually in workspace
- **Kit fix:** kit harness reads Aegis secrets and writes auth files

### #9 — Pause breaks Telegram long-polling [EXPECTED]
- VM pause (SIGSTOP) freezes Telegram polling connection
- On resume, gateway may not reconnect immediately
- **Not a bug** — expected behavior, but UX consideration for always-on bots

### #10 — OPENCLAW_HOME double-nesting
- OPENCLAW_HOME=/workspace/.openclaw → config at /workspace/.openclaw/.openclaw/openclaw.json
- **Workaround:** accept nesting, put config at double-nested path

### #11 — Agent errors not logged [CRITICAL for debugging]
- OpenClaw logs `isError=true` but never logs actual error message
- Only discoverable by reading session .jsonl files
- Makes debugging inside a VM extremely painful

### #12 — Interactive onboarding required
- `openclaw onboard` requires interactive terminal (Y/N prompts)
- **Workaround:** manually write config + auth files

---

## Fixed Issues — Aegis

| # | Issue | Fix |
|---|-------|-----|
| 2 | No /etc/resolv.conf in OCI images | Harness writes resolv.conf before read-only remount |
| 5 | Default 512MB RAM, no per-instance config | Added memory_mb and vcpus to API, registry, MCP |
| 13 | No /etc/hosts in OCI images | Harness writes /etc/hosts before read-only remount |
| 14 | TSI ~32KB outbound payload limit (the blocker) | gvproxy networking backend (virtio-net). See GVPROXY_NETWORKING.md |
| 15 | Apps on localhost unreachable via gvproxy | Harness port proxy: guestIP:port → 127.0.0.1:port |
| 16 | Kernel OOM killer (512MB default) | memory_mb: 2048 via API + NODE_OPTIONS heap size |
| 17 | `ip` command missing in Debian images | Netlink syscalls in harness (zero rootfs dependency) |

### Implementation details (this session)
1. **gvproxy CLI flags** — corrected `--listen-vfkit unixgram://` (was `--listen vfkit:unixgram://`)
2. **vsockConn wrapper** — Go's net.FileConn doesn't support AF_VSOCK; custom net.Conn over raw fd
3. **Netlink network setup** — replaced `ip` shell-outs with raw RTM_NEWLINK/RTM_NEWADDR/RTM_NEWROUTE
4. **Harness port proxy** — per-port TCP proxy on guest IP, transparent to apps binding to localhost
5. **expose_ports in run RPC** — lifecycle manager sends guest ports to harness for proxy setup

## Fixed Issues — OpenClaw (resolved by image/config choice)

| # | Issue | Resolution |
|---|-------|------------|
| 3 | Alpine missing build deps | Switched to node:22 (Debian) |
| 4 | OpenClaw bundles llama.cpp | Debian image has all headers |

---

## Lessons for Kit System

1. **Kits need a pid2 harness** that translates Aegis secrets → app-specific auth files
2. **Kits should use pre-built images** to avoid 20-min npm install on first boot
3. **Idle timeout needs to be kit-configurable** — always-on bots shouldn't pause
4. **Config bootstrapping is complex** — each app has its own config format, paths, auth mechanisms
5. **Error observability matters** — Aegis should surface app-level errors, not just process exit codes

---

## Startup Command (working)

```sh
export HOME=/workspace
export OPENCLAW_HOME=/workspace/.openclaw
export npm_config_prefix=/workspace/.npm-global
export PATH=/workspace/.npm-global/bin:$PATH
export NODE_OPTIONS="--max-old-space-size=1536"

# First boot: install
if [ ! -f /workspace/.npm-global/bin/openclaw ]; then
  npm install -g openclaw@latest
fi

# Run gateway
exec openclaw gateway --allow-unconfigured
```

## Instance Creation (via API)

```bash
curl -s --unix-socket ~/.aegis/aegisd.sock -X POST http://aegis/v1/instances \
  -H 'Content-Type: application/json' -d '{
    "handle": "claw",
    "command": ["sh", "-c", "export HOME=/workspace && export OPENCLAW_HOME=/workspace/.openclaw && export npm_config_prefix=/workspace/.npm-global && export PATH=/workspace/.npm-global/bin:$PATH && export NODE_OPTIONS=\"--max-old-space-size=1536\" && if [ -f /workspace/.npm-global/bin/openclaw ]; then true; else npm install -g openclaw@latest; fi && exec openclaw gateway --allow-unconfigured"],
    "image_ref": "node:22",
    "workspace": "/Users/user/openclaw-workspace",
    "exposes": [{"port": 18789}],
    "secrets": ["ANTHROPIC_API_KEY", "TELEGRAM_BOT_TOKEN", "OPENAI_API_KEY"],
    "memory_mb": 2048
  }'
```

## Prerequisites

```bash
brew install slp/krun/gvproxy   # required for large payload support
```
