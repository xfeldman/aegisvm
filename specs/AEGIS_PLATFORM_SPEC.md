# Aegis: Local Agent Runtime Platform

**ARM64-first, MicroVM-based**

**Version:** v2.0 Draft
**Date:** 2026-02-17
**Audience:** engineering, ops, kit developers

---

## 1. Summary

Aegis is a local agent runtime platform that runs AI agent workloads inside isolated microVMs. It provides VM-level isolation, hybrid hot-start (sub-second resume), scale-to-zero port serving, task execution, and a kit system for application-specific integration.

Aegis uses **Firecracker** (Linux/KVM) as its production VMM backend and **libkrun** (macOS/HVF) for native Apple Silicon development. Both share the same aegisd control plane and guest harness — the VMM is an implementation detail.

Aegis is designed to be the **substrate** that any agent platform builds on — not an agent platform itself. Agent-specific concerns (chat protocols, data APIs, orchestration logic) belong in kits. Aegis handles the hard infrastructure: VMs, snapshots, networking, routing, and lifecycle.

### Core Principles

- **Pause** == hot RAM retained (no disk snapshot written)
- **Terminate** == restore from disk layers (canonical state)
- **Disk layers are canonical; memory snapshots are cache** — enforced in lifecycle, GC, publish, and security
- **Secrets are never persisted in snapshots** — re-injected on every restore
- **No per-publish memory snapshot** — publishing produces disk artifacts only
- **Aegis semantics are backend-independent; optimizations are backend-capability-dependent.** If a backend can't do memory snapshot/restore, Aegis still behaves correctly by restarting from disk layers. No feature degrades into incorrect behavior — only into slower recovery.
- **Usable without kits** — `aegis run` works out of the box
- **Kits are integration accelerators**, not requirements

---

## 2. Goals and Non-Goals

### 2.1 Goals

- Run untrusted or semi-trusted agent workloads with VM-level isolation
- Two execution modes: (A) task execution and (B) port serving with scale-to-zero
- Hybrid hot-start: short-idle pause/resume + disk-layer restore for longer idle (memory snapshots are an optional accelerator, never required for correctness)
- Internet egress from sandboxes while keeping ingress controlled via a local router
- Fixed parameters allowed (arch, base Linux image, runtime versions) to maximize caching and speed
- Simple, stable addressing for apps (AppID, ReleaseID), and transparent routing
- Kit system for application-specific integration without modifying Aegis core
- First-class macOS support via libkrun (native Apple HVF, no nested virtualization)

### 2.2 Non-Goals (v1)

- Global web-scale multi-region routing
- Automatic horizontal autoscaling per app beyond small team concurrency (can be added later)
- Running arbitrary guest OS distributions (we ship a fixed minimal image)
- Perfectly persistent in-VM state across long idle without explicit persistence (use workspace volumes)
- Managing application-specific protocols (chat, data APIs, orchestration)

---

## 3. Architecture

### 3.1 Components

```
┌─────────────────────────────────────────────────────────────┐
│  Host (Linux with KVM, or macOS with libkrun/HVF)           │
│                                                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────────┐  │
│  │ aegisd   │  │  Router   │  │ Registry │  │  CLI       │  │
│  │  (Go)    │  │  (HTTP    │  │ (SQLite) │  │  (aegis)   │  │
│  │          │  │  reverse  │  │          │  │            │  │
│  │          │  │  proxy)   │  │          │  │            │  │
│  └────┬─────┘  └────┬─────┘  └──────────┘  └────────────┘  │
│       │              │                                      │
│  ┌────┴──────────────┴──────────────────────────────────┐   │
│  │  VMM Backend (Firecracker on Linux, libkrun on macOS) │   │
│  │  ┌─────────┐  ┌─────────┐  ┌─────────┐               │   │
│  │  │  VM 1   │  │  VM 2   │  │  VM 3   │  ...          │   │
│  │  │ harness │  │ harness │  │ harness │               │   │
│  │  └─────────┘  └─────────┘  └─────────┘               │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

- **aegisd** (Go): local control plane daemon managing microVMs, snapshots, pooling, quotas, logs, artifacts. Abstracts VMM backend (Firecracker or libkrun). Communicates with VMs over vsock.
- **Agent harness** (guest): minimal init/PID1 inside each VM, handling `runTask` and `startServer` via vsock RPC. Same binary regardless of VMM backend.
- **Router** (host): connection-aware lifecycle proxy that maps requests to sandbox instances. Aegis owns the router — kits configure URL schemes but do not replace it.
- **Registry** (host): SQLite database storing release pointers, snapshot refs, instance endpoints, kit configurations.
- **CLI** (`aegis`): user-facing command-line interface. Same interface on Linux and macOS.

### 3.2 Trust Boundaries

- **Host** (aegisd + router) is trusted
- **Guest VMs** are untrusted — they get outbound internet but inbound only from router
- **Workspace volume** is the persistence boundary — VM memory is treated as ephemeral
- **Kits** are trusted (installed by admin) but cannot bypass platform constraints

---

## 4. Core Concepts

### 4.1 Identifiers

| Identifier | Description |
|---|---|
| `AppID` | Stable identity of a deployable unit within a workspace. Does not change on publish. |
| `ReleaseID` | Immutable published version pointer (monotonic vN or content hash). |
| `SessionID` | Per-user browser session identifier for auth, tracking, and optional stickiness. |
| `InstanceID` | Running microVM identifier, transient. |

Kits alias these to domain-specific terms. For example, Famiglia calls an App a "canvas" in its UI. Aegis core uses the generic terms.

### 4.2 ServeTarget

A **ServeTarget** is the core declarative object that describes what a VM exposes and how the router handles it. Every serve-mode instance has one.

```yaml
# ServeTarget definition
serve_target:
  app_id: "app-abc123"
  release_id: "v3"                     # Which release to serve
  expose:
    - port: 80
      protocol: http                   # HTTP-aware (path routing, loading page)
      ui: true                         # User-facing UI (friendly wake page, CLI "open" link)
    - port: 8080
      protocol: http
    - port: 5432
      protocol: tcp                    # TCP-level proxy (opaque byte forwarding)
    - port: 9090
      protocol: grpc                   # gRPC-aware (request queuing during wake)
  policy: serve                        # serve, keep-alive, etc.
  wake_on_connect: true                # Default: true. Router wakes VM on connection.
