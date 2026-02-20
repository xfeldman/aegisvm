# Router

Canonical reference for the AegisVM ingress router embedded in aegisd.

---

## 1. Overview

The router is a connection-aware lifecycle proxy embedded in aegisd. It provides two ingress paths:

- **Main HTTP router** (`127.0.0.1:8099`) — handle-based routing with HTTP reverse proxy and WebSocket support
- **Per-port TCP proxies** — one listener per `--expose` port per instance, L4 TCP relay with wake-on-connect

Both paths share the same core: ensure instance is running, dial the VMM backend, relay traffic. The router is not a separate process — it runs inside the aegisd daemon.

### Always-proxy architecture

All user-facing ports are owned by the router, not by the VMM. When you run `aegis instance start --expose 80`, the router allocates a host port (e.g., `:52000`) and starts listening on it immediately. The VMM still allocates an internal port for TSI forwarding, but users never see it.

This design ensures wake-on-connect works on every exposed port, even when the VM is paused (SIGSTOP freezes the VMM worker process and its TSI port forwarding).

```
User → :52000 (router-owned, always listening)
  → EnsureInstance (wake/resume/boot if needed)
  → GetEndpoint → 127.0.0.1:<vmm-internal>
  → L4 TCP relay (bidirectional io.Copy)
```

Two port layers:

| Layer | Owner | Lifetime | Visible to user |
|-------|-------|----------|-----------------|
| **Public port** | Router | Instance create → instance delete | Yes |
| **Backend port** | VMM (TSI) | VM boot → VM stop | No |

Public ports are stable across pause/resume/stop/restart. They are freed only when the instance is deleted (or the daemon shuts down). Backend ports are ephemeral — reallocated on each VM boot.

## 2. Per-Port TCP Proxy

Each `--expose` port gets its own TCP listener. The proxy is pure L4 — it relays raw TCP bytes without inspecting the protocol. This means HTTP, WebSocket, gRPC, and arbitrary TCP protocols all work through exposed ports.

### Connection flow

1. Client connects to public port (e.g., `:52000`)
2. Router increments connection count (prevents idle timer race)
3. `EnsureInstance()` — resume if paused, boot if stopped, wait if starting, no-op if running
4. `GetEndpoint()` — resolve guest port → VMM backend address
5. Dial backend, bidirectional `io.Copy`
6. On connection close, decrement connection count

### Wake-on-connect

When a connection arrives on a public port and the VM is not running:

| Instance state | Action | Latency |
|---|---|---|
| Running | Proxy immediately | 0 |
| Paused | SIGCONT resume, then proxy | <100ms |
| Stopped | Cold boot, then proxy | ~500ms |
| Starting | Wait until running (up to 30s) | Variable |

### Port allocation

`--expose` supports Docker-style port mapping:

```
--expose 80          → random public port → guest 80
--expose 8080:80     → public 8080 → guest 80
--expose 8080:80/tcp → public 8080 → guest 80, protocol tcp
```

Random ports are allocated with `net.Listen("tcp", "127.0.0.1:0")`. Deterministic ports bind to the specified port. Ports below 1024 require root.

**Stability policy:**
- Deterministic ports (`8080:80`): stable always (same port every time)
- Random ports (`80`): stable across pause/resume/stop/restart within a daemon session. NOT stable across daemon restart.

## 3. Main HTTP Router

The main router listens on `127.0.0.1:8099` (configurable via `RouterAddr`). It provides HTTP-level routing for instances by handle, with WebSocket upgrade support.

### Instance resolution

When an HTTP request arrives on the main router, it resolves the target instance:

1. **Header: `X-Aegis-Instance`** — route by instance ID or handle
2. **Default fallback** — if exactly one instance exists, use it

With multiple instances and no explicit routing, the router returns 503. Use per-port endpoints (from `--expose`) to reach specific instances directly — each instance gets its own port.

### HTTP reverse proxy

For standard HTTP requests, the router uses `httputil.ReverseProxy` to forward to the VMM backend with proper header handling.

### WebSocket support

WebSocket connections are detected by `Upgrade: websocket` header. The router hijacks the connection and switches to raw TCP bidirectional relay. WebSocket connections count as active connections for idle tracking.

## 4. Idle Behavior

The router drives VM lifecycle through connection counting and idle timers.

### Connection tracking

Every connection (both per-port TCP and main router HTTP) increments the active connection count. The count is incremented **before** `EnsureInstance` to prevent the idle timer from firing during wake. When a connection closes, the count decrements.

Raw TCP connections (database clients, long-lived streams) keep the VM alive as long as they are open. The VM will not pause while any connection is active.

### Idle cascade

When the last active connection closes, the idle timer begins:

1. **Pause** (`PauseAfterIdle`, default 60s) — VM is paused via SIGSTOP. Process and memory retained. Next connection triggers SIGCONT resume.

2. **Stop** (`StopAfterIdle`, default 5min) — VM is stopped, resources freed. Instance remains in list as STOPPED. Next connection triggers cold boot.

Any incoming connection at any point resets the idle timer and wakes the VM.

## 5. Concurrency

Multiple connections can arrive simultaneously, including while the VM is stopped:

- `bootInstance()` sets state to `Starting` under a lock. The first caller boots the VM. Concurrent callers see `Starting` and wait via `waitForRunning()` (polls until `Running`).
- `resumeInstance()` checks `StatePaused` under a lock. Double SIGCONT is a no-op (safe).
- Per-port connections track activity independently — each increments/decrements the connection count.

## 6. Timeouts

| Parameter | Default | Description |
|---|---|---|
| EnsureInstance | 30s | Maximum time to wait for VM to become ready |
| PauseAfterIdle | 60s | Time after last connection closes before SIGSTOP |
| StopAfterIdle | 5min | Time after pause before VM is fully stopped |
| Backend dial | 5s | Timeout for connecting to VMM backend port |

## 7. Error Behavior

### Per-port TCP proxy

If `EnsureInstance` fails or the backend can't be dialed, the TCP connection is closed. No HTTP error response — the client sees a connection reset. This is inherent to L4 proxying (no protocol to send errors over).

### Main HTTP router

Returns `503 Service Unavailable` with `Retry-After: 3` if:
- Instance can't be waked/booted
- Backend port can't be reached
- No matching instance found

## 8. API Response

Instance endpoints are returned as public (router-owned) ports:

```json
{
  "endpoints": [
    {"guest_port": 80, "public_port": 52000, "protocol": "http"}
  ],
  "router_addr": "127.0.0.1:8099"
}
```

`public_port` is the port users connect to. `router_addr` is the main HTTP router for handle-based routing.

## 9. Limitations

- **No TLS termination.** The router speaks plain TCP/HTTP. For HTTPS, place a reverse proxy in front.
- **No per-instance custom error pages.** The 503 response is the same for all instances.
- **No protocol detection.** Per-port proxies are pure L4 — no HTTP-specific features (headers, path routing, error responses) on exposed ports. Use the main router on `:8099` for HTTP semantics.
- **Ports not stable across daemon restart.** Public ports are randomized on allocation. Persistence is not yet implemented.
