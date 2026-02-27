# Tether

Tether is a bidirectional message channel between the host and agents running inside VMs. It enables host-to-agent communication for delegation, debugging, and orchestration — without SSH, exec, or log parsing.

## Key properties

- **Wake-on-message**: sending a message to a stopped or paused instance wakes it automatically (~500ms cold boot, ~35ms resume)
- **Sessioned**: each conversation gets an independent session with its own history
- **Ordered**: frames have monotonic sequence numbers for reliable cursor-based reading
- **Async**: send a message and read responses later — no blocking required
- **Universal**: works with any instance that has a guest agent (kit instances by default, all instances after core agent rollout)

## Quick start

### Send a message to an agent

```bash
# Via MCP (Claude Code)
tether_send(instance="my-agent", text="Analyze the data in /workspace/data.csv")
# Returns: {msg_id, session_id, ingress_seq}

# Via API
curl -X POST --unix-socket ~/.aegis/aegisd.sock \
  http://aegis/v1/instances/my-agent/tether \
  -H 'Content-Type: application/json' \
  -d '{"v":1,"type":"user.message","session":{"channel":"host","id":"default"},"payload":{"text":"Analyze the data"}}'
```

### Read the response

```bash
# Via MCP — long-poll until response arrives (up to 10s)
tether_read(instance="my-agent", after_seq=1, wait_ms=10000)
# Returns: {frames: [...], next_seq, timed_out}

# Via API
curl --unix-socket ~/.aegis/aegisd.sock \
  "http://aegis/v1/instances/my-agent/tether/poll?channel=host&session_id=default&after_seq=1&wait_ms=10000"
```

### Typical pattern (Claude calling in a loop)

```
1. tether_send(instance="agent1", text="Research X and summarize")
   → {ingress_seq: 5}

2. tether_read(instance="agent1", after_seq=5, wait_ms=10000)
   → {frames: [{type:"status.presence", state:"thinking"}, {type:"assistant.delta", text:"I found..."}], next_seq: 8}

3. tether_read(instance="agent1", after_seq=8, wait_ms=10000)
   → {frames: [{type:"assistant.done", text:"Full summary here..."}], next_seq: 12}
   # Done — got assistant.done
```

## MCP tools

### tether_send

Send a message to an in-VM agent. Fire-and-forget — does not wait for a response.

| Parameter | Required | Description |
|-----------|----------|-------------|
| `instance` | Yes | Instance handle or ID |
| `text` | Yes | Message text |
| `session_id` | No | Session identifier for threading. Default: `"default"` |

Returns `{msg_id, session_id, ingress_seq}`. Use `ingress_seq` as `after_seq` in the first `tether_read` to skip past your own message.

### tether_read

Read response frames from an in-VM agent. Supports long-polling.

| Parameter | Required | Description |
|-----------|----------|-------------|
| `instance` | Yes | Instance handle or ID |
| `session_id` | No | Must match `tether_send`. Default: `"default"` |
| `after_seq` | No | Cursor — return frames with seq > this. Start with 0 or `ingress_seq` |
| `limit` | No | Max frames. Default: 50, max: 200 |
| `wait_ms` | No | Long-poll timeout (ms). Block until frames arrive or timeout. Default: 0 (immediate), max: 30000 |
| `types` | No | Filter by frame type. Example: `["assistant.done"]` |

Returns `{frames, next_seq, timed_out}`. Use `next_seq` as `after_seq` in the next call.

## API endpoints

### Tether ingress

```
POST /v1/instances/{id}/tether
```

Send a tether frame. Triggers wake-on-message if instance is paused or stopped. Returns `{msg_id, session_id, ingress_seq}`.

### Tether poll

```
GET /v1/instances/{id}/tether/poll?channel=host&session_id=default&after_seq=0&limit=50&wait_ms=10000
```

Read egress frames with filtering and long-poll support. Returns `{frames, next_seq, timed_out}`.