```

The ServeTarget is how the router knows:
- Which ports to listen on and proxy
- Which protocol to use (affects wake behavior)
- Which app/release maps to which instance
- Whether to wake on connect or require the VM to be pre-started

Kits declare default ServeTargets in their manifest. Users can override per-instance.

### 4.3 Artifact and Snapshot Tiers

Three tiers, aligned with the canonical rule: **disk layers are canonical, memory snapshots are cache.**

| Tier | What it is | Contains | Canonical? | Lifecycle |
|---|---|---|---|---|
| **Base snapshot** | Golden VM image | OS + harness + warmed runtimes. May include a memory snapshot for fast boot (few copies). | Rebuildable | Versioned by `BaseRevision`. Rebuilt on upgrade. |
| **Release** | Published build output | Artifact (static bundle, server code, assets, metadata) + disk overlay (COW dm-snapshot referencing base rootfs). **No memory snapshot.** | **Yes — canonical** | Durable, subject to GC (§10). |
| **Cached instance snapshot** | Optional memory snapshot of a running/paused instance | Full VM state (RAM + disk) for ultra-fast resume. | **No — cache only** | Ephemeral, TTL-based (hours). Deleted under memory/storage pressure. Never treated as source of truth. |

Publishing a release produces **disk artifacts only** — never a memory snapshot. This is the key invariant that keeps snapshots reproducible and GC safe. See §10 for full details.

### 4.4 Instances

An instance is a running microVM. On cold start, it is **booted from disk layers** (base rootfs + release overlay). On warm resume, it is **resumed from a cached instance snapshot** if one exists, otherwise booted fresh. Default policy: one active instance per `(AppID, ReleaseID)`, with optional future extension to multiple instances.

---

## 5. Kit System

### 5.1 What Is a Kit

A kit is a **reproducible execution environment + integration layer** that runs on top of Aegis. It defines what goes inside the VM, how the agent communicates with external services, and how URLs are structured.

### 5.2 Official Kits

| Kit | Purpose |
|---|---|
| **Famiglia** | Team agents with chat and data integration (aliases App as "canvas" in its UI) |
| **OpenClaw** | Multi-agent autonomous runtime |

### 5.3 Aegis Core Owns (Non-Negotiable Platform Layer)

These are platform primitives. Kits cannot replace, bypass, or override them:

- MicroVM lifecycle (create, pause, resume, terminate)
- Snapshot tiers and pause semantics
- Workspace mount model
- Networking substrate (TAP, NAT, router)
- Resource limits and quotas
- Task execution primitive
- Artifact capture
- Secret injection mechanism
- Router and HTTP exposure
- Vsock harness protocol

### 5.4 Kit Provides (Agent Environment Contract)

A kit provides:

- **Base rootfs/image** (or layered on top of Aegis base image)
- **agentd plugin / SDK** that runs inside the VM
- **Default execution policies** (stateless, build, serve, etc.)
- **URL routing scheme** (path prefix, session stickiness requirements)
- **Default network allowlist rules** (internal service access)
- **Secret requirements** declaration
- **Task spec schema**

### 5.5 Kit Manifest + Hooks Contract

A kit is registered via a **manifest** (static configuration) and optional **hooks** (runtime callbacks).

#### Kit Manifest (Static)

```yaml
# aegis-kit.yaml
name: famiglia
version: 1.0.0
description: "Team canvas agents with XMPP chat and Space data integration"

image:
  base: ghcr.io/famiglia/agent-base:latest
  layers: []                              # Optional additional layers

secrets:
  required:
    - name: FAMIGLIA_API_KEY
      scope: per_app
      generated: true
    - name: XMPP_PASSWORD
      scope: per_app
      generated: true
    - name: ANTHROPIC_API_KEY
      scope: per_workspace
      user_provided: true
      optional: true

routing:
  scheme: "/agent/{agentId}/{path}"
  session_stickiness: false
  websocket: true

networking:
  egress: allow
  internal_hosts:
    - famiglia-web:3000
    - ejabberd:5222
    - ejabberd:5280
  long_lived_connections: true

policies:
  default_task: stateless
  default_serve: serve

resources:
  memory: 512mb
  cpu: 1
  disk: 1gb
