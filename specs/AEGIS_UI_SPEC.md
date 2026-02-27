# Aegis UI Spec

## Overview

Desktop app for managing AegisVM instances — lifecycle, logs, command execution, and agent chat via tether. Built with Wails (Go backend + web frontend + native webview). Not a replacement for the CLI — a visual companion for monitoring and interacting with instances.

Think Docker Desktop, but for microVMs with an agent chat panel.

**Kit-agnostic.** The UI manages all instances, not just Agent Kit. Tether chat works with any instance that has a guest agent. The Chat tab is one of many interactions — logs, exec, and config work on any VM.

**Daemon-first.** The UI requires a running aegisd. No offline mode, no direct registry access. All state lives in aegisd.

**Security model.** Local user = root of VM. The UI can read/write workspace files and execute commands inside any instance. This matches the CLI and MCP — the local user owns the VMs.

## Technology

**Wails v2** (stable, production-ready). v3 is alpha — migrate when stable.

- Go backend: talks to aegisd via the existing unix socket API
- Frontend: Svelte (lightweight, fast, good DX)
- Native webview: macOS WebKit, Linux WebKitGTK, Windows WebView2
- System tray: daemon status indicator, quick actions
- Binary: ~10MB, ~30MB RAM

The Go backend is a thin client over the aegisd API — no business logic duplication. Every operation is an API call to the running daemon.

**Two modes, same code:**

| Mode | Binary | Frontend served via | System tray |
|------|--------|-------------------|-------------|
| **Desktop app** | `aegis-ui` (Wails) | Native webview | Yes (v3) |
| **Web mode** | `aegis ui` (CLI subcommand) | HTTP server → browser | No |

Both modes use the **same frontend bundle** and the **same Go backend**. The Wails app embeds the frontend in a native window; `aegis ui` serves it over HTTP and opens the browser.

```bash
aegis ui              # serves UI, opens browser to http://localhost:PORT
aegis ui --port 9090  # custom port
```

`aegis ui` is the escape hatch for: Linux without GUI, SSH access, CI debugging, or when the native app isn't installed. No Wails dependency, no code signing, no native build issues.

## Daemon Management

The app auto-starts aegisd on launch if not already running.

- On launch: check daemon status → if not running, attempt `aegis up`
- If auto-start succeeds: proceed normally
- If auto-start fails (permissions, missing backend, etc.): show error with the exact command to run manually + "Copy command" button. Do not hide the failure.
- Header shows daemon status (running/version) at all times
- If daemon dies unexpectedly, UI shows "Reconnecting to aegisd..." overlay, auto-retries with 2s backoff
- On app quit: daemon keeps running (background service, not tied to UI lifecycle)

## Error & Loading States

Every async action follows a consistent pattern:

- **Buttons** show busy state: "Disabling..." "Deleting..." (disabled during operation)
- **Toast notifications** for action results: success (auto-dismiss 4s) or error (persistent until dismissed)
- **Daemon disconnected**: all polling stops, overlay with "Reconnecting to aegisd...", auto-reconnects
- **Instance state transitions**: shown via polling (5s), no user-triggered transitions to wait on
- **Tether send with no response**: after 60s without `assistant.done`, show inline "Agent still processing..." with cancel button (maps to `control.cancel` tether frame). Pending state persists across UI reconnects.

No silent failures. Every action the user takes gets visible feedback.

## New API: Workspace File Access

Add to aegisd (small, high-impact for UI):

```
GET /v1/instances/{id}/workspace?path=.aegis/blobs/abc123.png
```

File access to the instance's workspace. Required for:
- Chat image rendering (read blob store files)
- Chat image upload (write to blob store)
- Config editor (read/write agent.json)
- Avoids `exec cat` for file reads (slow, binary encoding issues, timeout-prone)

Read: `GET /v1/instances/{id}/workspace?path=...` — returns raw file content, 10MB limit.
Write: `POST /v1/instances/{id}/workspace?path=...` — request body is file content, 10MB limit.

**Path security:** Server must normalize and validate path against workspace root. Reject `..` traversal, absolute paths, and symlinks escaping the workspace. Paths are always relative to the workspace root.

**Size limit:** 10MB per request. Server returns HTTP 413 with descriptive message if file exceeds limit.

**Blob writes are idempotent** — content-addressed (SHA256 + extension). Safe to retry on failure without creating duplicates.

