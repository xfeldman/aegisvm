# Agent Cron Spec

## Problem

Agents can only react to messages. There's no way to run recurring tasks — health checks, polling, periodic reports, data collection, scheduled deploys. The user must manually send tether messages to trigger work.

## Design Constraint: Scale-to-Zero

The VM pauses when idle. A naive cron (goroutine inside the agent, with keepalive held) defeats this — the VM never pauses. An agent with `*/5 * * * *` would burn resources 24/7 even when sleeping between fires.

**The scheduler must live outside the VM.**

## Architecture

**Two components, clean split:**

| Component | Where | What it does |
|-----------|-------|-------------|
| **Cron tools** | `aegis-agent` (inside VM) | CRUD for cron entries — just file writes to `/workspace/.aegis/cron.json` |
| **Cron scheduler** | `aegis-gateway` (host) | Watches cron file, fires tether messages on schedule, VM wakes on message |

This mirrors the Telegram pattern: gateway handles external triggers → tether message → wake-on-message → agent processes → VM idles → pauses.

```
┌────────────────────────────┐     ┌────────────────────────────┐
│  HOST (always running)     │     │  VM (pauses when idle)     │
│                            │     │                            │
│  aegis-gateway             │     │  aegis-agent               │
│  ├─ Telegram poller        │     │  ├─ cron_create tool       │
│  ├─ Cron scheduler    ─────┼─────┼──►  handleUserMessage()   │
│  │   watches cron.json     │     │  ├─ cron_list tool         │
│  │   fires tether msgs     │     │  ├─ cron_delete tool       │
│  │                         │     │  └─ writes cron.json       │
│  └─ Config watcher (3s)    │     │                            │
└────────────────────────────┘     └────────────────────────────┘
```

No keepalive. VM pauses freely between cron fires. Gateway wakes it via tether.

## Cron File

```
/workspace/.aegis/cron.json
```

Workspace is host-mounted, so the gateway reads it directly from the host filesystem. Same pattern as blob storage.

```json
{
  "entries": [
    {
      "id": "cron-1",
      "schedule": "*/5 * * * *",
      "message": "Check if the web server is responding. If not, restart it.",
      "session": "health-check",
      "on_conflict": "skip",
      "enabled": true,
      "created_at": "2026-02-26T15:00:00Z"
    },
    {
      "id": "cron-2",
      "schedule": "0 9 * * *",
      "message": "Summarize yesterday's git commits and post to the team channel.",
      "session": "daily-digest",
      "on_conflict": "queue",
      "enabled": true,
      "created_at": "2026-02-26T15:05:00Z"
    }
  ],
  "next_id": 3
}
```

Fields:
- `id` — auto-generated, `cron-<N>`
- `schedule` — standard 5-field cron expression (minute hour dom month dow)
- `message` — text injected as a synthetic user message when the cron fires
- `session` — session ID for the cron's conversation (isolates cron work from interactive sessions)
- `on_conflict` — what to do when the previous run is still active: `"skip"` (default) or `"queue"`
- `enabled` — can be paused without deleting
- `created_at` — timestamp

## Concurrency: `on_conflict`

When a cron job fires but the previous run for the same session is still in progress:

- **`skip`** (default) — drop this fire. Log it, move on. Prevents backlog buildup. Right choice for health checks, monitoring, anything where the latest state matters more than every invocation.
- **`queue`** — send the message anyway. The agent's session is serial, so it queues behind the current run. Right choice for append-only tasks (log collection, data accumulation) where every fire matters.

The gateway tracks which cron sessions have an active (unfinished) run. A run is "active" from tether send until `assistant.done` is received on the egress stream for that session. If the gateway restarts, all sessions are assumed idle (safe — worst case we fire one extra).

## Deduplicate Fires

The gateway tracks `lastFiredMinute` per entry in memory. A cron entry never fires twice for the same `YYYY-MM-DDTHH:MM` minute. On gateway restart, tracking resets — this means at most one duplicate fire on restart, which is acceptable.

```go
type cronState struct {
    lastFiredMinute string // "2026-02-26T15:05"
    active          bool   // run in progress
}
```

## Time Evaluation

