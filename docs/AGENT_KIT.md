# Aegis Agent Kit

Aegis Agent Kit turns AegisVM into a messaging-driven agent platform. The defining property: **messaging ingress lives outside the VM**, enabling wake-on-message and true scale-to-zero. The agent VM consumes zero CPU when idle. A new message wakes it in milliseconds.

Agent Kit is an optional add-on — core AegisVM works without it. Install via `brew install aegisvm-agent-kit` or `make install-kit` (from source).

## Components

| Component | Runs on | Binary | Shipped by |
|-----------|---------|--------|------------|
| **Aegis Gateway** | Host | `aegis-gateway` | Agent Kit |
| **Aegis Tether** | Host ↔ Guest | Built into aegisd + harness | Core |
| **Agent Runtime** | Guest (VM) | `aegis-agent` | Agent Kit |
| **MCP Guest** | Guest (VM) | `aegis-mcp-guest` | Core |

```
Telegram ──► Gateway (host) ──► aegisd tether API ──► harness ──► agent (VM)
                                                                      │
                                                                      ▼
Telegram ◄── Gateway (host) ◄── aegisd tether stream ◄── harness ◄── LLM API
```

## Quick start

### 0. Install the kit

```bash
# From Homebrew
brew install aegisvm-agent-kit

# Or from source
make install-kit
```

This installs the kit manifest to `~/.aegis/kits/agent.json` and the `aegis-gateway` + `aegis-agent` binaries.

Verify installation:

```bash
aegis kit list
# NAME         VERSION    STATUS     DESCRIPTION
# agent        v0.4.0     ok         Messaging-driven LLM agent with Telegram integration
```

### 1. Store your API keys

```bash
aegis secret set OPENAI_API_KEY sk-...
aegis secret set TELEGRAM_BOT_TOKEN 123456:ABC-...
```

The agent runtime supports OpenAI and Anthropic. Set whichever key you want to use:

- `OPENAI_API_KEY` — uses GPT-4o
- `ANTHROPIC_API_KEY` — uses Claude Sonnet

### 2. Start the agent instance

```bash
aegis instance start --kit agent --name my-agent --secret OPENAI_API_KEY
```

The `--kit agent` flag is a preset that supplies the command (`aegis-agent`), image (`python:3.12-alpine`), and spawn capabilities from the kit manifest. You can override any default with explicit flags — for example, `--kit agent -- sh` gives you a debug shell in a kit-configured VM.

This boots a VM with the agent runtime. The agent listens for tether frames on `127.0.0.1:7778` inside the VM, calls the LLM API, and streams responses back through the tether.

Verify it's running:

```bash
aegis instance info my-agent
aegis logs my-agent
```

You should see:

```
aegis-agent starting
LLM provider: OpenAI
agent listening on 127.0.0.1:7778
```

### 3. Configure the gateway

Create `~/.aegis/gateway.json`:

```json
{
  "telegram": {
    "bot_token_secret": "TELEGRAM_BOT_TOKEN",
    "instance": "my-agent",
    "allowed_chats": ["*"]
  }
}
```

| Field | Description |
|-------|-------------|
| `bot_token_secret` | Name of the aegis secret containing the Telegram bot token |
| `bot_token` | Alternative: inline bot token (not recommended) |
| `instance` | Handle of the agent instance to route messages to |
| `allowed_chats` | Chat IDs allowed to interact, or `["*"]` for all |

### 4. Start the gateway

The gateway starts automatically with `aegis up` when `~/.aegis/gateway.json` exists. The CLI discovers it through the Agent Kit manifest's `daemons` list.

```bash
aegis up
# aegis v0.4.0
# aegisd: started
# aegis-gateway: started (agent kit)
```

If no gateway config is present, you'll see:

```
aegis-gateway: no config (create ~/.aegis/gateway.json to enable)
```

To suppress all kit daemons:

```bash
aegis up --no-daemons
```

You can also start the gateway manually:

```bash
aegis-gateway
```

`aegis down` stops both the daemon and all kit daemons.

Send a message to your Telegram bot — you should see a streaming response with typing indicators.

### 5. Test without Telegram

You can test the tether round-trip directly via the aegisd API:

```bash
# Send a message
curl -X POST --unix-socket ~/.aegis/aegisd.sock \
  http://aegis/v1/instances/my-agent/tether \
  -H 'Content-Type: application/json' \
  -d '{
    "v": 1,
    "type": "user.message",
    "session": {"channel": "test", "id": "1"},
    "payload": {"text": "Hello!"}
  }'

# Stream the response
curl -N --unix-socket ~/.aegis/aegisd.sock \
  http://aegis/v1/instances/my-agent/tether/stream
```

