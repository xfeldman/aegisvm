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

## Friction Points — Aegis Issues

### #1 — Idle timer pauses VM during setup [HIGH — OPEN]
- **Problem:** npm install runs silently → Aegis idle timer (60s, connection-based only) pauses VM mid-install
- **Root cause:** idle timer only tracks inbound network connections, not process activity
- **Workaround:** keepalive curl pokes from host every 30s to exposed port
- **Proper fix:** consider process liveness for idle, or per-instance idle timeout override
- **Impact:** affects ANY app with a setup/build phase before serving

### #2 — No /etc/resolv.conf in OCI images [FIXED]
- **Problem:** OCI images don't have /etc/resolv.conf → DNS fails
- **Fix:** harness writes /etc/resolv.conf before read-only remount (mount_linux.go)

### #13 — No /etc/hosts in OCI images [FIXED]
- **Problem:** OCI images don't have /etc/hosts → localhost resolution may fail
- **Fix:** harness writes /etc/hosts with localhost entries before read-only remount

### #5 — Default 512MB RAM, no per-instance config [FIXED]
- **Fix:** added memory_mb and vcpus to instance create API, registry, MCP tools

### #7 — npm reinstalls on every restart [MEDIUM — WORKAROUND]
- **Problem:** global npm install to /usr/local is on rootfs overlay, lost on restart
- **Workaround:** install to workspace via npm_config_prefix=/workspace/.npm-global
- **Proper fix:** kit system with pre-built images or persistent install layer

## Friction Points — OpenClaw Issues (not Aegis)

### #3 — Alpine missing build dependencies
- node:22-alpine lacks git, cmake, build-base for native modules (koffi, node-llama-cpp)
- **Resolution:** switched to node:22 (Debian)

### #4 — OpenClaw bundles llama.cpp
- Compiles llama.cpp even when not needed, requires linux-headers, 5min+ compile, lots of RAM
- **Resolution:** use Debian image which has all headers

### #8 — Secrets don't reach OpenClaw auth store [MEDIUM]
- OpenClaw doesn't read API keys from env vars for agent runtime
- Wants keys in auth-profiles.json per agent
- **Workaround:** write auth-profiles.json manually with correct format:
  ```json
  {"version":1,"profiles":{"openai:default":{"type":"api_key","provider":"openai","key":"sk-..."}}}
  ```
- Also register in config: `auth.profiles.openai:default: {provider:"openai",mode:"api_key"}`
- **Proper fix:** kit harness reads Aegis secrets and writes auth files

### #9 — Pause breaks Telegram long-polling [EXPECTED]
- VM pause (SIGSTOP) freezes Telegram polling connection
- On resume, gateway may not reconnect immediately
- **Not a bug** — expected behavior, but UX consideration for always-on bots

### #10 — OPENCLAW_HOME double-nesting
- OPENCLAW_HOME=/workspace/.openclaw → config at /workspace/.openclaw/.openclaw/openclaw.json
- OpenClaw treats OPENCLAW_HOME as parent of .openclaw/, not as .openclaw/ itself
- **Workaround:** accept nesting, put config at double-nested path

### #11 — Agent errors not logged [CRITICAL for debugging]
- OpenClaw logs `isError=true` but never logs actual error message
- Only discoverable by running `openclaw agent --local` or reading session .jsonl files
- Session files show `"stopReason":"error","errorMessage":"Connection error."`
- Makes debugging inside a VM extremely painful

### #12 — Interactive onboarding required
- `openclaw onboard` requires interactive terminal (Y/N prompts)
- `openclaw setup --non-interactive --accept-risk` works but has other requirements
- **Workaround:** manually write config + auth files

### #14 — "Connection error." on all LLM API calls [FIXED — gvproxy]
- **Symptom:** every agent run fails with `stopReason: "error"`, `errorMessage: "Connection error."`, `totalTokens: 0`
- **Affects:** both Anthropic and OpenAI providers
- **Root cause: libkrun TSI has a ~32KB limit on outbound HTTP request bodies.**
  - OpenClaw sends ~46KB per API call (system prompt, tools, conversation context)
  - When request body exceeds ~32KB, the TSI vsock transport closes the socket
  - undici reports: `UND_ERR_SOCKET`, `cause: "other side closed"`
  - OpenAI SDK wraps this as `APIConnectionError("Connection error.")`
