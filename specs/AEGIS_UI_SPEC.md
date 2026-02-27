# Aegis UI Spec

## Overview

Desktop app for managing AegisVM — instance lifecycle, logs, exec, and agent chat via tether. Built with Wails (Go backend + web frontend + native webview). Not a replacement for the CLI — a visual companion for monitoring and interacting with instances.

Think Docker Desktop, but for microVMs with an agent chat panel.

## Technology

**Wails v2** (stable, production-ready). v3 is alpha — migrate when stable.

- Go backend: talks to aegisd via the existing unix socket API
- Frontend: Svelte (lightweight, fast, good DX) or React
- Native webview: macOS WebKit, Linux WebKitGTK, Windows WebView2
- System tray: daemon status indicator, quick actions
- Binary: ~10MB, ~30MB RAM

The Go backend is a thin client over the aegisd API — no business logic duplication. Every operation is an API call to the running daemon.

## Pages

### 1. Dashboard

Overview of all instances.

```
┌─────────────────────────────────────────────────────┐
│  AegisVM                              [●] Running   │
├─────────────────────────────────────────────────────┤
│  Instances (5)                        [+ New]       │
│                                                     │
│  ● my-agent      agent   running   512MB   0:42:15 │
│  ● browser-agent agent   paused    2048MB  1:15:03 │
│  ● web-server    —       running   512MB   0:05:22 │
│  ◌ test-agent    agent   stopped   512MB   2d ago  │
│  ◌ old-instance  —       disabled  512MB   5d ago  │
│                                                     │
│  Secrets: 4 | Kits: agent            [Settings]     │
└─────────────────────────────────────────────────────┘
```

- Instance list with status, kit, memory, uptime
- Color-coded status: green=running, yellow=paused, gray=stopped/disabled
- Quick actions: start/stop/pause/resume/delete (right-click or action buttons)
- Daemon status in header (running/stopped, version)
- "New instance" button → instance creation dialog

### 2. Instance Detail

Per-instance view with tabs.

#### Info tab

```
┌─────────────────────────────────────────────────────┐
│  ← my-agent                          [■ Stop]      │
├──────┬──────┬───────┬───────┬────────┬──────────────┤
│ Info │ Logs │ Exec  │ Chat  │ Config │              │
├──────┴──────┴───────┴───────┴────────┴──────────────┤
│                                                     │
│  ID:        inst-1772143240906457000                │
│  Handle:    my-agent                                │
│  Kit:       agent                                   │
│  Image:     python:3.12-alpine                      │
│  State:     running                                 │
│  Memory:    512MB (used: 78MB)                      │
│  Workspace: ~/.aegis/data/workspaces/my-agent       │
│  Created:   2026-02-27 10:15:00                     │
│  Uptime:    42m 15s                                 │
│                                                     │
│  Ports:                                             │
│    8080 → localhost:54516 (http)                     │
│                                                     │
│  Secrets:                                           │
│    OPENAI_API_KEY, BRAVE_SEARCH_API_KEY             │
│                                                     │
└─────────────────────────────────────────────────────┘
```

#### Logs tab

Real-time log streaming (via `GET /v1/instances/{id}/logs?follow=true`).

```
┌─────────────────────────────────────────────────────┐
│ [stdout ▼] [auto-scroll ✓] [clear]    [download]   │
├─────────────────────────────────────────────────────┤
│ 2026/02/27 10:15:01 aegis-agent starting            │
│ 2026/02/27 10:15:01 LLM provider: OpenAI            │
│ 2026/02/27 10:15:01 MCP [aegis]: loaded 7 tools     │
│ 2026/02/27 10:15:01 agent listening on 127.0.0.1:77 │
│ 2026/02/27 10:15:30 tool bash: 743 bytes result     │
│ 2026/02/27 10:15:35 tool write_file: 44 bytes resul │
│ █                                                   │
└─────────────────────────────────────────────────────┘
```

- Stdout/stderr filter toggle
- Auto-scroll with pause on manual scroll-up
- Search/filter within logs
- Download log as file

#### Exec tab

Interactive shell-like exec interface.

```
┌─────────────────────────────────────────────────────┐
│ $ ls /workspace                                     │
│ sessions/  .aegis/  app.py  requirements.txt        │
│                                                     │
│ $ free -m                                           │
│               total   used   free   shared   avail  │
│ Mem:           482     78    319        0      393   │
│                                                     │
│ $ cat /workspace/.aegis/agent.json                  │
│ {"mcp":{"dall-e":{"command":"npx",...}}}             │
│                                                     │
│ $ █                                                 │
├─────────────────────────────────────────────────────┤
│ [Enter command...]                          [Run]   │
└─────────────────────────────────────────────────────┘
```

