# Host Agent Tether Spec

**Status:** Draft
**Scope:** Enable host agents (Claude Code, Cursor, etc.) to communicate with in-VM agents via the tether protocol.

---

## 1. What This Enables

Tether is a sessioned, framed, duplex message channel into an instance. Today, Telegram is the only producer/consumer. This spec adds the host agent as another producer/consumer.

Use cases:
- **Delegation**: "go do X inside that VM, stream me progress"
- **Debugging**: "what are you doing / show internal state"
- **Orchestration**: agent-to-agent communication without exec/log spelunking
- **Wake-on-message**: host agent messages wake stopped/paused VMs (same as Telegram)

The guest agent runtime doesn't change — it already handles messages from any `session.channel`. The gateway doesn't change — it handles `channel: "telegram"`. Host agent uses `channel: "host"`.

---

## 2. MCP Tools

### tether_send

Fire-and-forget message to an in-VM agent. Does not wait for a response.

```json
{
  "name": "tether_send",
  "description": "Send a message to an agent running inside a VM instance. The message is delivered via the tether protocol. The instance wakes automatically if paused or stopped. Use tether_read to receive the agent's response.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "instance": { "type": "string", "description": "Instance handle or ID" },
      "text": { "type": "string", "description": "Message text to send to the agent" },
      "session_id": { "type": "string", "description": "Session identifier for conversation threading. Use a stable ID per conversation (e.g. 'debug-session-1'). Defaults to 'default'." }
    },
    "required": ["instance", "text"]
  }
}
```

**Behavior:**
1. Build tether frame: `v=1, type="user.message", session={channel:"host", id:<session_id>}, payload={text:<text>}`
2. POST to `/v1/instances/{instance}/tether` (triggers wake-on-message)
3. Return `{msg_id, session_id, ingress_seq}` immediately

The returned `ingress_seq` is the sequence number of the sent frame. Use it as `after_seq` in the first `tether_read` call to skip past your own message and start reading the agent's response. This avoids a race where the agent responds before the host starts reading.

### tether_read

Read response frames from an in-VM agent. Supports long-polling.

```json
{
  "name": "tether_read",
  "description": "Read response frames from an agent inside a VM instance. Returns new frames since the given cursor. Supports long-polling: set wait_ms to block until frames arrive or timeout. Call in a loop for streaming-like behavior.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "instance": { "type": "string", "description": "Instance handle or ID" },
      "session_id": { "type": "string", "description": "Session ID (must match the one used in tether_send). Defaults to 'default'." },
      "after_seq": { "type": "integer", "description": "Return frames with seq > this value. Use next_seq from previous tether_read response. Start with 0." },
      "limit": { "type": "integer", "description": "Max frames to return. Default: 50, max: 200." },
      "wait_ms": { "type": "integer", "description": "Long-poll timeout in milliseconds. If no frames available, block up to this duration. 0 = return immediately. Default: 0, max: 30000." },
      "types": { "type": "array", "items": { "type": "string" }, "description": "Filter by frame types. Example: ['assistant.delta', 'assistant.done']. Default: all types." },
      "reply_to_msg_id": { "type": "string", "description": "Filter frames that are replies to a specific msg_id. Useful for isolating one task's response in a multi-task session." }
    },
    "required": ["instance"]
  }
}
```

**Behavior:**
1. Query tether store for egress frames matching `(instance, channel="host", session_id)` with `seq > after_seq`
2. Apply `types` filter if specified
3. If frames found → return immediately (up to `limit`)
4. If none and `wait_ms > 0` → block until frame arrives or timeout
5. Return `{frames: [...], next_seq: <max_seq_returned>, timed_out: bool}`

**Output frame format:**
```json
{
  "frames": [
    {"type": "status.presence", "seq": 1, "ts": "...", "payload": {"state": "thinking"}},
    {"type": "assistant.delta", "seq": 2, "ts": "...", "payload": {"text": "Here's what I found..."}},
    {"type": "assistant.done", "seq": 5, "ts": "...", "payload": {"text": "Complete response text"}}
  ],
  "next_seq": 5,
  "timed_out": false
}
```

**Usage pattern (Claude calling in a loop):**
```
1. tether_send(instance="agent1", text="Analyze the data in /workspace/data.csv")
   → {msg_id: "host-1234", session_id: "default", seq: 0}

2. tether_read(instance="agent1", after_seq=0, wait_ms=10000)
   → {frames: [{type:"status.presence",...}, {type:"assistant.delta",...}], next_seq: 3}

3. tether_read(instance="agent1", after_seq=3, wait_ms=10000)
   → {frames: [{type:"assistant.delta",...}, {type:"assistant.done",...}], next_seq: 7}
   # Done — got assistant.done
```

---

## 3. Tether Store Changes

### Sequence numbers

Add per-instance monotonic sequence numbers to the tether store. Currently frames only have timestamps. Timestamps are garbage as cursors — seq gives stable pagination, deterministic resume, race-free `after_seq`.

```go
type Frame struct {
    // ... existing fields ...
    Seq int64 `json:"seq"` // per-instance monotonic sequence
}
```

