# Gateway Channel Abstraction & Exec Mode

**Status:** Draft
**Scope:** Extract Telegram into a pluggable channel interface; add `/exec` command to Telegram; add wake-on-exec to the exec API endpoint; adopt `sendMessageDraft` for native streaming.

---

## 1. Problem

`aegis-gateway` is a 1300-line monolith. Telegram polling, Telegram API calls, cron scheduling, egress routing, config watching, and tether client code all live in one file with no abstraction boundary. Adding a second messenger (Discord, Slack, WhatsApp) means forking the entire file or stuffing more cases into the switch.

Separately, there's no way to run shell commands on an instance from Telegram. The exec API exists (`POST /v1/instances/{id}/exec`) but it's only reachable from the CLI/UI. Telegram users must ask the agent to run commands on their behalf — slow, indirect, and the agent may refuse or hallucinate.

Additionally, the current Telegram streaming uses `sendMessage` + `editMessageText` throttled to 1 edit/sec. Telegram Bot API 9.3 (Dec 2025) introduced `sendMessageDraft` — a purpose-built streaming method. Available to all bots since Bot API 9.5 (Mar 2026). This eliminates the edit-throttle workaround and enables faster, smoother streaming.

---

## 2. Design

### 2.1 Channel Interface

A `Channel` is a server-side adapter that bridges an external messaging protocol to tether. The gateway core manages channels; channels manage their own protocol.

```go
// Channel bridges an external messaging service to tether.
// Each implementation owns its own protocol (polling, webhooks, websockets)
// and translates between that protocol and tether frames.
type Channel interface {
    // Name returns the channel identifier used in tether session routing
    // (e.g. "telegram", "discord"). Must match frame.Session.Channel.
    Name() string

    // Start begins the channel's ingress loop (polling, webhook listener, etc).
    // The channel must respect ctx cancellation for clean shutdown.
    Start(ctx context.Context, deps ChannelDeps) error

    // HandleFrame is called for each egress frame whose Session.Channel
    // matches this channel's Name(). The channel decides which frame types
    // to handle (delta, done, reasoning, presence, etc).
    HandleFrame(frame TetherFrame)

    // Reconfigure hot-reloads channel config. Called when the gateway
    // config file changes. The channel should update its state atomically.
    // If the new config disables this channel, it should stop gracefully.
    Reconfigure(cfg json.RawMessage)

    // Stop shuts down the channel. Called on gateway shutdown or when
    // the channel is removed from config.
    Stop()
}
```

`ChannelDeps` provides the gateway services a channel needs:

```go
type ChannelDeps struct {
    // SendTether delivers a user.message frame to the instance via tether.
    // Handles wake-on-message internally.
    SendTether func(frame TetherFrame) error

    // Exec runs a command on the instance and streams output.
    // Returns an NDJSON reader. Caller must close.
    // Handles wake-on-exec internally (see §3).
    Exec func(command []string) (io.ReadCloser, error)

    // InstanceHandle is the instance this gateway serves.
    InstanceHandle string

    // WorkspacePath returns the host workspace path (lazy-resolved, cached).
    WorkspacePath func() string

    // BlobStore returns the workspace blob store for image ingress.
    BlobStore func() *blob.WorkspaceBlobStore

    // Logger for channel-scoped logging.
    Logger *log.Logger
}
```

### 2.2 Gateway Core

The gateway core shrinks to:

1. **Config watcher** — watches `~/.aegis/kits/{handle}/gateway.json`, detects changes
2. **Channel registry** — maps channel name → `Channel` implementation
3. **Egress subscriber** — single `GET /tether/stream`, routes frames to `channel.HandleFrame()` by `frame.Session.Channel`
4. **Cron scheduler** — stays in core (it's not a messenger channel, it's a timer → tether injector)
5. **aegisClient** — shared HTTP client for aegisd socket

On config change, the core calls `channel.Reconfigure(cfg)` for each registered channel. Channels that appear in config get started; channels that disappear get stopped.

### 2.3 Config Format

No change to the config file structure. Each top-level key maps to a channel:

```json
{
  "telegram": {
    "bot_token": "...",
    "allowed_chats": ["*"]
  }
}
```

Future channels add their own top-level key:

```json
{
  "telegram": { "bot_token": "..." },
  "discord":  { "bot_token": "...", "allowed_guilds": ["..."] }
}
```

The core iterates config keys, matches them to registered channel factories, and calls `Start` or `Reconfigure`.

### 2.4 Channel Factory Registry

```go
var channelFactories = map[string]func() Channel{
    "telegram": func() Channel { return &TelegramChannel{} },
}
```

Compiled-in for now. No plugin system needed — adding a channel means adding a Go file to `cmd/aegis-gateway/` and registering the factory.

---

## 3. Wake-on-Exec

The exec endpoint (`POST /v1/instances/{id}/exec`) currently returns `409 Conflict` for stopped instances. This blocks the Telegram exec mode — the user would have to send a tether message first just to wake the instance before running a command.