- **Fix:** gvproxy networking backend (virtio-net) — replaces TSI with a real NIC. No payload size limit.
  - See `specs/GVPROXY_NETWORKING.md` for full design
  - `brew install slp/krun/gvproxy`, restart aegisd
  - Tested: 50KB and 100KB POST bodies work, Telegram bot + agent conversations work end-to-end

### #15 — Apps binding to localhost unreachable via gvproxy [FIXED — harness port proxy]
- **Problem:** gvproxy forwards traffic to guest's eth0 IP (192.168.127.2), not localhost. Apps binding to 127.0.0.1 are unreachable from the host.
- **Root cause:** TSI intercepted AF_INET at the kernel level, making all traffic appear local. gvproxy uses a real NIC, so traffic arrives at the guest's IP.
- **Fix:** Harness starts a Go TCP proxy for each exposed port: `guestIP:port → 127.0.0.1:port`. Listens on the guest IP specifically (not 0.0.0.0) to avoid conflicts with apps that bind to 127.0.0.1 on the same port. Completely transparent — apps don't need any configuration changes.

### #16 — Kernel OOM killer with default 512MB RAM [FIXED — memory_mb via API]
- **Problem:** OpenClaw (Node.js) needs ~400MB heap + kernel overhead. Default 512MB VM RAM triggers kernel OOM killer silently.
- **Symptom:** Process killed with no stdout/stderr output, only visible in `dmesg`.
- **Fix:** `memory_mb: 2048` in API create request + `NODE_OPTIONS="--max-old-space-size=1536"` in command.
- **Note:** CLI `--memory` flag doesn't exist yet — must use API directly.

### #17 — `ip` command missing in Debian OCI images [FIXED — netlink syscalls]
- **Problem:** Harness used `ip` commands to configure eth0 in gvproxy mode. Debian-slim (node:22) doesn't include iproute2.
- **Fix:** Replaced `ip` commands with raw netlink syscalls in the harness. Zero dependency on rootfs tools for network setup.

## Current Status

### Working
- VM boots with node:22 Debian, 2GB RAM ✓
- OpenClaw installs and persists across restarts ✓
- Gateway starts, serves control UI on port 18789 ✓
- Telegram bot connects, receives messages, pairing works ✓
- Agent conversations work end-to-end (46KB+ API payloads via gvproxy) ✓
- OpenAI API reachable from VM (curl, Node.js fetch, SDK) ✓
- DNS resolution works (gvproxy built-in DNS at gateway) ✓
- /etc/hosts works (harness fix) ✓
- Wake-on-connect works for gateway port ✓
- Auth profiles correctly configured ✓
- Ingress via gvproxy port forwarding + harness port proxy ✓
- Network setup via netlink (works on any OCI image) ✓

### Remaining Issues
- #1 — Idle timer pauses VM during setup (keepalive workaround)
- #7 — npm reinstalls on every restart (workspace workaround)
- CLI `--memory` flag not implemented (must use API)

## Aegis Fixes Made During This Exercise

1. **Harness: /etc/resolv.conf injection** — writes nameserver before read-only remount
2. **Harness: /etc/hosts injection** — writes localhost entries before read-only remount
3. **API: per-instance memory_mb and vcpus** — full stack (API, registry, MCP, daemon restore)
4. **gvproxy networking backend** — virtio-net via gvproxy, eliminates TSI ~32KB outbound limit
5. **Harness: vsock control channel** — AF_VSOCK replaces TCP/TSI for harness ↔ aegisd RPC
6. **Harness: netlink network setup** — raw syscalls instead of `ip` commands (works on any OCI image)
7. **gvproxy CLI flags fix** — corrected `--listen-vfkit unixgram://` (was `--listen vfkit:unixgram://`)
8. **vsockConn wrapper** — Go's net.FileConn doesn't support AF_VSOCK, custom net.Conn wrapper
9. **Harness port proxy** — TCP proxy for exposed ports: guestIP:port → 127.0.0.1:port (transparent to apps)

## Lessons for Kit System

1. **Kits need a pid2 harness** that translates Aegis secrets → app-specific auth files
2. **Kits should use pre-built images** to avoid 20-min npm install on first boot
3. **Idle timeout needs to be kit-configurable** — always-on bots shouldn't pause
4. **Config bootstrapping is complex** — each app has its own config format, paths, auth mechanisms
5. **Error observability matters** — Aegis should surface app-level errors, not just process exit codes
6. **Bundled apps are hard to debug** — patching node_modules doesn't work when the app bundles its deps
7. **Per-instance env vars** would help for NODE_OPTIONS, debug flags etc. (Aegis already has this via --env)

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
