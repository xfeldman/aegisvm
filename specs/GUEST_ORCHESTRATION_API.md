# Guest Orchestration API

**Allows guest processes to spawn and manage Aegis instances from inside a VM.**

**Status:** Design spec
**Date:** 2026-02-21
**Depends on:** [AEGIS_v3_PLATFORM_SPEC.md](AEGIS_v3_PLATFORM_SPEC.md), [KIT_BOUNDARY_SPEC.md](KIT_BOUNDARY_SPEC.md)

---

## 1. Problem

Agent workloads need to orchestrate infrastructure. A Telegram bot receives "build me a website" and needs to spawn a work instance with the right image, workspace, and exposed ports. Today this requires calling aegisd from the host — the guest has no way to manage instances.

## 2. Core Principle

**Guests don't talk to aegisd directly.** They talk to a guest-side broker (built into the harness) which enforces a capability policy, then forwards allowed requests to aegisd over the existing control channel.

This preserves:
- Per-instance scoping (guest only sees its own children)
- Safe spawn controls (resource ceilings, image allowlists)
- Auditability (all requests flow through the harness → aegisd)
- Future multi-tenant story

## 3. Harness as Permanent Control Plane

The harness is PID 1 and the long-lived root process of the VM. It never exits voluntarily — if the primary process crashes, the harness stays alive (it already does today: it sends `processExited` notification and waits). The guest API server runs as a goroutine inside the harness, so it's available for the entire VM lifetime.

If the harness itself crashes, the VM is effectively dead (PID 1 exit = kernel panic). This is acceptable — there's nothing to recover to. The host detects the vmm-worker exit and marks the instance as stopped.

**Invariant:** The harness (PID 1) outlives all guest processes. The guest API is available from boot until VM death.

## 4. Architecture

```
┌─ Guest VM ─────────────────────────────────────────────┐
│                                                         │
│  guest process (OpenClaw, agent, tool)                  │
│       │                                                 │
│       │ HTTP (127.0.0.1:7777)                           │
│       │ or unix socket (/run/aegis/guestd.sock)         │
│       ▼                                                 │
│  harness (PID 1)                                        │
│    ├── primary process manager                          │
│    ├── activity monitor                                 │
│    ├── port proxy                                       │
│    └── guest API server  ← NEW                          │
│         ├── validates capability token                   │
│         ├── scopes requests to allowed operations        │
│         └── forwards to aegisd via control channel       │
│              │                                          │
└──────────────│──────────────────────────────────────────┘
               │ vsock (control channel)
               ▼
          aegisd (host)
            ├── validates token (signature + claims)
            ├── enforces resource ceilings
            └── creates/manages child instances
```

### Why not expose aegisd socket directly?

Once any guest process can hit aegisd, you've lost per-instance scoping, spawn controls, auditability, and the multi-tenant story. The guest broker keeps the surface stable and controllable.

## 5. Capability Tokens

### 4.1 Injection

At instance boot, aegisd generates a capability token and passes it to the harness via the `run` RPC:

```json
{
  "method": "run",
  "params": {
    "command": ["..."],
    "expose_ports": [18789],
    "capability_token": "eyJ..."
  }
}
```

The harness makes the token available to the guest API server. Guest processes authenticate to the API using this token.

### 4.2 Token Format

