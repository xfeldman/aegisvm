# gvproxy Networking for libkrun Backend

**Post-implementation specification — documents the gvproxy virtio-net networking backend added to replace TSI for large-payload workloads.**

**Date:** 2026-02-21
**Depends on:** [IMPLEMENTATION_NOTES.md](IMPLEMENTATION_NOTES.md) (M0 §1.2 TSI transport), [IMPLEMENTATION_KICKOFF.md](IMPLEMENTATION_KICKOFF.md) (§11 networking)

---

## 1. Problem

TSI (Transparent Socket Impersonation) — libkrun's default networking — has a hard limit of ~32KB on outbound HTTP request bodies. Any POST body larger than ~32KB causes `UND_ERR_SOCKET` / "other side closed" at the application level. The root cause is in libkrun's TSI implementation, which buffers outbound data through a vsock transport with a fixed-size window.

This blocks any workload that sends large API payloads. The immediate trigger was OpenClaw, which sends ~46KB per LLM API call (tool definitions + conversation context). The limit also affects any AI agent framework making tool-use calls to the Anthropic or OpenAI APIs.

Small requests (< ~30KB) work fine. The threshold is between 30–35KB. Ingress is unaffected (TSI handles inbound port mapping correctly at any size).

## 2. Solution: gvproxy virtio-net

Replace TSI with gvproxy (from `containers/gvisor-tap-vsock`), giving the guest a real NIC via virtio-net. gvproxy runs as a host-side process per VM, providing:

- **virtio-net data plane** via unixgram socket (no payload size limit)
- **NAT gateway** at 192.168.127.1 (outbound internet access)
- **Built-in DNS** at the gateway address
- **Port forwarding** via HTTP API (replaces TSI's `krun_set_port_map`)

The control channel (harness ↔ aegisd RPC) switches from TCP-over-TSI to AF_VSOCK, mapped to a unix socket on the host via `krun_add_vsock_port()`.

## 3. Design Principle: Unified Harness

The harness is designed so both libkrun (macOS) and a future Firecracker (Linux) backend share the same code. One harness binary, two backends. The harness never knows which backend it's running on — it reads environment variables and acts accordingly.

| Concern | Harness (guest, unified) | libkrun + gvproxy | Firecracker (future) |
|---|---|---|---|
| Control channel | `connect(AF_VSOCK, CID=2, port=N)` | `krun_add_vsock_port()` → unix socket | vsock → unix socket |
| Data networking | Configure eth0 (IP from env) | gvproxy (virtio-net) | tap device |
| Port forwarding | N/A (host-side) | gvproxy HTTP API | iptables/nft |
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
  ├── gvproxy (per-VM, host process, stays alive during pause)
  │     ├── unixgram socket (virtio-net data plane)
  │     └── unix socket (HTTP API for port forwarding)
  │
  └── aegis-vmm-worker (per-VM, cgo + libkrun)
        ├── krun_disable_implicit_vsock()
        ├── krun_add_vsock(tsi_features=0)  ← vsock without TSI
        ├── krun_add_vsock_port(5000, ctl.sock)  ← control channel
        ├── krun_add_net_unixgram(net.sock)  ← virtio-net via gvproxy
        └── krun_start_enter()  ← process becomes VM
              └── aegis-harness (PID 1)
                    ├── setupNetwork()  ← configure eth0
                    └── dialVsock(port=5000)  ← control channel
```

### 4.2 Ingress (Router → Guest)

Two-hop model, unchanged from TSI era — only the entity listening on BACKEND_PORT changes:

```
Client → PUBLIC_PORT (router TCP listener, unchanged)
       → BACKEND_PORT (gvproxy-mapped, was TSI-mapped)
       → 192.168.127.2:GUEST_PORT
```

- **PUBLIC_PORT**: Owned by the router's L4 proxy (unchanged)
- **BACKEND_PORT**: Random host port, allocated by LibkrunVMM.StartVM, forwarded by gvproxy
- Router dials `127.0.0.1:BACKEND_PORT` exactly as before

### 4.3 Boot Sequence

Ordering matters — gvproxy port forwarding must be set up after the harness connects (proving the VM is alive) and before the instance is marked RUNNING:

1. Allocate BACKEND_PORTs (random host ports for each exposed guest port)
2. Start gvproxy → wait for API socket ready
3. Start vmm-worker → VM boots
4. Harness mounts filesystems, configures eth0, connects via vsock
5. aegisd accepts harness connection on unix socket
6. Expose ports via gvproxy HTTP API (`POST /services/forwarder/expose`)
7. Send `run` RPC to harness
8. Mark instance RUNNING

### 4.4 Pause Behavior

On VM pause (SIGSTOP on vmm-worker):

- **gvproxy stays alive** — it's a separate host process, not SIGSTOPped
- BACKEND_PORT listeners remain active (gvproxy owns them)
- Router still accepts PUBLIC_PORT connections → calls `EnsureInstance()` → SIGCONT resumes VM
- On resume: guest unfreezes, traffic flows through existing gvproxy forwarders
- No reconnection needed — gvproxy forwarders are host-side and don't freeze

### 4.5 Cleanup

On VM stop:

1. Kill vmm-worker process
2. Kill gvproxy process
3. Remove sockets: `net-{vmID}.sock`, `api-{vmID}.sock`, `ctl-{vmID}.sock`
4. Remove PID file: `gvproxy-{vmID}.pid`

On daemon startup (orphan reaping):

1. Scan `{DataDir}/sockets/` for `gvproxy-*.pid` files
2. Kill any leftover gvproxy processes from previous daemon run
3. Remove stale sockets and PID files

## 5. Configuration

### 5.1 Config Fields

Added to `internal/config/config.go`:

```go
NetworkBackend string  // "auto" (default), "tsi", "gvproxy"
GvproxyBin     string  // path to gvproxy binary (auto-detected)
```

### 5.2 Resolution

`Config.ResolveNetworkBackend()` resolves `"auto"` at daemon startup:

- If gvproxy binary found → `"gvproxy"`
- If gvproxy not found → `"tsi"` with loud WARNING log

Search order for gvproxy binary:

1. Same directory as aegisd binary
2. `/opt/homebrew/bin/gvproxy`
3. `/usr/local/bin/gvproxy`
4. `/usr/bin/gvproxy`

### 5.3 Fallback Warning

When falling back to TSI, aegisd logs:

```
WARNING: network backend: tsi (gvproxy not found — known outbound payload limit ~32KB)
```

This log appears at daemon startup and is visible to operators. The `network_backend` field is also exposed in the `/v1/status` API response under `capabilities`.

## 6. Implementation Details

### 6.1 Files Modified

| File | Changes |
|------|---------|
| `internal/config/config.go` | `NetworkBackend`, `GvproxyBin` fields, `ResolveNetworkBackend()`, `findGvproxy()` |
| `internal/vmm/vmm.go` | `NetworkBackend` field in `BackendCaps` |
| `internal/vmm/libkrun.go` | Dual gvproxy/TSI path in `StartVM()`, gvproxy cleanup in `StopVM()` |
| `cmd/aegis-vmm-worker/main.go` | `NetworkMode`/`GvproxySocket`/`VsockPort` in `WorkerConfig`, gvproxy C API branch |
| `internal/harness/main.go` | `connectToHost()`: vsock preferred over TCP/TSI |
| `internal/harness/mount_linux.go` | `setupNetwork()`: configure eth0 via `ip` commands |
| `cmd/aegisd/main.go` | `ResolveNetworkBackend()`, `ReapOrphanGvproxies()`, backend logging |
| `internal/api/server.go` | `network_backend` in status API |

### 6.2 Files Added

| File | Purpose |
|------|---------|
| `internal/vmm/gvproxy.go` | gvproxy process lifecycle: start, expose/unexpose ports, stop, orphan reaping |
| `internal/harness/vsock_linux.go` | AF_VSOCK dial using raw syscalls (no external deps) |
| `internal/harness/vsock_other.go` | Stub for non-Linux builds |

### 6.3 Files NOT Modified

The following were explicitly not touched — the changes are fully contained in the VMM layer:

- VMM interface (`vmm.go` — only `BackendCaps` extended, interface unchanged)
- Lifecycle manager (`lifecycle/manager.go`)
- Router (`router/`)
- Registry (`registry/`)
- MCP server (`cmd/aegis-mcp/`)

## 7. Design Decisions

### 7.1 Why gvproxy (not passt, not vde, not custom)

gvproxy from `containers/gvisor-tap-vsock` was chosen because:

1. **libkrun native support**: `krun_add_net_unixgram()` with `NET_FLAG_VFKIT` is designed for gvproxy's vfkit mode
2. **Podman ecosystem**: Widely used, well-maintained, macOS-native (no Linux dependencies)
3. **Built-in DNS + NAT**: No separate DNS server or iptables setup needed
4. **HTTP API for port forwarding**: Clean programmatic interface, no shell commands
5. **Single static binary**: No runtime dependencies, easy to distribute

Alternatives considered:

- **passt**: Also supported by libkrun (`krun_add_net_unixstream`), but requires host-side configuration and doesn't provide a port forwarding API
- **vmnet-helper (macOS)**: Requires elevated privileges, complex setup
- **Custom userspace stack**: Too much work, reinventing the wheel

### 7.2 Why vsock for control (not TCP-over-virtio-net)

The control channel uses vsock (mapped to a unix socket) rather than TCP over the new virtio-net NIC because:

1. **Ordering**: The control channel must be available before eth0 is configured. vsock is available immediately at boot; virtio-net requires IP configuration first.
2. **Isolation**: Control traffic stays on a separate path from data traffic. If the guest's network stack breaks, the control channel still works.
3. **Firecracker compatibility**: Firecracker also uses vsock for control. Using vsock now means the harness code is already compatible.

### 7.3 Why per-VM gvproxy (not shared)

Each VM gets its own gvproxy process rather than sharing one gvproxy across all VMs:

1. **Isolation**: A crash in one gvproxy doesn't affect other VMs
2. **Lifecycle simplicity**: gvproxy lives and dies with its VM. No reference counting, no shared state.
3. **Port forwarding scope**: Each gvproxy manages its own forwarding rules. No risk of cross-VM leakage.
4. **Resource overhead**: gvproxy is lightweight (~5MB RSS). The overhead of multiple instances is negligible.

### 7.4 Why eth0 setup uses `ip` commands (not netlink syscalls)

The plan called for netlink syscalls for reliability. We chose `ip` commands instead:

1. **Universally available**: `ip` from iproute2 or busybox is present in Alpine base rootfs and all OCI images
2. **Debuggable**: `ip` commands are human-readable in logs, unlike raw netlink
3. **Maintainable**: 10 lines of exec vs ~200 lines of raw netlink Go code
4. **Reliable enough**: The harness controls the rootfs — `ip` binary presence is guaranteed

If this proves fragile in edge cases (e.g., minimal OCI images stripping busybox), we can switch to netlink later without changing the interface.

### 7.5 Why TSI fallback (not hard requirement)

gvproxy is preferred but not required. When not found:

1. **Graceful degradation**: Existing TSI path still works for small-payload workloads
2. **Developer experience**: No new binary dependency for basic usage (echo, sleep, simple HTTP)
3. **Loud warning**: The ~32KB limit is clearly communicated at daemon startup
4. **Easy upgrade**: Install gvproxy, restart daemon, done

### 7.6 MAC address choice

The guest NIC uses a fixed MAC address `5a:94:ef:e4:0c:ee`. This is fine because:

1. Each VM has its own gvproxy (no MAC collision within a broadcast domain)
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

// 4. Add virtio-net device via gvproxy's unixgram socket
//    Guest gets a real NIC (eth0). gvproxy handles NAT/DNS/forwarding.
krun_add_net_unixgram(ctx_id, "/path/to/net-{vmID}.sock",
    -1, mac, COMPAT_NET_FEATURES, NET_FLAG_VFKIT);
```

Key: `krun_add_net_unixgram()` with `NET_FLAG_VFKIT` tells libkrun to speak the vfkit protocol to gvproxy. `COMPAT_NET_FEATURES` enables standard virtio-net offloads (checksumming, TSO).

TSI and virtio-net cannot coexist for outbound traffic — TSI hijacks AF_INET at the kernel level. Once we add a virtio-net device, TSI is disabled. Both ingress AND egress go through gvproxy.

## 9. gvproxy HTTP API

Port forwarding is managed via gvproxy's HTTP API on a unix socket:

```bash
# Expose: host:8080 → guest:80
POST /services/forwarder/expose
{"local": "127.0.0.1:8080", "remote": "192.168.127.2:80"}

# Unexpose
POST /services/forwarder/unexpose
{"local": "127.0.0.1:8080"}
```

The `local` field uses `127.0.0.1:PORT` (not `:PORT`) to bind on localhost only. This matches the existing TSI behavior where mapped ports are only accessible locally.

## 10. Verification Plan

### Basic

1. Build: `cd ~/work/aegis && make`
2. Start aegisd, verify `"network backend: gvproxy"` in startup log
3. Test small payload: `aegis run -- echo hello`
4. Test DNS: `aegis run --image alpine -- wget -qO- https://example.com`
5. Test port exposure: create instance with `--expose 80`, verify HTTP through router

### Large Payload (the original blocker)

6. 50KB POST body from inside VM → external endpoint
7. 100KB POST body (well above the TSI limit)
8. OpenClaw's 46KB API call pattern

### Resilience

9. Test without gvproxy installed → falls back to TSI with warning
10. Test pause/resume → gvproxy stays alive, traffic resumes
11. Test daemon restart → orphan gvproxies cleaned up
12. Concurrent outbound (10 parallel large POSTs) → no buffering issues

### Integration Tests

See `test/integration/network_test.go` for automated verification of:
- Network backend detection in status API
- DNS resolution from guest
- Small and large HTTP payloads from guest (egress)
- Ingress via router (small and large)
- Concurrent outbound requests

## 11. Future Work

- **Remove TSI fallback** once gvproxy is proven stable across all workloads
- **Make vsock the only control channel** (drop TCP/TSI legacy path in harness)
- **Per-VM unique MAC addresses** if multi-VM networking is added
- **gvproxy health monitoring** to detect and recover from gvproxy crashes
- **Bandwidth/rate limiting** via gvproxy configuration for multi-tenant scenarios
