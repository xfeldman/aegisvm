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
- Outbound: LLM APIs + Telegram API via TSI

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

### #14 — "Connection error." on all LLM API calls [BLOCKING — ROOT CAUSE FOUND]
- **Symptom:** every agent run fails with `stopReason: "error"`, `errorMessage: "Connection error."`, `totalTokens: 0`
- **Affects:** both Anthropic and OpenAI providers
- **Root cause: libkrun TSI has a ~32KB limit on outbound HTTP request bodies.**
  - OpenClaw sends ~46KB per API call (system prompt, tools, conversation context)
  - When request body exceeds ~32KB, the TSI vsock transport closes the socket
  - undici reports: `UND_ERR_SOCKET`, `cause: "other side closed"`
  - OpenAI SDK wraps this as `APIConnectionError("Connection error.")`
- **Evidence:**
  - Request bodies <30KB work (get valid API responses or 400 errors)
  - Request bodies >35KB fail with socket close
  - curl and small SDK calls work because their payloads are under the limit
  - OpenClaw's 46KB payload consistently fails
- **Fix needed:** libkrun TSI vsock transport needs to handle large outbound payloads
- **Workaround:** none within Aegis — would need to either fix libkrun or use a proxy on the host side

## Current Status

### Working
- VM boots with node:22 Debian, 2GB RAM ✓
- OpenClaw installs and persists across restarts ✓
- Gateway starts, serves control UI on port 18789 ✓
- Telegram bot connects, receives messages, pairing works ✓
- OpenAI API reachable from VM (curl, Node.js fetch, SDK) ✓
- DNS resolution works (harness fix) ✓
- /etc/hosts works (harness fix) ✓
- Wake-on-connect works for gateway port ✓
- Auth profiles correctly configured ✓

### Not Working
- Agent fails on every LLM API call with "Connection error." (0 tokens)
- Both Anthropic and OpenAI providers affected
- Root cause unknown — SDK works directly but fails through OpenClaw's wrapper

## Aegis Fixes Made During This Exercise

1. **Harness: /etc/resolv.conf injection** — writes nameserver before read-only remount
2. **Harness: /etc/hosts injection** — writes localhost entries before read-only remount
3. **API: per-instance memory_mb and vcpus** — full stack (API, registry, MCP, daemon restore)

## Lessons for Kit System

1. **Kits need a pid2 harness** that translates Aegis secrets → app-specific auth files
2. **Kits should use pre-built images** to avoid 20-min npm install on first boot
3. **Idle timeout needs to be kit-configurable** — always-on bots shouldn't pause
4. **Config bootstrapping is complex** — each app has its own config format, paths, auth mechanisms
5. **Error observability matters** — Aegis should surface app-level errors, not just process exit codes
6. **Bundled apps are hard to debug** — patching node_modules doesn't work when the app bundles its deps
7. **Per-instance env vars** would help for NODE_OPTIONS, debug flags etc. (Aegis already has this via --env)

## Startup Command (working, minus the LLM issue)

```sh
export HOME=/workspace
export OPENCLAW_HOME=/workspace/.openclaw
export npm_config_prefix=/workspace/.npm-global
export PATH=/workspace/.npm-global/bin:$PATH

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
    "command": ["sh", "-c", "...startup command..."],
    "image_ref": "node:22",
    "workspace": "/Users/user/openclaw-workspace",
    "exposes": [{"port": 18789, "public_port": 18789}],
    "secrets": ["ANTHROPIC_API_KEY", "TELEGRAM_BOT_TOKEN", "OPENAI_API_KEY"],
    "memory_mb": 2048
  }'
```