```

#### Kit Hooks (Runtime, Optional)

| Hook | When | Purpose |
|---|---|---|
| `render_env(app, secrets) -> env_map` | Before VM boot/restore | Kit transforms secrets + config into the final env var set for the VM |
| `validate_config(app_config) -> ok/error` | On app creation/update | Kit validates app-specific configuration |
| `on_publish(app, release, artifacts) -> ok` | After successful publish | Kit performs post-publish actions (e.g., register with external service) |

Hooks are optional. If a kit provides no hooks, Aegis uses default behavior (pass secrets as env vars directly, no validation, no post-publish).

### 5.6 Kit Constraints

Kits **do not**:

- Control VM lifecycle
- Replace router
- Override snapshot semantics
- Bypass resource limits

### 5.7 Kit-less Operation

Aegis must be fully usable without any kit installed. Running:

```bash
aegis run --image base:python -- python main.py
```

Must provide:

- Isolated microVM
- Internet egress
- Workspace volume mounted
- HTTP exposed if requested (`--expose`)
- No kit required

This is the **minimum credibility threshold**. Kits are integration accelerators — not required for core function.

### 5.8 Kit Maturity Test

If any of these kits can be built without modifying Aegis core, the design is correct:

- GitHub Actions kit (CI task runner)
- LangChain kit (agent orchestration)
- Browser automation kit (Playwright/Puppeteer sandbox)
- Code interpreter kit (Jupyter-style execution)

---

## 6. Execution Modes

Two modes. Everything is scale-to-zero by default.

### 6.1 Mode A: Task

Run a command, collect output, done. VM lifecycle is tied to task completion.

1. Restore VM from base snapshot (or version snapshot if task is version-specific)
2. Send `runTask(spec)` to guest harness over vsock
3. Collect logs + artifacts; optional: task may produce/update a published version
4. On completion: pause briefly (allows follow-up tasks without cold boot), then terminate. Next use restores from disk layers.

### 6.2 Mode B: Serve

Expose ports, wake on connection, hibernate on idle. VM lifecycle is tied to connection activity.

1. Connection arrives on any exposed port (HTTP, TCP, gRPC, WebSocket)
2. Router ensures instance exists — paused? resume (~ms). Terminated? restore from disk layers (~1s). Running? proceed.
3. Router proxies connection to the VM
4. Router tracks connection as activity (resets idle timer)
5. Last connection closes → idle timer starts → pause → terminate

**All serve-mode apps use the same mechanism.** A web UI is a serve-mode VM that exposes port 80 with HTML. A Postgres sandbox is a serve-mode VM that exposes port 5432. An MCP server is a serve-mode VM that exposes its tool endpoint. Same router, same wake-on-connect, same scale-to-zero.
### 6.3 Port Exposure Model

VMs declare which ports to expose. All exposed ports get wake-on-connect:

```yaml
# Per-instance or in kit manifest
expose:
  - port: 80
    protocol: http        # HTTP-aware: can inspect path, return loading page while VM wakes
    ui: true              # This is a user-facing UI (see below)
  - port: 8080
    protocol: http
  - port: 5432
    protocol: tcp          # TCP-level proxy: opaque, forwards bytes after VM is ready
  - port: 9090
    protocol: grpc         # gRPC-aware: can queue requests during wake
```

For HTTP, the router can return a brief loading response while the VM wakes. For raw TCP, the connection holds until the VM is ready (client TCP timeout is typically 30s+ — plenty of time for a ~1s restore).

#### The `ui` hint

`ui: true` tells Aegis this port serves a user-facing interface. It does not change routing or lifecycle — it's a hint that enables:

- **Friendly wake page**: router shows a branded loading page while the VM boots (instead of raw 503)
- **CLI integration**: `aegis app info` shows an "Open in browser" URL for `ui: true` ports
- **WebSocket defaults**: enables WebSocket-friendly proxy settings (longer timeouts, no buffering)

Kits mark user-facing ports as `ui: true`.

### 6.4 What Stays Running vs What Hibernates

```
Default (wake on demand, hibernate on idle):
  Everything with exposed ports and no active connections.
  Web UIs, API endpoints, database sandboxes, MCP servers,
  dev servers, chat agents (via kit message proxy).

Running while working:
  VMs with active connections (router tracks these).
  VMs executing tasks (until task completes).

Running always (explicit opt-in):
  keep_alive: true — cron workers, background processors,
  stream consumers. The exception, not the default.
```

**Default is: nothing runs unless triggered.**

### 6.5 Event Sources

Any event can trigger `instances/ensure`. The router is just the built-in event source for connections. Kits can add more:

| Event source | Provider | Trigger mechanism |
|---|---|---|
| HTTP/TCP connection | Aegis router (built-in) | Connection arrives on exposed port |
| Webhook (Slack, GitHub) | Aegis router (it's just HTTP) | POST to exposed port |
| Chat message (XMPP, etc.) | Kit (host-side proxy) | Proxy calls `instances/ensure` via aegisd API |
| Queue message | Kit (coordinator/consumer) | Consumer calls `instances/ensure` via aegisd API |
| Cron / schedule | aegisd (built-in timer) | Timer calls `instances/ensure` |

All event sources use the same API. `instances/ensure` is idempotent: stopped → restore, paused → resume, running → no-op.

---

## 7. Hybrid Hot-Start Policy

Combines two mechanisms:

1. **Pause/Resume** for ultra-fast returns within a short idle window (RAM retained)
2. **Snapshot/Restore** for longer idle (RAM freed)

### 7.1 Default Timeouts (Tunable)

| Parameter | Default | Notes |
|---|---|---|
| `pauseAfterIdle` | 60s | After last request/session end, pause VM (RAM retained, instant resume) |
| `terminateAfterIdle` | 20m | After extended idle, terminate VM; restore from snapshot on next use |
| `taskPauseWindow` | 30s | Short window after task completion to allow follow-up actions |
| `maxRuntimeTask` | 15m | Hard timeout for a task run (per spec) |
| `cachedInstanceTTL` | 2h | If cached-resume policy is enabled |

### 7.2 VM Warm Pool

Aegis maintains a pool of pre-booted VMs (restored from base snapshot, harness ready, waiting for task assignment). When a kit requests `instances/ensure`, the pool provides an instant VM instead of cold-booting.

```yaml
# Kit manifest
policies:
  warm_pool: 3          # Keep N pre-booted VMs in standby (0 = disabled)