## Operational Invariants

### OperationResult

Every Go backend method that mutates state returns a consistent shape:

```go
type OperationResult struct {
    Success bool   `json:"success"`
    Message string `json:"message"`
}
```

Standardizes toast messages, error surfaces, and UX language across all actions. Frontend never invents error phrasing — it displays `result.Message`.

### Instance lifecycle philosophy

Aegis manages instance lifecycle non-interactively. The UI does **not** expose start/stop/pause/resume controls. The only user actions are:

- **Disable** — tells aegis "don't auto-wake this instance". Stops the VM, closes port listeners, prevents wake-on-connect/wake-on-message.
- **Enable** — re-enables a disabled instance. Aegis can auto-wake it again on activity.
- **Delete** — removes the instance entirely.

All other lifecycle transitions (boot, pause on idle, resume on activity, stop after extended idle) are managed automatically by aegisd. This matches the CLI philosophy.

### Instance state and action availability

| Instance state | Exec | Chat | Config | Disable | Delete |
|---|---|---|---|---|---|
| running | yes | yes | yes | yes | yes |
| paused | auto-wake | auto-wake | read-only | yes | yes |
| stopped | auto-wake | auto-wake | no | yes | yes |
| disabled | no | no | no | enable | yes |
| starting | wait | wait | read-only | yes | — |

- Exec and Chat on a paused/stopped instance: API auto-wakes, then proceeds
- Exec and Chat on a disabled instance: disabled in UI with tooltip
- Starting: buttons show spinner, wait for running state

### Status display

Color-coded status with disabled as a distinct visual state:

| State | Dot color | Label |
|---|---|---|
| running | green | running |
| paused | yellow | paused |
| stopped | gray | stopped |
| disabled | red | disabled |
| starting | blue | starting |

Dashboard status bar shows separate counters: Running: N | Paused: N | Stopped: N | Disabled: N

### Cancel semantics

Cancel is best-effort:
- Sends `control.cancel` tether frame
- If `assistant.done` already emitted, cancel is ignored (done is authoritative final state)
- UI must treat `assistant.done` as the terminal state regardless of cancel timing
- If cancel arrives before done: agent should stop tool execution and emit a short `assistant.done` with partial results

### Config restart behavior

After "Save + Restart":
- Config written via workspace API
- `self_restart` sent via tether — agent exits cleanly after current response
- UI shows "Restarting..." state on all tabs
- Chat tab disables input until agent comes back (restart notification frame received)
- Sessions are preserved — tether replay loads prior history on reconnect
- If restart fails (bad config, MCP server not found): agent logs the error, UI shows it in Logs tab. Agent still runs with previous config.

### Chat long-poll semantics

- Server blocks up to `wait_ms` milliseconds
- Returns immediately when new frames arrive (no unnecessary delay)
- Returns empty `frames: []` with `timed_out: true` on timeout
- Client starts next poll immediately on return — no sleep between polls

### Exec output limits

- Output capped at 1MB per command. Truncated with `... (truncated)` marker if exceeded.
- Prevents UI freeze from unbounded output (e.g. `cat /dev/zero`)
- Exit code and duration always reported regardless of truncation

### Log stream resilience

- If log SSE stream drops (daemon restart, VM pause): auto-reconnect with 2s backoff
- Show "Log stream reconnected" indicator on reconnect
- Clear stale stream state on reconnect — don't accumulate zombie listeners

### Chat tab on non-agent instances

- If tether `user.message` is sent but no `status.presence` or `assistant.delta` arrives within 10s: show "No agent runtime detected. This VM does not include an agent that processes messages."
- Distinguishes "agent is slow" (presence frame received) from "no agent at all" (nothing)

### Chat reconnect invariants

- `tether_poll` returns only frames matching the requested `session_id` — no cross-session pollution
- Sequence numbers are globally monotonic per instance (not per session), but filtering by session is server-side
- On reconnect: UI polls with `after_seq=0` and **replaces** local chat history (server is authoritative). No client-side merge.
- Duplicate frames are impossible given seq-based cursor

## Pages

### 1. Dashboard

Overview of all instances.