**All cron expressions evaluate in host local time.** `time.Now()` in Go returns local time — this is the intuitive choice for the target audience (home servers, indie hackers). "9am" means 9am on the machine.

Tool responses include the evaluated timezone for clarity:
```json
{"ok": true, "id": "cron-1", "note": "schedule evaluated in host local time (CET)"}
```

No `timezone` field for v1. If needed later, add an optional field per entry.

## Gateway Validation Limits

The gateway enforces hard limits when loading `cron.json`:

- **Max 20 entries.** Entries beyond 20 are silently ignored (logged as warning).
- **Min 1-minute interval.** This is already the cron floor — `*/1 * * * *` is the fastest possible. No sub-minute schedules.

These limits prevent runaway from any source — whether the agent, user, or a rogue process writing to the workspace.

## Agent Tools (inside VM)

Five tools in `aegis-agent`, compiled as built-ins. All they do is read/write `/workspace/.aegis/cron.json`. The gateway picks up changes via mtime polling (within 3 seconds).

### `cron_create`

```json
{
  "schedule": "string, required — cron expression (e.g. '*/5 * * * *', '0 9 * * 1-5')",
  "message": "string, required — the task description sent as a user message when the cron fires",
  "session": "string, optional — session ID for the cron's conversation (default: 'cron-{id}')",
  "on_conflict": "string, optional — 'skip' (default) or 'queue'"
}
```

Returns: `{"ok": true, "id": "cron-1", "note": "schedule evaluated in host local time"}`

Validates the cron expression before saving. Rejects invalid syntax. Max 20 entries enforced.

### `cron_list`

No params. Returns all entries:

```json
{
  "count": 2,
  "entries": [
    {"id": "cron-1", "schedule": "*/5 * * * *", "message": "Check server health", "session": "health-check", "on_conflict": "skip", "enabled": true},
    {"id": "cron-2", "schedule": "0 9 * * *", "message": "Daily digest", "session": "daily-digest", "on_conflict": "queue", "enabled": false}
  ]
}
```

### `cron_delete`

```json
{"id": "string, required"}
```

Returns: `{"ok": true}`

### `cron_enable` / `cron_disable`

```json
{"id": "string, required"}
```

Returns: `{"ok": true, "enabled": true}` or `{"ok": true, "enabled": false}`

## Gateway Scheduler (host-side)

The scheduler runs in `aegis-gateway` alongside the existing Telegram poller and config watcher.

### File watching

Same pattern as `gateway.json` — mtime-based polling on the 3-second ticker:

```go
// In the existing run() ticker loop:
case <-ticker.C:
    gw.checkConfigChange(ctx)   // existing
    gw.checkCronChange(ctx)     // new
```

When `cron.json` changes, reload entries. Parse schedules. Apply validation limits (max 20, skip invalid).

### Cron evaluation

Once per minute (separate ticker, aligned to minute boundary), evaluate all enabled entries:

```go
case <-cronTicker.C:
    now := time.Now().Truncate(time.Minute)
    nowKey := now.Format("2006-01-02T15:04")
    for id, entry := range gw.cronEntries {
        if !entry.Enabled {
            continue
        }
        state := gw.cronState[id]
        if state.lastFiredMinute == nowKey {
            continue // already fired this minute
        }
        if !entry.schedule.Matches(now) {
            continue
        }
        if state.active && entry.OnConflict == "skip" {
            log.Printf("cron [%s]: skipped (previous run still active)", id)
            continue
        }
        state.lastFiredMinute = nowKey
        gw.cronState[id] = state
        log.Printf("cron [%s]: firing → session %s", id, entry.Session)
        go gw.fireCron(entry)
    }
```

### Firing a cron job

`fireCron` sends a tether message — identical to how Telegram messages are forwarded:

```go
func (gw *Gateway) fireCron(entry CronEntry) {
    frame := TetherFrame{
        V:       1,
        Type:    "user.message",
        TS:      time.Now().UTC().Format(time.RFC3339Nano),
        Session: SessionID{Channel: "cron", ID: entry.Session},
        MsgID:   fmt.Sprintf("cron-%s-%d", entry.ID, time.Now().Unix()),
        Payload: buildCronPayload(entry),
    }
    gw.aegisClient.postTetherFrame(instanceID, frame)
}
```

