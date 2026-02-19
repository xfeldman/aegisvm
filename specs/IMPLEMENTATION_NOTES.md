# Aegis Implementation Notes

**Post-implementation documentation — corrections, discoveries, and actual design choices recorded after each milestone.**

**Date:** 2026-02-19
**Depends on:** [IMPLEMENTATION_KICKOFF.md](IMPLEMENTATION_KICKOFF.md), [AEGIS_PLATFORM_SPEC.md](AEGIS_PLATFORM_SPEC.md)

Each section is written after completing the corresponding milestone. These are not plans — they are records of what was actually built and how it differs from the original engineering decisions in IMPLEMENTATION_KICKOFF.md.

---
## 1. M0 Implementation Notes

**Documented after completing M0. These are corrections and additions to the engineering decisions above, based on what we learned building against libkrun 1.17.4 on macOS ARM64 (M1).**

### 1.1 `krun_start_enter()` Takes Over the Process

**Original assumption (§1.4):** cgo wrapper around libkrun's C API, called directly from aegisd.

**Reality:** `krun_start_enter()` never returns on success. It takes over the calling process — the process becomes the VM, and `exit()` is called when the guest shuts down. This means aegisd cannot call libkrun directly.

**Solution:** A separate **`aegis-vmm-worker`** binary (Go + cgo) is spawned as a subprocess per VM. aegisd itself has **no cgo**. The worker receives its configuration via the `AEGIS_VMM_CONFIG` environment variable, configures libkrun, and calls `krun_start_enter()`. aegisd manages worker processes via `os/exec`.

```
aegisd (Go, no cgo, long-lived daemon)
  └── spawns per-VM: aegis-vmm-worker (Go + cgo, linked against libkrun)
        └── krun_start_enter() — process becomes the VM
              └── aegis-harness (PID 1 inside VM)
```

This is a significant architectural improvement over direct cgo: aegisd is a pure Go binary with no C dependencies, and VM crashes cannot take down the daemon.

### 1.2 IPC: TSI Outbound TCP, Not Vsock

**Original assumption (§1.1):** JSON-RPC 2.0 over AF_VSOCK. Host connects to guest via vsock.

**Reality:** Standard libkrun (non-EFI) uses a minimal kernel from libkrunfw. AF_VSOCK (socket family 40) is **not available** inside the guest. `krun_add_vsock_port2(..., listen=true)` creates a host-side unix socket, but the guest has no AF_VSOCK listener to receive forwarded connections.

However, libkrun's **TSI (Transparent Socket Impersonation)** transparently intercepts outbound AF_INET `connect()` calls in the guest and routes them through vsock to the host. The host then completes the actual TCP connection. This means the guest can reach `127.0.0.1:PORT` on the host.

**Solution:** Invert the connection direction. aegisd starts a TCP listener on `127.0.0.1:0` (random port) per task. The port is passed to the guest harness via the `AEGIS_HOST_ADDR` environment variable. The harness connects **outbound** to the host listener via TSI. JSON-RPC 2.0 flows over this TCP connection.

```
aegisd                                    Guest VM
  │                                         │
  ├── net.Listen("tcp", "127.0.0.1:0")     │
  │   → listening on :59123                 │
  │                                         │
  ├── spawn vmm-worker (AEGIS_HOST_ADDR=    │
  │     "127.0.0.1:59123")                  │
  │                                         │
  │                                    harness starts
  │                                    net.Dial("tcp",
  │                              ←──── "127.0.0.1:59123")
  │                                    (TSI intercepts,
  │                                     routes via vsock)
  │                                         │
  ├── ln.Accept() → conn                    │
  │                                         │
  ├── conn.Write(runTask RPC) ────────────► │
  │                                    execute command
  │   ◄──────────── log notifications ──── │
  │   ◄──────────── result ──────────────── │
  │                                         │
  └── conn.Write(shutdown RPC) ───────────► exit
```

The JSON-RPC protocol is unchanged from the spec. Only the transport and connection direction differ.

This is clean, backend-specific but abstractable, and not leaky to core. Firecracker can use AF_VSOCK directly when it arrives in M4 — same protocol, different transport.

### 1.2.1 ControlChannel Abstraction

To prevent transport logic from leaking into core, the VMM interface defines a `ControlChannel`:

```go
type ControlChannel interface {
    Send(msg []byte) error
    Recv() ([]byte, error)
    Close() error
}
```

`StartVM` returns a ready-to-use `ControlChannel`. Core code (task runner, future lifecycle manager) calls `Send`/`Recv`/`Close` — it never sees TCP, vsock, or unix sockets.

- **libkrun backend** returns a TCP-backed channel (via `NetControlChannel` wrapping `net.Conn`)
- **Firecracker backend** (M4) will return a vsock-backed channel (same wrapper, different `net.Conn`)
- Any future backend provides its own implementation

The concrete implementation (`NetControlChannel`) wraps any `net.Conn` with newline-delimited framing, so it works for both TCP and vsock connections.

### 1.2.2 RootFS Abstraction

Similarly, rootfs format is backend-specific and must not leak into core:

```go
type RootFSType int
const (
    RootFSDirectory  RootFSType = iota  // libkrun: host directory via krun_set_root
    RootFSBlockImage                     // Firecracker: raw ext4 block device
)

type RootFS struct {
    Type RootFSType
    Path string
}
```

`VMConfig.Rootfs` uses this type. `BackendCaps.RootFSType` declares what the backend expects. The image pipeline (M2+) produces the right artifact based on the active backend's declared type. Core never assumes ext4 or directory — it asks the backend what it needs.

### 1.3 Kernel Cmdline 2048-Byte Limit

**Not in original spec.**

On aarch64, libkrun embeds **all environment variables** into the Linux kernel command line, which has a hard limit of 2048 bytes. Passing `NULL` for the `envp` parameter of `krun_set_exec()` inherits the host's full environment (typically 4-8KB on macOS), causing a `TooLarge` panic in `vmm::builder::build_microvm`.

**Solution:** Always pass an explicit minimal environment to `krun_set_exec()`:

```go
envVars := []string{
    "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "HOME=/root",
    "TERM=linux",
    fmt.Sprintf("AEGIS_HOST_ADDR=%s", hostAddr),
}
```

This is a libkrun-specific constraint. Firecracker does not embed env vars in the kernel cmdline. Future env-heavy features (secret injection, kit env) will need to use an alternative channel (e.g., a file passed via virtiofs) instead of `krun_set_exec` env on the libkrun backend.

### 1.4 macOS Hypervisor Entitlement

**Not in original spec.**

On macOS, Apple's Hypervisor.framework requires the calling binary to be codesigned with the `com.apple.security.hypervisor` entitlement. Without it, `krun_start_enter()` fails with `VmSetup(VmCreate)` (errno -22).

The vmm-worker binary must be signed after every build:

```bash
codesign --sign - --entitlements entitlements.plist --force bin/aegis-vmm-worker
```

This is handled automatically by the Makefile on macOS.

### 1.5 libkrunfw Runtime Loading

**Not in original spec.**

libkrun dynamically loads `libkrunfw` (the bundled kernel) via `dlopen()` at runtime. If the library path is not in the default search path, the VM fails to start silently or with an opaque error.