`Append()` assigns the next sequence number atomically.

**Scope:** `seq` is per-instance, not per-session. All channels (host, telegram, future) share one monotonic counter per instance. This gives global ordering across channels, simpler store implementation, and easier debugging.

**Persistence:** `seq` must not reset on instance restart. The counter is persisted (or derived from the append log). Otherwise cursors break across reboots — a host agent holding `after_seq=15` would re-read old frames if seq restarted at 0.

### Filtered query

Add a query method that supports channel + session_id filtering:

```go
func (s *Store) Query(instanceID string, opts QueryOpts) []Frame
```

```go
type QueryOpts struct {
    Channel   string   // filter by session.channel (e.g. "host")
    SessionID string   // filter by session.id
    AfterSeq  int64    // frames with seq > this
    Types     []string // filter by frame type
    Limit     int      // max results
}
```

### Long-poll notification

Event-driven, not sleep-loop. Per `(instanceID, channel, sessionID)` notifier:

```go
func (s *Store) WaitForFrames(ctx context.Context, instanceID string, opts QueryOpts, timeout time.Duration) []Frame
```

Implementation: when `Append()` adds a frame, wake all waiters for that instance. Waiters re-query with their own filters. Don't try to track per-(channel, session) wait channels — per-instance notification is simpler and sufficient.

Minimal approach: per-instance `sync.Cond` or broadcast channel. Waiters wake, re-query, return if matching frames found, otherwise go back to sleep until timeout.

---

## 4. aegisd API Changes

### Existing (unchanged)

- `POST /v1/instances/{id}/tether` — ingress (already supports any channel)
- `GET /v1/instances/{id}/tether/stream` — egress SSE stream (kept for gateway use)

### New: poll endpoint

```
GET /v1/instances/{id}/tether/poll?channel=host&session_id=default&after_seq=0&limit=50&wait_ms=10000
```

Returns JSON object with frames array. Supports long-poll via `wait_ms`.

**Response:**
```json
{
  "frames": [...],
  "next_seq": 12,
  "timed_out": false
}
```

This is the primary endpoint for `tether_read`. No SSE parsing, no long-lived connections, clean request/response with optional blocking. Cancellation via request context.

The MCP server uses:
1. `POST /tether` for `tether_send`
2. `GET /tether/poll` for `tether_read`

---

## 5. Session Conventions

| Field | Value | Description |
|-------|-------|-------------|
| `session.channel` | `"host"` | Identifies frames from/to host agents |
| `session.id` | Caller-chosen | Stable per conversation. e.g. `"debug-1"`, `"task-analyze"` |
| `msg_id` | Auto-generated | `"host-{timestamp}"` for correlation |

The guest agent runtime routes by `session.channel + session.id` — each session gets independent conversation history. A host agent session doesn't interfere with Telegram sessions.

**Presence convention:** The guest agent runtime SHOULD emit `status.presence` frames (`{"state": "thinking"}`) before long operations. This enables host agents to show progress and improves delegation UX. The agent already does this for Telegram — same behavior applies to all channels.

---

## 6. Session Isolation in Guest Runtime

The guest agent runtime must key sessions by `channel:session_id`, not just `session_id`. This prevents cross-channel collision:

- Telegram session `"12345"` → session key `telegram:12345`
- Host session `"default"` → session key `host:default`

Even if session IDs collide across channels, they get independent conversation histories.

---

## 7. Security Model

Host agent tether uses the same auth as all aegisd API calls — unix socket access. The MCP server talks to aegisd over the socket, same as the CLI.

For v1:
- `tether_send` requires same access as other host API calls (unix socket)
- Any host agent can talk to any instance (same as exec, logs, etc.)

For later:
- Instance-level permission checks (which callers can tether to which instances)
- Rate limiting on tether ingress
- Audit log of host agent interactions

---

## 8. Implementation Plan

### Phase 1: Ship it

1. Add `seq` field to tether store frames (per-instance monotonic)
2. Add `Query()` method to tether store (channel + session_id + after_seq filtering)
3. Add `WaitForFrames()` to tether store (event-driven long-poll, not sleep loop)
4. Add `GET /v1/instances/{id}/tether/poll` endpoint to aegisd API
5. Add `tether_send` tool to `aegis-mcp`
6. Add `tether_read` tool to `aegis-mcp` (calls `/tether/poll`)
7. Update guest agent session keying to `channel:session_id`
8. Test: Claude sends message to in-VM agent, reads streaming response via long-poll loop

### Phase 2: Streaming MCP (future)

9. Add HTTP/SSE transport to `aegis-mcp`
10. Add `tether_send_stream` tool that streams deltas back to Claude in real-time
11. No aegisd changes needed — just transport upgrade in MCP server

---

## 9. What Doesn't Change

- **Guest agent runtime** — already handles messages from any channel
- **Gateway** — continues handling `channel: "telegram"` only
- **Tether protocol** — same frame format, same envelope
- **Wake-on-message** — works for host agent messages (same `EnsureInstance` path)
- **aegisd tether ingress/egress** — same API, additive changes only
