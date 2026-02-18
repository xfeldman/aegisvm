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

### 1.1 Control Channel Wire Protocol

**Decision: JSON-RPC 2.0 over ControlChannel.**

- Human-readable, debuggable with `socat`
- Trivial to implement in Go (host) and any guest language
- No code generation step (vs protobuf)
- Performance is not a bottleneck — control plane messages (start/stop/health), not data plane
- Upgrade path: switch to protobuf later if profiling shows serialization overhead matters (it won't)

Wire format: newline-delimited JSON-RPC 2.0 messages over a ControlChannel (TSI TCP on macOS/libkrun, AF_VSOCK on Linux/Firecracker — see §7.2 for transport details). Host is the client, guest harness is the server.

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
| `internal/lifecycle/manager.go` | STOPPED → RUNNING ⇄ PAUSED → STOPPED state machine, idle timers |
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

This is clean, backend-specific but abstractable, and not leaky to core. Firecracker can use AF_VSOCK directly when it arrives in M4 — same protocol, different transport.

### 7.2.1 ControlChannel Abstraction

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

### 7.2.2 RootFS Abstraction

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

---

## 8. M1 Implementation Notes

**Documented after completing M1. These are design choices and corrections to §1.8, §1.9, §1.11 based on building serve mode against the M0 codebase and libkrun 1.17.4 on macOS ARM64.**

### 8.1 Port Mapping via `krun_set_port_map`, Not Custom Tunneling

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

### 8.2 Pause/Resume via SIGSTOP/SIGCONT, Not libkrun API

**Original assumption (§1.5):** PauseVM/ResumeVM implemented via the VMM backend's native API.

**Reality:** libkrun's C API does not expose pause/resume. But each VM is a vmm-worker subprocess (§7.1), and `SIGSTOP` freezes the entire process — vCPU threads, TSI networking, everything. `SIGCONT` resumes it. The guest doesn't know it was paused. RAM stays allocated. TSI port mappings survive.

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

### 8.3 VMM Interface Extensions for Serve Mode

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

### 8.4 Harness `startServer` RPC — Long-Lived Processes

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

### 8.5 Lifecycle Manager — State Machine + Idle Timers

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

### 8.6 Router — Simpler Than Spec

**Original assumption (§1.8):** Path-based routing (`/app/{appId}/...`), WebSocket via `nhooyr.io/websocket`, per-protocol wake behavior matrix.

**Reality for M1:** Single-instance routing. All traffic to `:8099` goes to the one active serve instance. No path parsing, no app ID resolution. This is correct for M1 — multi-app routing is an M2 concern.

**Simplifications over the spec:**

- **No external dependencies** — WebSocket upgrade handled via `net.Conn` hijack + bidirectional `io.Copy`, not `nhooyr.io/websocket`. Raw TCP proxying is sufficient for WebSocket since we're just forwarding bytes.
- **No per-protocol wake behavior matrix** — all protocols get the same treatment: ensure instance is running, then proxy. If the instance is booting, HTML clients get a loading page with `<meta http-equiv="refresh" content="3">`, non-HTML clients get `503 + Retry-After: 3`.
- **Routing lookup is trivial** — `GetDefaultInstance()` returns the first instance in the map. Path-based resolution comes in M2.

The router embeds in aegisd as designed (§1.8) — it's a goroutine with an `http.Server`, shares the lifecycle manager in-process.

### 8.7 SQLite Registry — Minimal M1 Schema

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

### 8.8 CLI `--expose` Flag

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

### 8.9 API Additions

Three new routes on the unix socket API:

| Method | Path | Purpose |
|---|---|---|
| `POST /v1/instances` | Create + boot a serve instance | `{"command": [...], "expose_ports": [80]}` |
| `GET /v1/instances/{id}` | Get instance state | Returns `{id, state}` |
| `DELETE /v1/instances/{id}` | Stop + remove instance | Sends shutdown RPC, kills VM |

Task routes (`/v1/tasks/*`) are unchanged and continue to work for task mode.

### 8.10 aegisd Init Sequence (M1)

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

### 8.11 Project Structure (Actual, M1)

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

### 8.12 M1 Dependencies (Actual)

```
macOS (confirmed working):
  All M0 dependencies (go 1.26+, libkrun, Docker, codesign)
  + modernc.org/sqlite v1.46.1 (pure Go, no cgo — first external dep)

Not needed for M1 (deferred):
  nhooyr.io/websocket (raw hijack sufficient)
  internal/network/ package (TSI handles everything)
```

---

## 9. M2 Implementation Notes

**Documented after completing M2. These are design choices and corrections to §1.2, §1.3, §1.9, and the M2 milestone table, based on building the image pipeline, app/release system, and multi-app routing on top of the M1 codebase.**

### 9.1 OCI Image Pipeline — Directory-Based, Not ext4

**Original assumption (§1.2):** `go-containerregistry` for pull/unpack, `mkfs.ext4 -d` to produce an ext4 rootfs image.

**Reality:** libkrun uses directory-based rootfs via `krun_set_root()` (§7.6). There is no need for ext4 image creation on macOS. The image pipeline unpacks OCI layers directly into a directory, and libkrun mounts it via virtiofs.

**Actual pipeline:**

1. `image.Pull(ctx, "python:3.12")` → resolves reference, pulls linux/arm64 manifest via `go-containerregistry`, handles both single-platform images and multi-platform index manifests
2. `image.Unpack(img, destDir)` → extracts layers in order into a directory tree, handles OCI whiteout files (`.wh.` prefix for file deletion, `.wh..wh..opq` for opaque directory replacement)
3. `image.Cache` → digest-keyed directory cache at `~/.aegis/data/images/sha256_{digest}/`. `GetOrPull(ctx, ref)` returns the cached directory or pulls + unpacks + caches atomically (via tmp dir + rename)
4. `image.InjectHarness(rootfsDir, harnessBin)` → copies the harness binary into the rootfs at `/usr/bin/aegis-harness`

Harness injection happens on the **release copy**, not the cache. The cache contains the clean OCI image; each release gets its own copy with the harness baked in. Any existing `/usr/bin/aegis-harness` in the OCI image is intentionally overwritten.

**PID 1 guarantee:** `krun_set_exec()` always runs `/usr/bin/aegis-harness` as guest PID 1, regardless of the OCI image's `ENTRYPOINT` or `CMD`. The image's entrypoint is ignored — the harness starts user commands via RPC (`runTask`/`startServer`). This is by design: the harness must be PID 1 for signal handling, mount setup, and host communication.

**Platform invariant:** `Pull()` enforces linux/arm64. For multi-platform index manifests, it selects the linux/arm64 variant and fails with "no linux/arm64 variant found" if absent. For single-manifest images, it validates the config's `OS` and `Architecture` fields after pull — a `linux/amd64` image will fail with an explicit error rather than unpacking successfully and crashing at VM boot with an opaque exec format error.

ext4 conversion (`mkfs.ext4 -d`) is deferred to M4 when Firecracker needs block device images. The existing `BackendCaps.RootFSType` abstraction handles this — the image pipeline will produce the right artifact based on the active backend's declared type.

### 9.2 Overlay — tar Pipe, Not cp

**Original assumption (§1.3):** Full rootfs copy per release on macOS.

**Reality:** Correct — macOS has no device-mapper, so each release gets a full copy. But the copy method matters: macOS `cp -a` breaks busybox-style symlink layouts (§7.6). The `CopyOverlay` implementation uses a tar pipe:

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

### 9.3 Registry Schema — apps + releases Tables

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

### 9.4 Workspace Volumes via virtiofs

**Original assumption (§1.3):** Workspace volume is bind-mounted, separate from rootfs.

**Reality:** libkrun supports `krun_add_virtiofs(ctx, tag, path)` which creates an independent virtio-fs device pointing to a host directory. The guest mounts it by tag.

**Implementation:**

1. **Host side:** `LibkrunVMM.StartVM()` checks `VMConfig.WorkspacePath`. If set, it passes `"workspace:/path"` in the `MappedVolumes` field of `WorkerConfig`. The vmm-worker calls `krun_add_virtiofs(ctx, "workspace", path)`.

2. **Guest side:** The harness checks `AEGIS_WORKSPACE=1` (set by vmm-worker when volumes are configured). If set, it mounts the `workspace` virtiofs tag at `/workspace` and **fails fatally** if the mount fails — preventing silent data-loss bugs where files appear to write but don't persist. If `AEGIS_WORKSPACE` is not set, the harness skips the mount entirely. The mount code is in `mount_linux.go` (build-tagged, no-op on macOS).

3. **App workspaces:** Each app gets a workspace directory at `~/.aegis/data/workspaces/{appID}/`. This is created when `aegis app serve` is called and passed to the instance via `lifecycle.WithWorkspace(path)`.

### 9.5 Lifecycle Manager — Functional Options for Instances

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

### 9.6 Multi-App Router — Path + Header Routing with Fallback

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

### 9.7 `--image` Flag on `aegis run`

**New:** `aegis run --image alpine:3.21 -- echo hello` pulls an OCI image, creates a temporary rootfs with the harness injected, runs the task, and cleans up.

The flow in `TaskStore.runTask()`:

1. If `req.Image` is set → `imageCache.GetOrPull(ctx, image)` → get cached rootfs directory
2. `overlay.Create(ctx, cachedDir, "task-"+taskID)` → full copy to temp release dir
3. `image.InjectHarness(overlayDir, harnessBin)` → bake harness into temp rootfs
4. Use temp rootfs as `VMConfig.Rootfs.Path`
5. After task completes → `overlay.Remove("task-"+taskID)` (deferred cleanup)

Without `--image`, behavior is unchanged — the base rootfs is used.

**Crash resilience:** On daemon startup, `CopyOverlay.CleanStaleTasks(1h)` scans the releases directory for `task-*` entries older than 1 hour and removes them. This prevents disk leaks from crashed tasks that didn't run their deferred cleanup.

### 9.8 App Lifecycle — Publish + Serve

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

### 9.9 CLI App Commands

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

### 9.10 Config Additions

Three new paths in `config.Config`:

```go
ImageCacheDir  string  // ~/.aegis/data/images     — digest-keyed OCI cache
ReleasesDir    string  // ~/.aegis/data/releases    — release rootfs copies
WorkspacesDir  string  // ~/.aegis/data/workspaces  — app workspace volumes
```

All three directories are created by `cfg.EnsureDirs()` at daemon startup.

### 9.11 Harness — Rootfs Immutability + Platform-Specific Mounts

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

### 9.12 aegisd Init Sequence (M2)

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

### 9.13 Project Structure (Actual, M2)

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

### 9.14 M2 Dependencies (Actual)

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