**Solution:** aegisd sets `DYLD_FALLBACK_LIBRARY_PATH=/opt/homebrew/lib:/usr/local/lib:/usr/lib` when spawning the vmm-worker process.

### 1.6 Directory-Based Rootfs (libkrun Standard Mode)

**Original assumption (§1.6):** ext4 image via `mkfs.ext4 -d`.

**Reality:** libkrun's standard (non-EFI) mode uses `krun_set_root(ctx, directory_path)` which takes a **host directory** as the root filesystem (chroot-style, exposed to the guest via virtio-fs). It does not consume block device images.

**Solution for M0:** Build the rootfs as a directory (Alpine ARM64 extracted from Docker), not an ext4 image. The harness binary is placed at `rootfs/usr/bin/aegis-harness`. The directory is stored at `~/.aegis/base-rootfs/`.

ext4 images will be needed for Firecracker (M4) which requires raw block devices. The base rootfs build produces a directory; `make ext4` in `base/` converts it to an ext4 image when needed.

Note: macOS `cp` cannot handle busybox symlinks correctly. Use `tar cf - -C rootfs . | tar xf - -C target/` for copying the rootfs.

### 1.7 M0 Dependencies (Actual)

```
macOS (confirmed working):
  go 1.26+
  libkrun 1.17.4 (via: brew tap slp/krun && brew install libkrun)
  Docker (for base rootfs build only — Alpine image extraction)
  Xcode command line tools (codesign)

Not needed for M0 (deferred):
  e2fsprogs / mkfs.ext4 (only needed for Firecracker ext4 images, M4)
  krunvm (optional CLI, not used by aegis)
```

### 1.8 Project Structure (Actual, M0)

The project structure matches §3 with one addition — `cmd/aegis-vmm-worker/`:

```
aegis/
├── cmd/
│   ├── aegisd/main.go              # Daemon (no cgo)
│   ├── aegis/main.go               # CLI
│   ├── aegis-harness/main.go       # Guest PID 1 (linux/arm64, static)
│   └── aegis-vmm-worker/main.go    # Per-VM helper (cgo, libkrun)
├── internal/
│   ├── vmm/vmm.go                  # VMM interface (frozen)
│   ├── vmm/libkrun.go              # LibkrunVMM (spawns worker subprocesses)
│   ├── harness/                    # Guest harness (main, rpc, exec)
│   ├── api/                        # aegisd HTTP API (server, tasks)
│   └── config/                     # Config + platform detection
├── base/Makefile                   # Base rootfs build
├── entitlements.plist              # macOS hypervisor entitlement
├── Makefile                        # Top-level build
└── CLAUDE.md
```

---

## 2. M1 Implementation Notes

**Documented after completing M1. These are design choices and corrections to §1.8, §1.9, §1.11 based on building serve mode against the M0 codebase and libkrun 1.17.4 on macOS ARM64.**

### 2.1 Port Mapping via `krun_set_port_map`, Not Custom Tunneling

**Original assumption (§1.11):** libkrun networking would need investigation; possibly insufficient for serve mode.

**Reality:** libkrun's TSI has a port mapping API (`krun_set_port_map`) that controls exactly which guest listening ports are exposed on which host ports. Without calling it, all guest listening ports are exposed on the same port number on the host (which causes conflicts with multiple VMs). With explicit mappings, we get predictable, conflict-free host ports.

**Solution:** At VM creation, aegisd allocates a random available host port per exposed guest port (bind `:0`, read the assigned port, close). These mappings are passed to the vmm-worker as `["8234:80"]` (host:guest). The worker calls `krun_set_port_map()` before `krun_start_enter()`.

```
aegisd                          vmm-worker                   Guest VM
  │                                │                           │
  ├─ allocate host port            │                           │
  │  net.Listen(":0") → :8234     │                           │
  │  close listener                │                           │
  │                                │                           │
  ├─ WorkerConfig.PortMap =        │                           │
  │    ["8234:80"]                 │                           │
  │                                │                           │
  │                                ├─ krun_set_port_map()      │
  │                                ├─ krun_start_enter()       │
  │                                │                           │
  │                                │                    python -m http.server 80
  │                                │                    (listens on :80)
  │                                │                           │
  curl 127.0.0.1:8234 ──────────────────── TSI ──────────────► :80
```

The traffic path is standard TCP — no multiplexing, no custom tunneling. The router proxies to `127.0.0.1:{mapped_port}` and the request reaches the guest server transparently.

This also means no `internal/network/` package was needed for M1. libkrun's built-in TSI + port mapping handles everything on macOS. The network package remains deferred to M4 (Firecracker TAP/bridge).

### 2.2 Pause/Resume via SIGSTOP/SIGCONT, Not libkrun API

**Original assumption (§1.5):** PauseVM/ResumeVM implemented via the VMM backend's native API.

**Reality:** libkrun's C API does not expose pause/resume. But each VM is a vmm-worker subprocess (§1.1), and `SIGSTOP` freezes the entire process — vCPU threads, TSI networking, everything. `SIGCONT` resumes it. The guest doesn't know it was paused. RAM stays allocated. TSI port mappings survive.

**Solution:**

```go
func (l *LibkrunVMM) PauseVM(h Handle) error {
    return inst.cmd.Process.Signal(syscall.SIGSTOP)
}

func (l *LibkrunVMM) ResumeVM(h Handle) error {
    return inst.cmd.Process.Signal(syscall.SIGCONT)
}
```

This gives sub-second resume without any libkrun API changes. `Capabilities().Pause` is now `true` for the libkrun backend.

This approach is macOS/libkrun-specific. Firecracker (M4) has native `Pause`/`Resume` in its API, so the FirecrackerVMM implementation will use the real thing. The VMM interface abstracts the difference — core doesn't know or care whether pause means SIGSTOP or a hypervisor-level vCPU halt.

### 2.3 VMM Interface Extensions for Serve Mode

**Two additions to the frozen VMM interface:**

```go
// In VMConfig (input to CreateVM):
type PortExpose struct {
    GuestPort int
    Protocol  string  // "http", "tcp", "grpc"
}

VMConfig.ExposePorts []PortExpose  // new field

// New method on VMM interface:
HostEndpoints(h Handle) ([]HostEndpoint, error)

type HostEndpoint struct {
    GuestPort int
    HostPort  int
    Protocol  string
}
```

`ExposePorts` tells the backend which guest ports to map. `HostEndpoints` returns the resolved mappings after `StartVM` completes (the backend allocates host ports). This keeps port allocation backend-specific — Firecracker might use different port forwarding mechanisms.

The interface remains backward-compatible: `ExposePorts` is nil for task mode (no port mapping), and `HostEndpoints` returns an empty slice for VMs without exposed ports.

### 2.4 Harness `startServer` RPC — Long-Lived Processes

**New capability.** M0 harness only had `runTask` (block until process exits). Serve mode needs `startServer` — start a background process and keep it running.

**Key differences from `runTask`:**

| | `runTask` | `startServer` |
|---|---|---|
| Returns | After process exits | Immediately after process starts |
| Process lifetime | Request-scoped | Lives until shutdown |
| Readiness | N/A | Polls `readiness_port` via TCP connect |
| Notification | None | `serverReady` or `serverFailed` |