Signed JWT (HMAC-SHA256, keyed by aegisd's master key):

```json
{
  "iss": "aegisd",
  "sub": "inst-1771698287908038000",
  "iat": 1740160000,
  "exp": 1740246400,
  "cap": {
    "spawn": true,
    "max_children": 5,
    "allowed_images": ["node:22", "python:3.12", "alpine"],
    "max_memory_mb": 4096,
    "max_vcpus": 4,
    "allowed_secrets": ["ANTHROPIC_API_KEY", "OPENAI_API_KEY"],
    "allowed_workspaces": ["/Users/user/projects/*"],
    "max_expose_ports": 3
  }
}
```

### 4.3 Claims

| Claim | Type | Description |
|-------|------|-------------|
| `sub` | string | Parent instance ID (scopes all operations to this parent) |
| `exp` | int | Expiry timestamp (revoked on instance stop) |
| `cap.spawn` | bool | Can this instance spawn children? |
| `cap.spawn_depth` | int | How many levels of nesting allowed (1 = can spawn children but children can't spawn). Decremented for each child's token. 0 = no spawning. |
| `cap.max_children` | int | Maximum concurrent child instances |
| `cap.allowed_images` | []string | OCI image refs this instance can use (glob supported) |
| `cap.max_memory_mb` | int | Per-child memory ceiling |
| `cap.max_vcpus` | int | Per-child vCPU ceiling |
| `cap.allowed_secrets` | []string | Secrets this instance can pass to children |
| `cap.allowed_workspaces` | []string | Host paths children can mount (glob supported) |
| `cap.max_expose_ports` | int | Maximum exposed ports per child |

### 4.4 Token Lifecycle

- **Created** by aegisd when the instance boots
- **Injected** via the `run` RPC params
- **Validated** by aegisd on every privileged request: signature + expiry + claims + **instance state check** (the `sub` instance must be currently running — if not, the token is effectively revoked)
- No explicit revocation list needed — aegisd checks that the instance ID in the token corresponds to a running instance before honoring any request

### 4.5 Child Token Minting

When a parent spawns a child, aegisd mints a new token for the child. The child's capabilities are the **intersection** of the parent's capabilities and any further restrictions:

- `spawn_depth` = parent's `spawn_depth - 1` (0 = no spawning)
- `spawn` = `true` only if `spawn_depth > 0`
- `max_children` = inherited from parent (or lower if specified)
- `allowed_images` = inherited from parent (child cannot add images parent doesn't have)
- `max_memory_mb` = inherited from parent (child cannot raise ceiling)
- `max_vcpus` = inherited from parent
- `allowed_secrets` = inherited from parent (child cannot access secrets parent can't)
- `allowed_workspaces` = inherited from parent
- `max_expose_ports` = inherited from parent

**Rule: child capabilities can only be equal to or stricter than parent.** No escalation is possible. This is enforced by aegisd at token minting time.

### 4.6 Default Token (no capabilities)

Instances created without capabilities get a token with `cap.spawn: false`. The guest API server is still available (for keepalive, logs, self-info) but cannot spawn children.

## 6. Guest API

### 5.1 Transport

HTTP server inside the VM, managed by the harness:

- `127.0.0.1:7777` — HTTP (simple, any process can call it)
- `/run/aegis/guestd.sock` — Unix socket (better security via filesystem perms)

Both serve the same API. **No authentication required inside the VM** — the harness automatically attaches the capability token when forwarding requests to aegisd. The token never leaves the harness; guest processes don't see it.

This is correct because:
- Any process in the VM can already reach the API (shared network namespace)
- The real security boundary is the host-side token validation
- Not exposing the token prevents accidental logging, leaking, or "forgot the header" bugs

### 5.2 Environment Variables

Injected by the harness into the primary process environment:

```
AEGIS_GUEST_API=http://127.0.0.1:7777
AEGIS_INSTANCE_ID=inst-1771698287908038000
```

Note: `AEGIS_CAP_TOKEN` is NOT exposed to guest processes. It lives only in harness memory.

### 5.3 Endpoints

#### Instance Management

```
POST   /v1/instances              Spawn a child instance
GET    /v1/instances              List children of this instance
GET    /v1/instances/{id}         Get child instance info
POST   /v1/instances/{id}/start   Start/restart a stopped child
POST   /v1/instances/{id}/stop    Stop a child
DELETE /v1/instances/{id}         Delete a child
POST   /v1/instances/{id}/exec    Exec command in child
GET    /v1/instances/{id}/logs    Stream child logs
```

#### Self

```
GET    /v1/self                   This instance's info (ID, state, endpoints)
POST   /v1/self/keepalive         Acquire/renew keepalive lease
DELETE /v1/self/keepalive         Release keepalive lease
```

### 5.4 Spawn Request

```json
POST /v1/instances
Authorization: Bearer eyJ...

{
  "handle": "work-abc123",
  "command": ["sh", "-c", "node build.js && python3 -m http.server 8080"],
  "image_ref": "node:22",
  "workspace": "/Users/user/projects/my-site",
  "exposes": [{"port": 8080}],
  "secrets": ["ANTHROPIC_API_KEY"],
  "memory_mb": 2048,
  "env": {"NODE_ENV": "production"}
}
```

Response:

```json
{
  "id": "inst-1771699000000000000",
  "handle": "work-abc123",
  "state": "starting",
  "parent_id": "inst-1771698287908038000",
  "endpoints": [{"guest_port": 8080, "public_port": 55001, "protocol": "http"}]
}
```

### 5.5 Validation Flow

```
Guest process → POST /v1/instances (no auth header needed)
    │
    ▼
Harness guest API server:
    1. Attach capability token from harness memory
    2. Forward request + token to aegisd via control channel RPC
    │
    ▼
aegisd (host):
    1. Verify token signature (master key)
    2. Check token expiry
    3. Check cap.spawn == true
    4. Check cap.spawn_depth > 0 (prevent unbounded nesting)
    5. Check cap.max_children not exceeded
    6. Check image_ref in cap.allowed_images
    7. Check memory_mb <= cap.max_memory_mb
    8. Check secrets subset of cap.allowed_secrets
    9. Check workspace in cap.allowed_workspaces
    10. Create child instance with parent_id set
    11. Child's token gets cap.spawn_depth decremented (or spawn: false if depth=0)
    12. Return instance info
```

The harness does **zero** token validation — no signature check, no claims check. It just forwards. All capability enforcement happens on the host in aegisd, where the signing key lives.

## 7. Control Channel RPC Extensions

New RPC methods on the existing harness ↔ aegisd control channel:

### Guest → Host (via harness)

The harness attaches the capability token automatically. Guest processes never see the token.

```json
{"jsonrpc":"2.0","method":"guest.spawn","id":1,"params":{
  "token": "<attached by harness>",
  "request": { "handle": "work-1", "command": [...], "image_ref": "node:22", ... }
}}

{"jsonrpc":"2.0","method":"guest.list_children","id":2,"params":{"token":"<attached>"}}

{"jsonrpc":"2.0","method":"guest.stop_child","id":3,"params":{"token":"<attached>","child_id":"inst-..."}}

{"jsonrpc":"2.0","method":"guest.exec_child","id":4,"params":{"token":"<attached>","child_id":"inst-...","command":[...]}}

{"jsonrpc":"2.0","method":"guest.child_logs","id":5,"params":{"token":"<attached>","child_id":"inst-..."}}

{"jsonrpc":"2.0","method":"guest.self_info","id":6,"params":{}}

{"jsonrpc":"2.0","method":"guest.keepalive","id":7,"params":{"ttl_ms":30000,"reason":"build"}}
```

### Host → Guest (responses)

Standard JSON-RPC 2.0 responses with result or error.

## 8. Guest MCP Server

For LLM agents that use MCP (Model Context Protocol), the harness also exposes an **MCP server** over stdio:

```
aegis-mcp-guest (stdio, runs inside VM)
```

Tools:

| Tool | Description |
|------|-------------|
| `instance_spawn` | Spawn a child instance |
| `instance_list` | List children |
| `instance_info` | Get child info |
| `instance_stop` | Stop a child |
| `instance_exec` | Exec command in child |
| `instance_logs` | Tail child logs |
| `keepalive_acquire` | Acquire keepalive lease |
| `keepalive_release` | Release keepalive lease |

The MCP server talks to the guest API server (localhost:7777), not directly to the control channel. Same capability token authentication.

This is a separate binary included in the base rootfs alongside the harness. Agents invoke it via stdio MCP protocol. It's the same pattern as the host-side `aegis-mcp` but scoped to the guest's capabilities.

## 9. Parent-Child Instance Relationship

### 8.1 Registry

Child instances have a `parent_id` field linking to the spawning instance. This enables:

- `GET /v1/instances?parent_id=inst-...` — list children of a parent
- Cascade stop: when parent stops, children are stopped (configurable)
- Resource accounting: children's resource usage attributed to parent

### 8.2 Orphan Policy

When a parent instance stops, **all children are stopped** (cascade). This is the only policy for MVP — it's simple, predictable, and prevents orphaned instances consuming resources.

Future: `detach` (children become top-level) and `pause` (children resume when parent restarts) can be added if needed.

### 8.3 Child Instance Visibility

- Parent can see and manage its own children (via guest API)
- Parent cannot see other instances (scoped by token's `sub` claim)
- Children cannot see the parent or siblings (no upward visibility)
- aegisd API (host-side) can see all instances including parent-child relationships

## 10. Implementation Plan

### Phase 1: Guest API Server (MVP)

- [ ] HTTP server goroutine in harness (127.0.0.1:7777)
- [ ] `guest.spawn` RPC on control channel
- [ ] `guest.list_children`, `guest.stop_child` RPCs
- [ ] `guest.self_info`, `guest.keepalive` RPCs
- [ ] Capability token generation in aegisd (simple HMAC-SHA256 JWT)
- [ ] Token injection via `run` RPC params
- [ ] Host-side validation + enforcement in aegisd
- [ ] `parent_id` field in registry
- [ ] `AEGIS_GUEST_API`, `AEGIS_CAP_TOKEN`, `AEGIS_INSTANCE_ID` env vars

### Phase 2: Full API + MCP

- [ ] `guest.exec_child`, `guest.child_logs` RPCs
- [ ] Unix socket transport (/run/aegis/guestd.sock)
- [ ] `aegis-mcp-guest` binary (stdio MCP server)
- [ ] Orphan policy (cascade/detach/pause)
- [ ] Resource accounting (children's usage → parent)

### Phase 3: Kit Integration

- [ ] OpenClaw Kit: bot instance spawns work instances via guest API
- [ ] Token capabilities configured per-kit in manifest
- [ ] Dynamic port exposure for child instances

## 11. Security Considerations

1. **Token never leaves the harness.** Guest processes don't see the token. The harness attaches it when forwarding to aegisd. No risk of accidental logging or leaking.

2. **Host validates everything.** The harness forwards requests but doesn't enforce capabilities. All validation happens in aegisd where the signing key lives. A compromised harness can't escalate privileges.

3. **Resource ceilings are enforced on host.** Even if a guest crafts a request for 128GB RAM, aegisd checks `cap.max_memory_mb` and rejects it.

4. **No upward visibility.** Children can't see the parent. This prevents confused-deputy attacks where a child instance manipulates the parent.

5. **Token expiry + revocation.** Tokens expire with the instance. When the parent stops, its token is invalid, and all child management requests fail.

6. **Spawn depth prevents unbounded nesting.** `cap.spawn_depth` is decremented for each generation. A depth of 1 means "can spawn children, but children cannot spawn grandchildren." Prevents runaway orchestration trees.

7. **Audit logging.** Every spawn is logged by aegisd: `parent_id → child_id → image_ref → resource spec`. This is essential for debugging and future resource accounting.