```
┌─────────────────────────────────────────────────────────────────┐
│  AegisVM v0.4.6                                   [●] Running  │
├─────────────────────────────────────────────────────────────────┤
│  Instances (5)  Running: 3 | Paused: 1 | Stopped: 1   [↻] [+]  │
│                                                                 │
│  ● my-agent      agent  running  http://localhost:54516  42m   │
│  ● browser-agent agent  paused                           1h    │
│  ● web-server    —      running  http://localhost:8080   5m    │
│  ◌ test-agent    agent  stopped                          2d    │
│  ◌ old-instance  —      disabled                         5d    │
│                                                                 │
│  Secrets: 4 | Kits: agent                          [Settings]  │
└─────────────────────────────────────────────────────────────────┘
```

- Instance list: status, kit, **public URL** (clickable → opens browser), uptime/age
- Port display: show only what the user connects to (`http://localhost:54516`), hide internal guest port mapping
- Color-coded status: green=running, yellow=paused, gray=stopped, red=disabled
- Quick actions: **Disable**, **Enable** (on disabled instances), and **Delete** (no start/stop/pause/resume — aegis manages lifecycle)
- Daemon status + backend in header
- 5s auto-refresh polling

### 2. Instance Detail

Per-instance view with tabs.

#### Info tab

```
┌─────────────────────────────────────────────────────────────────┐
│  ← my-agent  ● running  42m       [Disable] [Delete]          │
├──────┬──────┬──────────────────┬───────┬────────┬───────────────┤
│ Info │ Logs │ Exec             │ Chat  │ Config │               │
├──────┴──────┴──────────────────┴───────┴────────┴───────────────┤
│                                                                 │
│  ID:        inst-1772143240906457000                            │
│  Handle:    my-agent                                            │
│  Kit:       agent                                               │
│  Image:     python:3.12-alpine                                  │
│  State:     running                                             │
│  Memory:    512MB (used: 78MB)                                  │
│  Workspace: ~/.aegis/data/workspaces/my-agent                   │
│  Created:   2026-02-27 10:15:00                                 │
│  Uptime:    42m 15s                                             │
│                                                                 │
│  Ports:                                                         │
│    http://localhost:54516 → :80  [open ↗]                       │
│                                                                 │
│  Secrets:                                                       │
│    OPENAI_API_KEY, BRAVE_SEARCH_API_KEY                         │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

#### Logs tab

```
┌─────────────────────────────────────────────────────────────────┐
│ [stdout ▼] [auto-scroll ✓] [clear]                 [download]  │
├─────────────────────────────────────────────────────────────────┤
│ 10:15:01 aegis-agent starting                                  │
│ 10:15:01 LLM provider: OpenAI                                  │
│ 10:15:01 MCP [aegis]: loaded 7 tools                           │
│ 10:15:01 agent listening on 127.0.0.1:7778                     │
│ 10:15:30 tool bash: 743 bytes result                           │
│ 10:15:35 tool write_file: 44 bytes result                      │
│ █                                                              │
└─────────────────────────────────────────────────────────────────┘
```

Real-time log streaming via Wails events (Go backend reads log SSE, emits to frontend).

- Stdout/stderr filter toggle
- Auto-scroll with pause on manual scroll-up
- Search/filter within logs
- Download log as file

#### Command Runner tab

Execute commands inside the VM. Not a terminal (no PTY) — a command runner.

```
┌─────────────────────────────────────────────────────────────────┐
│ $ ls /workspace                                    0 ✓  0.1s  │
│ sessions/  .aegis/  app.py  requirements.txt       [copy]     │
│                                                                │
│ $ free -m                                          0 ✓  0.0s  │
│               total   used   free   shared   avail [copy]     │
│ Mem:           482     78    319        0      393              │
│                                                                │
│ $ █                                                            │
├─────────────────────────────────────────────────────────────────┤
│ [Enter command...]                                     [Run]   │
└─────────────────────────────────────────────────────────────────┘
```

- Command input with Enter to execute
- Output displayed inline
- **Exit code + duration** shown per command
- **Copy output** button per command
- Command history (up/down arrows)
- Uses `POST /v1/instances/{id}/exec` API

#### Chat tab

Tether-based chat interface. Shown for **all instances** with tether. If no agent runtime: "No agent runtime — messages will be delivered but may not get a response."

```
┌─────────────────────────────────────────────────────────────────┐
│ Session: ui:default ▼              [New session] [Sessions]    │
├─────────────────────────────────────────────────────────────────┤
│                                                                │
│  You: Find me a photo of a Brussels Griffon                    │
│                                                                │
│  Agent: ● thinking...                                          │
│         ● tool: image_search                                   │
│         ● tool: bash                                           │
│         ● tool: respond_with_image                             │
│                                                                │
│  Agent: Here's a Brussels Griffon!                             │
│         [image: brussels_griffon.jpg]                          │
│                                                                │
│  You: Generate one with a top hat                              │
│                                                                │
│  Agent: ● tool: image_generate                                 │
│         [generated image]                                      │
│         Here's your dapper Brussels Griffon!                   │
│                                                                │
├─────────────────────────────────────────────────────────────────┤
│ [📎] [Type a message...]                    [Cancel] [Send]    │
│       Drop or paste images here                                │
└─────────────────────────────────────────────────────────────────┘
```

**Session ID convention:**

- UI sessions: `ui:default`, `ui:research-1`, etc. (user can rename)
- Avoids collision with `host:default` (CLI/MCP), `telegram:123` (gateway), `cron:health` (cron)

**Streaming state machine:**

1. User sends message → `tether_send(session_id="ui:default")` → get `ingress_seq`
2. Start long-poll loop: `tether_poll(after_seq=ingress_seq, wait_ms=5000)`
3. On `status.presence` → show "● thinking..." / "● tool: X" indicator
4. On `assistant.delta` → append text to current assistant message (group contiguous deltas into one message)
5. On `assistant.done` → finalize message, render images inline, clear indicators
6. If no `assistant.done` after 60s → show inline "Agent still processing..." + [Cancel] button
7. On poll return → immediately start next poll with `next_seq` (no sleep between polls)

**Cancel:** maps to `control.cancel` tether frame. Pending message state persists across UI restart (stored in local Svelte store, keyed by instance + session + seq).

**Reconnect/resume:**

- On page load or reconnect: poll with `after_seq=0` to load full session history
- Sequence numbers are stable — no duplicates, no missed frames
- If poll fails (daemon restart), retry with 2s backoff

**Images (bidirectional — tether supports images in both directions):**

Agent → User:
- `assistant.done` with `images` field → fetch via workspace file API: `GET /v1/instances/{id}/workspace?path=.aegis/blobs/{key}`
- Display inline in chat bubble, clickable for full-size view
- Supports generated images (image_generate), found images (image_search + respond_with_image), and any blob the agent produces

User → Agent:
- Drag & drop, paste, or 📎 button to attach images
- UI writes image to workspace blob store via workspace write API, includes image ref in tether `user.message` payload
- Agent receives it as an image content block (same format as Telegram photo ingress)
- Supports PNG, JPEG, GIF, WebP

**Markdown:** Agent responses rendered as markdown (code blocks, lists, links, bold/italic).

#### Kit Config tab (Kit instances only)

Generic config editor driven by the kit manifest's `config` field. The core UI has **no hardcoded knowledge** of specific kit config files — everything is declared in the kit manifest.

```
┌─────────────────────────────────────────────────────────────────┐
│ [Agent] [Gateway]                                               │
├─────────────────────────────────────────────────────────────────┤
│ .aegis/agent.json  workspace       [Save] [Save + Restart]      │
├─────────────────────────────────────────────────────────────────┤
│ {                                                               │
│   "model": "openai/gpt-4.1",          ← syntax highlighted     │
│   "mcp": {                                                      │
│     "playwright": { ... }                                       │
│   }                                                              │
│ }                                                                │
└─────────────────────────────────────────────────────────────────┘
```

- Tab shown only for kit instances (`instance.kit` is set)
- Kit manifest declares configs in two places: kit-level `config[]` (workspace) and daemon-level `instance_daemons[].config` (host). API flattens both into one array with computed `location` field.
- Sub-tabs when kit has multiple config files (e.g. Agent + Gateway)
- **Workspace** configs: read/write via workspace file API. "Save + Restart" sends tether message prompting `self_restart`
- **Host** configs: read/write via `GET/POST /v1/instances/{id}/kit-config?file=` (files at `~/.aegis/kits/{handle}/`)
- JSON syntax highlighting (single-pass tokenizer, keys/strings/numbers/bools colored)
- Live JSON validation with error bar
- Ghost example preview: when config is empty, kit's example config renders at 30% opacity with a "Use this example" button. Disappears on first keystroke
- Tab key inserts 2 spaces

### 3. Secrets

```
┌─────────────────────────────────────────────────────────────────┐
│  Secrets                                            [+ Add]    │
├─────────────────────────────────────────────────────────────────┤
│  OPENAI_API_KEY          2 instances    2026-02-23   [Delete]  │
│  TELEGRAM_BOT_TOKEN      1 instance     2026-02-26   [Delete]  │
│  BRAVE_SEARCH_API_KEY    2 instances    2026-02-27   [Delete]  │
│  ANTHROPIC_API_KEY       0 instances    2026-02-20   [Delete]  │
└─────────────────────────────────────────────────────────────────┘
```

- List/add/delete secrets (values write-only, never displayed)
- **"Used by" count** — computed client-side from instance `secret_keys` lists. No new API.
- Secret picker in New Instance dialog

### 4. New Instance Dialog

```
┌─────────────────────────────────────────────────────────────────┐
│  New Instance                                                  │
├─────────────────────────────────────────────────────────────────┤
│  Name:     [my-agent          ]                                │
│  Kit:      [agent ▼] (none, agent)                             │
│  Image:    [python:3.12-alpine] (auto from kit)                │
│  Memory:   [512   ] MB                                         │
│  Command:  [aegis-agent       ] (auto from kit)                │
│  Workspace:[                  ] (auto if empty)                │
│                                                                │
│  Secrets:  [✓] OPENAI_API_KEY                                  │
│            [✓] BRAVE_SEARCH_API_KEY                            │
│            [ ] TELEGRAM_BOT_TOKEN                              │
│            [ ] ANTHROPIC_API_KEY                                │
│                                                                │
│  Ports:    [8080:80  ] [+ Add]                                 │
│                                                                │
│            [Cancel]                         [Create & Start]   │
└─────────────────────────────────────────────────────────────────┘
```

- Kit selection pre-fills image, command, memory
- Secret checkboxes from available secrets
- Port mapping fields
- Workspace: if empty, daemon auto-creates. UI shows resolved path after creation.

## System Tray

```
  [AegisVM ●]
  ├── Status: Running (5 instances)
  ├── ──────────
  ├── my-agent (running)       →  Open | Disable
  ├── browser-agent (paused)   →  Open | Disable
  ├── web-server (running)     →  Open | Disable
  ├── ──────────
  ├── Open Dashboard
  └── Quit