Session channel is `"cron"`. Each cron entry gets its own session, keeping conversations isolated.

Payload:
```json
{
  "text": "Check if the web server is responding. If not, restart it.",
  "user": {"id": "cron", "name": "Scheduled Task"}
}
```

### Tracking active runs

The gateway's egress stream (already subscribed for Telegram) also sees cron session frames. When `assistant.done` arrives for a cron session, mark the run as inactive:

```go
// In processEgressStream, for cron channel frames:
if frame.Session.Channel == "cron" && frame.Type == "assistant.done" {
    gw.markCronIdle(frame.Session.ID)
}
```

### Wake-on-message

If the VM is paused, the tether message triggers wake-on-message (already implemented in aegisd). The VM resumes, agent processes the message, goes idle, pauses again. Zero changes needed to the wake mechanism.

## Cron Expression Parsing

Standard 5-field: `minute hour day-of-month month day-of-week`

Supported syntax per field:
- `*` — any value
- `N` — exact value
- `*/N` — every N (step)
- `N-M` — range (inclusive)
- `N,M,O` — list
- `N-M/S` — range with step

**Not supported:** Named days/months (`MON`, `JAN`), `L`, `W`, `#`, `@yearly` shortcuts. Parser rejects these with a clear error.

Valid ranges per field:

| Field | Min | Max |
|-------|-----|-----|
| Minute | 0 | 59 |
| Hour | 0 | 23 |
| Day of month | 1 | 31 |
| Month | 1 | 12 |
| Day of week | 0 | 6 (0=Sunday) |

Implementation: `internal/cron` package, ~120 lines. No external deps.

```go
type Schedule struct {
    Minute, Hour, Dom, Month, Dow []int // expanded value sets
}

func Parse(expr string) (*Schedule, error)
func (s *Schedule) Matches(t time.Time) bool
```

`Parse` splits on whitespace, expands each field into a sorted set of valid values. `Matches` checks if all 5 fields match the given time (truncated to minute). Strict: returns error on anything it doesn't understand.

## What this does NOT include

- **Sub-minute precision.** Cron fires once per minute at most.
- **Missed fire catch-up.** If the gateway was down and a fire was missed, it's skipped. Next scheduled fire works normally.
- **Cron output routing.** Responses stay in the cron session. The agent can explicitly forward to Telegram or other channels via tools if needed.
- **Timezone per entry.** All expressions evaluate in host local time. Optional timezone field is a future enhancement.
- **`replace` concurrency mode.** Only `skip` and `queue`. Cancel semantics are complex for marginal benefit.

## Implementation

### Files to create/modify

| File | Action | Changes |
|------|--------|---------|
| `internal/cron/cron.go` | Create | Schedule struct, Parse(), Matches() |
| `internal/cron/cron_test.go` | Create | Parse + Matches tests |
| `cmd/aegis-agent/cron.go` | Create | CronStore (load/save cron.json), cron expression validation |
| `cmd/aegis-agent/tools.go` | Modify | 5 tool defs + dispatch + handlers |
| `cmd/aegis-agent/cron_test.go` | Create | CronStore tests |
| `cmd/aegis-gateway/main.go` | Modify | Cron file watching, 1-min ticker, fireCron(), active run tracking |

### Execution order

1. `internal/cron/cron.go` — parser + matcher
2. `internal/cron/cron_test.go` — unit tests
3. `cmd/aegis-agent/cron.go` — CronStore (file CRUD)
4. `cmd/aegis-agent/tools.go` — 5 tools
5. `cmd/aegis-agent/cron_test.go` — CronStore tests
6. `cmd/aegis-gateway/main.go` — scheduler integration
7. Build + test

### Verification

```bash
go test ./internal/cron/ ./cmd/aegis-agent/
make all

# E2E test:
# 1. Start agent instance with workspace
# 2. Via tether: "Create a cron job that runs every minute and writes the current time to /workspace/timestamps.txt"
# 3. Wait 2-3 minutes
# 4. Check /workspace/timestamps.txt has entries
# 5. Via tether: "List my cron jobs"
# 6. Via tether: "Disable cron-1"
# 7. Verify no new entries appear
# 8. Verify VM pauses between cron fires (check instance state)
```
