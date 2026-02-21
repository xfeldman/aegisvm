# OpenClaw Kit for Aegis

**Multi-agent autonomous runtime for software engineering tasks**

**Version:** v1.0 Draft
**Date:** 2026-02-17
**Depends on:** [AEGIS_PLATFORM_SPEC.md](AEGIS_PLATFORM_SPEC.md)

---

## 1. Summary

OpenClaw is a multi-agent autonomous runtime where multiple AI agents collaborate on software engineering tasks — planning, coding, reviewing, testing, and deploying — inside isolated Aegis microVMs.

Unlike Famiglia (one agent per app, chat-driven, interactive UI), OpenClaw runs **swarms of agents** that communicate with each other, share a codebase, and operate autonomously for hours with minimal human oversight.

OpenClaw is the second official Aegis kit and serves as the **architectural stress test** — if Aegis's abstractions work for both Famiglia (interactive single-agent) and OpenClaw (autonomous multi-agent), the platform design is correct.

---

## 2. Kit Manifest

```yaml
# aegis-kit.yaml (aegis-kit-openclaw)
name: openclaw
version: 1.0.0
description: "Multi-agent autonomous runtime for software engineering"
homepage: https://github.com/openclaw/aegis-kit-openclaw

image:
  base: ghcr.io/openclaw/agent-base:latest

secrets:
  required:
    - name: ANTHROPIC_API_KEY
      description: "AI provider API key"
      scope: per_workspace
      user_provided: true
    - name: GITHUB_TOKEN
      description: "GitHub access for cloning repos and creating PRs"
      scope: per_workspace
      user_provided: true
      optional: true
    - name: SESSION_TOKEN
      description: "Internal auth token for agent-to-coordinator communication"
      scope: per_session
      generated: true

routing:
  scheme: "/session/{sessionId}/canvas"
  session_stickiness: true
  websocket: true                       # Live log streaming
  expose:
    - port: 80
      protocol: http
      ui: true                          # Live Canvas is user-facing

networking:
  egress: allow
  internal_hosts:
    - openclaw-coordinator:9090         # Session coordinator (host-side)
  inter_vm: true                        # Agents in same session can reach each other
  long_lived_connections: true

policies:
  default_task: long-running            # Extended timeout for autonomous work
  default_serve: serve           # Dashboard only
  warm_pool: 3                          # Keep 3 pre-booted VMs ready

resources:
  memory: 1gb                           # Agents need more RAM (language servers, builds)
  cpu: 2
  disk: 5gb                             # Full repos + node_modules / venvs
  max_agents_per_session: 8
  max_sessions_per_workspace: 3

workspace:
  mode: shared                          # All agents in a session share one workspace volume
```

### 2.1 Kit Hooks

| Hook | Implementation |
|---|---|
| `render_env(app, secrets)` | Adds session ID, coordinator URL, agent role, peer agent endpoints |
| `validate_config(app_config)` | Validates agent role is one of: architect, coder, reviewer, tester, browser |
| `on_publish(app, release, artifacts)` | Collects session artifacts (diffs, test results) into session report |

---

## 3. Core Concepts

### 3.1 Sessions

A **session** is a multi-agent collaboration scoped to a task. It is a kit-level abstraction — Aegis sees individual VM instances; OpenClaw groups them into sessions.

```
Session: "fix-auth-bug-1234"
├── Architect agent (VM 1) — plans the approach, delegates subtasks
├── Coder agent (VM 2) — implements the fix
├── Coder agent (VM 3) — implements related test changes
├── Reviewer agent (VM 4) — reviews the diff
└── Tester agent (VM 5) — runs the test suite

All share: workspace volume (the git repo)
All connected: session-scoped internal network
Coordinated by: openclaw-coordinator (host-side service)
```

A session maps to Aegis resources as:

| OpenClaw concept | Aegis resource |
|---|---|
| Session | N agent instances + 1 canvas instance + 1 shared workspace volume + 1 network group |
| Agent | 1 Aegis VM instance (task/long-running policy) |
| Workspace | 1 Aegis shared workspace volume |
| Live Canvas | 1 Aegis app (serve policy, port 80, scale-to-zero) |

### 3.2 Agent Roles

Each agent has a role that determines its tools, system prompt, and permissions:

| Role | Tools | Description |
|---|---|---|
| `architect` | Read files, search, plan | Analyzes the task, creates a plan, delegates subtasks to coders |
| `coder` | Shell, editor, git, LSP | Writes and modifies code |
| `reviewer` | Read files, git diff, comment | Reviews changes, requests fixes |
| `tester` | Shell, test runner | Runs tests, reports results |
| `browser` | Headless browser, web fetch | Researches docs, APIs, Stack Overflow |

### 3.3 Coordinator

The **coordinator** is a host-side service (not a VM) that manages session lifecycle:

- Receives the initial task from the user
- Spawns agents (creates Aegis instances)
- Routes messages between agents
- Tracks task progress and agent status
- Enforces approval gates (human-in-the-loop)
- Collects results and artifacts
- Tears down the session when complete

The coordinator communicates with agents over HTTP (via the session internal network) and with Aegis over the aegisd API (unix socket).

```
User ──► openclaw CLI / API
              │
              ▼
         Coordinator (host-side)
              │
    ┌─────────┼─────────┬─────────┐
    ▼         ▼         ▼         ▼
  Agent 1   Agent 2   Agent 3   Agent 4
  (architect) (coder)  (coder)  (tester)
    │         │         │         │
    └─────────┴─────────┴─────────┘
              Shared workspace volume
```

---

## 4. How OpenClaw Differs from Famiglia

| Dimension | Famiglia | OpenClaw | Aegis implication |
|---|---|---|---|
| VMs per task | 1 | 2-8 | Multi-instance orchestration |
| Workspace sharing | Never (per-agent) | Always (per-session) | **Shared workspace volumes needed** |
| Inter-VM communication | None | Required | **Session-scoped networks needed** |
| Primary execution mode | `serve` | Long-running tasks | Extended task timeouts |
| Task duration | Minutes | Hours | `maxRuntimeTask` >> 15m |
| Human interaction | Continuous (chat) | Sparse (approval gates) | No XMPP — HTTP callbacks |
| Warm VM pool | Optional | Critical | Pool pre-allocation |
| Canvas | Interactive canvas in room tab | Live Canvas with A2UI (execution flow, dashboards) | Both use serve-mode apps |

---

## 5. Aegis Core Additions Required

Designing the OpenClaw kit reveals three capabilities that Aegis core must support but that Famiglia alone would not have surfaced.

### 5.1 Shared Workspace Volumes

**Current spec:** One workspace volume per app, isolated.

**OpenClaw needs:** Multiple VMs mounting the same workspace volume (e.g., a git repo that all agents edit concurrently).

**Proposed addition to Aegis core:**

```yaml
# In kit manifest
workspace:
  mode: shared          # "isolated" (default, per-instance) or "shared" (per-session/group)
```

When `mode: shared`, Aegis creates a single named volume and mounts it into all instances that declare the same workspace group. File-level coordination (locking, merge conflicts) is the kit's responsibility — Aegis just provides the shared mount.

Implementation: bind-mount the same host directory into multiple VM rootfs overlays. No new technology needed.

### 5.2 Session-Scoped Internal Networks

**Current spec:** VMs are isolated. Ingress only from router. Egress to internet + kit-declared internal hosts.

**OpenClaw needs:** Agents in the same session to reach each other over HTTP (for message passing, status checks, artifact exchange).

**Proposed addition to Aegis core:**

```yaml
# In kit manifest
networking:
  inter_vm: true        # Allow instances in the same session/group to reach each other
```

When `inter_vm: true`, Aegis puts all instances in the same "network group" on a shared bridge/subnet. Each VM gets a stable hostname (`agent-{instanceId}.session.local`) resolvable by peers.

- Only VMs in the same group can reach each other
- Does not open inbound from outside the group
- Router still handles external ingress
- Kit declares the policy; Aegis enforces the network topology

### 5.3 VM Warm Pool

**Current spec:** Listed as open question.

**OpenClaw needs:** When a session spawns 5 agents, they should all start within seconds, not sequentially wait for boot.

**Proposed addition to Aegis core:**

```yaml
# In kit manifest
policies:
  warm_pool: 3          # Keep N pre-booted VMs in standby
```

Aegis maintains a pool of pre-booted VMs (restored from base snapshot, harness ready, waiting for task assignment). When a kit requests `instances/ensure`, the pool provides an instant VM instead of cold-booting.

Pool size is configurable per-kit with a platform-wide maximum. Pool VMs are generic (base snapshot only) — kit-specific setup (workspace mount, secrets, network group) happens at assignment time.

---

## 6. Execution Model

### 6.1 Session Lifecycle