```

- Green/yellow/gray/red dot for daemon status
- Quick instance actions: Open (detail view), Disable/Enable
- No start/stop/pause/resume — aegis manages lifecycle
- No daemon start/stop in tray (auto-managed on app launch)
- Tray stays shallow — never add instance creation, secret editing, or config editing here

## Go Backend

Thin API client. No business logic — all state lives in aegisd.

```go
type App struct {
    socketPath string
}

// Daemon
func (a *App) EnsureDaemon() error
func (a *App) DaemonStatus() (*DaemonStatus, error)

// Instances
func (a *App) ListInstances() ([]Instance, error)
func (a *App) GetInstance(id string) (*Instance, error)
func (a *App) CreateInstance(req CreateRequest) (*Instance, error)
func (a *App) DisableInstance(id string) error
func (a *App) DeleteInstance(id string) error

// Exec
func (a *App) ExecCommand(id string, command []string) (*ExecResult, error)

// Logs
func (a *App) StreamLogs(id string) // emits Wails events to frontend

// Tether
func (a *App) TetherSend(id, sessionID, text string) (*TetherSendResult, error)
func (a *App) TetherPoll(id, sessionID string, afterSeq int64) (*TetherPollResult, error)
func (a *App) TetherCancel(id, sessionID string) error

// Workspace file access
func (a *App) ReadWorkspaceFile(id, path string) ([]byte, error)  // GET workspace API
func (a *App) WriteWorkspaceFile(id, path string, data []byte) error // POST workspace API

