# Aegis Kit Boundary Spec
## Responsibility Split Between Core and Kits

**Version:** v1.0
**Date:** 2026-02-19
**Depends on:** [aegis_architectural_pivot_spec.md](aegis_architectural_pivot_spec.md)

---

## 1. The Boundary

Aegis is a runtime. Kits are integration layers.

The boundary is defined by one rule: **if it requires knowledge of what the agent does, it belongs in a kit.** Aegis knows how to run VMs, route traffic, and manage lifecycle. It does not know what a "canvas" is, what "v2" means, or whether a server is "ready."

---

## 2. Aegis Core Owns

These are platform primitives. Kits cannot replace, bypass, or override them.

### Runtime

| Capability | Description |
|-----------|-------------|
| Instance lifecycle | Create, start, pause, resume, stop, delete |
| VM isolation | Hypervisor boundary (Firecracker/libkrun) |
| Resource limits | CPU, memory, disk per instance |
| ControlChannel | JSON-RPC 2.0 between host and guest harness |
| Demuxer | Persistent recv loop, RPC call/response, notification routing |

### Storage

| Capability | Description |
|-----------|-------------|
| OCI image pull + cache | Digest-keyed, platform-resolved (linux/arm64) |
| Workspace volumes | Per-instance persistent storage, survives VM restarts |
| Snapshot/restore | Optional, backend-dependent |

### Networking

| Capability | Description |
|-----------|-------------|
| Ingress routing | `exposes`-based proxy, resume-on-ingress |
| Egress | VM outbound internet access |
| Port mapping | Guest port → host port allocation |

### Observability

| Capability | Description |
|-----------|-------------|
| Log capture | Per-instance ring buffer + NDJSON persistence |
| Log sources | `server`, `exec`, `system` tagging |
| Log streaming | Follow, since, tail, exec_id filtering |
| Exec | Run commands in running instances, stream output |

### Security

| Capability | Description |
|-----------|-------------|
| Secret encryption | AES-256-GCM at rest |
| Secret injection | Environment variables at process start |
| Trust boundary | Host trusted, guest untrusted |

---

## 3. Kits Own

These are application-specific concerns. Aegis core does not implement them.

### Serving Semantics

| Capability | Why kit, not core |
|-----------|-------------------|
| Readiness detection | Different apps have different readiness signals (HTTP 200, TCP accept, custom health endpoint) |
| Readiness gating | Whether to hold traffic until ready is an application decision |
| Process supervision | Restart-on-crash, graceful reload — application-level |
| Multiple processes | Some agents run server + worker — kit orchestrates |
| Hot reload | Replace running code without restart — kit-specific |

### Application Versioning

| Capability | Why kit, not core |
|-----------|-------------------|
| Version identifiers | v1, v2, sha, tag, label — domain-specific |
| Publishing/promotion | draft → staging → live — workflow-specific |
| Rollback | Which version to roll back to — requires version history |
| Release artifacts | What constitutes a "release" differs per kit |
| Migration | Schema changes, data transforms — application-specific |

**Core invariant:** Aegis does not implement application versioning. Versioning, publishing, and artifact promotion are responsibilities of kits or external orchestration layers.

### Routing Semantics

| Capability | Why kit, not core |
|-----------|-------------------|
| URL scheme | `/agent/{id}/canvas`, `/session/{id}/...` — domain-specific |
| Session stickiness | Whether requests from the same user go to the same instance |
| Canvas routing | Multiple canvases per agent — Famiglia-specific |
| Path rewriting | Strip/add prefixes — application-specific |

### Agent Integration

| Capability | Why kit, not core |
|-----------|-------------------|
| Chat protocols | XMPP, WebSocket, proprietary — per-platform |
| Data APIs | REST endpoints, GraphQL — per-platform |
| Multi-agent orchestration | Coordinator patterns, swarm management — per-kit |
| Authentication | Token generation, validation — per-platform |

---

## 4. The Interface Between Core and Kit

Kits interact with Aegis through these mechanisms only:

### 4.1 Instance API

Kits create and manage instances via the standard API:

```
POST   /v1/instances           Start instance from imageRef
DELETE /v1/instances/{id}      Stop instance
POST   /v1/instances/{id}/exec Execute command
GET    /v1/instances/{id}/logs Stream logs
```

### 4.2 Harness SDK

Kits include an SDK inside the guest VM that:

- Reads secrets from environment variables
- Writes structured logs to stdout/stderr
- Manages the application process lifecycle
- Implements readiness detection (if needed)
- Handles graceful shutdown on SIGTERM

The SDK runs as a child process of the harness, not as a replacement for it. The harness is always PID 1.