```

- Pool VMs are generic (base snapshot only) — kit-specific setup (workspace mount, secrets, network group) happens at assignment time
- Pool size is configurable per-kit with a platform-wide maximum
- Pool is replenished asynchronously after VMs are claimed
- Under memory pressure, pool is drained before running VMs are terminated

### 7.3 Memory Pressure Behavior

- If host memory pressure is high, aegisd may skip pause and terminate immediately (restore later)
- Release snapshots remain durable; base snapshot is rebuildable
- Prefer terminate over pause under pressure

---

## 8. Networking

### 8.1 VM Network Model

Each VM gets a TAP interface and a private IP (recommended) or an ephemeral host-port mapping (simpler for v0).

### 8.2 Egress Policy

**Default: allow internet egress.** Agents need external APIs (AI providers, web services, MCPs).

Egress can optionally be restricted via:

- DNS allowlist
- HTTP proxy allowlist
- Per-kit or per-run configuration

Final authority: Aegis. Kits can declare defaults; Aegis enforces.

### 8.3 Ingress Policy

**Inbound: deny by default.** Only the Aegis router can reach VMs.

- Host firewall blocks VM IPs from LAN
- Router is the sole ingress path for external traffic
- Kits cannot open additional inbound paths from outside

### 8.4 Inter-VM Networking

**Default: disabled.** VMs are fully isolated from each other.

When a kit declares `inter_vm: true`, Aegis places instances in the same network group on a shared bridge/subnet:

- Each VM gets a stable hostname (`agent-{instanceId}.session.local`) resolvable by peers
- Only VMs in the same network group can reach each other
- Does not open inbound from outside the group — router rule still applies
- Kit declares the policy; Aegis enforces the network topology

```yaml
# Kit manifest
networking:
  inter_vm: true        # false by default
```

Network groups are scoped by a kit-provided group identifier (e.g., session ID). Aegis creates and tears down the bridge when the first/last VM in the group starts/stops.

### 8.5 Kit Network Configuration

Kits declare additional internal hosts the VM needs to reach:

```yaml
# Example: Famiglia kit network config
networking:
  egress: allow
  internal_hosts:
    - famiglia-web.internal:3000    # Agent Data API
    - ejabberd.internal:5222        # XMPP (long-lived TCP)
  long_lived_connections: true      # Don't timeout TCP connections
```

Aegis configures the VM's network to allow these routes. The VM maintains its own connections — Aegis does not manage application-level protocols.

### 8.6 DNS

Host-provided resolver, optionally through a policy-enforcing DNS proxy.

---

## 9. Persistence and Artifacts

### 9.1 Workspace Volumes

Each app/workspace gets a persistent volume mounted into the VM:

- Code, build outputs, user artifacts
- Survives VM termination (attached to AppID, not InstanceID)
- Scoped to a workspace for user data (separate from published releases)

#### Workspace Modes

| Mode | Description | Use case |
|---|---|---|
| `isolated` (default) | One volume per instance. No sharing. | Single-agent workloads (Famiglia) |
| `shared` | One named volume mounted into multiple instances. | Multi-agent workloads sharing a codebase (OpenClaw) |

When `mode: shared`, Aegis creates a single named volume and bind-mounts it into all instances that declare the same workspace group. File-level coordination (locking, merge conflicts) is the kit's responsibility — Aegis provides the shared mount.

```yaml
# Kit manifest declares workspace mode
workspace:
  mode: shared          # or "isolated"
```

**Critical invariant:** Workspace volumes are **never** part of release overlays or any snapshot tier. The workspace is mutable, the release overlay is immutable — they are separate disk layers with separate lifecycles. Terminate/snapshot must never capture workspace state into the release overlay or cached instance snapshot. This is especially important with shared workspaces where multiple VMs are writing concurrently.

### 9.2 Artifact Capture

Task execution can produce artifacts (logs, built bundles, export files). aegisd captures these and stores them in configured storage (local filesystem, S3-compatible, etc.).

### 9.3 No Reliance on VM Memory

VM memory is ephemeral beyond the pause window. All durable state must be on the workspace volume or captured as artifacts.

---

## 10. Snapshot Tiers, Storage, and Garbage Collection

### 10.1 Snapshot Tiers

| Tier | Description | Retention |
|---|---|---|
| **Base snapshot** | OS + harness + warmed runtimes. Versioned by `BaseRevision`. | Rebuildable, keep current + previous |
| **Release artifact** | Published output of a build (static bundle, server code, assets, metadata). | Durable, subject to GC |
| **Release disk overlay** | COW overlay disk (dm-snapshot) referencing shared base rootfs, containing only release-specific files. | Durable, subject to GC |
| **Cached instance snapshot** | Optional per-app cached state for ultra-fast resume. | Ephemeral, TTL-based (hours), deleted under pressure |

### 10.2 Disk Layering Model

No full VM snapshots per publish. Instead, persist a base rootfs and per-version overlays:

```
base_rootfs         ← Immutable, shared by all apps
  └── release_overlay  ← COW overlay (dm-snapshot), immutable delta for ReleaseID
