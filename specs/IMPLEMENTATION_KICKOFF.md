# Aegis Implementation Kickoff

**From spec to code — engineering decisions, project structure, and Milestone 0**

**Date:** 2026-02-18
**Depends on:** [AEGIS_PLATFORM_SPEC.md](AEGIS_PLATFORM_SPEC.md)

---

## 0. Development Platform

**Primary dev machine: macOS Apple Silicon (M1).** This drives the entire milestone order.

Firecracker requires Linux KVM. It cannot run on macOS — not in Docker, not in Lima, not anywhere on M1 (no nested virtualization support). Developing Firecracker-first would mean zero local testing for the first ~8 weeks of the project.

**Therefore: libkrun is the M0 backend.** libkrun uses Apple's Hypervisor.framework directly — native on M1, no nested virt. microsandbox already proves it works for agent sandboxing. Firecracker is added later as the Linux production backend, validated by the conformance test suite.

This does not change any architecture, invariants, or semantics. It only changes which VMM implements the interface first.

**Discipline rule:** When developing against libkrun, keep Firecracker constraints in mind. Do not build macOS-only shortcuts into core. Specifically:
- Raw block devices (no format magic)
- Network model must be backend-agnostic
- No host filesystem tricks that won't work on Linux

---

## 1. Engineering Decisions

### 1.1 Vsock Wire Protocol

**Decision: JSON-RPC 2.0 over vsock.**