The harness now tracks server processes via a `serverTracker` and kills them all on shutdown. The readiness probe (`waitForPort`) does TCP connect attempts with 200ms backoff for up to 30 seconds, then sends a `serverReady` notification to the host.

```json
// Request
{"jsonrpc":"2.0","method":"startServer","params":{"command":["python","-m","http.server","80"],"readiness_port":80},"id":1}

// Immediate response (process started)
{"jsonrpc":"2.0","result":{"pid":42},"id":1}

// Async notification (port accepting connections)
{"jsonrpc":"2.0","method":"serverReady","params":{"port":80}}
```

### 2.5 Lifecycle Manager — State Machine + Idle Timers

**New package: `internal/lifecycle/manager.go`**

The lifecycle manager owns all serve-mode instances and drives transitions:

```
STOPPED → STARTING → RUNNING ⇄ PAUSED → STOPPED
```

Key design choices:

**1. Idempotent `EnsureInstance(id)`** — the single entry point for the router. If stopped → boot. If paused → SIGCONT. If running → noop. If starting → block until running (with ctx timeout). The router never needs to know the current state; it just calls `EnsureInstance` and gets back either success (instance is running) or an error. The router's request context carries a 30s timeout, so if boot takes too long the call fails and the router serves a loading page (HTML with meta-refresh) or 503+Retry-After.

**2. Connection-counted idle** — the router calls `ResetActivity(id)` on each new connection and `OnConnectionClose(id)` when it ends. The idle timer only starts when active connections drop to zero. This prevents the VM from pausing while requests are in flight.

**3. Two-stage idle shutdown:**

| Timer | Default | Action |
|---|---|---|
| `PauseAfterIdle` | 60s | SIGSTOP the worker process |
| `TerminateAfterIdle` | 20min | StopVM (kill process, free resources), state → STOPPED |

The pause timer starts when the last connection closes. If a new request arrives while paused, SIGCONT resumes in <100ms and the terminate timer is cancelled. If no request arrives for 20 minutes, the VM is stopped and resources freed — but the instance stays in the map as STOPPED. The next request reboots it from scratch (true scale-to-zero). Explicit user stop (`StopInstance` / `DELETE /v1/instances/{id}`) removes the instance entirely.

**4. State change callbacks** — the manager fires `onStateChange(id, state)` on every transition. aegisd hooks this to persist state to the SQLite registry.

### 2.6 Router — Simpler Than Spec

**Original assumption (§1.8):** Path-based routing (`/app/{appId}/...`), WebSocket via `nhooyr.io/websocket`, per-protocol wake behavior matrix.

**Reality for M1:** Single-instance routing. All traffic to `:8099` goes to the one active serve instance. No path parsing, no app ID resolution. This is correct for M1 — multi-app routing is an M2 concern.

**Simplifications over the spec:**

- **No external dependencies** — WebSocket upgrade handled via `net.Conn` hijack + bidirectional `io.Copy`, not `nhooyr.io/websocket`. Raw TCP proxying is sufficient for WebSocket since we're just forwarding bytes.
- **No per-protocol wake behavior matrix** — all protocols get the same treatment: ensure instance is running, then proxy. If the instance is booting, HTML clients get a loading page with `<meta http-equiv="refresh" content="3">`, non-HTML clients get `503 + Retry-After: 3`.
- **Routing lookup is trivial** — `GetDefaultInstance()` returns the first instance in the map. Path-based resolution comes in M2.

The router embeds in aegisd as designed (§1.8) — it's a goroutine with an `http.Server`, shares the lifecycle manager in-process.

### 2.7 SQLite Registry — Minimal M1 Schema

**Original assumption (§1.9):** Six tables (apps, releases, instances, kits, secrets, workspaces).

**Reality:** M1 only needs `instances`. Apps, releases, kits, secrets, and workspaces are all M2+ concerns. Shipping unused tables would be premature — the schema will evolve as those features are built.

```sql
CREATE TABLE IF NOT EXISTS instances (
    id          TEXT PRIMARY KEY,
    state       TEXT NOT NULL DEFAULT 'stopped',
    command     TEXT NOT NULL,         -- JSON array
    expose_ports TEXT NOT NULL DEFAULT '[]',  -- JSON array of ints
    vm_id       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
```

**Implementation choice: `modernc.org/sqlite`** — pure Go SQLite, no cgo. This is the first (and so far only) external dependency. The entire module is `CGO_ENABLED=0` compatible, keeping aegisd a pure Go binary. WAL mode enabled for concurrent read performance.

DB path: `~/.aegis/data/aegis.db`.

### 2.8 CLI `--expose` Flag

**New:** `aegis run --expose PORT -- <command>` enters serve mode. Without `--expose`, behavior is unchanged (task mode).

```bash
# Serve mode (M1)
aegis run --expose 80 -- python -m http.server 80
# → Serving on http://127.0.0.1:8099
# → Instance: inst-1739893456
# → Press Ctrl+C to stop

# Task mode (M0, unchanged)
aegis run -- echo hello
# → hello
```

Serve mode creates an instance via `POST /v1/instances`, prints the router URL, and blocks until Ctrl+C. On interrupt it sends `DELETE /v1/instances/{id}` for clean shutdown.

Multiple `--expose` flags can be specified to expose multiple ports (e.g., `--expose 80 --expose 443`).

### 2.9 API Additions

Three new routes on the unix socket API:

| Method | Path | Purpose |
|---|---|---|
| `POST /v1/instances` | Create + boot a serve instance | `{"command": [...], "expose_ports": [80]}` |
| `GET /v1/instances/{id}` | Get instance state | Returns `{id, state}` |
| `DELETE /v1/instances/{id}` | Stop + remove instance | Sends shutdown RPC, kills VM |

Task routes (`/v1/tasks/*`) are unchanged and continue to work for task mode.

### 2.10 aegisd Init Sequence (M1)

```go
cfg := config.DefaultConfig()
backend := vmm.NewLibkrunVMM(cfg)
reg := registry.Open(cfg.DBPath)           // new: SQLite
lm := lifecycle.NewManager(backend, cfg)    // new: state machine
rtr := router.New(lm, cfg.RouterAddr)       // new: HTTP proxy on :8099
server := api.NewServer(cfg, backend, lm, reg)

rtr.Start()
server.Start()
// ... wait for signal ...
lm.Shutdown()    // stops all VMs
rtr.Stop()
server.Stop()
reg.Close()
```

The lifecycle manager's `onStateChange` callback persists state transitions to the registry. On shutdown, `lm.Shutdown()` stops all running/paused instances before the registry and router close.

### 2.11 Project Structure (Actual, M1)

Three new packages, no new binaries:

```
aegis/
├── cmd/
│   ├── aegisd/main.go              # + lifecycle, registry, router init
│   ├── aegis/main.go               # + --expose flag, serve mode
│   ├── aegis-harness/main.go       # unchanged
│   └── aegis-vmm-worker/main.go    # + krun_set_port_map
├── internal/
│   ├── vmm/vmm.go                  # + PortExpose, HostEndpoint, HostEndpoints()
│   ├── vmm/libkrun.go              # + port mapping, SIGSTOP/SIGCONT, endpoints
│   ├── vmm/channel.go              # unchanged
│   ├── harness/                    # + startServer RPC, server tracker, waitForPort
│   ├── api/                        # + instance CRUD routes
│   ├── config/                     # + RouterAddr, DBPath, idle timeouts
│   ├── lifecycle/manager.go        # NEW: state machine, idle timers
│   ├── router/router.go            # NEW: HTTP proxy, wake-on-connect
│   └── registry/                   # NEW: SQLite, instances CRUD
│       ├── db.go
│       └── instances.go
├── go.mod                          # + modernc.org/sqlite
├── go.sum                          # NEW
└── ...
```

### 2.12 M1 Dependencies (Actual)

```
macOS (confirmed working):
  All M0 dependencies (go 1.26+, libkrun, Docker, codesign)
  + modernc.org/sqlite v1.46.1 (pure Go, no cgo — first external dep)

Not needed for M1 (deferred):
  nhooyr.io/websocket (raw hijack sufficient)
  internal/network/ package (TSI handles everything)
```

---

## 3. M2 Implementation Notes

**Documented after completing M2. These are design choices and corrections to §1.2, §1.3, §1.9, and the M2 milestone table, based on building the image pipeline, app/release system, and multi-app routing on top of the M1 codebase.**

### 3.1 OCI Image Pipeline — Directory-Based, Not ext4

**Original assumption (§1.2):** `go-containerregistry` for pull/unpack, `mkfs.ext4 -d` to produce an ext4 rootfs image.

**Reality:** libkrun uses directory-based rootfs via `krun_set_root()` (§1.6). There is no need for ext4 image creation on macOS. The image pipeline unpacks OCI layers directly into a directory, and libkrun mounts it via virtiofs.

**Actual pipeline:**

1. `image.Pull(ctx, "python:3.12")` → resolves reference, pulls linux/arm64 manifest via `go-containerregistry`, handles both single-platform images and multi-platform index manifests
2. `image.Unpack(img, destDir)` → extracts layers in order into a directory tree, handles OCI whiteout files (`.wh.` prefix for file deletion, `.wh..wh..opq` for opaque directory replacement)
3. `image.Cache` → digest-keyed directory cache at `~/.aegis/data/images/sha256_{digest}/`. `GetOrPull(ctx, ref)` returns the cached directory or pulls + unpacks + caches atomically (via tmp dir + rename)
4. `image.InjectHarness(rootfsDir, harnessBin)` → copies the harness binary into the rootfs at `/usr/bin/aegis-harness`

Harness injection happens on the **release copy**, not the cache. The cache contains the clean OCI image; each release gets its own copy with the harness baked in. Any existing `/usr/bin/aegis-harness` in the OCI image is intentionally overwritten.

**PID 1 guarantee:** `krun_set_exec()` always runs `/usr/bin/aegis-harness` as guest PID 1, regardless of the OCI image's `ENTRYPOINT` or `CMD`. The image's entrypoint is ignored — the harness starts user commands via RPC (`runTask`/`startServer`). This is by design: the harness must be PID 1 for signal handling, mount setup, and host communication.

**Platform invariant:** `Pull()` enforces linux/arm64. For multi-platform index manifests, it selects the linux/arm64 variant and fails with "no linux/arm64 variant found" if absent. For single-manifest images, it validates the config's `OS` and `Architecture` fields after pull — a `linux/amd64` image will fail with an explicit error rather than unpacking successfully and crashing at VM boot with an opaque exec format error.

ext4 conversion (`mkfs.ext4 -d`) is deferred to M4 when Firecracker needs block device images. The existing `BackendCaps.RootFSType` abstraction handles this — the image pipeline will produce the right artifact based on the active backend's declared type.

### 3.2 Overlay — tar Pipe, Not cp

**Original assumption (§1.3):** Full rootfs copy per release on macOS.

**Reality:** Correct — macOS has no device-mapper, so each release gets a full copy. But the copy method matters: macOS `cp -a` breaks busybox-style symlink layouts (§1.6). The `CopyOverlay` implementation uses a tar pipe:

```bash
tar -C source -cf - . | tar -C dest -xf -
```

This preserves all symlinks, hardlinks, and permissions correctly. The overlay interface is simple:

```go
type Overlay interface {
    Create(ctx context.Context, sourceDir, destID string) (path, error)
    Remove(id string) error
    Path(id string) string
}
```

`CopyOverlay` stores copies under `~/.aegis/data/releases/{releaseID}/`. The `dm.go` (device-mapper) implementation is deferred to M4.

**Atomic create:** `Create()` writes into a `.tmp` staging directory, then does an atomic `os.Rename()` to the final path. A crash during the tar copy leaves only the staging dir, which is cleaned up by `CleanStale()` on daemon restart. The DB insert happens only after a successful `Create()`, so a crash can never leave a registry entry pointing to a half-baked rootfs.

### 3.3 Registry Schema — apps + releases Tables

**Original assumption (§1.9):** Six tables. M1 shipped with just `instances`.

**M2 adds two tables:**

```sql
CREATE TABLE IF NOT EXISTS apps (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    image       TEXT NOT NULL,
    command     TEXT NOT NULL DEFAULT '[]',
    expose_ports TEXT NOT NULL DEFAULT '[]',
    config      TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS releases (
    id           TEXT PRIMARY KEY,
    app_id       TEXT NOT NULL REFERENCES apps(id),
    image_digest TEXT NOT NULL,
    rootfs_path  TEXT NOT NULL,
    label        TEXT,
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
```

Key differences from the spec schema (§1.9):

- `apps.image` stores the OCI image reference directly (e.g., `python:3.12`), not a kit reference. Kits are M3.
- `apps.command` and `apps.expose_ports` are stored as JSON arrays — the app definition includes everything needed to serve.
- `releases.rootfs_path` is the absolute path to the release's rootfs directory on the host. On macOS this is a directory; on Linux (M4+) it could be a dm-snapshot ref.
- `releases.overlay_ref` from §1.9 is replaced by `rootfs_path` — simpler and more explicit.
- No `base_revision` column — the image digest serves the same purpose (identifying what the release was built from).
- `kits`, `secrets`, `workspaces` tables deferred to M3.

Migration is idempotent — `CREATE TABLE IF NOT EXISTS` runs on every `registry.Open()`.

### 3.4 Workspace Volumes via virtiofs

**Original assumption (§1.3):** Workspace volume is bind-mounted, separate from rootfs.

**Reality:** libkrun supports `krun_add_virtiofs(ctx, tag, path)` which creates an independent virtio-fs device pointing to a host directory. The guest mounts it by tag.

**Implementation:**

1. **Host side:** `LibkrunVMM.StartVM()` checks `VMConfig.WorkspacePath`. If set, it passes `"workspace:/path"` in the `MappedVolumes` field of `WorkerConfig`. The vmm-worker calls `krun_add_virtiofs(ctx, "workspace", path)`.