- Command input with Enter to execute
- Output displayed inline (like a terminal)
- Command history (up/down arrows)
- Uses `POST /v1/instances/{id}/exec` API

#### Chat tab (Agent Kit instances only)

Tether-based chat interface. The main UX differentiator.

```
┌─────────────────────────────────────────────────────┐
│ Session: default ▼        [New session] [Sessions]  │
├─────────────────────────────────────────────────────┤
│                                                     │
│  You: Find me a photo of a Brussels Griffon         │
│                                                     │
│  Agent: ● thinking...                               │
│         ● tool: image_search                        │
│         ● tool: bash                                │
│         ● tool: respond_with_image                  │
│                                                     │
│  Agent: Here's a Brussels Griffon!                  │
│         [image: brussels_griffon.jpg]                │
│                                                     │
│  You: Generate one with a top hat                   │
│                                                     │
│  Agent: ● tool: image_generate                      │
│         [generated image]                           │
│         Here's your dapper Brussels Griffon!         │
│                                                     │
├─────────────────────────────────────────────────────┤
│ [Type a message...]                        [Send]   │
└─────────────────────────────────────────────────────┘
```

- Real-time streaming via tether poll (long-poll loop)
- Shows tool calls as they happen (presence frames)
- Renders images inline (from blob store via workspace path)
- Session selector (dropdown: default, research-1, telegram_123, cron_health-check)
- Session history loaded from tether poll with `after_seq=0`
- Markdown rendering for agent responses
- File/image upload (drag & drop → write to workspace → include in message)

#### Config tab (Agent Kit instances only)

Editor for `/workspace/.aegis/agent.json` with syntax highlighting.

```
┌─────────────────────────────────────────────────────┐
│ /workspace/.aegis/agent.json          [Save + ↻]    │
├─────────────────────────────────────────────────────┤
│ {                                                   │
│   "model": "openai/gpt-5.2",                       │
│   "max_tokens": 4096,                               │
│   "disabled_tools": [],                              │
│   "mcp": {},                                         │
│   "memory": {                                        │
│     "inject_mode": "relevant"                        │
│   }                                                  │
│ }                                                    │
├─────────────────────────────────────────────────────┤
│ [Save + Restart] applies changes and calls          │
│ self_restart automatically.                          │
└─────────────────────────────────────────────────────┘
```

- JSON editor with validation
- "Save + Restart" button: writes file via exec, sends self_restart via tether
- Shows current tool list (from agent, via tether query)

### 3. Secrets

```
┌─────────────────────────────────────────────────────┐
│  Secrets                                [+ Add]     │
├─────────────────────────────────────────────────────┤
│  OPENAI_API_KEY          2026-02-23    [Delete]     │
│  TELEGRAM_BOT_TOKEN      2026-02-26    [Delete]     │
│  BRAVE_SEARCH_API_KEY    2026-02-27    [Delete]     │
│  ANTHROPIC_API_KEY       2026-02-20    [Delete]     │
└─────────────────────────────────────────────────────┘
```

- List/add/delete secrets
- Values never displayed (write-only)
- Used when creating new instances (secret picker in creation dialog)

### 4. New Instance Dialog

```
┌─────────────────────────────────────────────────────┐
│  New Instance                                       │
├─────────────────────────────────────────────────────┤
│  Name:     [my-agent          ]                     │
│  Kit:      [agent ▼] (none, agent)                  │
│  Image:    [python:3.12-alpine] (auto from kit)     │
│  Memory:   [512   ] MB                              │
│  Command:  [aegis-agent       ] (auto from kit)     │
│  Workspace:[                  ] (auto if empty)     │
│                                                     │
│  Secrets:  [✓] OPENAI_API_KEY                       │
│            [✓] BRAVE_SEARCH_API_KEY                 │
│            [ ] TELEGRAM_BOT_TOKEN                   │
│            [ ] ANTHROPIC_API_KEY                     │
│                                                     │
│  Ports:    [8080:80  ] [+ Add]                      │
│                                                     │
│            [Cancel]              [Create & Start]   │
└─────────────────────────────────────────────────────┘
```

- Kit selection pre-fills image, command, memory
- Secret checkboxes from available secrets
- Port mapping fields
- Workspace path (auto-created if empty)

## System Tray

```
  [AegisVM ●]
  ├── Status: Running (5 instances)
  ├── ──────────
  ├── my-agent (running)       →  Open | Stop
  ├── browser-agent (paused)   →  Open | Resume
  ├── web-server (running)     →  Open | Stop
  ├── ──────────
  ├── Open Dashboard
  ├── ──────────
  ├── Start Daemon
  ├── Stop Daemon
  └── Quit
```