- Human-readable, debuggable with `socat`
- Trivial to implement in Go (host) and any guest language
- No code generation step (vs protobuf)
- Performance is not a bottleneck — vsock messages are control plane (start/stop/health), not data plane
- Upgrade path: switch to protobuf later if profiling shows serialization overhead matters (it won't)

Wire format: newline-delimited JSON-RPC 2.0 messages over `AF_VSOCK`. Host is the client, guest harness is the server.

```json
{"jsonrpc":"2.0","method":"runTask","params":{"command":["python","main.py"]},"id":1}
{"jsonrpc":"2.0","result":{"exitCode":0,"artifacts":[]},"id":1}
```

Streaming (logs): server sends JSON-RPC notifications:

```json
{"jsonrpc":"2.0","method":"log","params":{"stream":"stdout","line":"hello world"}}
```

### 1.2 OCI Image → Rootfs Pipeline

**Decision: go-containerregistry for pull/unpack, ext4 rootfs via `mkfs.ext4 -d`.**

**Not needed for M0.** M0 uses a single hardcoded `base.ext4`. This pipeline is introduced in M2.

Pipeline (M2+):

1. `aegis run --image base:python` resolves to an OCI reference (local cache or remote registry)
2. Pull image layers using `go-containerregistry` (google/go-containerregistry) — no containerd dependency
3. Unpack layers into a directory tree
4. Create ext4 filesystem image: `mkfs.ext4 -d <dir> rootfs.ext4`
5. Cache the rootfs image keyed by image digest — rebuild only on image change

Why not containerd: adds a daemon dependency. Aegis should be a single binary. go-containerregistry is a library, not a daemon.

### 1.3 Disk Layering: Platform-Dependent Overlays

**Overlay mechanism is an implementation detail, not a semantic contract.** The canonical rule is:

- Release = immutable rootfs + artifact
- Restore-from-disk-layers is canonical
- Workspace volume is never part of any overlay

How overlays are implemented differs by platform:

**Linux (M4+): device-mapper snapshots.**

Firecracker block devices are raw virtio-blk. Firecracker does not consume qcow2.

```
base.ext4 (raw)  →  dm device (read-only origin)
                        └── dm-snapshot (COW, writable)  →  /dev/mapper/release-v3
                                                               ↑
                                                          Firecracker sees this as raw block device
```

- Base rootfs exposed as read-only dm device
- Each release creates a dm-snapshot (COW) on top
- Writes go to snapshot's COW area; base remains immutable
- True COW with kernel-level efficiency
- On restore, aegisd must reconstruct the dm-snapshot mapping from `overlay_ref` before VM boot — `overlay_ref` is not just metadata, it's the input to `dmsetup create`

**macOS (M0–M3): full rootfs copy per release.**

macOS has no device-mapper. For MVP:

- Base rootfs is a raw ext4 image
- Each release copies the base and applies changes on top
- Slower than COW but correct and simple
- Sufficient for dev workflow — releases are infrequent

This does not break any invariant. The release is still an immutable disk artifact. Restore still boots from disk layers. The copy is just a less efficient way to produce the artifact.

### 1.4 libkrun Go Bindings

**Decision: cgo wrapper around libkrun's C API. First backend implemented (M0).**

- libkrun exposes a C API (`libkrun.h`) — straightforward to wrap with cgo
- microsandbox (Rust) validates that libkrun works for this use case
- cgo adds build complexity but avoids shelling out or maintaining a separate Rust process
- The VMM interface is narrow (7 methods) — the cgo surface is small
- Alternative: if cgo proves painful, shell out to `krunvm` CLI (libkrun ships one) as a fallback

For Firecracker (M4): use `firecracker-go-sdk` (official, well-maintained).

### 1.5 VMM Interface — Lock This First

**Before writing any VMM implementation, define and freeze the interface.**

```go
type VMM interface {
    CreateVM(config VMConfig) (Handle, error)
    StartVM(h Handle) error
    PauseVM(h Handle) error
    ResumeVM(h Handle) error
    StopVM(h Handle) error
    Snapshot(h Handle, path string) error
    Restore(snapshotPath string) (Handle, error)
    Capabilities() BackendCaps
}

type BackendCaps struct {
    Pause           bool   // Pause/resume with RAM retained
    SnapshotRestore bool   // Save/restore full VM memory to disk
    Name            string // "libkrun" or "firecracker"
}
```

No conditional logic in aegisd core. Core calls `vmm.CreateVM()` — it doesn't know or care which backend is active. libkrun implements it first. Firecracker implements it second. Both pass the same conformance tests.

### 1.6 Base Rootfs

**Decision: Alpine Linux ARM64, minimal.**

MVP is **ARM64-only**. x86_64 is a future milestone, not in scope for M0-M5.

```
Base rootfs contents:
  Alpine Linux 3.21 (aarch64)
  busybox + apk
  aegis-harness binary (Go, statically linked, GOARCH=arm64)
  /workspace mount point
  /run/secrets mount point (tmpfs, for secret injection)
```

No Node.js, no Python, no language runtimes in the base. Kits layer those on top. The base is ~30MB.

Build process:

```bash
# Build base rootfs (one-time, versioned by BaseRevision)
alpine_rootfs=$(mktemp -d)
apk --root $alpine_rootfs --arch aarch64 --initdb add alpine-base busybox
cp aegis-harness $alpine_rootfs/usr/bin/
mkfs.ext4 -d $alpine_rootfs base.ext4
```

### 1.7 Secret Injection Model

**Decision: harness stores secrets in memory, applies them to spawned processes via `execve` env.**

The flow:

1. aegisd decrypts secrets from registry
2. aegisd calls `injectSecrets(secrets)` via vsock to the harness
3. Harness stores secrets in an in-memory map — **not** exported to PID 1's own environment
4. Every `runTask` / `startServer` call constructs a full env (base env + injected secrets + per-task overrides) and passes it to `execve`
5. Child processes inherit secrets via their env; harness process itself does not expose them

This is critical: you cannot retroactively set environment variables for an already-running process tree. The harness holds secrets and applies them at spawn time.

Per-task env override is supported — task params can add/override env vars, merged on top of injected secrets.

Secrets are:
- Never written to disk inside the VM
- Never in any snapshot tier
- Re-injected on every boot/restore via vsock
- Held only in harness memory, applied via `execve` to child processes

### 1.8 Router

**Decision: embedded in aegisd, using Go stdlib.**

- `net/http/httputil.ReverseProxy` for HTTP
- Raw `net.Conn` bidirectional copy for TCP
- `nhooyr.io/websocket` for WebSocket upgrade
- No separate process — router is a goroutine inside aegisd
- Listens on a single host port (default: `127.0.0.1:8099`)
- Path-based routing: `/app/{appId}/...` → resolve instance → proxy

Why embedded: fewer moving parts, single binary, shared state with lifecycle manager (router needs to call `instances/ensure` which is an in-process call).

#### Wake behavior per protocol (implement in M1)

| Protocol | While VM is waking | After VM ready |
|---|---|---|
| **HTTP** | Return a tiny `Waking...` HTML page with `<meta http-equiv="refresh" content="1">` auto-retry. Configurable per-kit (`ui: true` ports get this; API ports get 503 + `Retry-After: 1`). | Normal reverse proxy |
| **TCP** | Hold the connection open, buffer nothing. Client's TCP timeout (~30s) provides ample wake window. If VM doesn't become ready within 10s, close with RST. | Bidirectional byte copy |
| **WebSocket** | Do NOT accept the upgrade. Return 503 with `Retry-After: 1`. Client retries. | Accept upgrade, bidirectional frames |

### 1.9 Registry Schema

**Decision: SQLite with these tables. Not needed for M0 (in-memory map is sufficient).**

Introduced in M1 when instance state needs to survive aegisd restarts.

```sql
CREATE TABLE apps (
  id          TEXT PRIMARY KEY,
  kit         TEXT,
  config      TEXT,
  created_at  TEXT DEFAULT (datetime('now'))
);

CREATE TABLE releases (
  id            TEXT PRIMARY KEY,
  app_id        TEXT NOT NULL REFERENCES apps(id),
  base_revision TEXT NOT NULL,
  overlay_ref   TEXT NOT NULL,         -- dm-snapshot ref (Linux) or rootfs path (macOS)
  artifact_path TEXT,
  label         TEXT,
  created_at    TEXT DEFAULT (datetime('now'))
);

CREATE TABLE instances (
  id            TEXT PRIMARY KEY,
  app_id        TEXT REFERENCES apps(id),
  release_id    TEXT REFERENCES releases(id),
  state         TEXT NOT NULL DEFAULT 'STOPPED',
  endpoint      TEXT,
  vm_id         TEXT,
  network_group TEXT,
  workspace_id  TEXT,
  last_active   TEXT,
  created_at    TEXT DEFAULT (datetime('now'))
);

CREATE TABLE kits (
  name        TEXT PRIMARY KEY,
  version     TEXT NOT NULL,
  config      TEXT NOT NULL,
  image_ref   TEXT NOT NULL,
  installed_at TEXT DEFAULT (datetime('now'))
);

CREATE TABLE secrets (
  id          TEXT PRIMARY KEY,
  app_id      TEXT REFERENCES apps(id),
  name        TEXT NOT NULL,
  value       BLOB NOT NULL,           -- Encrypted at rest (AES-256-GCM)
  scope       TEXT NOT NULL,
  created_at  TEXT DEFAULT (datetime('now')),
  UNIQUE(app_id, name)
);

CREATE TABLE workspaces (
  id          TEXT PRIMARY KEY,
  app_id      TEXT REFERENCES apps(id),
  path        TEXT NOT NULL,
  mode        TEXT NOT NULL DEFAULT 'isolated',
  group_id    TEXT,
  created_at  TEXT DEFAULT (datetime('now'))
);
```

### 1.10 Secret Host-Side Storage

**Decision: encrypted in SQLite registry, decrypted only at injection time.**

- Secrets stored as encrypted blobs in the `secrets` table
- Encryption key derived from a host-local master key (`~/.aegis/master.key`, generated on `aegis up`)
- At VM boot/restore: decrypt → send via vsock `injectSecrets()` → harness holds in memory (see §1.7)
- Simple symmetric encryption (AES-256-GCM) — not a vault, just protection at rest

### 1.11 Network Setup

**Linux (M4+): TAP + bridge + nftables. Private IP per VM.**

```
Host
  └── aegis-br0 (bridge, 10.0.100.0/24)
        ├── tap-vm1 (10.0.100.2) → VM 1
        ├── tap-vm2 (10.0.100.3) → VM 2
        └── ...

Egress: nftables MASQUERADE on aegis-br0
Ingress: DROP all except from router (aegisd process)
```

Use **nftables** (not iptables). `aegis doctor` checks for nftables availability.

**macOS (M0–M3): libkrun's built-in virtual network.**

libkrun provides its own networking via TSI (transparent socket impersonation) or gvproxy. Evaluate during M0 — the exact mechanism depends on libkrun version. If libkrun networking is insufficient for M1 (egress needed for serve mode), investigate alternatives before M1 starts.

---

## 2. Milestone 0: Truly Minimal

**The simplest `aegis run` that works end-to-end. macOS ARM64 via libkrun.**

```bash
aegis up
aegis run -- echo "hello from aegis"
# Output: hello from aegis
aegis down
```

### What M0 includes

- aegisd starts, listens on unix socket
- CLI sends task request to aegisd
- aegisd creates a **libkrun VM** from a hardcoded local `base.ext4`
- Harness inside VM receives `runTask` via vsock JSON-RPC
- Harness executes the command
- Stdout/stderr streams back via vsock notifications
- aegisd collects output, returns to CLI
- VM terminates
- CLI prints output

### What M0 explicitly does NOT include

- `--image` flag (hardcoded base.ext4, no OCI pipeline)
- Router / serve mode / port exposure
- Releases / publishing / overlays
- SQLite registry (in-memory map only)
- Kits
- Secrets
- Warm pool
- Pause/resume
- Networking (VM runs isolated, no egress)
- Workspace volumes
- Firecracker / Linux support

### M0 key deliverables

1. **VMM interface defined and frozen** (`internal/vmm/vmm.go`)
2. **LibkrunVMM implementation** — CreateVM, StartVM, StopVM
3. **Guest harness** — PID 1, vsock JSON-RPC server, `runTask` handler
4. **Base rootfs** — Alpine ARM64 ext4 with harness baked in
5. **Working dev loop** — `make all && aegis up && aegis run -- echo hello`

M0 is: **boot a VM on your Mac, run a command, get output.** Everything else layers on top.

---

## 3. Project Structure

```
aegis/
├── cmd/
│   ├── aegisd/              # Daemon entry point
│   │   └── main.go
│   └── aegis/               # CLI entry point
│       └── main.go
│
├── internal/
│   ├── vmm/                 # VMM abstraction layer
│   │   ├── vmm.go           # VMM interface + BackendCaps — LOCK THIS FIRST
│   │   ├── libkrun.go       # LibkrunVMM implementation (M0, primary dev backend)
│   │   └── firecracker.go   # FirecrackerVMM implementation (M4, Linux production)
│   │
│   ├── harness/             # Guest harness (compiled into base rootfs)
│   │   ├── main.go          # PID 1, vsock server
│   │   ├── rpc.go           # JSON-RPC handler (runTask, startServer, health, etc.)
│   │   ├── exec.go          # Process execution + log streaming
│   │   └── secrets.go       # In-memory secret store, applied via execve env
│   │
│   ├── router/              # Connection-aware lifecycle proxy (M1)
│   │   ├── router.go        # HTTP/TCP/WS reverse proxy
│   │   ├── wake.go          # Wake-on-connect logic + per-protocol behavior
│   │   └── activity.go      # Connection tracking + idle timers
│   │
│   ├── registry/            # SQLite state store (M1+, in-memory for M0)
│   │   ├── db.go            # Schema init + migrations
│   │   ├── apps.go          # App CRUD
│   │   ├── releases.go      # Release CRUD
│   │   ├── instances.go     # Instance state management
│   │   ├── kits.go          # Kit registration
│   │   └── secrets.go       # Encrypted secret storage
│   │
│   ├── lifecycle/           # Instance lifecycle state machine (M1+)
│   │   ├── manager.go       # State transitions, idle timers, pressure handling
│   │   └── pool.go          # Warm VM pool (M5)
│   │
│   ├── network/             # VM networking (M1+)
│   │   ├── bridge.go        # Bridge/TAP creation (Linux)
│   │   ├── nat.go           # nftables MASQUERADE rules (Linux)
│   │   ├── libkrun_net.go   # libkrun networking (macOS)
│   │   └── groups.go        # Inter-VM network groups (M5)
│   │
│   ├── image/               # OCI image → rootfs (M2+)
│   │   ├── pull.go          # Pull from registry (go-containerregistry)
│   │   ├── unpack.go        # Unpack layers
│   │   └── rootfs.go        # Create ext4 image via mkfs.ext4 -d
│   │
│   ├── overlay/             # Disk layering (M2+)
│   │   ├── overlay.go       # Overlay interface
│   │   ├── dm.go            # device-mapper snapshot (Linux)
│   │   └── copy.go          # Full rootfs copy (macOS fallback)
│   │
│   ├── secrets/             # Secret encryption + injection (M3+)
│   │   ├── store.go         # Master key, AES-256-GCM encrypt/decrypt
│   │   └── inject.go        # Vsock injection at boot/restore
│   │
│   ├── api/                 # aegisd HTTP API (unix socket)
│   │   ├── server.go        # HTTP server setup
│   │   ├── tasks.go         # POST /v1/tasks, GET /v1/tasks/{id}, etc.
│   │   ├── apps.go          # POST /v1/apps/{id}/publish, GET /v1/apps/{id}
│   │   ├── instances.go     # POST /v1/instances/ensure, etc.
│   │   └── kits.go          # POST /v1/kits/register, etc.
│   │
│   └── config/              # Configuration + platform detection
│       ├── config.go        # Defaults, data directories
│       └── platform.go      # Detect Linux/macOS, KVM/HVF, nftables
│
├── base/                    # Base rootfs build
│   ├── Makefile             # Build base.ext4 (ARM64 Alpine + harness)
│   └── overlay/             # Files to include in base rootfs
│       └── usr/bin/aegis-harness
│
├── test/
│   └── conformance/         # Backend conformance test suite
│       ├── task_test.go
│       ├── serve_test.go
│       ├── lifecycle_test.go
│       ├── secrets_test.go
│       └── network_test.go
│
├── go.mod
├── go.sum
├── Makefile                 # Build aegisd, aegis CLI, harness, base rootfs
└── README.md
```

---

## 4. Milestones

### M0: Boot + Run — libkrun on macOS (2-3 weeks)

**Goal:** `aegis run -- echo hello` works on macOS ARM64.

| Component | What to build | Done when |
|---|---|---|
| `internal/vmm/vmm.go` | VMM interface + BackendCaps | **Frozen.** Compiles. |
| `internal/vmm/libkrun.go` | LibkrunVMM: CreateVM, StartVM, StopVM via cgo | A libkrun VM boots and shuts down on Mac |
| `internal/harness` | PID 1, vsock JSON-RPC, `runTask` | Receives command, executes, streams output |
| `base/` | Hardcoded Alpine ARM64 ext4 with harness | VM boots to harness |
| `cmd/aegisd` | Daemon on unix socket | Starts, accepts connections |
| `cmd/aegis` | CLI | `aegis up` / `aegis run` / `aegis down` work |
| `internal/api/tasks.go` | `POST /v1/tasks` | CLI submits task, gets output |

No OCI, no SQLite, no networking, no router, no Firecracker.

### M1: Serve + Router + Networking (2-3 weeks)

**Goal:** `aegis run --expose 80 -- python -m http.server 80` serves HTTP, wakes on request after pause.

| Component | What to build |
|---|---|
| `internal/router` | HTTP/TCP/WS reverse proxy with wake-on-connect and per-protocol wake behavior |
| `internal/lifecycle/manager.go` | RUNNING → PAUSED → TERMINATED state machine, idle timers |
| `internal/vmm/libkrun.go` | PauseVM, ResumeVM |
| `internal/network/libkrun_net.go` | libkrun networking for VM egress |
| `internal/registry/db.go` | SQLite schema, instance state persistence |

### M2: Releases + Apps + Overlays (2-3 weeks)

**Goal:** `aegis app publish` creates a release. `aegis app serve` serves it with scale-to-zero.

| Component | What to build |
|---|---|
| `internal/image` | OCI pull (go-containerregistry), unpack, `mkfs.ext4 -d` rootfs |
| `internal/overlay/copy.go` | Full rootfs copy per release (macOS — no dm-snapshot) |
| `internal/overlay/overlay.go` | Overlay interface (dm-snapshot added in M4) |
| `internal/registry/releases.go` | Release + overlay management |
| `internal/registry/apps.go` | App lifecycle |
| Workspace volumes | Bind-mount host directory into VM (separate from rootfs) |

### M3: Kits + Secrets + Conformance Suite (2-3 weeks)

**Goal:** Kits work. Secrets are injected correctly. Conformance suite exists and passes on libkrun.

| Component | What to build |
|---|---|
| `internal/registry/kits.go` | Kit manifest parsing + registration |
| `internal/secrets` | Master key, AES-256-GCM encrypt/decrypt |
| `internal/harness/secrets.go` | In-memory secret store, `execve` env construction |
| Kit hooks | `render_env`, `validate_config`, `on_publish` |
| API: kits | `POST /v1/kits/register`, `GET /v1/kits` |
| `test/conformance/` | **Full conformance test suite — all required tests pass on libkrun** |
| `aegis test conformance` | CLI wrapper |
| `aegis doctor` | Print backend + capability matrix |

Conformance suite is written **before** adding Firecracker so it defines the contract, not the implementation.

### M4: Firecracker on Linux (2-3 weeks)

**Goal:** `aegis up` works on Linux ARM64. Conformance tests pass on both backends.

| Component | What to build |
|---|---|
| `internal/vmm/firecracker.go` | FirecrackerVMM: full implementation via firecracker-go-sdk |
| `internal/overlay/dm.go` | device-mapper snapshot create/remove for COW overlays |
| `internal/network/bridge.go` | TAP + bridge + nftables NAT |
| `internal/config/platform.go` | Detect Linux + KVM, select Firecracker backend |
| Conformance | Run full suite against Firecracker — **fix any mismatches before M5** |

If conformance tests reveal VMM interface leaks, fix the interface. This is the architectural validation moment.

### M5: Multi-VM + Production (2-3 weeks)

**Goal:** Shared workspaces, inter-VM networking, warm pool, GC. Both kits can be built.

| Component | What to build |
|---|---|
| Shared workspace mode | Same host dir bind-mounted into multiple VMs |
| Inter-VM networking | Network groups, per-group bridge/subnet |
| Warm VM pool | Pre-booted VMs, claim-on-demand |
| Snapshot GC | Retention policies, safe overlay removal |

---

## 5. Build + Development

### Build

```makefile
# Build everything (ARM64)
make all

# Individual targets
make aegisd              # Build daemon (host arch — darwin/arm64 on Mac)
make aegis               # Build CLI (host arch)
make harness             # Build guest harness (GOARCH=arm64 GOOS=linux, static)
make base-rootfs         # Build base.ext4 from Alpine ARM64 + harness
make test                # Run unit tests
make conformance         # Run backend conformance suite (M3+)
```

### Development Loop (macOS)

```bash
brew install libkrun                    # One-time
make all
./aegisd &                              # Start daemon (libkrun backend)
./aegis run -- echo hello               # Test
./aegis down                            # Stop
```

### Dependencies

**macOS (M0–M3):**
```
go 1.23+
libkrun (via Homebrew)
mkfs.ext4 (via Homebrew: brew install e2fsprogs)
```

**Linux (M4+, additional):**
```
firecracker binary (ARM64, downloaded by Makefile)
dmsetup (device-mapper utils)
nftables
```

No qemu-img. No containerd. No Docker for runtime.

---

## 6. What NOT to Build

- **Custom guest kernel** — use a standard vmlinux for ARM64.
- **Custom init system** — the harness IS PID 1. No systemd, no openrc.
- **Container runtime** — no containerd, no runc. VMs run processes directly.
- **Image build system** — use existing OCI tools (docker build, buildah). Aegis just pulls and unpacks.
- **qcow2 anything** — Firecracker doesn't consume it. Use dm-snapshot (Linux) or full copy (macOS).
- **x86_64 support** — ARM64-only for M0-M5.
- **Configuration language** — YAML manifests, no DSL.
- **Web UI** — CLI only. Kits build their own UIs.
- **Auth/multi-tenancy** — single-user, local-only for v1. Unix socket auth is sufficient.
- **iptables** — use nftables on Linux. `aegis doctor` checks availability.
- **macOS-only shortcuts in core** — no conditional logic based on platform in aegisd core. All platform differences live behind the VMM and overlay interfaces.

---

## 7. M0 Implementation Notes

**Documented after completing M0. These are corrections and additions to the engineering decisions above, based on what we learned building against libkrun 1.17.4 on macOS ARM64 (M1).**

### 7.1 `krun_start_enter()` Takes Over the Process

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

### 7.2 IPC: TSI Outbound TCP, Not Vsock

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

### 7.3 Kernel Cmdline 2048-Byte Limit

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

### 7.4 macOS Hypervisor Entitlement

**Not in original spec.**

On macOS, Apple's Hypervisor.framework requires the calling binary to be codesigned with the `com.apple.security.hypervisor` entitlement. Without it, `krun_start_enter()` fails with `VmSetup(VmCreate)` (errno -22).

The vmm-worker binary must be signed after every build:

```bash
codesign --sign - --entitlements entitlements.plist --force bin/aegis-vmm-worker
```

This is handled automatically by the Makefile on macOS.

### 7.5 libkrunfw Runtime Loading

**Not in original spec.**

libkrun dynamically loads `libkrunfw` (the bundled kernel) via `dlopen()` at runtime. If the library path is not in the default search path, the VM fails to start silently or with an opaque error.

**Solution:** aegisd sets `DYLD_FALLBACK_LIBRARY_PATH=/opt/homebrew/lib:/usr/local/lib:/usr/lib` when spawning the vmm-worker process.

### 7.6 Directory-Based Rootfs (libkrun Standard Mode)

**Original assumption (§1.6):** ext4 image via `mkfs.ext4 -d`.

**Reality:** libkrun's standard (non-EFI) mode uses `krun_set_root(ctx, directory_path)` which takes a **host directory** as the root filesystem (chroot-style, exposed to the guest via virtio-fs). It does not consume block device images.

**Solution for M0:** Build the rootfs as a directory (Alpine ARM64 extracted from Docker), not an ext4 image. The harness binary is placed at `rootfs/usr/bin/aegis-harness`. The directory is stored at `~/.aegis/base-rootfs/`.

ext4 images will be needed for Firecracker (M4) which requires raw block devices. The base rootfs build produces a directory; `make ext4` in `base/` converts it to an ext4 image when needed.

Note: macOS `cp` cannot handle busybox symlinks correctly. Use `tar cf - -C rootfs . | tar xf - -C target/` for copying the rootfs.

### 7.7 M0 Dependencies (Actual)

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

### 7.8 Project Structure (Actual, M0)

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