```
User submits task
    │
    ▼
Coordinator analyzes task, plans agent allocation
    │
    ▼
Coordinator calls aegisd: create shared workspace, clone repo
    │
    ▼
Coordinator spawns agents:
    POST /v1/instances/ensure (architect, workspace=shared, network_group=session-123)
    POST /v1/instances/ensure (coder-1, workspace=shared, network_group=session-123)
    POST /v1/instances/ensure (coder-2, workspace=shared, network_group=session-123)
    │
    ▼
Agents boot (from warm pool: instant; cold: <1s)
    │
    ▼
Architect plans → delegates subtasks → coders implement
    │
    ▼
Coordinator spawns reviewer + tester as needed
    │
    ▼
Review pass → tests pass → coordinator collects diff
    │
    ▼
Approval gate: user approves → coordinator creates PR
    │
    ▼
Session complete → coordinator terminates all agents
    │
    ▼
Workspace volume retained (for inspection) or cleaned up
```

### 6.2 Task Execution Policy

OpenClaw agents run long autonomous tasks, not interactive serving:

```json
{
  "policy": "long-running",
  "cpu_millis": 2000,
  "mem_mb": 1024,
  "network": {
    "egress": "allow",
    "inter_vm": true,
    "network_group": "session-abc123"
  },
  "workspace": {
    "mode": "shared",
    "volume_id": "ws-abc123"
  },
  "command": ["openclaw-agent", "--role", "coder", "--task-id", "subtask-1"],
  "timeouts": {
    "max_runtime": "4h"
  },
  "secrets": ["ANTHROPIC_API_KEY", "GITHUB_TOKEN", "SESSION_TOKEN"]
}
```

### 6.3 Execution Policies

| Policy | When used | Lifecycle |
|---|---|---|
| `long-running` | Agent working on a subtask | Boot from warm pool, run for hours, terminate on completion. Pause if idle >5m waiting for input from coordinator. |
| `stateless` | One-off analysis or tool call | Boot, run, collect output, destroy. |
| `serve` | Session dashboard | Serve monitoring UI, standard pause/terminate on idle. |

Note: `long-running` is a new policy preset that OpenClaw needs. It extends `stateless` with:
- Much longer `maxRuntimeTask` (4h default vs 15m)
- Pause-on-idle instead of terminate-on-idle (agent may be waiting for peer)
- Heartbeat requirement (agent must report progress every N minutes or get terminated)

---

## 7. Agent Communication

### 7.1 Architecture

Agents communicate via the coordinator. No direct agent-to-agent messaging for v1 — the coordinator acts as a message bus.

```
Agent 1 (architect)
    │ POST /coordinator/tasks/assign {agent: "coder-1", task: "implement auth fix"}
    ▼
Coordinator
    │ POST /agent-2/tasks/accept {task: "implement auth fix", context: {...}}
    ▼
Agent 2 (coder-1)
    │ POST /coordinator/tasks/complete {task: "implement auth fix", result: "committed abc123"}
    ▼
Coordinator
    │ POST /agent-4/tasks/assign {agent: "reviewer", task: "review commit abc123"}
    ▼
Agent 4 (reviewer)
```

### 7.2 Why Coordinator-Mediated

- **Auditability**: All messages flow through one point, logged for human review
- **Approval gates**: Coordinator can intercept and require human approval before forwarding
- **Fault tolerance**: If an agent crashes, coordinator reassigns the task
- **Rate limiting**: Coordinator controls how fast agents spawn and how many LLM calls are made

### 7.3 Inter-VM Network Usage

Although communication is coordinator-mediated, inter-VM networking is still needed for:

- Direct file transfer between agents (large artifacts, build outputs)
- LSP server sharing (one agent runs a language server, others connect)
- Peer health checks
- Future: direct agent-to-agent negotiation (v2)

---

## 8. Live Canvas

OpenClaw features a **Live Canvas** — an agent-driven visual workspace (A2UI) where agents render UI, map execution flows, draft interfaces, and update tasks in real time.

At the Aegis level, it maps to:

```
OpenClaw Canvas = (AppID, ReleaseID, ServeTarget with port 80, ui: true)
```

| OpenClaw concept | Aegis mapping |
|---|---|
| Canvas ID (per session) | `AppID` |
| Canvas version | `ReleaseID` |
| Canvas URL | ServeTarget expose port 80, `ui: true` |
| Live Canvas SPA | Kit-provided React app served from workspace volume |