2. **Guest side:** The harness checks `AEGIS_WORKSPACE=1` (set by vmm-worker when volumes are configured). If set, it mounts the `workspace` virtiofs tag at `/workspace` and **fails fatally** if the mount fails — preventing silent data-loss bugs where files appear to write but don't persist. If `AEGIS_WORKSPACE` is not set, the harness skips the mount entirely. The mount code is in `mount_linux.go` (build-tagged, no-op on macOS).

3. **App workspaces:** Each app gets a workspace directory at `~/.aegis/data/workspaces/{appID}/`. This is created when `aegis app serve` is called and passed to the instance via `lifecycle.WithWorkspace(path)`.

### 3.5 Lifecycle Manager — Functional Options for Instances

**Change:** `CreateInstance` now accepts variadic `InstanceOption` functions:

```go
func (m *Manager) CreateInstance(id string, cmd []string, ports []vmm.PortExpose, opts ...InstanceOption) *Instance

// Available options:
lifecycle.WithApp(appID, releaseID)     // associate with an app
lifecycle.WithRootfs(path)              // custom rootfs (instead of base)
lifecycle.WithWorkspace(path)           // workspace volume
```

The `Instance` struct gained four new fields: `AppID`, `ReleaseID`, `RootfsPath`, `WorkspacePath`. `bootInstance()` uses `inst.RootfsPath` if set, otherwise falls back to `cfg.BaseRootfsPath`. This preserves M1 backward compatibility — existing code that calls `CreateInstance(id, cmd, ports)` without options gets the base rootfs.

New method: `GetInstanceByApp(appID) *Instance` — used by the router and serve handler to find the running instance for an app.

### 3.6 Multi-App Router — Path + Header Routing with Fallback

**Original assumption (§1.8):** Path-based routing (`/app/{appId}/...`).

**M2 reality:** The router supports two app resolution methods, plus a fallback:

```go
type AppResolver interface {
    GetAppByName(name string) (appID string, ok bool)
}
```

Resolution order on each request:

1. **Path prefix:** `/app/{name}/...` → strip `/app/{name}` prefix, route to named app. The backend sees the path remainder (e.g., `/app/myapp/status` → backend sees `/status`).
2. **Header:** `X-Aegis-App: myapp` → route to named app (useful for programmatic clients).
3. **Default fallback:** `GetDefaultInstance()` — first instance in the map (M1 backward compat).

The default fallback means `curl http://127.0.0.1:8099/` works when a single app is served. **Multi-app concurrent serve** requires path or header routing — plain root requests go to the default instance, which is non-deterministic with multiple apps.

The resolver is implemented as a thin adapter wrapping the registry DB in `cmd/aegisd/main.go`.

### 3.7 `--image` Flag on `aegis run`

**New:** `aegis run --image alpine:3.21 -- echo hello` pulls an OCI image, creates a temporary rootfs with the harness injected, runs the task, and cleans up.

The flow in `TaskStore.runTask()`:

1. If `req.Image` is set → `imageCache.GetOrPull(ctx, image)` → get cached rootfs directory
2. `overlay.Create(ctx, cachedDir, "task-"+taskID)` → full copy to temp release dir
3. `image.InjectHarness(overlayDir, harnessBin)` → bake harness into temp rootfs
4. Use temp rootfs as `VMConfig.Rootfs.Path`
5. After task completes → `overlay.Remove("task-"+taskID)` (deferred cleanup)

Without `--image`, behavior is unchanged — the base rootfs is used.

**Crash resilience:** On daemon startup, `CopyOverlay.CleanStaleTasks(1h)` scans the releases directory for `task-*` entries older than 1 hour and removes them. This prevents disk leaks from crashed tasks that didn't run their deferred cleanup.

### 3.8 App Lifecycle — Publish + Serve

**New API routes (7 total):**

| Method | Path | Description |
|--------|------|-------------|
| `POST /v1/apps` | Create app | `{name, image, command, expose_ports}` |
| `GET /v1/apps` | List apps | |
| `GET /v1/apps/{id}` | Get app (by ID or name) | |
| `DELETE /v1/apps/{id}` | Delete app + releases + stop instances | |
| `POST /v1/apps/{id}/publish` | Publish release | Pull → overlay → inject → record |
| `GET /v1/apps/{id}/releases` | List releases | |
| `POST /v1/apps/{id}/serve` | Serve app | Boot from latest release |

**Publish flow** (`handlePublishApp`):

1. Resolve app → get image reference
2. `imageCache.GetOrPull(ctx, app.Image)` → cached or freshly pulled rootfs directory
3. `overlay.Create(ctx, cachedDir, releaseID)` → full copy to release dir
4. `image.InjectHarness(releaseDir, harnessBin)` → bake harness into release copy
5. `registry.SaveRelease(release)` → record in DB with digest, rootfs path, optional label

**Serve flow** (`handleServeApp`):

1. Resolve app → get latest release → verify it exists
2. Create workspace directory at `~/.aegis/data/workspaces/{appID}/`
3. `lifecycle.CreateInstance(id, cmd, ports, WithApp(...), WithRootfs(...), WithWorkspace(...))`
4. Boot instance asynchronously → scale-to-zero via existing idle timers
5. If already serving → return existing instance info (idempotent)

**App resolution:** All app endpoints accept either app ID (`app-1739...`) or app name (`myapp`) — the handler tries by ID first, then by name. This makes the CLI ergonomic: `aegis app publish myapp` works.

### 3.9 CLI App Commands

**New subcommand group:** `aegis app <subcommand>`

```bash
aegis app create --name myapp --image python:3.12 --expose 80 -- python -m http.server 80
aegis app publish myapp [--label v1]
aegis app serve myapp         # blocks until Ctrl+C
aegis app list                # table format: NAME, IMAGE, ID
aegis app info myapp          # details + release list
aegis app delete myapp
```

`aegis app serve` behaves like `aegis run --expose` — it prints the router address, blocks until Ctrl+C, then cleans up the instance. The app and its releases persist across serve sessions.

### 3.10 Config Additions

Three new paths in `config.Config`:

```go
ImageCacheDir  string  // ~/.aegis/data/images     — digest-keyed OCI cache
ReleasesDir    string  // ~/.aegis/data/releases    — release rootfs copies
WorkspacesDir  string  // ~/.aegis/data/workspaces  — app workspace volumes
```

All three directories are created by `cfg.EnsureDirs()` at daemon startup.

### 3.11 Harness — Rootfs Immutability + Platform-Specific Mounts

**Rootfs immutability:** libkrun's `krun_set_root()` exposes the host release directory via virtiofs **read-write**. Without protection, any guest write to `/usr/`, `/etc/`, etc. mutates the release directory on the host, breaking immutability.

The harness enforces immutability via `mountEssential()`:

1. Mount writable tmpfs on `/tmp`, `/run`, `/var` (these need writes)
2. Mount `/proc`
3. **Remount `/` read-only** (`MS_REMOUNT | MS_RDONLY`)

The read-only remount only affects the root virtiofs — `/tmp`, `/run`, `/var`, and `/workspace` are separate mounts and remain writable. Guest processes that try to write outside these directories get `EROFS` (Read-only file system).

The mount code uses `syscall.Mount` which is Linux-only. To keep `go build ./internal/...` working on macOS (for development), the code is split into build-tagged files:

- `mount_linux.go` — real implementations using `syscall.Mount`
- `mount_other.go` — no-op stubs (`//go:build !linux`)

The harness binary is always built with `GOOS=linux GOARCH=arm64`, so the real mount code is used inside VMs. The stubs exist only to satisfy the Go compiler when building on macOS.

### 3.12 aegisd Init Sequence (M2)

```go
cfg := config.DefaultConfig()
backend := vmm.NewLibkrunVMM(cfg)
reg := registry.Open(cfg.DBPath)
imgCache := image.NewCache(cfg.ImageCacheDir)          // new
ov := overlay.NewCopyOverlay(cfg.ReleasesDir)           // new
lm := lifecycle.NewManager(backend, cfg)
rtr := router.New(lm, cfg.RouterAddr, &appResolver{reg}) // new: app resolver
server := api.NewServer(cfg, backend, lm, reg, imgCache, ov) // new: image + overlay

rtr.Start()
server.Start()
// ... wait for signal ...
lm.Shutdown()
rtr.Stop()
server.Stop()
reg.Close()
```

### 3.13 Project Structure (Actual, M2)

Three new packages, no new binaries:

```
aegis/
├── cmd/
│   ├── aegisd/main.go              # + image cache, overlay, app resolver init
│   ├── aegis/main.go               # + --image flag, app subcommand group
│   ├── aegis-harness/main.go       # unchanged
│   └── aegis-vmm-worker/main.go    # + krun_add_virtiofs for workspace volumes
├── internal/
│   ├── vmm/vmm.go                  # unchanged
│   ├── vmm/libkrun.go              # + MappedVolumes in WorkerConfig, workspace path
│   ├── vmm/channel.go              # unchanged
│   ├── harness/main.go             # mount code extracted to platform files
│   ├── harness/mount_linux.go      # NEW: mountEssential + mountWorkspace
│   ├── harness/mount_other.go      # NEW: no-op stubs for macOS builds
│   ├── harness/rpc.go              # unchanged
│   ├── harness/exec.go             # unchanged
│   ├── api/server.go               # + imageCache, overlay fields; new app routes
│   ├── api/tasks.go                # + Image field, image pull in runTask
│   ├── api/apps.go                 # NEW: app CRUD + publish + serve handlers
│   ├── config/config.go            # + ImageCacheDir, ReleasesDir, WorkspacesDir
│   ├── config/platform.go          # unchanged
│   ├── lifecycle/manager.go        # + InstanceOption, app fields, GetInstanceByApp
│   ├── router/router.go            # + AppResolver interface, multi-app routing
│   ├── registry/db.go              # + apps, releases tables in migration
│   ├── registry/instances.go       # unchanged
│   ├── registry/apps.go            # NEW: App CRUD
│   ├── registry/releases.go        # NEW: Release CRUD
│   ├── image/                      # NEW: OCI image pipeline
│   │   ├── pull.go                 #   Pull with platform resolution
│   │   ├── unpack.go               #   Layer extraction + whiteout handling
│   │   └── cache.go                #   Digest-keyed cache + InjectHarness
│   └── overlay/                    # NEW: rootfs copy management
│       ├── overlay.go              #   Overlay interface
│       └── copy.go                 #   tar-pipe CopyOverlay
├── test/integration/
│   ├── helpers_test.go             # unchanged
│   ├── m0_test.go                  # unchanged
│   ├── m1_test.go                  # unchanged
│   └── m2_test.go                  # NEW: image pull, app lifecycle, cache tests
├── go.mod                          # + github.com/google/go-containerregistry
└── ...
```

### 3.14 M2 Dependencies (Actual)

```
macOS (confirmed working):
  All M0 + M1 dependencies
  + github.com/google/go-containerregistry v0.20.7 (pure Go, no cgo)
    → OCI image pull/manifest/layer handling
    → transitive deps: docker/cli, opencontainers/image-spec, etc.

Not needed for M2 (deferred):
  mkfs.ext4 / e2fsprogs (only for Firecracker ext4 images, M4)
  device-mapper / dmsetup (Linux COW overlays, M4)
```

---

## 4. M3 Implementation Notes

**Documented after completing M3. These are design choices and corrections to §1.7, §1.10, and the M3 milestone table, based on building the secret, kit, and conformance systems on top of the M2 codebase.**

### 4.1 Secret Injection — Via Existing `env` RPC Field, Not `injectSecrets`

**Original assumption (§1.7):** Harness stores secrets in an in-memory map, received via a dedicated `injectSecrets` RPC. Harness applies them at `execve` time.

**Reality for M3:** No harness changes were needed. Secrets are decrypted on the host and merged into the `env` map of the existing `runTask`/`startServer` RPC params. The harness already passes `env` to child processes via `execve` — secrets arrive as normal environment variables.

**This approach is valid only while "restore" means "cold boot from disk layers."** In M3, restore always means: stop VM → boot fresh VM from release rootfs → send new `startServer` RPC with full env. There is no memory snapshot restore. Specifically:

- **Terminate → restore** = cold boot. A new `startServer` RPC is sent with the full env (including secrets). The harness is a fresh process; no stale state.
- **Pause → resume** = memory retained (SIGSTOP/SIGCONT). The child process keeps its environment in memory. No re-injection needed because nothing was lost.

**When memory snapshot restore arrives (M4+), this model breaks.** Snapshot restore reconstitutes a running harness from a memory image — the harness is already alive, child processes may already be running, and there is no `startServer` RPC to carry env. At that point, one of two approaches is needed:

1. **`injectSecrets` RPC** (§1.7's original design): send secrets to the running harness via a dedicated RPC; harness stores them in memory and applies them to any newly spawned child processes. Already-running children would not receive updated secrets without a restart.
2. **"Restart server process with env" contract**: after snapshot restore, the harness kills and re-spawns the server process with a fresh `execve` that includes the current secrets. Simpler than option 1 but adds a cold-start penalty to snapshot restore.

The choice between these is deferred to M4. For M3's cold-boot-only model, merging into the existing `env` field is correct and simpler.

**Bug fix:** The `startServer` RPC in `lifecycle/manager.go` was not passing `env` at all — the `"env"` key was missing from the RPC params. This meant secrets (and any environment variables) were silently dropped in serve mode. Fixed by adding `"env": inst.Env` to the `startServer` RPC params.

### 4.2 Secret Encryption — AES-256-GCM, Auto-Generated Master Key

**Matches §1.10.** Implementation is straightforward:

```go
type Store struct {
    masterKey []byte  // 32 bytes AES-256
    keyPath   string
}

func NewStore(keyPath string) (*Store, error)  // load or auto-generate
func (s *Store) Encrypt(plaintext []byte) ([]byte, error)   // nonce || ciphertext
func (s *Store) Decrypt(ciphertext []byte) ([]byte, error)
func (s *Store) EncryptString(value string) ([]byte, error)
func (s *Store) DecryptString(ciphertext []byte) (string, error)
```

Key details:

- **Master key location:** `~/.aegis/master.key` (configurable via `config.MasterKeyPath`)
- **Auto-generation:** If the key file doesn't exist, 32 random bytes are generated via `crypto/rand` and written with `0600` permissions. The parent directory is created with `0700`.
- **Format:** Each encrypted value is `nonce || ciphertext` (12-byte nonce prepended to the GCM ciphertext). No separate nonce storage needed.
- **Key rotation:** Not supported in M3. Deleting `~/.aegis/master.key` invalidates all stored secrets — they become undecryptable garbage. The user must re-set all secrets after key deletion. A proper rotation mechanism (decrypt-all-with-old-key, re-encrypt-with-new-key) is a future concern, not needed for local single-user use.
- **No new dependencies:** stdlib `crypto/aes`, `crypto/cipher`, `crypto/rand`.

### 4.3 Registry Schema — secrets + kits Tables

**Implements the tables from §1.9.** Two new tables added to `registry/db.go` `migrate()`:

```sql
CREATE TABLE IF NOT EXISTS secrets (
    id         TEXT PRIMARY KEY,
    app_id     TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL,
    value      BLOB NOT NULL,
    scope      TEXT NOT NULL DEFAULT 'per_app',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(app_id, name)
);

CREATE TABLE IF NOT EXISTS kits (
    name         TEXT PRIMARY KEY,
    version      TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    config       TEXT NOT NULL,
    image_ref    TEXT NOT NULL,
    installed_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

Key design choices:

- **`secrets.value` is BLOB** — stores the raw encrypted bytes (nonce || ciphertext). Never exposed in JSON responses (tagged `json:"-"` in the Go struct).
- **`secrets.scope`** — `"per_app"` (scoped to one app) or `"per_workspace"` (shared across all apps). Workspace secrets have `app_id = ""`.
- **`UNIQUE(app_id, name)`** — upsert semantics. Setting a secret with the same app+name replaces the previous value.
- **`kits.config`** is stored as JSON TEXT — a typed `KitConfig` struct with nested `Secrets`, `Routing`, `Networking`, `Policies`, `Resources` sub-structs.
- **Cascade delete:** `DeleteApp()` now deletes from `secrets` before `releases` and `apps` in a single transaction.

### 4.4 Secret Resolution — Workspace + App Merge

Secrets are resolved at instance boot / task creation time by `Server.resolveSecrets(appID)`:

```
1. Load workspace secrets (scope = "per_workspace")  → base env
2. Load app secrets (scope = "per_app", app_id = X)  → overlay on top
3. App-scoped secrets win on name collision
4. Return merged map[string]string
```

This merged env is:

- **Serve mode:** Passed to `lifecycle.WithEnv(env)` → stored in `Instance.Env` → included in `startServer` RPC.
- **Task mode:** Merged into `CreateTaskRequest.Env` (explicit env in the request takes precedence over secrets).

### 4.5 Secret API — Names Only, Never Values

Five new routes:

| Method | Path | Purpose |
|--------|------|---------|
| `PUT /v1/apps/{id}/secrets/{name}` | Set app secret | `{"value": "..."}` |
| `GET /v1/apps/{id}/secrets` | List app secrets | Names + metadata only |
| `DELETE /v1/apps/{id}/secrets/{name}` | Delete app secret | |
| `PUT /v1/secrets/{name}` | Set workspace secret | `{"value": "..."}` |
| `GET /v1/secrets` | List workspace secrets | Names + metadata only |

The list endpoints return `secretResponse{Name, Scope, CreatedAt}` — the encrypted value is tagged `json:"-"` and never serialized. This is validated by `TestSecretNotLeakedInResponse` in the conformance suite.

### 4.6 Kit System — Registry + Manifest + No-Op Hooks

**Original assumption (M3 table):** Kit manifest parsing, registration, kit hooks (`render_env`, `validate_config`, `on_publish`).

**M3 reality:** Kit registration and manifest parsing are implemented. Hooks are defined as an interface with a `DefaultHooks` pass-through — real kit hooks come when Famiglia/OpenClaw are built.

**Kit manifest format (YAML on disk, JSON in API):**

```yaml
name: famiglia
version: "0.1.0"
description: "Famiglia AI agent kit"
image: ghcr.io/famiglia/agent:latest
config:
  secrets:
    required:
      - name: ANTHROPIC_API_KEY
        description: "Anthropic API key"
  routing:
    default_port: 8080
    healthcheck: /health
  resources:
    memory_mb: 1024
    vcpus: 2
```

**YAML parsing lives server-side only.** `kit.ParseFile(path)` (in `internal/kit/`) uses `gopkg.in/yaml.v3` to parse the full manifest with all nested config sections, validates required fields (name, version, image), and returns a `*Manifest`. `manifest.ToKit()` converts to a `*registry.Kit` for storage. The CLI does **not** import `yaml.v3` — see §4.7 for how the CLI handles manifest files.

**Hooks interface:**

```go
type Hooks interface {
    RenderEnv(app *registry.App, secrets map[string]string) (map[string]string, error)
    ValidateConfig(appConfig map[string]string) error
    OnPublish(app *registry.App, release *registry.Release) error
}

type DefaultHooks struct{}  // all methods are pass-through/no-op
```

Real kit implementations will satisfy this interface. `DefaultHooks` is the M3 placeholder.

**Kit API routes:**

| Method | Path | Purpose |
|--------|------|---------|
| `POST /v1/kits` | Register kit | `{name, version, image_ref, config}` |
| `GET /v1/kits` | List kits | |
| `GET /v1/kits/{name}` | Get kit | |
| `DELETE /v1/kits/{name}` | Uninstall kit | |

**New dependency:** `gopkg.in/yaml.v3` (pure Go, no cgo) — for kit manifest YAML parsing.

### 4.7 CLI Commands — `secret` + `kit`

Two new top-level command groups:

```bash
# Secrets
aegis secret set myapp API_KEY sk-test123
aegis secret list myapp
aegis secret delete myapp API_KEY
aegis secret set-workspace GLOBAL_KEY value123
aegis secret list-workspace

# Kits
aegis kit install manifest.yaml     # reads YAML, POSTs JSON
aegis kit list                      # table: NAME, VERSION, IMAGE
aegis kit info famiglia             # detailed view
aegis kit uninstall famiglia
```

**CLI-side manifest parsing:** The `kit install` command uses a simple line-based parser (`parseSimpleYAML`) that extracts only top-level scalar fields (`name`, `version`, `image`, `description`) from the manifest YAML. It does **not** parse nested `config:` sections — it sends the top-level fields as JSON to `POST /v1/kits`. This avoids pulling `gopkg.in/yaml.v3` into the CLI binary.

This means `aegis kit install` currently registers the kit's identity (name, version, image) but drops the nested config. For full config registration, use the API directly or add a future `POST /v1/kits/manifest` endpoint that accepts raw YAML and parses server-side with the full `kit.ParseFile()` pipeline.

### 4.8 Enhanced `aegis doctor` — Capability Matrix

When the daemon is running, `aegis doctor` now queries `GET /v1/status` and displays:

```
Aegis Doctor
============

Go:       installed
libkrun:  found at /opt/homebrew/lib/libkrun.dylib
e2fsprogs: found

aegisd:   running

Backend:     libkrun
Capabilities:
  Pause/Resume:          yes
  Memory Snapshots:      no
  Boot from disk layers: yes
Installed kits: 0
```

The status response was enhanced to include:

```json
{
  "status": "running",
  "backend": "libkrun",
  "capabilities": {
    "pause_resume": true,
    "memory_snapshots": false,
    "boot_from_disk_layers": true
  },
  "kit_count": 0
}
```

### 4.9 Conformance Suite — Contract Before Firecracker

**Original assumption (M3 table):** `test/conformance/` directory, `aegis test conformance` CLI wrapper.

**Reality:** Conformance tests live in `test/integration/` alongside M0-M2 tests, using the same `TestMain` pattern and build tag (`integration`). No separate CLI wrapper — `make test-m3` runs them. This keeps the test infrastructure simple and avoids duplication.

**M3-specific tests** (`test/integration/m3_test.go`):

| Test | What it verifies |
|------|------------------|
| `TestSecretInjectionTask` | Set secret → run task with `app_id` → verify env var in output |
| `TestSecretNotLeakedInResponse` | `GET /secrets` returns names only, no encrypted values |
| `TestWorkspaceSecrets` | Workspace secret visible in list endpoint |
| `TestKitInstallListInfo` | Full kit CRUD lifecycle via API |
| `TestDoctorCapabilities` | `aegis doctor` shows Backend, Pause/Resume, Installed kits |

**Conformance tests** (`test/integration/conformance_test.go`):

| Test | What it verifies |
|------|------------------|
| `TestConformanceTaskRun` | Run task, capture stdout, verify output |
| `TestConformanceServeRequest` | Expose port, HTTP request, verify response |
| `TestConformancePauseOnIdle` | Wait for idle timeout, verify pause, request wakes (skipped in SHORT mode) |
| `TestConformanceSecretInjection` | Set secret, run task with `app_id`, verify env var |
| `TestConformanceSecretNotOnDisk` | `grep` for secret value inside VM — verify not found |
| `TestConformanceEgressWorks` | `wget` external URL from inside VM |
| `TestConformanceMemorySnapshot` | `t.Skip("libkrun: no snapshot support")` |
| `TestConformanceCachedResume` | `t.Skip("libkrun: no snapshot support")` |

The conformance tests are written **before** Firecracker (M4) arrives. They define the behavioral contract — when `FirecrackerVMM` is implemented, these same tests must pass without modification. The two snapshot tests are capability-gated: they skip on libkrun and will be unskipped when Firecracker adds snapshot support.

**Makefile:**

```makefile
test-m3: all
    $(GO) test -tags integration -v -count=1 -timeout 15m \
        -run 'TestSecret|TestKit|TestDoctor|TestConformance' ./test/integration/
```

### 4.10 Task AppID Resolution

`CreateTaskRequest` gained an `AppID` field:

```go
type CreateTaskRequest struct {
    Command []string          `json:"command"`
    Env     map[string]string `json:"env,omitempty"`
    Image   string            `json:"image,omitempty"`
    AppID   string            `json:"app_id,omitempty"`
}
```

**`AppID` accepts either an app ID (`app-1739...`) or an app name (`myapp`).** When set, `handleCreateTask` calls `resolveApp(appID)` — which tries by ID first, then by name (same resolution used by all app endpoints since M2) — to get the canonical app ID, then decrypts that app's secrets and merges them into `req.Env`. Explicit env values in the request take precedence over secrets (no overwrite if key already exists).

This enables secret-injected tasks without modifying the harness. From the CLI: `aegis run --app myapp -- sh -c 'echo $SECRET'` would send `app_id: "myapp"` in the task request. The daemon resolves the name, decrypts secrets, and merges them into the env before the RPC is sent to the harness.

### 4.11 aegisd Init Sequence (M3)

```go
cfg := config.DefaultConfig()
backend := vmm.NewLibkrunVMM(cfg)
reg := registry.Open(cfg.DBPath)                        // + secrets, kits tables
imgCache := image.NewCache(cfg.ImageCacheDir)
ov := overlay.NewCopyOverlay(cfg.ReleasesDir)
ss := secrets.NewStore(cfg.MasterKeyPath)                // new: AES-256-GCM
lm := lifecycle.NewManager(backend, cfg)
rtr := router.New(lm, cfg.RouterAddr, &appResolver{reg})
server := api.NewServer(cfg, backend, lm, reg, imgCache, ov, ss) // new: secret store

rtr.Start()
server.Start()
// ... wait for signal ...
lm.Shutdown()
rtr.Stop()
server.Stop()
reg.Close()
```

### 4.12 Project Structure (Actual, M3)

Three new packages, no new binaries:

```
aegis/
├── cmd/
│   ├── aegisd/main.go              # + secret store init, pass to NewServer
│   ├── aegis/main.go               # + secret, kit command groups; enhanced doctor
│   ├── aegis-harness/main.go       # unchanged
│   └── aegis-vmm-worker/main.go    # unchanged
├── internal/
│   ├── vmm/                        # unchanged
│   ├── harness/                    # unchanged (secrets via existing env field)
│   ├── api/
│   │   ├── server.go               # + secretStore field, secret/kit routes, enhanced status
│   │   ├── tasks.go                # + AppID field, secret resolution
│   │   ├── apps.go                 # + secret resolution in handleServeApp
│   │   ├── secrets.go              # NEW: secret CRUD handlers + resolveSecrets
│   │   └── kits.go                 # NEW: kit CRUD handlers
│   ├── config/config.go            # + MasterKeyPath
│   ├── lifecycle/manager.go        # + Env field, WithEnv option, env in startServer RPC
│   ├── router/                     # unchanged
│   ├── registry/
│   │   ├── db.go                   # + secrets, kits tables in migration
│   │   ├── apps.go                 # + cascade delete secrets in DeleteApp
│   │   ├── secrets.go              # NEW: Secret CRUD
│   │   └── kits.go                 # NEW: Kit CRUD + KitConfig types
│   ├── secrets/                    # NEW: encryption package
│   │   ├── store.go                #   AES-256-GCM encrypt/decrypt, master key management
│   │   └── store_test.go           #   8 tests
│   ├── kit/                        # NEW: kit manifest + hooks
│   │   ├── manifest.go             #   YAML parser, Manifest → Kit conversion
│   │   ├── manifest_test.go        #   4 tests
│   │   └── hooks.go                #   Hooks interface + DefaultHooks no-op
│   ├── image/                      # unchanged
│   └── overlay/                    # unchanged
├── test/integration/
│   ├── helpers_test.go             # + apiPut, apiDeleteAllowFail, waitForTaskOutput
│   ├── m0_test.go                  # unchanged
│   ├── m1_test.go                  # unchanged
│   ├── m2_test.go                  # unchanged
│   ├── m3_test.go                  # NEW: secret + kit + doctor tests
│   └── conformance_test.go         # NEW: backend conformance suite
├── go.mod                          # + gopkg.in/yaml.v3
└── ...
```

### 4.13 M3 Dependencies (Actual)

```
macOS (confirmed working):
  All M0 + M1 + M2 dependencies
  + gopkg.in/yaml.v3 (pure Go, no cgo — kit manifest parsing)

No new system dependencies.
stdlib crypto/aes, crypto/cipher, crypto/rand for secret encryption.
```