workspace_volume    ← Mutable, separate mount, NEVER part of any overlay or snapshot
```

These are separate disk layers with separate lifecycles. The workspace volume is mounted alongside the rootfs stack, not inside it. Publish, snapshot, and GC operations never touch the workspace.

### 10.3 Retention and GC Rules

- **Always keep:** (a) current published release per app; (b) releases referenced by pinned links, deployments, or audit policies
- **Keep last N versions** (default N=20) or last X days (default 90d), whichever is larger; configurable per workspace
- **Delete eligible versions** by removing: (1) version artifact, (2) version overlay, (3) cached-instance snapshots for that version
- **Deleted versions** return 404 (or tombstone) even if older cached data exists
- **GC is safe** because all overlays are referenced by an explicit `snapshot_index`; no reference means deletable

### 10.4 Compatibility and Upgrade Strategy

- Include `BaseRevision` in every `Release` record
- A `Release` may only be restored with a compatible base (same `BaseRevision`)
- Upgrading the base creates a new `BaseRevision`; apps can be rebuilt (new release) against it
- Cached instance snapshots carry a short TTL and are invalidated on base upgrade

---

## 11. Secret and Environment Variable Injection

### 11.1 Platform Responsibility

Aegis owns the mechanism for injecting secrets and environment variables into VMs:

- **How** secrets get into the VM (vsock channel, encrypted volume, etc.)
- **Isolation** between runs and between apps
- **Lifecycle** (rotation, revocation)
- **Per-run scoping** (a secret injected for one task is not visible to the next)

### 11.2 Kit Declaration

Kits declare what secrets they need. They do not control how injection works:

```yaml
# Example: Famiglia kit secret requirements
required_secrets:
  - name: FAMIGLIA_API_KEY
    description: "Scoped API key for Agent Data API"
    scope: per_app
  - name: XMPP_PASSWORD
    description: "Auto-generated XMPP credentials"
    scope: per_app
  - name: ANTHROPIC_API_KEY
    description: "User-provided AI API key"
    scope: per_workspace
    user_provided: true
```

### 11.3 Injection Model

Secrets are injected as environment variables inside the VM at boot/restore time. They are:

- Never written to disk inside the VM
- Not included in snapshots
- Re-injected on every restore
- Available only to the agent process (not to arbitrary processes in the VM)

---

## 12. Router

The router is a **connection-aware VM lifecycle proxy**. It accepts connections on any exposed port, wakes the target VM if needed, proxies the connection, and tracks activity for idle-based hibernation. It is the mechanism that makes scale-to-zero work.

### 12.1 Aegis Owns the Router

The router is a core platform component. If kits could replace it:

- Snapshot/lifecycle integration breaks (router triggers pause/restore)
- Scale-to-zero semantics become ambiguous
- Kit complexity explodes

### 12.2 What the Router Does

- **TCP/HTTP/WebSocket/gRPC reverse proxy** — protocol-aware where beneficial, opaque TCP fallback for everything else
- **Instance resolution** — maps `(instanceId, port)` to VM endpoint
- **Wake-on-connect** — connection arrives for stopped/paused VM → calls `instances/ensure` → VM wakes → proxy connection
- **Activity tracking** — every connection/request resets the idle timer; last connection close starts the idle countdown
- **HTTP loading page** — for HTTP ports, optionally returns a brief loading response while VM wakes (configurable per-kit)
- **Health checks** — verifies VM is ready to receive traffic after wake

### 12.3 Kit Configures the Router

Kits define routing policy, not routing implementation:

| Kit provides | Example |
|---|---|
| URL/port mapping scheme | `/agent/{agentId}/...` on port 80 |
| Path prefix | `/famiglia/`, `/openclaw/` |
| Session stickiness | Sticky by SessionID, or round-robin |
| Loading page | Custom HTML shown during VM wake (HTTP only) |
| Exposed ports | Which ports to proxy and at what protocol level |

### 12.4 Connection Flow

```
Connection arrives (any protocol, any exposed port)
    │
    ▼
Router resolves: which instance owns this port?
    │
    ├── Check instance state:
    │     TERMINATED? → restore from disk layers (~1s)
    │     PAUSED?     → resume (~ms)
    │     RUNNING?    → proceed
    │
    ├── Wait for readiness (harness health check)
    │
    ├── Proxy connection to VM endpoint
    │     HTTP:  forward request, return response
    │     TCP:   forward bytes bidirectionally
    │     WS:    upgrade and forward frames
    │
    ├── Track connection as activity (reset idle timer)
    │
    └── On connection close:
          Last connection? → start idle timer
          Timer expires   → pause → later terminate
