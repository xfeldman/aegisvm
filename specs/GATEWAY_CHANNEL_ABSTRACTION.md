# Gateway Channel Abstraction & Exec Mode

**Status:** Draft
**Scope:** Extract Telegram into a pluggable channel interface; add `/exec` command to Telegram; add wake-on-exec to the exec API endpoint.

---

## 1. Problem

`aegis-gateway` is a 1300-line monolith. Telegram polling, Telegram API calls, cron scheduling, egress routing, config watching, and tether client code all live in one file with no abstraction boundary. Adding a second messenger (Discord, Slack, WhatsApp) means forking the entire file or stuffing more cases into the switch.

Separately, there's no way to run shell commands on an instance from Telegram. The exec API exists (`POST /v1/instances/{id}/exec`) but it's only reachable from the CLI/UI. Telegram users must ask the agent to run commands on their behalf — slow, indirect, and the agent may refuse or hallucinate.

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
    // ingress is called to deliver user messages to the instance via tether.
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

## 4. Telegram `/exec` Mode

### 4.1 UX

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

### 4.2 State Machine

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

### 4.3 Exec Handler

```go
func (ch *TelegramChannel) handleExecMessage(chatID int64, text string) {
    // 1. Parse command: split by whitespace into []string
    //    Use shlex-style splitting to handle quotes:
    //    "grep 'hello world' file.txt" → ["grep", "hello world", "file.txt"]
    command := shelxSplit(text)
    if len(command) == 0 {
        return
    }

    // 2. Start typing indicator
    go ch.sendTypingLoop(ctx, chatID)

    // 3. Call exec API via deps.Exec(command)
    //    Returns NDJSON stream reader
    body, err := ch.deps.Exec(command)
    if err != nil {
        ch.sendMessage(chatID, "exec failed: " + err.Error())
        return
    }
    defer body.Close()

    // 4. Accumulate output lines from NDJSON stream
    //    Each line is {"stream":"stdout|stderr","data":"..."} or {"done":true,"exit_code":N}
    var output strings.Builder
    scanner := bufio.NewScanner(body)
    for scanner.Scan() {
        var entry execEntry
        json.Unmarshal(scanner.Bytes(), &entry)
        if entry.Done {
            // Send final message with output + exit code
            msg := formatExecOutput(output.String(), entry.ExitCode)
            ch.sendMessage(chatID, msg)
            return
        }
        if entry.Data != "" {
            output.WriteString(entry.Data)
        }
    }
}
```

### 4.4 Output Formatting

- Wrap output in markdown code block (triple backtick)
- Append `exit: N` after the code block
- Truncate to 4000 chars (Telegram limit is 4096) with `... (truncated)` suffix
- Empty output → show just the exit code
- Non-zero exit code → prefix with warning indicator

### 4.5 Safety

- **No shell wrapping.** Commands are split into `[]string` and passed directly to the exec API. No `sh -c`. The instance's exec runs the command array directly via the harness, which does `execvp` — no shell interpretation, no injection vector.
- **Shlex splitting** handles quotes so users can pass arguments with spaces: `grep "hello world" file.txt` works as expected.
- **allowed_chats** still applies — exec mode inherits the same access control as chat mode.
- **No persistent state** — exec mode is in-memory on the gateway. If the gateway restarts, all chats revert to chat mode.

---

## 5. File Layout (Post-Refactor)

```
cmd/aegis-gateway/
  main.go              # Gateway core: config watch, egress subscribe, channel registry, cron
  channel.go           # Channel interface, ChannelDeps, factory registry
  telegram.go          # TelegramChannel implementation (polling, API, exec mode, image ingress)
  types.go             # Shared types: TetherFrame, SessionID, etc.
```

`main.go` drops from ~1300 lines to ~400 (core + cron). `telegram.go` is ~600 lines (the Telegram-specific code relocated + exec mode added). `channel.go` is ~50 lines (interface + factory map). `types.go` is ~80 lines.

---

## 6. Implementation Order

| Step | What | Where | Risk |
|------|------|-------|------|
| 1 | Wake-on-exec | `internal/api/server.go` | Low — one-line addition, same pattern as tether |
| 2 | Extract types | `cmd/aegis-gateway/types.go` | None — move, no behavior change |
| 3 | Define Channel interface | `cmd/aegis-gateway/channel.go` | None — new file |
| 4 | Extract TelegramChannel | `cmd/aegis-gateway/telegram.go` | Medium — must preserve all existing behavior |
| 5 | Refactor gateway core | `cmd/aegis-gateway/main.go` | Medium — must keep config watch, cron, egress routing |
| 6 | Add exec to aegisClient | `cmd/aegis-gateway/main.go` | Low — new HTTP method, same pattern as postTetherFrame |
| 7 | Add exec mode to TelegramChannel | `cmd/aegis-gateway/telegram.go` | Low — new state + handler |
| 8 | Test end-to-end | Manual | — |

Steps 1-5 are the refactor (no new features, pure restructure). Steps 6-7 add exec mode. Step 1 is independent and can land as a separate commit.

---

## 7. What This Does NOT Change

- **UI chat** — stays as a direct tether client (browser → HTTP). Not a gateway channel.
- **MCP tether_send/tether_read** — stays as a direct tether client. Not a gateway channel.
- **Cron** — stays in gateway core. It's a timer, not a messenger.
- **Tether protocol** — no frame format changes.
- **Config file location** — same path, same hot-reload.
- **Legacy mode** — preserved for backward compat (single-instance gateway without AEGIS_INSTANCE).
