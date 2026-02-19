# Aegis Architectural Pivot Spec
## Simplifying Core: Removing "Serve Mode" as a First-Class Concept

**Status:** Post-M3b+ refactor target
**Goal:** Simplify Aegis core by removing "serve/app-plane" as a first-class concept and moving serving semantics to harness/SDK (kits).
**Depends on:** [KIT_BOUNDARY_SPEC.md](KIT_BOUNDARY_SPEC.md) for the core/kit responsibility split

---

## 1. Core Principles

### P1 — Instances Are the Only First-Class Runtime Object

Aegis manages **instances**. There is no user-visible "app" object in core.

- Identity: `instanceId`
- Optional alias: `handle`
- All lifecycle operations target instances.

---

### P2 — "Serving" Is Not a Core Lifecycle Mode

Aegis core does not define "serve vs task".

An instance may:
- Exit quickly (task-style)
- Run indefinitely (agent-style)
- Run a server
- Do all three simultaneously

Aegis does not model these differently.

---

### P3 — Ingress Is Declarative, Routing Is Dumb

Aegis supports ingress via `exposes`, but does not track readiness.

Router behavior:
- Proxy traffic based solely on `exposes`
- No serverReady gating
- No implicit inference from open ports

---

### P4 — Resume-on-Ingress Is a Routing Optimization

If an instance is paused and receives inbound traffic to an exposed endpoint:

- Router MAY trigger resume
- Router retries proxy with bounded timeout
- Readiness remains guest responsibility

This is not "serve mode".

---

### P5 — SDK/Harness Owns Serving Semantics

Kits may implement:

- `serve.start(...)`
- `serve.stop()`
- readiness gating
- multiplexing
- canvas routing

These are **kit-level behaviors**, not Aegis core concerns.

---

### P6 — Aegis Does Not Implement Application Versioning

Versioning, publishing, and artifact promotion are responsibilities of kits or external orchestration layers.

Aegis runs instances from an `imageRef`. It does not know or care whether something is v1 or v2, whether it was "published", whether it belongs to a canvas, or whether it has history.

Kit responsibilities:
- Artifact generation
- Version identifiers (v1, v2, sha, tag)
- Promotion (draft → live)
- Rollback
- Canvas/session binding
- Multi-version switching
- Migration

What Aegis provides to make kit-level versioning possible:
- Start instance from arbitrary `imageRef`
- Replace instance (stop + start new from different `imageRef`)
- Snapshot instance (optional, generic)
- Expose stable `handle` → `instanceId` mapping

See [KIT_BOUNDARY_SPEC.md](KIT_BOUNDARY_SPEC.md) for the full responsibility split.

---

## 2. Core Architecture After Pivot

### Aegis Core Provides

- `aegisd` daemon
- Lifecycle manager (start/pause/resume/stop/snapshot)
- ControlChannel + demuxer
- LogStore (ring buffer + NDJSON + sources)
- Router (based purely on `exposes`)
- OCI image pull + cache
- Workspace volumes
- Secret injection
- Resource limits

---

### Removed from Core

- Serve mode / task mode distinction
- App plane terminology (`appId`, `releaseId`)
- `startServer` / `serverReady` / `serverFailed` RPC
- App CRUD (create, publish, serve, list, info, delete)
- Release management (publish, rollback, GC)
- Singleton serve policy
- Canvas semantics
- Readiness port polling

---

## 3. Data Model

### Instance

```
Instance {
  instanceId: string
  handle?: string               // stable alias, unique
  imageRef: string              // OCI image reference
  command: string[]             // what to run
  state: RUNNING | PAUSED | STOPPED
  createdAt: timestamp
  lastActivityAt: timestamp
  exposes: Expose[]             // ingress configuration
  env: map[string]string        // injected environment (including secrets)
  workspacePath?: string        // workspace volume mount
  idlePolicy?: IdlePolicy
  resources: { cpu, mem, disk }
}
```

---

### Expose

```
Expose {
  name: string
  proto: "http" | "tcp"
  listen: {
    host?: string               // default: 127.0.0.1
    port: int                   // 0 = auto-assign
    pathPrefix?: string         // for path-based routing
  }
  target: {
    port: int                   // guest port
  }
}
```

`exposes` are static configuration. Router does not track readiness.

---

### IdlePolicy

```
IdlePolicy {
  pauseAfter: duration          // default: 60s, RUNNING → PAUSED
  stopAfter: duration           // default: 20m, PAUSED → STOPPED (resources released)
}
```

