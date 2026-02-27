# Core Tether Listener Spec

**Status:** Draft
**Scope:** Make tether universal by adding a built-in listener to the harness. Every VM can receive tether messages. LLM response stays kit-only.

---

## 1. Two-Layer Model

### Layer 0: Tether Listener (core, default-on)

Built into the harness. Every VM gets this. No new binary, no new process.

On tether frame received:
- Ack immediately (`event.ack`) — always, unconditionally
- Persist to `/workspace/tether/inbox.ndjson` (append-only)
- If a responder is connected: forward the frame to it
- If no responder: do nothing (no fallback, no timeout guessing)

The host handles "no response" via `tether_read` timeout (`timed_out: true`). The harness never guesses whether a responder will appear.

### Layer 1: Responder Runtime (kit, optional)

The current `aegis-agent` — LLM bridge, sessions, streaming, MCP tool use. Installed via `--kit agent`. Connects to the harness's tether mailbox and consumes frames.

**Rule: every VM can receive tether; only kit instances reply intelligently.**

---

## 2. What the Harness Does

Today the harness receives `tether.frame` notifications via the control channel and forwards them to `127.0.0.1:7778` (where aegis-agent listens).

Change: the harness becomes the primary tether endpoint.

```
tether frame arrives via control channel
  │
  ├── persist to /workspace/tether/inbox.ndjson
  ├── send event.ack back to host
  │
  ├── if responder connected (unix socket or TCP):
  │     forward frame to responder
  │     responder handles LLM, streaming, etc.
  │
  └── if no responder:
        nothing — message is persisted, ack is sent
        host handles silence via tether_read timeout
```

### Inbox format

`/workspace/tether/inbox.ndjson` — one JSON frame per line, append-only:

```jsonl
{"v":1,"type":"user.message","ts":"...","session":{"channel":"host","id":"default"},"msg_id":"host-123","payload":{"text":"Hello"}}
{"v":1,"type":"user.message","ts":"...","session":{"channel":"telegram","id":"456"},"msg_id":"tg-456-1","payload":{"text":"Hi there"}}
```

This gives:
- Durability: messages survive VM restart
- Backlog: a responder added later can read the backlog
- Debugging: `cat /workspace/tether/inbox.ndjson` shows all received messages
- No dependency on responder being ready at boot

### Responder interface

The harness exposes a local unix socket at `/run/tether.sock` for responders:

- Responder connects and reads frames (newline-delimited JSON)
- Responder writes response frames back (deltas, done, presence)
- Harness forwards response frames to the control channel → aegisd → tether store

If no responder connects within a short window after a frame arrives (~100ms), the harness emits the fallback response.

Alternative (simpler): the responder reads `/workspace/tether/inbox.ndjson` and writes to `/workspace/tether/outbox.ndjson`. The harness watches outbox and forwards. File-based, no socket. Works with any language.

---

## 3. Frame Types

### New core frames

| Direction | Type | Description |
|-----------|------|-------------|
| Guest → Host | `event.ack` | Delivery receipt — harness received the frame |
| Guest → Host | `event.stored` | Frame persisted to inbox (optional, for delivery guarantees) |

### Existing frames (unchanged)

| Direction | Type | Owner |
|-----------|------|-------|
| Host → Guest | `user.message` | Host agent / gateway |
| Guest → Host | `status.presence` | Responder (kit) |
| Guest → Host | `assistant.delta` | Responder (kit) |
| Guest → Host | `assistant.done` | Responder (kit) |
| Guest → Host | `assistant.message` | Responder (kit, agent-initiated) |

---

## 4. User Experience

### No kit, no LLM key

```
$ aegis run -- python3 /workspace/app.py
# Host sends tether message:
tether_send(instance="myapp", text="status?")
# Gets: event.ack. tether_read times out (no responder).
# Message is persisted in /workspace/tether/inbox.ndjson.
```

Channel works. Messages persisted. User can add kit later — responder reads backlog.

### Kit without LLM key

```
$ aegis instance start --kit agent --name bot
# Agent starts in passive mode, acks, returns fallback.
# Add OPENAI_API_KEY later → agent starts responding with LLM.
```

### Kit with LLM key

```
$ aegis instance start --kit agent --name bot --env OPENAI_API_KEY
# Full agent: LLM responses, streaming, MCP tools.
```

---

## 5. What Changes

### Harness (`internal/harness/`)

- Add tether inbox persistence (`/workspace/tether/inbox.ndjson`)
- Add `event.ack` emission on frame receipt
- Add fallback response when no responder connected
- Add responder socket (`/run/tether.sock`) or file-based interface
- Existing `127.0.0.1:7778` forwarding becomes the "responder is connected" path

### Kit manifest

No changes. `--kit agent` still installs aegis-agent as the responder.

### Agent binary

- Connect to `/run/tether.sock` instead of (or in addition to) `127.0.0.1:7778`
- Or: keep 7778, harness checks both

### Image injection

No changes. aegis-agent stays kit-injected.

---

## 6. What Doesn't Change

- Tether protocol — same frames, same envelope
- Host MCP tools — `tether_send`/`tether_read` work identically
- Gateway — unchanged
- aegisd — unchanged
- Tether store — unchanged (ack and fallback frames flow through normally)

---

## 7. Implementation

Small — this is ~100 lines in the harness:

1. On `tether.frame` notification: write to inbox file, send `event.ack`
2. Try forwarding to responder (socket or 7778)
3. If no responder: nothing (message is persisted, ack sent, host handles timeout)
4. Responder (aegis-agent) connects to socket on startup, reads backlog + live frames, writes responses

---

## 8. Why Not "Default Agent"

Making the LLM agent default-on means:
- Choosing providers, keys, policies, tool-calling semantics in core
- Huge surface area + constant churn
- Arguments about "why is the default agent dumb compared to X"

With tether-listener-in-harness:
- Core ships reliable delivery + persistence
- Kit ships intelligence + streaming UX
- Clean boundary, no provider opinions in core