```

### 12.5 Protocol-Specific Behavior

| Protocol | During VM wake | Proxy mode | Notes |
|---|---|---|---|
| HTTP | Return 503 with `Retry-After` header, or loading page | Request/response | Path-based routing supported |
| WebSocket | Hold upgrade until VM ready | Bidirectional frames | Connection stays open, counts as activity |
| TCP (raw) | Hold connection until VM ready | Byte stream | Client TCP timeout (~30s) provides ample wake window |
| gRPC | Queue request until VM ready | HTTP/2 frames | Deadline propagated to VM |

---

## 13. APIs

### 13.1 aegisd HTTP API (Host)

All endpoints are local (unix socket or `127.0.0.1`) and authenticated via host token. JSON payloads.

#### Tasks

| Method | Path | Purpose | Key Fields |
|---|---|---|---|
| `POST` | `/v1/tasks` | Run a task in a sandbox | `appId?`, `releaseId?`, `imageRef`, `cmd/spec`, `timeouts`, `policy`, `secrets`, `kit?` |
| `GET` | `/v1/tasks/{id}` | Task status + metadata | `state`, `startedAt`, `endedAt`, `exitCode` |
| `GET` | `/v1/tasks/{id}/logs` | Stream task logs | `follow=true` |
| `GET` | `/v1/tasks/{id}/artifacts` | List/download artifacts | `paths`, `sizes`, `mime` |

#### Apps

| Method | Path | Purpose | Key Fields |
|---|---|---|---|
| `POST` | `/v1/apps/{appId}/publish` | Build + publish a new release | `sourceRef`, `buildSpec`, `releaseLabel` |
| `GET` | `/v1/apps/{appId}` | Resolve current release + snapshot | `currentReleaseId`, `snapshotRef` |

#### Instances

| Method | Path | Purpose | Key Fields |
|---|---|---|---|
| `POST` | `/v1/instances/ensure` | Ensure an instance exists for serving | `appId`, `releaseId`, `reason=connection\|warm\|event`, `expose?` |
| `GET` | `/v1/instances/{id}` | Instance state + endpoint | `state`, `endpoint`, `lastActiveAt` |
| `POST` | `/v1/instances/{id}/pause` | Pause instance (RAM retained) | `reason` |
| `POST` | `/v1/instances/{id}/terminate` | Terminate instance (RAM freed) | `reason` |

#### Kits

| Method | Path | Purpose | Key Fields |
|---|---|---|---|
| `POST` | `/v1/kits/register` | Register a kit | `name`, `config`, `image`, `secrets` |
| `GET` | `/v1/kits` | List registered kits | |
| `GET` | `/v1/kits/{name}` | Kit details | `name`, `config`, `status` |

### 13.2 Router Integration API

Router calls aegisd to resolve and ensure instances, then proxies traffic.

| Operation | Input | Output |
|---|---|---|
| Resolve instance | `instanceId` or `appId` | `instanceId`, `endpoint`, `state`, `exposedPorts` |
| Ensure instance | `instanceId`, `reason=connection\|warm\|event` | `instanceId`, `endpoint`, `state` |
| Report activity | `instanceId`, `port`, `connectionCount`, `lastSeenAt` | `ack` |
| Report idle | `instanceId` (last connection closed) | `ack` (triggers idle timer) |

### 13.3 Guest Harness Vsock RPC (Inside VM)

Binary or JSON RPC over vsock. Minimum commands:

| Command | Description |
|---|---|
| `runTask(spec)` | Execute task, stream logs, return exit status + artifact manifest |
| `startServer(serverSpec)` | Start server process, return listening ports + readiness probe info. Supports multiple ports/protocols. |
| `shutdown()` | Graceful stop |
| `health()` | Status check — returns per-port readiness |
| `injectSecrets(secrets)` | Inject secrets into agent environment |
| `listPorts()` | Return currently listening ports and their protocols |

---

## 14. Execution Policies

Execution "modes" describe what the user wants (task vs serve). Execution "policies" describe lifecycle, persistence, and security choices. Policies are orthogonal and selectable per run/build.

### 14.1 Policy Presets

| Policy | Purpose | Lifecycle |
|---|---|---|
| `stateless` | Fire-and-forget tasks | Restore from base (or warm pool), run task, collect artifacts, destroy. No per-run snapshots. |
| `build` | Publish a new version | Restore base, build app, produce version artifact + disk overlay only. **No memory snapshot persisted.** Optionally preflight server, then record snapshot_index. |
| `serve` | Expose ports, wake on connect | Ensure singleton instance; restore from overlay + base; expose ports via router; pause after short idle; terminate after longer idle. Scale-to-zero by default. |
| `long-running` | Autonomous agent tasks | Extended `maxRuntimeTask` (4h default). Pause-on-idle instead of terminate (agent may be waiting for peer input). Requires heartbeat — agent must report progress every N minutes or gets terminated as stale. |
| `keep-alive` | Always-on background services | VM stays running regardless of connection activity. For cron workers, stream consumers, background processors. The exception, not the default. |
| `cached-resume` | Accelerator for active instances | Keep paused instance for short window. If backend supports memory snapshots, optionally save instance state with TTL for faster restores. If not, degrades to boot from disk layers (still correct, slightly slower). |

### 14.2 Policy in Task Spec

```json
{
  "policy": "stateless",
  "cpu_millis": 2000,
  "mem_mb": 512,
  "network": { "egress": "allow" },
  "command": ["bash", "-lc", "python main.py"],
  "artifacts": { "capture": true },
  "kit": "famiglia",
  "secrets": ["FAMIGLIA_API_KEY", "ANTHROPIC_API_KEY"]
}
```

---

## 15. Lifecycle State Machines

### 15.1 Instance Lifecycle

```
STOPPED ──[ensureInstance]──► RESTORING ──[ready]──► RUNNING
                                                       │
                                          ┌────────────┤
                                          │            │
                                   [idle >= pause]  [memoryPressure]
                                          │            │
                                          ▼            │
                                       PAUSED          │
                                          │            │
                               ┌──────────┤            │
                               │          │            │
                          [request]  [idle >= term     │
                               │    OR pressure]       │
                               │          │            │
                               ▼          ▼            ▼
                            RUNNING    TERMINATED ◄────┘
