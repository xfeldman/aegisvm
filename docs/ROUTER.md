# HTTP Router

Canonical reference for the Aegis HTTP router embedded in aegisd.

---

## 1. Overview

The router is a connection-aware lifecycle proxy embedded in aegisd. It listens on `127.0.0.1:8099`, accepts incoming HTTP connections, wakes the target microVM if it is not already running, proxies the request into the VM, and tracks connection activity to drive idle-based hibernation and termination.

The router is not a separate process. It runs inside the aegisd daemon as a standard `net/http` handler. There is one router per aegisd instance.

## 2. App Resolution

When a request arrives, the router must decide which app instance to proxy it to. Three resolution methods are tried in priority order.

### Path prefix: `/app/{name}/...`

The router matches the URL path prefix `/app/{name}/` and routes to the app with that name. The prefix is stripped before forwarding -- the backend sees only the remainder of the path.

Example:

```
Request:   GET /app/myapp/api/data
Backend:   GET /api/data
```

### Header: `X-Aegis-App`

If the path prefix does not match, the router checks for the `X-Aegis-App` header. The value is the app name. The full request path is forwarded unchanged.

Example:

```
Request:   GET /api/data
Header:    X-Aegis-App: myapp
Backend:   GET /api/data
```

This method is useful for programmatic clients and SDKs that cannot control URL structure.

### Default fallback

If neither path prefix nor header is present, the router falls back to the first instance in its internal map. This works when only one app is running.

**Warning:** With multiple apps running concurrently, the default fallback is non-deterministic. The "first" entry depends on Go map iteration order, which is randomized. Always use path prefix or header routing when more than one app is active.

## 3. Wake-on-Connect

When a request arrives for an instance that is not in the Running state, the router triggers a wake sequence. The behavior depends on the instance's current state:

| Instance state | Action | Latency |
|---|---|---|
| Stopped | Cold boot from release rootfs. Router calls `EnsureInstance()`, which boots the VM and blocks until `serverReady`. | ~1-2s |
| Paused | Resume via SIGCONT. RAM is retained, process state is preserved. | <100ms |
| Running | No action needed. Proxy immediately. | 0 |
| Starting | Block until the instance transitions to Running or the request context times out (30s). | Up to 30s |

### Behavior during wake

While the VM is booting or resuming, the router does not drop the request. Instead, it returns a retry response whose format depends on the client:

- **HTML clients** (request `Accept` header contains `text/html`): The router returns an HTML loading page with `<meta http-equiv="refresh" content="3">`, causing the browser to automatically retry after 3 seconds.

- **All other clients**: The router returns `503 Service Unavailable` with a `Retry-After: 3` header. Programmatic clients should respect this header and retry.

## 4. Idle Behavior

The router uses connection counting and idle timers to manage VM lifecycle automatically.

### Connection tracking

Every proxied connection increments the active connection count for that instance. Each new connection (or activity on an existing connection) calls `ResetActivity`, which resets the idle timer. When a connection closes, the router calls `OnConnectionClose`, which decrements the count.

### Idle cascade

When the last active connection closes, the idle timer begins. The cascade proceeds through two stages:

1. **Pause** (`PauseAfterIdle`, default 60s) -- The VM is paused via SIGSTOP. The process and its memory are retained by the kernel but consume no CPU. The next request triggers a SIGCONT resume in under 100ms.

2. **Terminate** (`TerminateAfterIdle`, default 20min) -- The VM is terminated and its resources are freed. The next request triggers a full cold boot from disk layers.

Any incoming request at any point in this cascade resets the idle timer and wakes the VM if needed.

## 5. WebSocket Support

WebSocket connections are fully supported. The router detects the `Upgrade: websocket` header and switches from HTTP-level proxying to a raw TCP proxy:

1. The router hijacks the `net.Conn` from the HTTP handler.
2. It dials the backend inside the VM.
3. Two goroutines run bidirectional `io.Copy` between the client connection and the backend connection.
4. The WebSocket connection counts as an active connection for idle tracking purposes.
5. When either side closes, the router calls `OnConnectionClose`, which may start the idle timer if no other connections remain.

## 6. Timeouts

| Parameter | Default | Description |
|---|---|---|
| Request context | 30s | Maximum time the router will wait for a VM to become ready before returning an error to the client. Derived from the incoming HTTP request's context. |
| PauseAfterIdle | 60s | Time after the last connection closes before the VM is paused (SIGSTOP). Configurable in app config. |
| TerminateAfterIdle | 20min | Time after pause before the VM is terminated entirely. Configurable in app config. |
| Server readiness probe | 30s | Maximum time `EnsureInstance` will poll the harness TCP endpoint to confirm the guest server is accepting connections. Polls at 200ms intervals. |

## 7. Limitations (Current)

- **TCP-level proxy only.** The router operates at the HTTP/TCP layer. There is no gRPC-aware request queueing or multiplexing.
- **Single listen address.** The router binds to `127.0.0.1:8099` only. It does not listen on other interfaces or ports.
- **No TLS termination.** The router speaks plain HTTP. For HTTPS, place a reverse proxy (such as Caddy or nginx) in front of aegisd.
- **No per-app custom loading pages.** The loading page shown during wake is the same for all apps. Kit-configurable loading pages are planned for a future milestone.