**Fix:** Add `EnsureInstance` to `handleExecInstance`, same pattern as tether ingress.

In `internal/api/server.go`, `handleExecInstance`, after the disabled check and before calling `ExecInstance`:

```go
// Wake-on-exec: ensure instance is running before executing
ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
defer cancel()
if err := s.lifecycle.EnsureInstance(ctx, inst.ID); err != nil {
    if err == lifecycle.ErrInstanceDisabled {
        writeError(w, http.StatusConflict, "instance is disabled")
        return
    }
    writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("wake failed: %v", err))
    return
}
```

This replaces the current `ErrInstanceStopped` check — stopped instances now wake instead of 409'ing. Disabled instances still 409.

---

## 4. Telegram Streaming: `sendMessageDraft`

### 4.1 Background

The current streaming implementation uses a two-step workaround:
1. `sendMessage` on first `assistant.delta` → creates message, gets `message_id`
2. `editMessageText` on subsequent deltas → throttled to 1 edit/sec to avoid Telegram rate limits

This produces choppy, laggy output. Telegram Bot API 9.3 (Dec 31, 2025) introduced `sendMessageDraft` as a dedicated streaming method. Bot API 9.5 (Mar 1, 2026) made it available to all bots.

### 4.2 How `sendMessageDraft` Works

`sendMessageDraft` replaces the `sendMessage` + `editMessageText` loop with a single progressive call:

- **Parameters:** `chat_id`, `text`, `parse_mode`, `entities`, `link_preview_options`, `reply_parameters`, `message_thread_id`, `reply_markup` — same as `sendMessage`
- **Returns:** `Message` object (same as `sendMessage`)
- **Behavior:** Updates the message in-place as it's being generated. Telegram renders the message progressively on the client. No edit rate limits apply — this is a dedicated streaming transport, not a message-edit operation.

### 4.3 New Streaming Flow

```
assistant.delta (first)  → sendMessageDraft(chat_id, text) → get message_id
assistant.delta (Nth)    → sendMessageDraft(chat_id, accumulated_text) → update in-place
assistant.done           → sendMessage(chat_id, final_text) → finalize
```

**Key change:** On `assistant.done`, we send a final `sendMessage` to "commit" the draft into a permanent message. The draft is ephemeral — `sendMessage` at the end makes it persistent.

### 4.4 Throttle

Even though `sendMessageDraft` doesn't have edit rate limits, we still throttle to avoid flooding the Telegram API with tiny deltas. But the interval drops from 1000ms to **200ms** — 5x faster streaming.

```go
const draftThrottle = 200 * time.Millisecond
```

This is tunable. If Telegram imposes rate limits on `sendMessageDraft` in the future, we can increase it.

### 4.5 Exec Streaming

Exec output also benefits. Instead of accumulating all output and sending at the end, long-running commands stream progressively:

```
exec starts          → sendMessageDraft(chat_id, "```\n" + partial_output)
output accumulates   → sendMessageDraft(chat_id, "```\n" + accumulated, throttled)
exec done            → sendMessage(chat_id, "```\n" + final_output + "\n```\nexit: N")
```

This makes `pip install`, `make`, `git clone` etc. feel responsive instead of frozen.

### 4.6 activeReply Changes

The `activeReply` struct simplifies:

```go
type activeReply struct {
    chatID       int64
    messageID    int               // 0 until first draft sent
    text         string            // accumulated text
    lastDraft    time.Time         // throttle tracking (was lastEdit)
    typingCancel context.CancelFunc
}
```

`lastEdit` → `lastDraft`, `editTelegramMessage` → `sendMessageDraft`. The finalization path (`assistant.done`) switches from `editMessageText` to `sendMessage`.

---

## 5. Telegram `/exec` Mode

### 5.1 UX

```
User: /exec
Bot:  ⚡ Exec mode. Commands run directly on the instance.
      Send /exit to return to chat.

User: ls /workspace
Bot:  ```
      README.md
      src/
      package.json
      ```
      exit: 0

User: cat /workspace/src/main.py | head -5
Bot:  ```
      import os
      import sys
      from app import create_app

      app = create_app()
      ```
      exit: 0

