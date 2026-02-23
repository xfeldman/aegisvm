# Runtime Port Expose Spec

**Status:** Draft
**Scope:** Decouple port management from instance creation. Expose/unexpose ports at any time via CLI, API, guest API, and MCP.

---

## 1. Motivation

Port exposure should be a separate operation from instance creation. Create compute, then attach ingress. No `--expose` on create — one way to expose, one way to unexpose.

Use cases:
- Agent starts a web server mid-task, needs to expose it
- User creates instance first, decides on ports later
- Debugging: expose a port temporarily, then remove it
- Guest agent autonomously exposes services it starts

---

## 2. Port ownership

The **router** owns public port listeners. The **gvproxy backend** maps public ports to guest ports. The **harness port proxy** bridges localhost-bound apps to the guest IP (existing mechanism, orthogonal to exposure).

Expose flow:
1. Request arrives (CLI, host API, or guest API)
2. aegisd validates against `max_expose_ports` capability limit
3. Router allocates a public port listener → maps to instance + guest port
4. Mapping persisted to registry as `{guest_port, public_port, protocol}`
5. Done — traffic flows: public port → router → gvproxy → guest

No harness involvement in the exposure itself. The harness port proxy (guestIP:port → localhost:port bridge) is a separate, orthogonal concern — see section 8.

Unexpose flow:
1. Request arrives
2. Router frees the public port listener
3. Mapping removed from registry
4. Done

---

## 3. Surfaces

### CLI

```bash
aegis instance expose web 80              # expose guest port 80 (auto public port)
aegis instance expose web 80:8080         # expose guest 80 on public 8080
aegis instance expose web 3000/tcp        # with protocol hint
aegis instance unexpose web 80            # remove by guest port
```

Format: `GUEST[:PUBLIC][/PROTO]`. Guest port is always first — it's the identity.

### Host API

```
POST   /v1/instances/{id}/expose              {"guest_port": 80, "public_port": 8080, "protocol": "http"}
DELETE /v1/instances/{id}/expose/{guest_port}
```

`public_port` is optional (0 = auto-assign). `guest_port` is always the key.

Response:
```json
{"guest_port": 80, "public_port": 8080, "protocol": "http", "url": "http://127.0.0.1:8080"}
```

### Guest API (inside VM)

```
POST   http://127.0.0.1:7777/v1/self/expose             {"guest_port": 80, "public_port": 8080, "protocol": "http"}
DELETE http://127.0.0.1:7777/v1/self/expose/{guest_port}
```

`public_port` is optional (0 = auto). The guest knows what it's serving, so the API is guest-port-centric. Same response format. No capability token needed (self-operation, capped by `max_expose_ports`).

### Host MCP

```json
{"name": "instance_expose", "description": "Expose a port on an instance. Returns the public URL."}
{"name": "instance_unexpose", "description": "Remove an exposed port from an instance."}
```

### Guest MCP

```json
{"name": "expose_port", "description": "Expose a port from this VM to the host. Returns the public URL."}
{"name": "unexpose_port", "description": "Remove a previously exposed port."}
```

---

## 4. Protocol

The `protocol` field is advisory — it affects URL formatting and UI display, not routing behavior. The router is L4 and protocol-agnostic.

Allowed values for v1: `tcp`, `http`. Default: `http`.

---

## 5. Persistence and stability

All port mappings are persisted in the registry as `{guest_port, public_port, protocol}`. On daemon restart, aegisd replays all mappings:

- For enabled instances: `router.AllocatePort()` with the stored public port (deterministic restore)
- For disabled instances: mappings stored but no listeners opened (disabled = unreachable)
- On re-enable/start: listeners opened from persisted mappings

Public port stability:
- **Explicit** (`expose web 80:8080`) → public 8080 persisted, restored deterministically. If port is taken on restart, **error** (not silent fallback). User must resolve the conflict.
- **Auto** (`expose web 80`) → random public port assigned, persisted. On restart, same port if available, fallback to random if taken. Fallback is expected and documented.

---

## 6. Idempotency and conflict

| Action | Behavior |
|--------|----------|
| Expose same guest port with same public port | Idempotent — return existing mapping |
| Expose same guest port with different public port | 409 Conflict (use unexpose + expose, or add `replace: true`) |
| Expose different guest port on same public port | 409 Conflict (public port already in use) |
| Unexpose a port that isn't exposed | No-op, 200 OK |

---

## 7. Disabled instances

Allow changing exposure mappings while disabled (persist intent). Do not open listeners while disabled. On re-enable/start, replay mappings and open listeners.

For explicit public ports (`expose web 80:8080` while disabled): the mapping is stored as "desired." On enable, aegisd attempts to bind. If the port is taken, enable/start **fails with an error** — no silent fallback. The user must unexpose and re-expose with a different port, or free the conflicting port.

This preserves: **disabled = unreachable**, and **explicit = deterministic**.

---

## 8. Harness port proxy (orthogonal)

The harness port proxy (guestIP:port → localhost:port bridge) is a separate concern from port exposure. It exists because some apps bind to `127.0.0.1` inside the VM, making them unreachable via the guest's eth0 IP. The proxy bridges this gap.

The proxy does NOT need to know exposed ports upfront. It dynamically adds bridges when aegisd notifies the harness of a new exposure (via a `ports_changed` notification on the control channel). On unexpose, the harness stops the corresponding bridge.

This means: no "predeclared ports at boot time" requirement inside the guest.

---

## 9. `--expose` removal

`--expose` is removed from:
- CLI `instance start` and `run`
- Host API `createInstanceRequest`
- Host MCP `instance_start` tool
- Guest API `spawn` request

One way to expose: the dedicated expose operation. No sugar, no dual paths, no partial-failure ambiguity.

Breaking change — existing scripts using `--expose` on create need to be updated to two commands.

---

## 10. Changes

### CLI (`cmd/aegis/main.go`)
- Add `aegis instance expose` and `aegis instance unexpose` subcommands
- Remove `--expose` from `parseRunFlags`, `cmdRun`, `cmdInstanceStart`

### Host API (`internal/api/server.go`)
- Add `POST /v1/instances/{id}/expose` and `DELETE /v1/instances/{id}/expose`
- Remove `exposes` from `createInstanceRequest`

### Lifecycle manager (`internal/lifecycle/guest.go`)
- Add `guest.expose_port` and `guest.unexpose_port` handlers (no capability token, self-operation)

### Lifecycle manager (`internal/lifecycle/manager.go`)
- Add `ExposePort(id, guestPort, publicPort, protocol)` and `UnexposePort(id, guestPort)` methods
- Update `ExposePorts` on the instance, persist to registry

### Harness — guest API (`internal/harness/guestapi.go`)
- Add `POST /v1/self/expose` and `DELETE /v1/self/expose/{guest_port}` endpoints

### Guest MCP (`cmd/aegis-mcp-guest`)
- Add `expose_port` and `unexpose_port` tools

### Host MCP (`cmd/aegis-mcp`)
- Add `instance_expose` and `instance_unexpose` tools
- Remove `expose` parameter from `instance_start` tool

### Registry restore (`cmd/aegisd/main.go`)
- Already replays port mappings from registry — no change needed

---

## 11. What doesn't change

- Router — `AllocatePort()` / `FreePort()` already support dynamic allocation
- Harness port proxy — still bridges guestIP:port → localhost:port (orthogonal concern)
- Capability tokens — `max_expose_ports` already exists as the ceiling
- Registry schema — `expose_ports` column already stores the port list
- gvproxy backend — port mapping is router-level, not VMM-level