`stopAfter` triggers stop + resource cleanup. The final state is STOPPED — there is no separate TERMINATED state. An instance is either RUNNING, PAUSED, or STOPPED.


---

## 4. API After Pivot

### Instance API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/instances` | Create + start instance from imageRef |
| `GET` | `/v1/instances` | List instances |
| `GET` | `/v1/instances/{id}` | Get instance detail |
| `DELETE` | `/v1/instances/{id}` | Stop + remove instance |
| `POST` | `/v1/instances/{id}/pause` | Pause instance |
| `POST` | `/v1/instances/{id}/resume` | Resume instance |
| `POST` | `/v1/instances/{id}/exec` | Exec command in instance |
| `GET` | `/v1/instances/{id}/logs` | Stream instance logs |

### System API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/v1/status` | Daemon status |

### Removed

- `/v1/apps/*` — all app CRUD
- `/v1/tasks/*` — replaced by instance lifecycle. `aegis run` creates a normal instance and the CLI deletes it on completion or Ctrl+C. If the CLI crashes, the STOPPED instance is a few bytes in memory — cleaned up on `aegis down` or daemon restart. No special instance type, no GC policy.
- `/v1/kits/*` — kits register through their own mechanism, not core API

---

## 5. CLI After Pivot

```
aegis up                                      Start daemon
aegis down                                    Stop daemon
aegis status                                  Daemon status
aegis doctor                                  Diagnose environment

aegis instance start <imageRef> [options]      Start instance
  --name <handle>                               Alias for the instance
  --expose <guest_port>                         Expose a port (repeatable)
  --env KEY=VALUE                               Set environment variable (repeatable)
  --workspace <path>                            Mount workspace volume
  -- <command> [args...]                        Command to run

aegis instance list                            List instances
aegis instance info <handle|id>                Instance detail
aegis instance stop <handle|id>                Stop + remove
aegis instance pause <handle|id>               Pause
aegis instance resume <handle|id>              Resume

aegis exec <handle|id> -- <cmd> [args...]      Execute in instance
aegis logs <handle|id> [--follow]              Stream logs

aegis run [options] -- <cmd> [args...]         Sugar: start + follow + cleanup on exit
  --expose, --env, --workspace, --name           Same as instance start
```

There is no:
- `aegis serve`
- `aegis publish`
- `aegis app *`

`aegis run` is pure client-side sugar: `instance start` + follow logs + `DELETE` on exit/Ctrl+C. The instance is a normal instance — no special type, no flags. The CLI owns cleanup. If the CLI crashes, the stopped instance sits in memory until `aegis down`. For a local single-user tool, this is a non-problem.

---

## 6. Router Behavior

Router proxies only when:

- Expose matches
- Instance is RUNNING (or resumed)

Router does NOT:

- Wait for readiness
- Infer serving from port state
- Implement application-level health checks

Resume-on-ingress:

