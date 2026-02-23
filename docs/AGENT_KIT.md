# Aegis Agent Kit

Aegis Agent Kit adds an LLM agent runtime to AegisVM instances. Each agent runs inside an isolated microVM with its own filesystem, network, and tools — and can be reached from the host, from messaging apps, or from other agents.

The agent VM consumes zero CPU when idle. A message — from Claude Code, a messaging bot, or another agent — wakes it in milliseconds.

Agent Kit is an optional add-on — core AegisVM works without it. Install via `brew install aegisvm-agent-kit` or `make install-kit` (from source).

## What you get

**Delegation.** Claude on the host delegates a task to an isolated agent: "research this", "analyze that file", "run these tests." The agent has its own LLM context, its own workspace, and its own MCP tools. Results stream back via tether.

**Composability.** Agents can spawn sub-agents via the Guest API. A research agent spawns a data-processing agent, which spawns a visualization agent. Each runs in its own VM with its own resource limits.

**Messaging.** Connect agents to messaging apps (Telegram today, more planned). The gateway bridges messages to the agent, with wake-on-message and streaming responses. The agent VM sleeps between conversations.

**Isolation.** Each agent gets a real VM — separate kernel, no shared filesystem, explicit secret injection. A misbehaving agent can't affect the host or other agents.

## Components

| Component | Runs on | Binary | Purpose |
|-----------|---------|--------|---------|
| **Agent Runtime** | Guest (VM) | `aegis-agent` | LLM bridge, sessions, streaming, MCP tool use |
| **Gateway** | Host | `aegis-gateway` | Messaging app bridge (optional, per-instance) |
| **Tether** | Host ↔ Guest | Built into harness + aegisd | Bidirectional message channel (core) |
| **MCP Guest** | Guest (VM) | `aegis-mcp-guest` | Spawn/manage child instances from inside VM (core) |

```
Host agent (Claude) ──► tether ──► agent (VM) ──► LLM API
                                       │
Messaging app ──► Gateway ──► tether ──┘      ──► spawn child VMs
```

## Quick start

### 0. Install

```bash
brew tap xfeldman/aegisvm
brew install aegisvm aegisvm-agent-kit
```

### 1. Store your LLM key

```bash
aegis secret set OPENAI_API_KEY sk-...
```

Supports `OPENAI_API_KEY` (GPT-4o) or `ANTHROPIC_API_KEY` (Claude).

### 2. Start an agent

```bash
aegis instance start --kit agent --name my-agent --secret OPENAI_API_KEY
```

The agent is immediately reachable via tether. No messaging app needed.

```bash
aegis instance info my-agent
# Kit:         agent
# Gateway:     running
# Command:     aegis-agent
```

### 3. Talk to the agent

From Claude Code (via MCP):

```
tether_send(instance="my-agent", text="What Python libraries are best for data visualization?")
tether_read(instance="my-agent", after_seq=<ingress_seq>, wait_ms=15000)
```

Or via the API:

```bash
curl -X POST --unix-socket ~/.aegis/aegisd.sock \
  http://aegis/v1/instances/my-agent/tether \
  -H 'Content-Type: application/json' \
  -d '{"v":1,"type":"user.message","session":{"channel":"host","id":"test"},"payload":{"text":"Hello!"}}'

curl --unix-socket ~/.aegis/aegisd.sock \
  "http://aegis/v1/instances/my-agent/tether/poll?channel=host&session_id=test&after_seq=0&wait_ms=15000"
```

See [Tether](TETHER.md) for the full protocol reference.

### 4. Connect a messaging app (optional)

To bridge the agent to a messaging app, create a gateway config. Currently supported: **Telegram** (more channels planned).

```bash
aegis secret set TELEGRAM_BOT_TOKEN 123456:ABC-...

mkdir -p ~/.aegis/kits/my-agent
cat > ~/.aegis/kits/my-agent/gateway.json << 'EOF'
{
  "telegram": {
    "bot_token_secret": "TELEGRAM_BOT_TOKEN",
    "allowed_chats": ["*"]
  }
}
EOF
```