- Green/yellow/gray dot for daemon status
- Quick instance actions without opening the main window
- "Open" → brings up instance detail in main window

## Go Backend

The Wails Go backend is a thin API client. No business logic — all state lives in aegisd.

```go
// app.go — Wails bound methods

type App struct {
    socketPath string
}

// Instance management
func (a *App) ListInstances() ([]Instance, error)
func (a *App) GetInstance(id string) (*Instance, error)
func (a *App) CreateInstance(req CreateRequest) (*Instance, error)
func (a *App) StartInstance(id string) error
func (a *App) DisableInstance(id string) error
func (a *App) DeleteInstance(id string) error
func (a *App) PauseInstance(id string) error
func (a *App) ResumeInstance(id string) error

// Exec
func (a *App) ExecCommand(id string, command []string) (*ExecResult, error)

// Logs
func (a *App) StreamLogs(id string) // emits events to frontend

// Tether (chat)
func (a *App) TetherSend(id, sessionID, text string) (*TetherSendResult, error)
func (a *App) TetherPoll(id, sessionID string, afterSeq int64) (*TetherPollResult, error)

// Secrets
func (a *App) ListSecrets() ([]Secret, error)
func (a *App) SetSecret(name, value string) error
func (a *App) DeleteSecret(name string) error

// Daemon
func (a *App) DaemonStatus() (*DaemonStatus, error)
func (a *App) DaemonStart() error
func (a *App) DaemonStop() error

// Kits
func (a *App) ListKits() ([]Kit, error)
```

All methods talk to aegisd via `http+unix://~/.aegis/aegisd.sock`. Same API the CLI and MCP server use. The frontend calls these via Wails bindings (auto-generated TypeScript).

## Frontend

Svelte recommended (smallest bundle, fastest, simple reactivity). Alternatively React if team preference.

Key libraries:
- **xterm.js** — for the exec terminal (optional, can start simpler)
- **marked** / **markdown-it** — markdown rendering in chat
- **codemirror** or **monaco** — JSON editor for agent config (optional, can start with textarea)

### Polling strategy

- **Dashboard**: poll `ListInstances` every 3s
- **Logs**: SSE stream via Wails events (Go backend reads log stream, emits to frontend)
- **Chat**: tether long-poll loop (500ms between polls, `wait_ms=5000`)
- **Instance detail**: poll `GetInstance` every 5s for state/memory updates

## Project Structure

```
cmd/aegis-ui/
├── main.go              # Wails app entry
├── app.go               # bound methods (API client)
├── client.go            # aegisd unix socket HTTP client (reuse from CLI)
├── frontend/
│   ├── src/
│   │   ├── App.svelte
│   │   ├── pages/
│   │   │   ├── Dashboard.svelte
│   │   │   ├── InstanceDetail.svelte
│   │   │   ├── Secrets.svelte
│   │   │   └── NewInstance.svelte
│   │   ├── components/
│   │   │   ├── InstanceList.svelte
│   │   │   ├── LogViewer.svelte
│   │   │   ├── ExecTerminal.svelte
│   │   │   ├── ChatPanel.svelte
│   │   │   ├── ConfigEditor.svelte
│   │   │   └── SystemTray.svelte
│   │   └── lib/
│   │       ├── api.ts    # Wails binding wrappers
│   │       └── store.ts  # Svelte stores for state
│   ├── index.html
│   └── package.json
├── build/                # Wails build assets (icons, etc.)
└── wails.json            # Wails project config
```

## Implementation Order

1. **Scaffold** — `wails init`, Svelte template, basic window
2. **API client** — reuse unix socket client from CLI, bind to Wails
3. **Dashboard** — instance list with status, start/stop actions
4. **Instance detail: Info** — basic instance info display
5. **Instance detail: Logs** — log streaming
6. **Instance detail: Exec** — command execution
7. **Secrets page** — list/add/delete
8. **New instance dialog** — creation with kit/secret selection
9. **Instance detail: Chat** — tether-based agent chat (the big feature)
10. **Instance detail: Config** — agent.json editor
11. **System tray** — status + quick actions
12. **Polish** — keyboard shortcuts, dark mode, error handling

## What this does NOT include

- **Multi-machine.** This is a local management UI. No remote daemon connections (future).
- **Real-time metrics.** No CPU/memory graphs. Just current state from `instance info`.
- **Log search/indexing.** Just streaming display. Use the CLI for complex log queries.
- **Build/deploy pipelines.** This is a runtime manager, not a CI/CD tool.
