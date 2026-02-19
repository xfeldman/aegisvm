# HTTP Router

Canonical reference for the Aegis HTTP router embedded in aegisd.

---

## 1. Overview

The router is a connection-aware lifecycle proxy embedded in aegisd. It listens on `127.0.0.1:8099`, accepts incoming HTTP connections, wakes the target instance if it is not already running, proxies the request into the VM, and tracks connection activity to drive idle-based hibernation.

The router is not a separate process. It runs inside the aegisd daemon as a standard `net/http` handler.

## 2. Instance Resolution

When a request arrives, the router must decide which instance to proxy to. Three resolution methods are tried in priority order.

### Header: `X-Aegis-Instance`

The router checks for the `X-Aegis-Instance` header. The value is the instance ID.

Example:

```
Request:   GET /api/data
Header:    X-Aegis-Instance: inst-173f...
```

### Path prefix: `/{handle}/...`

The router matches the URL path prefix `/{handle}/` and routes to the instance with that handle. The prefix is stripped before forwarding -- the backend sees only the remainder of the path.

Example:

```
Request:   GET /myapp/api/data
Backend:   GET /api/data
```

### Default fallback

If neither header nor path prefix matches, and exactly one instance exists, the router uses it as the default. With multiple instances and no explicit routing, the router returns 503 and logs a warning.

## 3. Wake-on-Connect

When a request arrives for an instance that is not in the Running state, the router triggers a wake sequence:

| Instance state | Action | Latency |
|---|---|---|
| Stopped | Cold boot. Router calls `EnsureInstance()`. | ~500ms |
| Paused | Resume via SIGCONT. RAM preserved. | <100ms |
| Running | No action needed. Proxy immediately. | 0 |
| Starting | Block until Running or request timeout (30s). | Up to 30s |

### Behavior during wake

While the VM is booting or resuming, the router returns `503 Service Unavailable` with a `Retry-After: 3` header. No readiness gating -- the 503 means "could not proxy to backend."

## 4. Idle Behavior

The router uses connection counting and idle timers to manage VM lifecycle automatically.

### Connection tracking

Every proxied connection increments the active connection count for that instance. Each new connection calls `ResetActivity`, which resets the idle timer. When a connection closes, the count decrements.

### Idle cascade

When the last active connection closes, the idle timer begins:

1. **Pause** (`PauseAfterIdle`, default 60s) -- The VM is paused via SIGSTOP. The process and its memory are retained. The next request triggers SIGCONT resume in under 100ms.

2. **Stop** (`StopAfterIdle`, default 20min) -- The VM is stopped and resources freed. The next request triggers a cold boot.

Any incoming request at any point resets the idle timer and wakes the VM if needed.

## 5. WebSocket Support

WebSocket connections are fully supported. The router detects the `Upgrade: websocket` header and switches to raw TCP proxying with bidirectional `io.Copy`. WebSocket connections count as active connections for idle tracking.

## 6. Timeouts

| Parameter | Default | Description |
|---|---|---|
| Request context | 30s | Maximum time the router will wait for a VM to become ready. |
| PauseAfterIdle | 60s | Time after last connection closes before SIGSTOP. |
| StopAfterIdle | 20min | Time after pause before VM is fully stopped. |

## 7. Proxy Failure

If the router cannot reach the backend (process not listening, port not bound), it returns `503 Service Unavailable` with `Retry-After: 3`. This is not a readiness check -- it's a standard proxy error. The instance may be running but the process inside hasn't bound the port yet.

## 8. Limitations (Current)

- **Single listen address.** The router binds to `127.0.0.1:8099` only.
- **No TLS termination.** The router speaks plain HTTP. For HTTPS, place a reverse proxy in front.
- **No per-instance custom error pages.** The 503 response is the same for all instances.