```

| From | Event | To / Action |
|---|---|---|
| `STOPPED` | `ensureInstance` | `RESTORING` (restore from release snapshot) |
| `RESTORING` | `ready` | `RUNNING` (endpoint registered) |
| `RUNNING` | `idle >= pauseAfterIdle` | `PAUSED` (Firecracker pause) |
| `PAUSED` | request arrives | `RUNNING` (resume) |
| `PAUSED` | `idle >= terminateAfterIdle` OR `memoryPressure` | `TERMINATED` (kill VM; disk layers are canonical, RAM is discarded; optionally write cached instance snapshot with TTL) |
| `RUNNING` | `memoryPressure` | `TERMINATED` (prefer terminate over pause; next restore uses disk layers) |

**Snapshot rule applied:** Termination discards VM memory. Next boot restores from disk layers (base rootfs + release overlay). Cached instance snapshots are an optional acceleration cache with TTL — never the source of truth.

### 15.2 Task Lifecycle

| State | Meaning | Terminal? |
|---|---|---|
| `QUEUED` | Awaiting VM allocation/restore | No |
| `RUNNING` | Guest executing | No |
| `SUCCEEDED` | Exit code 0 | Yes |
| `FAILED` | Non-zero exit or harness error | Yes |
| `TIMED_OUT` | Exceeded `maxRuntimeTask` | Yes |
| `CANCELLED` | User/system cancelled | Yes |

---

## 16. Security and Limits

### 16.1 Baseline

- **cgroups v2** limits per VM: CPU shares/quotas, memory max, pids max, IO throttling
- **Egress** allowed, optionally policy-enforced (DNS allowlist, HTTP proxy allowlist)
- **No privileged devices**; minimal virtio set
- **No host filesystem mounts** beyond workspace volume
- **All ingress via router only**; host firewall blocks VM IPs from LAN
- **Secrets never on disk** inside VMs; never included in any snapshot tier (base, release, or cached instance). Re-injected on every boot/restore via vsock.

### 16.2 Default Resource Limits

| Resource | Default | Configurable |
|---|---|---|
| Memory | 512 MB | Yes (max 4 GB) |
| CPU | 1 vCPU | Yes (max 4 vCPU) |
| Disk (workspace) | 1 GB | Yes |
| Max concurrent VMs | 10 | Yes |
| Task timeout | 15 min | Yes (max 60 min) |

---

## 17. macOS Support

### 17.1 The Problem

Firecracker requires Linux with KVM. Running it on macOS via nested virtualization (KVM inside a Lima/VZ guest) is not viable:

| Chip | Nested Virt | Status |
|---|---|---|
| M1 | Not supported | Hardware limitation |
| M2 | Partial | Unreliable |
| M3 | Reported working | Unverified by Firecracker team |
| M4 | Broken | Lima issue #4498, open |

The Firecracker team: *"We do not plan to support macOS."* (issue #2845, closed NOT_PLANNED). Nested virt is a non-starter.

### 17.2 Solution: libkrun

**libkrun** (containers/libkrun project) creates lightweight microVMs using Apple's Hypervisor.framework **directly** — no nested virtualization, no KVM, no intermediary Linux VM. It incorporates code from Firecracker and rust-vmm, shares the same architectural DNA, and is already integrated into Lima as the `krunkit` VM backend (Lima 2.0+).

Two VMM backends, no fallback chain:

| Backend | Platform | Hypervisor | Status |
|---|---|---|---|
| **Firecracker** | Linux | KVM | Production-grade |
| **libkrun** | macOS (Apple Silicon) | Apple HVF | Experimental, actively developed |

### 17.3 User Experience

```bash
brew install aegis
aegis up                    # Detects platform, starts aegisd with correct VMM
aegis run ...               # Same commands on Linux and macOS
aegis down                  # Stops aegisd
```

`aegis up` detects the platform:

1. **Linux with KVM**: Firecracker backend.
2. **macOS with Apple Silicon**: libkrun backend.
3. **Neither**: Print clear error with instructions to attach a remote Linux target via SSH. No dead end.

### 17.4 Architecture: macOS

```
macOS Host (Apple Silicon)
  └── aegis CLI
        └── aegisd (runs natively on macOS)
              └── libkrun (Apple Hypervisor.framework)
                    └── Agent microVMs (ARM64 Linux guests)
```

No Lima. No nested virtualization. Native HVF.

### 17.5 Architecture: Linux

```
Linux Host (ARM64 or x86_64)
  └── aegisd (native)
        └── Firecracker (KVM)
              └── Agent microVMs
```

### 17.6 VMM Abstraction in aegisd

aegisd defines a VMM interface with explicit capability reporting:

```go
type VMM interface {
    CreateVM(config VMConfig) (Instance, error)
    StartVM(id string) error
    PauseVM(id string) error
    ResumeVM(id string) error
    StopVM(id string) error
    Snapshot(id string, path string) error     // May return ErrNotSupported
    Restore(snapshotPath string) (Instance, error) // May return ErrNotSupported
    Capabilities() BackendCaps
}

type BackendCaps struct {
    Pause           bool   // Pause/resume with RAM retained
    SnapshotRestore bool   // Save/restore full VM memory to disk
    Name            string // "firecracker" or "libkrun"
}
```

Two implementations: `FirecrackerVMM` and `LibkrunVMM`. Selected at aegisd startup based on platform detection. The rest of aegisd (lifecycle, snapshots, router, registry, kit system) is VMM-agnostic.

### 17.7 Backend Capability Profiles

| Capability | Firecracker (Linux) | libkrun (macOS) |
|---|---|---|
| Pause / Resume (RAM retained) | Yes | Yes (HVF VM pause) |
| Memory snapshot to disk | Yes | Not yet — treat as future |
| Restore from memory snapshot | Yes | Not yet |
| Boot from disk layers | Yes | Yes |

The canonical rule holds on both backends:

- **Short idle**: Pause (instant resume). Both backends support this.
- **Long idle**: Terminate, restart from disk layers. Both backends support this.
- **cached-resume policy**: On Firecracker, uses memory snapshot for faster-than-boot restores. On libkrun, gracefully degrades to boot from disk layers. Still fast enough for dev.

Policies compile into backend operations based on capabilities — never assume a capability that isn't reported.

### 17.8 Why libkrun

- Built on rust-vmm crates (same foundation as Firecracker)
- Active development by Red Hat / containers team
- Already integrated into Lima (`krunkit` backend) and Podman ecosystems
- Native macOS ARM64 support is a primary design goal
- GPU passthrough via Vulkan (future benefit for ML agents)

**Risk mitigation:** The VMM interface means we can swap backends without touching aegisd core. If a better macOS VMM appears, we add a third implementation.

---

## 18. Backend Conformance Test Suite

Every VMM backend must pass the same conformance tests. This is how we enforce the rule that **semantics are backend-independent**.

### 18.1 Required Tests (Must Pass on All Backends)

| Test | What it verifies |
|---|---|
| `task_run_artifacts` | Run a task, capture stdout/stderr + artifacts. Verify correct output. |
| `serve_request` | Expose port, send request to VM, verify response from VM. |
| `pause_on_idle` | After idle timeout, VM is paused. Resume on next request. Verify sub-second response. |
| `terminate_on_long_idle` | After long idle, VM is terminated. Verify it restarts correctly from disk layers. |
| `disk_layer_restore` | Terminate a VM. Boot fresh from base + release overlay. Verify app serves correctly. |
| `secret_injection` | Inject secrets, verify they're available inside VM as env vars. |
| `secret_not_on_disk` | After secret injection, verify no secret material exists on disk inside the VM or in any snapshot artifact. |
| `secret_reinjection` | Terminate and restore a VM. Verify secrets are re-injected and available without prior state. |
| `workspace_persistence` | Write to workspace volume, terminate VM, boot fresh. Verify workspace data survives. |
| `resource_limits` | Exceed memory/CPU limits inside VM. Verify enforcement (OOM kill, throttle). |
| `egress_works` | From inside VM, make HTTP request to external host. Verify success. |
| `ingress_blocked` | Attempt direct connection to VM IP from host (not via router). Verify rejection. |

### 18.2 Capability-Gated Tests (Only Run If Backend Reports Capability)

| Test | Capability | What it verifies |
|---|---|---|
| `memory_snapshot_restore` | `SnapshotRestore` | Snapshot a running VM to disk, terminate, restore from snapshot. Verify VM state is intact. |
| `cached_resume_acceleration` | `SnapshotRestore` | Verify cached-resume policy uses memory snapshot when available and that restore is faster than cold boot. |

If a backend doesn't report the capability, these tests are skipped — not failed. The required tests above guarantee correctness regardless.

### 18.3 Running Conformance

```bash
aegis test conformance                          # Run against active backend
aegis test conformance --backend=firecracker    # Explicit backend
aegis test conformance --backend=libkrun
```

---

## 19. CLI Reference

### 19.1 Platform Commands

```bash
aegis up                              # Start aegisd (auto-detects VMM backend)
aegis down                            # Stop aegisd
aegis status                          # Running VMs, resource usage
aegis doctor                          # Print backend + capability matrix
```

`aegis doctor` output example:

```
Aegis v2.0.0
Platform:    darwin/arm64
Backend:     libkrun (Apple Hypervisor.framework)
Capabilities:
  Pause/Resume:          yes
  Memory Snapshots:      no (restart from disk layers on long idle)
  Boot from disk layers: yes
