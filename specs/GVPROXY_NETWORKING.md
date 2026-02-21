# gvproxy Networking for libkrun Backend

**Post-implementation specification — documents the gvproxy virtio-net networking backend added to replace TSI for large-payload workloads.**

**Date:** 2026-02-21
**Depends on:** [IMPLEMENTATION_NOTES.md](IMPLEMENTATION_NOTES.md) (M0 §1.2 TSI transport), [IMPLEMENTATION_KICKOFF.md](IMPLEMENTATION_KICKOFF.md) (§11 networking)

---

## 1. Problem

TSI (Transparent Socket Impersonation) — libkrun's default networking — has a hard limit of ~32KB on outbound HTTP request bodies. Any POST body larger than ~32KB causes `UND_ERR_SOCKET` / "other side closed" at the application level. The root cause is in libkrun's TSI implementation, which buffers outbound data through a vsock transport with a fixed-size window.

This blocks any workload that sends large API payloads. The immediate trigger was OpenClaw, which sends ~46KB per LLM API call (tool definitions + conversation context). The limit also affects any AI agent framework making tool-use calls to the Anthropic or OpenAI APIs.

Small requests (< ~30KB) work fine. The threshold is between 30–35KB. Ingress is unaffected (TSI handles inbound port mapping correctly at any size).

## 2. Solution: In-Process gvproxy (gvisor-tap-vsock)

Replace TSI with the `gvisor-tap-vsock` Go library embedded directly in the vmm-worker process, giving the guest a real NIC via virtio-net:

- **virtio-net data plane** via unixgram socket (no payload size limit)
- **NAT gateway** at 192.168.127.1 (outbound internet access)
- **Built-in DNS** at the gateway address
- **Port forwarding** via in-process API calls (replaces TSI's `krun_set_port_map`)

The library runs as goroutines inside the vmm-worker process. SIGSTOP on the worker freezes both the VM and the network stack — zero CPU during pause, no separate process to manage.

The control channel (harness ↔ aegisd RPC) uses AF_VSOCK, mapped to a unix socket on the host via `krun_add_vsock_port()`.

## 3. Design Principle: Unified Harness

The harness is designed so both libkrun (macOS) and a future Firecracker (Linux) backend share the same code. One harness binary, two backends. The harness never knows which backend it's running on — it reads environment variables and acts accordingly.

| Concern | Harness (guest, unified) | libkrun + gvproxy | Firecracker (future) |
|---|---|---|---|
| Control channel | `connect(AF_VSOCK, CID=2, port=N)` | `krun_add_vsock_port()` → unix socket | vsock → unix socket |
| Data networking | Configure eth0 via netlink syscalls | gvproxy library (virtio-net) | tap device |
| Port forwarding | Port proxy: guestIP:port → 127.0.0.1:port | In-process gvproxy forwarder | iptables/nft |
| DNS | Use gateway IP | gvproxy DNS at gateway | host DNS forwarding |

### 3.1 Harness Environment Variables

The backend sets these before guest boot. The harness reads them to decide behavior:

```
AEGIS_VSOCK_PORT=5000          # vsock port for control channel (gvproxy mode)
AEGIS_VSOCK_CID=2              # vsock CID (default: host)
AEGIS_NET_IP=192.168.127.2/24  # guest IP — if set, configure eth0
AEGIS_NET_GW=192.168.127.1     # default route
AEGIS_NET_DNS=192.168.127.1    # DNS server (default = gateway)
AEGIS_HOST_ADDR=host:port      # legacy TSI mode (if AEGIS_VSOCK_PORT not set)
```

**Mode selection:** If `AEGIS_VSOCK_PORT` is set, use vsock. Otherwise, fall back to TCP/TSI via `AEGIS_HOST_ADDR`. This makes the harness backwards-compatible with TSI.

## 4. Architecture

### 4.1 Process Model

```
aegisd (daemon, no cgo)
  └── aegis-vmm-worker (per-VM, cgo + libkrun + gvproxy library)
        ├── gvproxy goroutines (gvisor-tap-vsock userspace network stack)
        │     ├── virtio-net packet loop (unixgram socket ↔ gVisor stack)
        │     ├── NAT + DNS (gVisor userspace TCP/IP)
        │     └── port forwarding listeners (pre-exposed before VM boot)
        │
        ├── krun_disable_implicit_vsock()
        ├── krun_add_vsock(tsi_features=0)  ← vsock without TSI
        ├── krun_add_vsock_port(5000, ctl.sock)  ← control channel
        ├── krun_add_net_unixgram(net.sock)  ← virtio-net via in-process gvproxy
        └── krun_start_enter()  ← main thread becomes VM, goroutines continue
              └── aegis-harness (PID 1)
                    ├── setupNetwork()  ← configure eth0 via netlink
                    ├── dialVsock(port=5000)  ← control channel
                    └── portProxy(guestIP:port → 127.0.0.1:port)  ← ingress bridge
```

**Key property:** SIGSTOP on the vmm-worker process freezes ALL threads — vCPUs, gvproxy goroutines, and port forwarding listeners. Zero CPU during pause. No separate process, no orphans, no CPU spin.

### 4.2 Ingress (Router → Guest)

Two-hop model, unchanged from TSI era — only the entity listening on BACKEND_PORT changes:

```
Client → PUBLIC_PORT (router TCP listener, unchanged)
       → BACKEND_PORT (gvproxy forwarder, in vmm-worker process)
       → 192.168.127.2:GUEST_PORT (guest eth0)
       → portProxy → 127.0.0.1:GUEST_PORT (if app binds to localhost)
```

- **PUBLIC_PORT**: Owned by the router's L4 proxy (unchanged)
- **BACKEND_PORT**: Random host port, allocated by aegisd, forwarded by in-process gvproxy
- Router dials `127.0.0.1:BACKEND_PORT` exactly as before

#### 4.2.1 Port Proxy (harness-side)

gvproxy forwards inbound traffic to the guest's eth0 IP (192.168.127.2). Apps that bind to `127.0.0.1` won't receive this traffic. The harness runs a TCP proxy for each exposed port:

- Listens on `guestIP:port` (e.g. `192.168.127.2:8080`)
- Forwards to `127.0.0.1:port`
- Binds to the specific guest IP (not `0.0.0.0`) to avoid conflicts with apps that bind to `0.0.0.0`
- Starts with a 2-second delay after the app, so apps binding to `0.0.0.0` take the port first (proxy skips with EADDRINUSE)
- Completely transparent — apps need no configuration changes

### 4.3 Boot Sequence

Port forwarding is pre-exposed in the worker before `krun_start_enter()`, eliminating the need for aegisd to talk to gvproxy:

1. aegisd allocates BACKEND_PORTs (random host ports for each exposed guest port)
2. aegisd spawns vmm-worker with `ExposePorts` in config
3. Worker initializes gvproxy library (`virtualnetwork.New()`)
4. Worker creates unixgram socket, starts packet loop goroutine
5. Worker pre-exposes ports via in-process `ServicesMux` (no HTTP socket)
6. Worker calls `krun_start_enter()` — main thread becomes VM
7. Harness boots, mounts filesystems, configures eth0 via netlink
8. Harness connects to aegisd via vsock
9. aegisd accepts connection, sends `run` RPC (with `expose_ports` for port proxy)
10. Harness starts port proxies (delayed), then starts primary process
11. aegisd marks instance RUNNING

### 4.4 Pause Behavior

On VM pause (SIGSTOP on vmm-worker):

- **Everything freezes** — VM, gvproxy goroutines, port forwarding listeners
- **Zero CPU** — no separate process to spin (was 100% CPU with separate gvproxy due to ENOBUFS retry loop on macOS)
- BACKEND_PORT TCP listeners are frozen but the kernel still accepts SYNs into the backlog
- Router accepts PUBLIC_PORT connections → `EnsureInstance()` → SIGCONT resumes worker
- On resume: gvproxy goroutines unfreeze, accept from backlog, traffic flows
- No reconnection needed — all state is in-process and survives SIGSTOP/SIGCONT

### 4.5 Cleanup

On VM stop:

1. Kill vmm-worker process (kills VM + gvproxy goroutines + port forwarding)
2. Remove control socket: `ctl-{vmID}.sock`
3. Worker's net socket auto-cleaned on process exit

No orphan reaping needed — no separate processes to orphan.

## 5. Configuration

### 5.1 Config Fields

In `internal/config/config.go`:

```go
NetworkBackend string  // "auto" (default), "tsi", "gvproxy"
```

No `GvproxyBin` field — the library is compiled into vmm-worker.

### 5.2 Resolution

`Config.ResolveNetworkBackend()` resolves `"auto"` at daemon startup:

- On darwin (macOS) → `"gvproxy"` (always available, compiled in)
- On linux → `"tsi"` (future: tap device backend)

No binary search, no installation step. `brew install gvproxy` is not needed.

## 6. Implementation Details

### 6.1 Library Usage in vmm-worker

```go
import (
    "github.com/containers/gvisor-tap-vsock/pkg/types"
    "github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
    "github.com/containers/gvisor-tap-vsock/pkg/transport"
)

// Create gVisor userspace network stack
vn, _ := virtualnetwork.New(&types.Configuration{
    Subnet:    "192.168.127.0/24",
    GatewayIP: "192.168.127.1",
    Protocol:  types.VfkitProtocol,
    DNS:       []types.Zone{{Name: "dns.internal.", DefaultIP: guestIP}},
})

// Create unixgram socket and start packet loop
conn, _ := transport.ListenUnixgram("unixgram://" + netSockPath)
go func() {
    vfkitConn, _ := transport.AcceptVfkit(conn)
    vn.AcceptVfkit(ctx, vfkitConn)
}()

// Pre-expose ports via in-process ServicesMux (no HTTP socket)
mux := vn.ServicesMux()
// POST /services/forwarder/expose via httptest.NewRequest (in-memory)
```

### 6.2 Files

| File | Role |
|------|------|
| `cmd/aegis-vmm-worker/main.go` | Embeds gvproxy library, `startInProcessNetwork()`, pre-exposes ports |
| `internal/vmm/libkrun.go` | Passes `ExposePorts` + `SocketDir` in WorkerConfig to worker |
| `internal/config/config.go` | `NetworkBackend` field, `ResolveNetworkBackend()` (platform-based) |
| `internal/harness/vsock_linux.go` | AF_VSOCK dial via raw syscalls, custom `vsockConn` net.Conn wrapper |
| `internal/harness/netlink_linux.go` | Raw netlink syscalls for eth0 setup (no `ip` binary dependency) |
| `internal/harness/portproxy.go` | TCP proxy: guestIP:port → 127.0.0.1:port for localhost-binding apps |
| `internal/harness/rpc.go` | `expose_ports` field in `runParams` for port proxy setup |
| `internal/lifecycle/manager.go` | Sends `expose_ports` in `run` RPC |

### 6.3 Deleted Files

| File | Reason |
|------|--------|
| `internal/vmm/gvproxy.go` | Process management no longer needed (was: spawn, PID files, orphan reaping, HTTP API client) |

### 6.4 Dependencies

```
github.com/containers/gvisor-tap-vsock v0.8.8
```

Compiled into vmm-worker only. aegisd, harness, CLI, and MCP server do not link it.

## 7. Design Decisions

### 7.1 Why gvproxy / gvisor-tap-vsock (not passt, not vde, not custom)

`containers/gvisor-tap-vsock` was chosen because:

1. **libkrun native support**: `krun_add_net_unixgram()` with `NET_FLAG_VFKIT` is designed for gvproxy's vfkit mode
2. **Go library**: Can be embedded in-process — no separate binary, no IPC overhead
3. **Podman ecosystem**: Widely used, well-maintained, macOS-native
4. **Built-in DNS + NAT**: No separate DNS server or iptables setup needed
5. **Programmatic port forwarding**: `ServicesMux` callable in-process via httptest

### 7.2 Why in-process (not separate process)

The original implementation spawned gvproxy as a separate host process. This was changed to in-process because:

1. **SIGSTOP CPU burn**: On macOS, when the vmm-worker is SIGSTOPped, a separate gvproxy enters a tight busy-spin — `sendto()` returns `ENOBUFS` immediately (macOS behavior), and gvproxy retries in a `for { continue }` loop with no backoff. Burns 100% CPU for the duration of the pause.
2. **In-process = zero CPU**: SIGSTOP freezes all threads in the worker, including gvproxy goroutines. No spin, no CPU usage during pause.
3. **No binary dependency**: `brew install gvproxy` no longer needed. Library compiled into vmm-worker.
4. **Simpler lifecycle**: No PID files, no orphan reaping, no process management. Worker death cleans up everything.
5. **No IPC**: Port forwarding calls are in-process Go function calls, not HTTP over unix socket.

### 7.3 Why vsock for control (not TCP-over-virtio-net)

The control channel uses vsock (mapped to a unix socket) rather than TCP over the new virtio-net NIC because:

1. **Ordering**: The control channel must be available before eth0 is configured. vsock is available immediately at boot; virtio-net requires IP configuration first.
2. **Isolation**: Control traffic stays on a separate path from data traffic. If the guest's network stack breaks, the control channel still works.
3. **Firecracker compatibility**: Firecracker also uses vsock for control. Using vsock now means the harness code is already compatible.

### 7.4 Why netlink syscalls for eth0 (not `ip` commands)

The harness configures eth0 using raw netlink syscalls (RTM_NEWLINK, RTM_NEWADDR, RTM_NEWROUTE) instead of shelling out to `ip`:

1. **No rootfs dependency**: Debian-slim (node:22) doesn't include iproute2. Netlink works on any OCI image, even distroless.
2. **Reliability**: No PATH issues, no missing binaries, no exec failures.
3. **Performance**: Direct syscalls, no fork/exec overhead.

### 7.5 Why TSI fallback (not hard requirement)

gvproxy is preferred but TSI is still supported for backwards compatibility:

1. **Graceful degradation**: TSI path still works for small-payload workloads
2. **Platform flexibility**: On future Linux hosts, the networking backend will be different (tap devices)

### 7.6 MAC address choice

The guest NIC uses a fixed MAC address `5a:94:ef:e4:0c:ee`. This is fine because:

1. Each VM has its own gvproxy instance (no MAC collision within a broadcast domain)
2. gvproxy's subnet (192.168.127.0/24) is per-VM, not shared
3. If we need multi-VM networking in the future, we'll generate unique MACs per VM

## 8. libkrun C API Usage

The gvproxy mode uses these libkrun APIs (in order):

```c
// 1. Disable implicit vsock+TSI (lets us configure our own)
krun_disable_implicit_vsock(ctx_id);

// 2. Add vsock WITHOUT TSI hijacking (tsi_features=0)
//    We only need vsock for the control channel, not for networking
krun_add_vsock(ctx_id, 0);

// 3. Map vsock port to unix socket for harness control channel
//    Guest: connect(AF_VSOCK, CID=2, port=5000) → Host: ctl-{vmID}.sock
krun_add_vsock_port(ctx_id, 5000, "/path/to/ctl-{vmID}.sock");

// 4. Add virtio-net device via in-process gvproxy's unixgram socket
//    Guest gets a real NIC (eth0). gvproxy handles NAT/DNS/forwarding.
krun_add_net_unixgram(ctx_id, "/path/to/net-{pid}.sock",
    -1, mac, COMPAT_NET_FEATURES, NET_FLAG_VFKIT);
```

Key: `krun_add_net_unixgram()` with `NET_FLAG_VFKIT` tells libkrun to speak the vfkit protocol to the gvproxy library's unixgram listener. `COMPAT_NET_FEATURES` enables standard virtio-net offloads (checksumming, TSO).

TSI and virtio-net cannot coexist for outbound traffic — TSI hijacks AF_INET at the kernel level. Once we add a virtio-net device, TSI is disabled. Both ingress AND egress go through gvproxy.

## 9. Verification

### Basic

1. Build: `cd ~/work/aegis && make`
2. Start aegisd, verify `"network backend: gvproxy"` in startup log (no binary path)
3. Verify no gvproxy processes: `pgrep gvproxy` returns nothing
4. Test small payload: `aegis run -- echo hello`
5. Test DNS: `aegis run --image alpine -- wget -qO- https://example.com`
6. Test port exposure: create instance with `--expose 80`, verify HTTP through router

### Large Payload (the original blocker)

7. 50KB POST body from inside VM → external endpoint
8. 100KB POST body (well above the TSI limit)
9. OpenClaw's 46KB API call pattern

### Pause/Resume

10. Test pause → verify 0% CPU on vmm-worker (no gvproxy spin)
11. Test wake-on-connect → curl to exposed port while paused → VM resumes
12. Test port forwarding after resume → traffic flows

### Port Proxy

13. App binding to `0.0.0.0` → proxy skips (EADDRINUSE), app handles directly
14. App binding to `127.0.0.1` → proxy bridges guestIP:port → localhost:port

### Integration Tests

See `test/integration/network_test.go` for automated verification.

## 10. Future Work

- **Remove TSI fallback** once gvproxy is proven stable across all workloads
- **Make vsock the only control channel** (drop TCP/TSI legacy path in harness)
- **Per-VM unique MAC addresses** if multi-VM networking is added
- **Activity-based idle detection** — implemented: harness sends periodic heartbeats with TCP connection count, CPU delta, and eth0 byte counters. aegisd resets idle timer on activity.
- **Keepalive leases** — implemented: kit/app can acquire a lease via JSON-RPC to prevent pause for a TTL duration. Per-instance `idle_policy` controls whether heartbeats are authoritative.