## How it works

### Tether protocol

Tether is a bidirectional framed message stream between host and guest. It rides on the existing control channel (JSON-RPC 2.0 over vsock) — no new sockets.

All tether messages use a single JSON-RPC notification method: `tether.frame`. Direction is inferred from the frame type prefix.

**Envelope:**

```json
{
  "v": 1,
  "type": "user.message",
  "ts": "2026-02-22T00:00:00.000Z",
  "session": {"channel": "telegram", "id": "123456"},
  "msg_id": "...",
  "seq": 1,
  "payload": {
    "text": "Hello!",
    "user": {"id": "123", "username": "johndoe", "name": "John"}
  }
}
```

The `user.message` payload includes user identity for group chat support. The agent prepends `[name]: ` to message content so the LLM knows who is speaking.

**Frame types:**

| Direction | Type | Description |
|-----------|------|-------------|
| Host → Guest | `user.message` | User input |
| Host → Guest | `control.cancel` | Cancel in-flight request |
| Guest → Host | `assistant.delta` | Streaming text chunk |
| Guest → Host | `assistant.done` | Complete response |
| Guest → Host | `status.presence` | Typing/thinking indicator |
| Guest → Host | `event.ack` | Message acknowledgment |

### Wake-on-message

When a Telegram message arrives and the agent instance is paused or stopped:

1. Gateway POSTs the tether frame to `POST /v1/instances/{id}/tether`
2. The API handler calls `EnsureInstance()` — wakes the VM if needed
3. Frame is forwarded through the harness to the agent runtime
4. Response streams back through the same path

The agent VM can be fully stopped and will boot in ~500ms on first message. Paused instances resume in ~35ms.

### Session persistence

The agent runtime stores conversation history as JSONL files in the workspace:

```
/workspace/sessions/telegram_123456.jsonl
```

Each line is a turn: `{"role":"user","content":"...","ts":"..."}`. Sessions survive VM restarts — the agent loads them from disk on next boot.

Context assembly uses a sliding window: system prompt + last N turns within a character budget. No summarization in v0.1.

### Streaming UX

The gateway renders streaming responses to Telegram:

- First `assistant.delta` → `sendMessage` (creates the reply)
- Subsequent deltas → `editMessageText` (throttled to max 1/sec)
- `assistant.done` → final edit with complete text
- Typing indicator runs continuously until response completes (3s refresh, 2min hard timeout)

## API reference

### Tether ingress

```
POST /v1/instances/{id}/tether
```

Send a tether frame to an instance. Triggers wake-on-message if the instance is paused or stopped. Returns `202 Accepted`.

### Tether egress stream

```
GET /v1/instances/{id}/tether/stream
```

NDJSON stream of egress frames (assistant output, presence, events). Returns recent frames first, then streams live. Same pattern as `GET /v1/instances/{id}/logs?follow=1`.

### Secret value

```
GET /v1/secrets/{name}
```

Returns the decrypted value of a secret. Used by the gateway to resolve `bot_token_secret`. Only accessible via the unix socket.

## Environment variables

### Agent runtime (inside VM)

| Variable | Description |
|----------|-------------|
| `ANTHROPIC_API_KEY` | Anthropic API key (uses Claude) |
| `OPENAI_API_KEY` | OpenAI API key (uses GPT-4o) |
| `AEGIS_SYSTEM_PROMPT` | Custom system prompt (default: "You are a helpful assistant.") |

### Gateway (host)

| Variable | Description |
|----------|-------------|
| `TELEGRAM_BOT_TOKEN` | Telegram bot token (alternative to config file) |
| `TELEGRAM_BOT_TOKEN_SECRET` | Name of aegis secret to resolve bot token from |
| `AEGIS_GATEWAY_INSTANCE` | Instance handle (alternative to config file) |
| `AEGIS_GATEWAY_CONFIG` | Path to gateway config file |

## Using a custom OCI image

Kit binaries (`aegis-agent`) are injected into OCI image overlays when `--kit agent` is used, so you can use any base image:

```bash
aegis instance start --kit agent --name my-agent --image node:20-alpine --secret OPENAI_API_KEY
```

The `aegis-agent` binary is placed at `/usr/bin/aegis-agent` inside the overlay. Core binaries (`aegis-harness`, `aegis-mcp-guest`) are always injected into every OCI overlay regardless of kit. No need to include any of them in your image.