### 4.3 Handle Mapping

Kits use `handle` as a stable alias for instances:

```
aegis instance start agent-base:latest --name my-agent --expose 80 -- python server.py
```

The kit can then reference `my-agent` instead of `inst-1771509275407527000`. Handle → instanceId mapping is managed by Aegis core. Kits own the naming convention.

### 4.4 Image References

Kits define their own images:

```yaml
# Kit provides a base image
image: ghcr.io/famiglia/agent-base:latest

# Kit may layer on top of Aegis base
image: ghcr.io/aegis/agent-base-python:latest
```

Aegis pulls and caches the image. The kit owns what's inside it.

### 4.5 Secret Declaration

Kits declare required secrets in their manifest:

```yaml
secrets:
  required:
    - name: ANTHROPIC_API_KEY
      scope: per_workspace
    - name: XMPP_PASSWORD
      scope: per_instance
      generated: true
```

Aegis stores, encrypts, and injects them. The kit defines what they mean.

---

## 5. What This Avoids

If versioning stays in core:
- `appId` and `releaseId` reappear
- `publish` and `serve` return as core concepts
- Registry grows (apps, releases, GC complexity)
- Core becomes opinionated about workflows
- Different kits fight over what "publish" means

If readiness stays in core:
- `startServer` / `serverReady` / `serverFailed` reappear
- Core must understand health check protocols
- Core must decide when to route traffic
- Different kits need different readiness signals
- Core becomes a serving framework, not a runtime

If routing semantics stay in core:
- URL schemes become core configuration
- Session stickiness becomes a core feature
- Path rewriting becomes core complexity
- Core must understand application topology

---

## 6. Kit Examples

### How Famiglia Uses Aegis

```
Famiglia kit:
  1. Creates instance: POST /v1/instances {imageRef: "famiglia-agent:latest", exposes: [{port: 80}]}
  2. Injects secrets: XMPP_USER, XMPP_PASSWORD, FAMIGLIA_API_KEY
  3. SDK inside VM: connects to XMPP, starts canvas server on :80
  4. Famiglia manages "canvas revisions" (versioning) in its own database
  5. On "publish new canvas": stops old instance, starts new with updated imageRef
  6. Readiness: SDK pings localhost:80 before marking ready in Famiglia's UI
```

Aegis sees: an instance running an image, exposing port 80. That's all.

### How OpenClaw Uses Aegis

```
OpenClaw kit:
  1. Creates N instances for a session (coordinator + workers)
  2. All share a workspace volume
  3. Coordinator manages agent lifecycle, code checkout, task assignment
  4. OpenClaw manages "sessions" and "task history" in its own database
  5. No "publish" concept — agents are ephemeral per session
  6. Readiness: coordinator polls worker health endpoints
```

Aegis sees: N instances with shared workspace. That's all.

### How a Generic Agent Uses Aegis (No Kit)

```
User:
  1. aegis run --expose 80 -- python3 server.py
  2. Done. No kit, no SDK, no publish.
  3. Server runs, traffic routes, scale-to-zero works.
  4. User manages versioning by stopping and starting with different code.
```

Aegis sees: an instance exposing port 80. That's all.

---

## 7. Invariants

1. **Aegis does not implement application versioning.** Versioning, publishing, and artifact promotion are responsibilities of kits or external orchestration layers.

2. **Aegis does not implement readiness detection.** Whether a server is "ready" is determined by the guest process or kit SDK, not by the platform.

3. **Aegis does not implement application routing semantics.** URL schemes, session stickiness, and path rewriting are kit concerns. Aegis routes based on `exposes` only.

4. **Kits cannot replace platform primitives.** A kit cannot provide its own VM backend, its own router, or its own secret storage. It uses what Aegis provides.

5. **Aegis is usable without any kit.** `aegis run -- echo hello` works. No kit installation, no SDK, no manifest.

---

## 8. Summary

```
┌─────────────────────────────────────────────────────┐
│                    Kit Layer                         │
│  Versioning, readiness, routing schemes, protocols,  │
│  orchestration, UI, session management               │
├─────────────────────────────────────────────────────┤
│                  Aegis Core                          │
│  VM lifecycle, ingress mapping, control channel,     │
│  logs, exec, secrets, workspace, image cache         │
├─────────────────────────────────────────────────────┤
│                  VMM Backend                         │
│  Firecracker (Linux/KVM) or libkrun (macOS/HVF)     │
└─────────────────────────────────────────────────────┘
```

The boundary is clean when you can describe what Aegis does without mentioning any application domain concept. If the description requires words like "canvas", "release", "ready", or "version" — it's a kit concern, not a core concern.