### 8.1 Canvas Use Cases

| Use case | What agents render |
|---|---|
| **Session dashboard** | Agent status, task progress, approval gates, live logs |
| **Execution flow** | Visual DAG of agent tasks, dependencies, and data flow |
| **Code review** | Side-by-side diff view with agent review comments |
| **Architecture diagram** | Agent-generated system diagrams updated as code changes |
| **Test results** | Live test runner output with pass/fail visualization |

### 8.2 Session Dashboard (Default Canvas)

```
┌──────────────────────────────────────────────┐
│  Session: fix-auth-bug-1234                  │
│  Status: Running (3/5 subtasks complete)     │
│  Duration: 47m                               │
├──────────────────────────────────────────────┤
│                                              │
│  Agents:                                     │
│    architect  — Plan complete                │
│    coder-1   — Implementing auth.ts fix      │
│    coder-2   — Updating test suite           │
│    reviewer   — Waiting for code             │
│    tester     — Waiting for review           │
│                                              │
│  Recent activity:                            │
│  [12:34] coder-1: Modified src/auth.ts (+23) │
│  [12:33] coder-2: Added test/auth.test.ts    │
│  [12:30] architect: Delegated subtask-3      │
│                                              │
│  [Approve & Merge] [Pause All] [Cancel]      │
│                                              │
├──────────────────────────────────────────────┤
│  Live logs: coder-1                          │
│  > Running: npm install                      │
│  > Added 234 packages                        │
│  > Modifying src/auth.ts...                  │
└──────────────────────────────────────────────┘
```

### 8.3 Implementation

The Live Canvas is a single Aegis app (one VM, `serve` policy) exposing port 80. It connects to the coordinator via WebSocket for real-time agent state and renders a SPA.

Each session gets its own canvas app. The canvas scales to zero when the user isn't viewing it — wakes instantly when they open the session URL.

Agents update the canvas by posting state changes to the coordinator, which pushes them to the canvas via WebSocket. Agents do not render HTML directly — the canvas SPA interprets agent state into visual components (A2UI pattern).

---

## 9. Base Image

```dockerfile
# ghcr.io/openclaw/agent-base
FROM aegis/base:latest

# Development tools
RUN apk add --no-cache \
    git openssh-client curl jq \
    nodejs npm python3 py3-pip \
    build-base

# Language servers (for code intelligence)
RUN npm install -g typescript typescript-language-server

# OpenClaw agent runtime
RUN npm install -g @openclaw/agent-runtime

# Headless browser (for browser agent role)
RUN npx playwright install chromium --with-deps

WORKDIR /workspace
ENTRYPOINT ["openclaw-agent"]
```

Heavier than Famiglia's base image because agents need full dev tooling. This is why `resources.memory: 1gb` and `resources.disk: 5gb`.

---

## 10. Agent SDK (`@openclaw/agent-runtime`)

```typescript
import { OpenClawAgent } from '@openclaw/agent-runtime'

const agent = new OpenClawAgent({
  role: process.env.AGENT_ROLE,       // 'coder', 'reviewer', etc.
  sessionId: process.env.SESSION_ID,
  coordinatorUrl: process.env.COORDINATOR_URL,
})

// Receive tasks from coordinator
agent.onTask(async (task) => {
  // task.type: 'implement', 'review', 'test', etc.
  // task.context: { files, description, constraints }

  // Use tools
  const result = await agent.tools.shell('npm test')
  await agent.tools.editFile('src/auth.ts', newContent)
  const diff = await agent.tools.git('diff')

  // Report progress (resets heartbeat timer)
  await agent.reportProgress('Implemented auth fix, running tests...')

  // Complete the task
  await agent.completeTask({
    result: 'success',
    artifacts: ['src/auth.ts', 'test/auth.test.ts'],
    summary: 'Fixed token validation in auth middleware',
  })
})

// Heartbeat (required — coordinator terminates agents that go silent)
agent.startHeartbeat({ intervalMs: 60_000 })

agent.start()
```

---

## 11. CLI

OpenClaw provides its own CLI that wraps Aegis operations:

```bash
# Session management
openclaw run "Fix the auth bug in issue #1234" --repo github.com/org/repo
openclaw run --plan plan.yaml                  # Pre-defined agent allocation
openclaw sessions                              # List active sessions
openclaw session abc123 --status               # Session details
openclaw session abc123 --logs coder-1         # Stream agent logs
openclaw session abc123 --approve              # Approve pending changes
openclaw session abc123 --cancel               # Cancel session

# Uses Aegis under the hood
openclaw doctor                                # Checks Aegis is running, kit is installed
```