User: /exit
Bot:  💬 Back to chat mode.
```

Long-running commands stream progressively via `sendMessageDraft` (see §4.5).

### 5.2 State Machine

Per-chat state, stored on the `TelegramChannel`:

```
execMode map[int64]bool   // chatID → in exec mode
```

In `handleTelegramMessage`:

```
if text == "/exec" → enter exec mode, send confirmation, return
if text == "/exit" && inExecMode → leave exec mode, send confirmation, return
if inExecMode → route to execHandler instead of tether
else → route to tether (existing behavior)
```

### 5.3 Exec Handler

```go
func (ch *TelegramChannel) handleExecMessage(chatID int64, text string) {
    // 1. Parse command: shlex-style split to handle quotes
    //    "grep 'hello world' file.txt" → ["grep", "hello world", "file.txt"]
    command := shlexSplit(text)
    if len(command) == 0 {
        return
    }

    // 2. Start typing indicator
    ctx, typingCancel := context.WithCancel(context.Background())
    defer typingCancel()
    go ch.sendTypingLoop(ctx, chatID)

    // 3. Call exec API via deps.Exec(command)
    body, err := ch.deps.Exec(command)
    if err != nil {
        ch.sendMessage(chatID, "exec failed: " + err.Error())
        return
    }
    defer body.Close()

    // 4. Stream output via sendMessageDraft, finalize with sendMessage
    var output strings.Builder
    var messageID int
    var lastDraft time.Time

    scanner := bufio.NewScanner(body)
    for scanner.Scan() {
        var entry execEntry
        json.Unmarshal(scanner.Bytes(), &entry)

        if entry.Done {
            typingCancel()
            msg := formatExecOutput(output.String(), entry.ExitCode)
            ch.sendMessage(chatID, msg) // finalize
            return
        }

        if entry.Data != "" {
            output.WriteString(entry.Data)
            // Stream draft if throttle elapsed
            if time.Since(lastDraft) >= draftThrottle {
                draft := formatExecDraft(output.String())
                messageID = ch.sendMessageDraft(chatID, draft)
                lastDraft = time.Now()
            }
        }
    }
}
```

### 5.4 Output Formatting

- Wrap output in markdown code block (triple backtick)
- Append `exit: N` after the code block
- Truncate to 4000 chars (Telegram limit is 4096) with `... (truncated)` suffix
- Empty output → show just the exit code
- Non-zero exit code → prefix with warning indicator

### 5.5 Safety

- **No shell wrapping.** Commands are split into `[]string` and passed directly to the exec API. No `sh -c`. The instance's exec runs the command array directly via the harness, which does `execvp` — no shell interpretation, no injection vector.
- **Shlex splitting** handles quotes so users can pass arguments with spaces: `grep "hello world" file.txt` works as expected.
- **allowed_chats** still applies — exec mode inherits the same access control as chat mode.
- **No persistent state** — exec mode is in-memory on the gateway. If the gateway restarts, all chats revert to chat mode.

---

## 6. File Layout (Post-Refactor)

```
cmd/aegis-gateway/
  main.go              # Gateway core: config watch, egress subscribe, channel registry, cron
  channel.go           # Channel interface, ChannelDeps, factory registry
  telegram.go          # TelegramChannel implementation (polling, API, exec mode, streaming)
  types.go             # Shared types: TetherFrame, SessionID, etc.
```

`main.go` drops from ~1300 lines to ~400 (core + cron). `telegram.go` is ~700 lines (Telegram code relocated + exec mode + sendMessageDraft). `channel.go` is ~50 lines (interface + factory map). `types.go` is ~80 lines.

---

## 7. Implementation Order

| Step | What | Where | Risk |
|------|------|-------|------|
| 1 | Wake-on-exec | `internal/api/server.go` | Low — same pattern as tether |
| 2 | Extract types | `cmd/aegis-gateway/types.go` | None — move, no behavior change |
| 3 | Define Channel interface | `cmd/aegis-gateway/channel.go` | None — new file |
| 4 | Extract TelegramChannel | `cmd/aegis-gateway/telegram.go` | Medium — must preserve all existing behavior |
| 5 | Refactor gateway core | `cmd/aegis-gateway/main.go` | Medium — must keep config watch, cron, egress routing |
| 6 | Replace editMessageText with sendMessageDraft | `cmd/aegis-gateway/telegram.go` | Low — same params, drop-in replacement |
| 7 | Add exec to aegisClient | `cmd/aegis-gateway/main.go` | Low — new HTTP method, same pattern |
| 8 | Add exec mode to TelegramChannel | `cmd/aegis-gateway/telegram.go` | Low — new state + handler |
| 9 | Test end-to-end | Manual | — |

Steps 1-5 are the refactor (no new features, pure restructure). Step 6 upgrades streaming. Steps 7-8 add exec mode. Step 1 is independent and can land as a separate commit.

---

## 8. What This Does NOT Change

- **UI chat** — stays as a direct tether client (browser → HTTP). Not a gateway channel.
- **MCP tether_send/tether_read** — stays as a direct tether client. Not a gateway channel.
- **Cron** — stays in gateway core. It's a timer, not a messenger.
- **Tether protocol** — no frame format changes.
- **Config file location** — same path, same hot-reload.
- **Legacy mode** — preserved for backward compat (single-instance gateway without AEGIS_INSTANCE).

---

## 9. Future Considerations (Not in Scope)

- **Exec rate limiting** — a chat flood could start many concurrent exec sessions. A per-channel `ExecLimiter` in `ChannelDeps` would cap concurrency. Not needed for v1 since `allowed_chats` already restricts access to trusted users.
- **Large output as file upload** — when exec output exceeds 4096 chars, upload as a `.txt` document instead of truncating. Nice-to-have for v2.