// Secrets
func (a *App) ListSecrets() ([]Secret, error)
func (a *App) SetSecret(name, value string) error
func (a *App) DeleteSecret(name string) error

// Kits
func (a *App) ListKits() ([]Kit, error)
```

## Frontend

Svelte with minimal dependencies:

- **marked** — markdown rendering in chat
- **codemirror** — JSON editor for config tab (or textarea for v0.1)

### Polling strategy

- **Dashboard**: 5s polling for instance list
- **Instance detail**: 5s polling for instance state
- **Logs**: NDJSON streaming via `fetch()` + `getReader()` (real-time, no polling)
- **Exec**: NDJSON streaming inline per command (same pattern as logs)
- **Chat**: classic long-poll — immediately next poll on return (`wait_ms=5000`), no sleep

## Project Structure

```
internal/client/              # Shared aegisd API client (Go)
├── client.go                 # HTTP client over unix socket
└── types.go                  # Request/response types

ui/
├── embed.go                  # //go:embed frontend/dist for production
└── frontend/
    ├── src/
    │   ├── App.svelte        # Hash-based router (#/, #/instance/foo)
    │   ├── pages/
    │   │   ├── Dashboard.svelte
    │   │   ├── InstanceDetail.svelte
    │   │   ├── Secrets.svelte
    │   │   └── NewInstance.svelte
    │   ├── components/
    │   │   ├── InstanceList.svelte
    │   │   ├── LogViewer.svelte
    │   │   ├── CommandRunner.svelte
    │   │   ├── ChatPanel.svelte
    │   │   ├── ConfigEditor.svelte
    │   │   └── Toast.svelte
    │   └── lib/
    │       ├── api.ts                  # fetch() wrappers for /api/v1/...
    │       └── store.svelte.ts         # Svelte 5 reactive stores
    ├── index.html
    └── package.json