Status:      ready
```

### 19.2 Kit-less Execution

```bash
aegis run --image base:python -- python main.py
aegis run --image base:node -- node agent.js
aegis run --image base:python --expose 8080 -- python server.py
aegis run --image base:python --expose 80:http --expose 5432:tcp -- python app.py
```

All exposed ports get wake-on-connect and scale-to-zero automatically.

### 19.3 Kit Management

```bash
aegis kit install famiglia            # Install Famiglia kit
aegis kit install openclaw            # Install OpenClaw kit
aegis kit list                        # List installed kits
aegis kit info famiglia               # Kit details
```

### 19.4 App Management

```bash
aegis app list                        # List apps
aegis app publish <appId>             # Publish new release
aegis app serve <appId>               # Start serving (wake-on-connect)
aegis app releases <appId>            # List releases
aegis app info <appId>                # App details, current release, exposed ports
```

### 19.5 Task Management

```bash
aegis task run <spec.json>            # Run a task
aegis task status <taskId>            # Task status
aegis task logs <taskId> --follow     # Stream logs
aegis task artifacts <taskId>         # List/download artifacts
```

---

## 20. Naming Conventions

| Component | Name |
|---|---|
| Platform | Aegis |
| Daemon | `aegisd` |
| CLI | `aegis` |
| Kit packages | `aegis-kit-famiglia`, `aegis-kit-openclaw` |
| Kit manifest file | `aegis-kit.yaml` |

---

## 21. Implementation Notes (v1)

- Single-host deployment first; design registry keys to support future multi-host (`hostId` field)
- Prefer unix socket for aegisd API to avoid accidental exposure
- VMM abstraction interface must be in place from day one — do not hardcode Firecracker
- Use `firecracker-go-sdk` for Linux VMM backend; evaluate libkrun Go/C bindings for macOS backend
- Keep snapshots versioned by `BaseRevision` to avoid restore incompatibilities
- macOS support (libkrun) is a first-class concern, not an afterthought
- aegisd written in Go — single static binary, no runtime dependencies
- Registry is SQLite — no external database needed for Aegis core

---

## 22. Open Questions

1. **Multi-host** — when (not if) do we need to support multiple hosts? How does the registry federation work?
2. **GPU passthrough** — needed for ML inference agents. Firecracker doesn't support GPU; libkrun has Vulkan passthrough — potential advantage on macOS.
3. **WebSocket support** — router needs to proxy WebSocket connections for interactive apps. Standard reverse proxy concern but needs explicit support.
4. **Kit versioning** — how do kit upgrades interact with existing apps/snapshots?
5. **libkrun pause parity** — does libkrun support pause/resume at the same level as Firecracker? Memory snapshot/restore is an optional accelerator (correctness never depends on it), but pause/resume is required for the short-idle fast path.
6. **Shared volume consistency** — when multiple VMs write to a shared workspace, do we need file locking at the Aegis level or leave it entirely to kits?
7. **Microsandbox collaboration** — microsandbox (zerocore-ai) already wraps libkrun for agent sandboxing. Use as component, fork, or build independently?

---

## Related Documents

- [FAMIGLIA_KIT_SPEC.md](FAMIGLIA_KIT_SPEC.md) — Famiglia kit specification
- [OPENCLAW_KIT_SPEC.md](OPENCLAW_KIT_SPEC.md) — OpenClaw kit specification
- Firecracker: https://github.com/firecracker-microvm/firecracker
- libkrun: https://github.com/containers/libkrun
- Microsandbox: https://github.com/zerocore-ai/microsandbox