| Parameter | Description |
|-----------|-------------|
| `channel` | Filter by session channel (e.g. `"host"`, `"telegram"`) |
| `session_id` | Filter by session ID |
| `after_seq` | Cursor — frames with seq > this |
| `limit` | Max frames (default 50, max 200) |
| `wait_ms` | Long-poll timeout in ms (default 0, max 30000) |
| `types` | Comma-separated frame types to include |
| `reply_to_msg_id` | Filter frames replying to a specific message |

### Tether stream (SSE)

```
GET /v1/instances/{id}/tether/stream
```

NDJSON stream of all egress frames. Used by the gateway for Telegram integration. For host agents, prefer the poll endpoint.

## Frame types

| Direction | Type | Description |
|-----------|------|-------------|
| Host → Guest | `user.message` | User/host input |
| Host → Guest | `control.cancel` | Cancel in-flight request |
| Guest → Host | `status.presence` | Thinking/typing indicator |
| Guest → Host | `assistant.delta` | Streaming text chunk |
| Guest → Host | `assistant.done` | Complete response |
| Guest → Host | `assistant.message` | Agent-initiated message (no prior user.message required) |
| Guest → Host | `event.ack` | Message acknowledgment |

## Sessions

Each tether conversation is identified by `channel` + `session_id`:

- `channel` identifies the source: `"host"` for host agents, `"telegram"` for Telegram, `"cron"` for scheduled tasks
- `session_id` is caller-chosen: `"default"`, `"debug-1"`, `"task-analyze"`, etc.

Sessions are independent — a host session doesn't interfere with Telegram sessions, even if session IDs collide. The guest agent keys sessions by `channel_session_id` internally.

## Sequence numbers

Every frame gets a monotonic `seq` number, per instance. Sequences are:

- **Global**: all channels share one counter per instance
- **Stable**: safe to use as cursors for pagination and resume
- **Persistent**: do not reset on instance restart

Use `after_seq` + `next_seq` for reliable cursor-based reading. No missed frames, no duplicates.

## How it works inside the VM

The guest agent (`aegis-agent`) handles tether frames automatically. When a `user.message` frame arrives:

1. Harness receives the frame via the control channel (vsock JSON-RPC)
2. Harness forwards it to the agent process on `127.0.0.1:7778`
3. Agent routes the message to the correct session (`channel:session_id`)
4. Agent processes the message (LLM call if API key present, fallback otherwise)
5. Agent emits `status.presence`, `assistant.delta`, and `assistant.done` frames back through the harness
6. Harness sends them to aegisd via the control channel
7. aegisd stores them in the tether store (with `seq` assigned)

The agent maintains independent session histories per `channel:session_id`. A host session (`host:debug-1`) doesn't interfere with a Telegram session (`telegram:12345`).

### Agent-initiated messages

The agent can emit frames without a prior `user.message` — for example, to report task completion or errors. These appear in the tether store with normal `seq` ordering and are readable via `tether_read`. The host agent reads them on its own schedule via polling.

### Without an LLM key

If no `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` is set, the agent runs in passive mode: it receives tether messages, stores them in the session, and responds with a fallback message. It does not make any external API calls.

## Use cases

**Delegation**: Claude sends a research task to a sandboxed agent, does other work, comes back to read the results.

**Debugging**: talk to a running agent to inspect state, ask what it's doing, get explanations — without parsing logs.

**Orchestration**: a host agent manages multiple in-VM agents, sending tasks and collecting results via tether.

**Multi-agent**: an in-VM agent can emit `assistant.message` frames without a prior `user.message` — for task completion notifications, error escalation, or telemetry.

**Messaging gateway**: the gateway bridges external messaging apps (Telegram) to agents via tether. Inbound messages become `user.message` frames; agent responses (`assistant.delta`, `assistant.done`) are rendered back in the messaging app. The gateway also fires cron-scheduled messages as synthetic `user.message` frames on the `"cron"` channel, enabling recurring tasks with scale-to-zero.

**Unsolicited messages**: the gateway delivers `assistant.done` frames that arrive without a preceding user message (e.g., restart notifications, future agent-push) as new messages in the messaging app.