Behind the scenes, `openclaw run` calls:

1. `aegisd POST /v1/tasks` (create workspace, clone repo)
2. `aegisd POST /v1/instances/ensure` (spawn agents from warm pool)
3. Starts coordinator on host
4. Monitors until completion or approval gate

---

## 12. What This Kit Proves About Aegis

### 12.1 Kit Maturity Test Results

| Test | Result | Notes |
|---|---|---|
| Can OpenClaw be built without modifying Aegis core? | **Almost** | Needs 3 additions: shared volumes, inter-VM networking, warm pool |
| Are those additions generic (useful beyond OpenClaw)? | **Yes** | GitHub Actions kit would also need shared volumes and warm pool |
| Does Famiglia break when these additions are made? | **No** | All additive, Famiglia uses `workspace.mode: isolated` (default) and `inter_vm: false` (default) |
| Is the kit boundary correct? | **Yes** | Session management, agent roles, coordinator, approval gates — all kit-level, none in Aegis core |

### 12.2 Aegis Core Additions Summary

These additions are generic platform capabilities, not OpenClaw-specific:

| Addition | Aegis spec section affected | Generic value |
|---|---|---|
| Shared workspace volumes (`workspace.mode: shared`) | §9 Persistence | Any multi-VM workflow (CI, batch processing) |
| Session-scoped inter-VM networks (`inter_vm: true`) | §8 Networking | Any multi-agent kit, distributed computing |
| VM warm pool (`warm_pool: N`) | §7 Hot-Start (new subsection) | Any kit needing fast spawn |
| `long-running` policy preset | §14 Execution Policies | Autonomous agents, CI runners |
| Heartbeat-based health check | §15 Lifecycle | Any long-lived VM needs liveness detection |

None of these are OpenClaw-specific. A GitHub Actions kit, a LangChain kit, or a distributed testing kit would need the same primitives. This confirms they belong in Aegis core, not hacked into OpenClaw.

---

## 13. Implementation Phases

### Phase 1: Single-Agent MVP

- [ ] OpenClaw agent runtime (TypeScript)
- [ ] Single-agent mode: `openclaw run "task" --agents 1`
- [ ] Basic coordinator (host-side, in-process)
- [ ] Shell, file, and git tools
- [ ] Uses Aegis `stateless` policy (existing)
- [ ] CLI: `openclaw run`, `openclaw sessions`

**Deliverable:** One agent works on a task autonomously in an Aegis VM.

### Phase 2: Multi-Agent Sessions

- [ ] Multi-agent spawning with role assignment
- [ ] Coordinator with task delegation and message routing
- [ ] Shared workspace volumes (requires Aegis core addition)
- [ ] Inter-VM networking (requires Aegis core addition)
- [ ] Agent-to-agent artifact passing
- [ ] Dashboard app (monitoring UI)
- [ ] Approval gates (human-in-the-loop)

**Deliverable:** Multiple agents collaborate on a task. User monitors via dashboard.

### Phase 3: Production

- [ ] VM warm pool integration (requires Aegis core addition)
- [ ] Persistent session history and replay
- [ ] Cost tracking (LLM API usage per session)
- [ ] GitHub PR integration (auto-create PRs with session artifacts)
- [ ] Agent role plugins (custom roles beyond the defaults)
- [ ] Session templates (pre-configured agent allocations for common tasks)

---

## 14. Open Questions

1. **Coordinator placement** — host-side process or its own Aegis VM? Host-side is simpler and has direct access to aegisd API. VM provides isolation but adds complexity.
2. **Git conflict resolution** — when two coders edit the same file, who wins? Coordinator-mediated merge? Optimistic locking?
3. **LLM routing** — should different agent roles use different models? (e.g., Claude for architect/reviewer, faster model for coder)
4. **Session persistence** — can a session be paused overnight and resumed? This requires workspace volume + session state in coordinator to survive restart.
5. **Agent scaling** — should coordinator dynamically add/remove agents based on task complexity?

---

## Related Documents

- [AEGIS_PLATFORM_SPEC.md](AEGIS_PLATFORM_SPEC.md) — Aegis platform specification
- [FAMIGLIA_KIT_SPEC.md](FAMIGLIA_KIT_SPEC.md) — Famiglia kit (first Aegis kit, interactive single-agent)