The gateway picks up the config within seconds and starts listening for messages. No restart needed.

## Use cases

### Agent delegation

Claude delegates long-running or specialized work to isolated agents:

```
Claude: tether_send("researcher", "Find the top 5 ML frameworks for time series forecasting, compare them")
        → agent boots, calls LLM with its own context, researches, responds
Claude: tether_read("researcher", wait_ms=30000)
        → "Here's my analysis: 1. Prophet..."
```

The agent has its own workspace, its own session history, and its own tools. Claude can delegate multiple tasks to multiple agents in parallel.

### Multi-agent orchestration

An agent can spawn sub-agents via the Guest API:

```
my-agent spawns:
  ├── data-agent (python:3.12-alpine) → cleans and transforms data
  ├── viz-agent (python:3.12-alpine) → generates charts
  └── report-agent → assembles final report
```

Each sub-agent runs in its own VM with resource limits inherited from the parent. No escalation possible.

### Messaging bot

Connect to a messaging app for conversational AI with zero infrastructure:

- Agent VM pauses when idle (zero CPU, zero cost)
- New message wakes the VM in ~500ms (cold) or ~35ms (resume)
- Streaming responses with typing indicators
- Session history persists across VM restarts
- Multiple bots run independently, each with its own agent

### Isolated code execution

Use the agent as a sandboxed code interpreter:

```
Claude: tether_send("sandbox", "Write a Python script that fetches Bitcoin price and plots a 30-day chart")
        → agent writes code, runs it in the VM, returns results
```

The agent has full filesystem access inside the VM but zero access to the host.

## How it works

### Tether

Tether is the bidirectional message channel between host and guest. Every instance has a built-in tether listener in the harness that acks messages and persists them to `/workspace/tether/inbox.ndjson`. Agent Kit adds the LLM responder that processes messages and streams intelligent responses.

See [Tether](TETHER.md) for protocol details, frame types, and the poll API.

### Wake-on-message

When a message arrives and the agent instance is paused or stopped:

1. Host agent (or gateway) POSTs the tether frame to aegisd
2. aegisd wakes the VM if needed (`EnsureInstance`)
3. Harness acks the frame and forwards it to the agent
4. Agent calls the LLM, streams the response back
5. Host reads the response via `tether_read` (or gateway renders it in the messaging app)

Cold boot: ~500ms. Resume from pause: ~35ms.

### Sessions

The agent maintains independent conversation histories per `channel:session_id`:

```
/workspace/sessions/host_default.jsonl      # Claude delegation session
/workspace/sessions/host_research-1.jsonl   # named research session
/workspace/sessions/telegram_123456.jsonl   # messaging app user
```

Sessions survive VM restarts. Context is assembled via sliding window (system prompt + last N turns within a character budget).

### Gateway

The gateway is a per-instance host-side process that bridges messaging apps with the agent via tether. It starts automatically with the kit instance but only activates when a config file is present at `~/.aegis/kits/{handle}/gateway.json`.

The gateway stays running while the VM sleeps — that's what enables wake-on-message. Hot-reloads config changes automatically. See [Kits](KITS.md) for how kit daemons work.

## Environment variables

### Agent runtime (inside VM)

| Variable | Description |
|----------|-------------|
| `OPENAI_API_KEY` | OpenAI API key (uses GPT-4o) |
| `ANTHROPIC_API_KEY` | Anthropic API key (uses Claude) |
| `AEGIS_SYSTEM_PROMPT` | Custom system prompt (default: "You are a helpful assistant.") |

### Gateway (host)

Each instance gets its own gateway process. Configure at `~/.aegis/kits/{handle}/gateway.json`. Multiple instances can run simultaneously, each with its own messaging bot.

## Custom OCI images

Kit binaries (`aegis-agent`) are injected into OCI image overlays when `--kit agent` is used, so you can use any base image:

```bash
aegis instance start --kit agent --name my-agent --image node:20-alpine --secret OPENAI_API_KEY
```

Core binaries (`aegis-harness`, `aegis-mcp-guest`) are always injected into every overlay regardless of kit.