- If instance is PAUSED and traffic arrives on an exposed port → resume
- Proxy retries with bounded timeout (default: 30s)
- If instance doesn't become responsive → 503
- Guest/kit owns readiness (503 is the guest's problem)

**503 semantics:** A 503 from the router means "could not proxy to the guest port." This can mean the instance is still resuming, the guest process hasn't started listening yet, or the guest process crashed. The router does not distinguish these cases — it tried, the connection failed, it returned 503. Clients should retry. Kits that need richer failure signals should implement health endpoints inside the guest.

---

## 7. Harness RPC After Pivot

### Kept

| Method | Description |
|--------|-------------|
| `run` | Run a command (async, fire-and-forget). Replaces both `runTask` and `startServer`. |
| `exec` | Exec a command with exec_id tagging. |
| `shutdown` | Graceful shutdown (SIGTERM to children, then exit). |
| `health` | Health check. |

### Removed

| Method | Why |
|--------|-----|
| `startServer` | Replaced by `run`. |
| `serverReady` | Readiness is kit/guest concern. |
| `serverFailed` | Failure is just: process exits → instance stops. |

### `run` RPC semantics

`run` replaces both `runTask` (synchronous, waits for exit) and `startServer` (async, fires readiness probe). The new `run` is always async and sets the **primary process** for the instance:

1. Harness starts the process as the instance's primary process
2. Returns immediately: `{pid, started_at}`
3. Streams stdout/stderr as `log` notifications (with `source: "server"`)
4. When process exits: sends `processExited` notification with `{exit_code}`
5. Harness does NOT exit on process exit — it stays alive for exec, logs, shutdown

**`run` sets the primary process, not "an additional process."** An instance has exactly one primary process. Calling `run` on an instance that already has a running primary process is an error. If a kit needs multiple managed processes (supervisor, sidecar), that's kit-level process management inside the guest — not multiple `run` calls.

The harness lifecycle is: start → run command → stay alive → shutdown. Process exit is an event, not a harness exit trigger. Whether to restart the process, stop the instance, or do nothing is decided by aegisd policy (default: mark instance STOPPED).

---

## 8. Log Sources After Pivot

| Source | Meaning |
|--------|---------|
| `server` | Output from the `run` command (was: server + boot) |
| `exec` | Exec command output |
| `system` | Lifecycle events |

The `boot` source is removed — there's no serverReady transition to distinguish "boot" from "running". All command output is `server`.

---

## 9. Refactor Map from M3b

### R1 — Terminology Cleanup
- Remove `appId`, `releaseId` from Instance struct
- Add `handle` (optional alias, unique)
- Add `imageRef` to Instance
- Remove `apps`, `releases` registry tables

### R2 — Remove Serve State Machine
- Delete `startServer` / `serverReady` / `serverFailed`
- Replace with `run` RPC (async, fire-and-forget)
- Process exit → `processExited` notification
- aegisd policy decides what to do (default: instance → STOPPED)

### R3 — Router Simplification
- Route purely from `exposes`
- Implement resume-on-ingress (PAUSED → RUNNING on traffic)
- Remove serverReady gating
- Remove app resolver (no app concept)

### R4 — Rename terminateAfter → stopAfter
- `TerminateAfterIdle` → `StopAfterIdle` in config
- `terminateInstance()` → `stopIdleInstance()` (or inline into existing stop logic)
- No TERMINATED state — only RUNNING, PAUSED, STOPPED

### R5 — Simplify Boot Sequence
- `bootInstance()`: CreateVM → StartVM → create demuxer → send `run` RPC → done
- No readiness wait, no readyCh/failCh coordination
- Instance transitions to RUNNING immediately after `run` RPC succeeds

### R6 — Preserve M3b Infrastructure
- No changes to demuxer architecture
- No changes to logstore
- No changes to exec + done channel flow
- Log source `boot` → `server` (rename only)

### R7 — CLI Restructure
- `aegis instance start` replaces `aegis app create + publish + serve`
- `aegis run` becomes client-side sugar: `instance start` + follow + `DELETE` on exit
- Remove `aegis app`, `aegis kit` from core CLI

### R8 — Registry Cleanup
- Remove `apps`, `releases` tables
- Keep `instances`, `secrets` tables
- `kits` table moves to kit layer (not core)

---

## 10. Migration Steps

1. Add `handle`, `imageRef` to Instance; keep `appId`/`releaseId` temporarily
2. Implement `run` RPC in harness (alongside existing `startServer`/`runTask`)
3. Refactor `bootInstance()` to use `run` instead of `startServer`
4. Simplify router: remove readiness gating, add resume-on-ingress
5. Add new CLI commands (`instance start/stop/pause/resume`)
6. Implement `aegis run` sugar
7. Remove app/release API endpoints and CLI commands
8. Remove `apps`, `releases` registry tables
9. Remove `startServer`/`runTask` from harness (only `run` remains)
10. Update all specs and docs

---

## 11. Acceptance Criteria

Pivot is complete when:

- No "serve mode" or "task mode" exists in core code or spec
- No "app" or "release" concept in core
- Router proxies based only on `exposes`, with resume-on-ingress
- Instances are controllable independent of what they're running
- Readiness logic exists only in harness/kit
- Versioning/publishing exists only in kit layer
- M3b logging, exec, and demuxer remain unchanged
- `aegis run -- echo hello` works (short-lived, CLI deletes on exit)
- `aegis run --expose 80 -- python3 -m http.server 80` works (long-lived with ingress)
- Both use the same code path (`run` RPC, not different modes)
- No `ephemeral` flag, no GC policy, no task-style special casing in the instance model

---

## Summary

Aegis becomes:

> A local scale-to-zero microVM runtime with ingress mapping and control channel.

Serving becomes:

> A harness/kit capability, not a core lifecycle mode.

Versioning becomes:

> A kit concern, not a platform primitive.