cmd/aegis/ui.go               # aegis ui subcommand (HTTP server + API proxy)
```

The Wails native app (`cmd/aegis-ui/`) will reuse the same `ui/frontend/` and `internal/client/` when implemented.

## Implementation Order (MVP-first)

Build `aegis ui` (web mode) first — validates the full stack without Wails complexity. Wrap in native app later.

1. ~~**Shared client library** — `internal/client/` reusable aegisd API client~~ **DONE**
2. ~~**Workspace file API** — `GET/POST /v1/instances/{id}/workspace` in aegisd~~ **DONE**
3. ~~**Frontend scaffold** — Svelte 5 + Vite in `ui/frontend/`~~ **DONE**
4. ~~**`aegis ui` command** — HTTP server serving embedded frontend + API proxy to aegisd, auto-starts daemon~~ **DONE**
5. ~~**Dashboard** — instance list with status, ports, disable/delete actions, 5s polling~~ **DONE**
6. ~~**Instance detail: Info + Logs + Exec** — tabbed view with metadata, real-time log streaming (NDJSON), command runner with streamed output/exit code/duration/copy~~ **DONE**
7. ~~**Chat** — tether long-poll, streaming, images (lightbox preview), chat history persisted to localStorage~~ **DONE**
8. ~~**New Instance page** — form with kit/secret selection, port exposure, auto-fills from kit defaults~~ **DONE**
9. ~~**Secrets page** — list/add/delete with inline form~~ **DONE**
10. ~~**Kit Config editor** — kit-manifest-driven JSON editor with syntax highlighting, ghost example preview, workspace + host config support~~ **DONE**
11. **Wails wrapper** — `cmd/aegis-ui/` native app using same frontend + backend
12. **System tray** — when Wails v3 stabilizes or via third-party lib
13. **Polish** — reconnect overlay, keyboard shortcuts, busy button states

### Implementation notes

- **Lifecycle controls**: UI only exposes Disable + Delete. No start/stop/pause/resume — aegis manages lifecycle automatically.
- **Exec on any enabled instance**: API auto-wakes paused/stopped instances. UI only disables exec input for disabled (`enabled=false`) instances.
- **Exec history**: persisted in Svelte store across tab switches, keyed by instance ID.
- **Instance sort order**: running → starting → paused → stopped, then by `updated_at` (most recent first). Fixed in API layer (benefits CLI + MCP too).
- **`updated_at`**: added to lifecycle `Instance` struct, touched on every state transition via `notifyStateChange`. Persisted in registry, restored on daemon restart.

Chat shipped in step 7 (tether long-poll, streaming, image lightbox, localStorage persistence). Deferred: image upload, multiple sessions, cancel button, markdown rendering (uses pre-wrap). Wails native app is step 12 — web mode works first.

## What this does NOT include (v0.1)

- **Multi-machine.** Local management only.
- **Real-time metrics.** No CPU/memory graphs.
- **Full terminal.** Command runner only, no PTY.
- **Log search/indexing.** Streaming display only.
- **File browser.** Workspace file access is API-only (chat images, config editor).
