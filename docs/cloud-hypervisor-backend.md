# Cloud Hypervisor VMM Backend

Linux VMM backend for AegisVM using [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) (CH). Feature parity with the macOS/libkrun backend: workspace volumes, pause/resume, memory snapshotting on stop, restore on cold restart.

## Why Cloud Hypervisor

Firecracker was the original Linux target but lacks virtiofs support — the only way to share host directories with the guest. CH is built on the same rust-vmm crates as Firecracker, adds virtiofs via a virtiofsd sidecar (mirroring vmm-worker on macOS), and has native snapshot/restore. Same kernel, same vsock protocol, same harness code.

## Architecture

### Feature Matrix

| Feature | macOS (libkrun) | Linux (Cloud Hypervisor) |
|---------|----------------|--------------------------|
| Workspace | virtiofs (built-in) | virtiofsd sidecar |
| Control channel | vsock | vsock |
| Pause/Resume | SIGSTOP/SIGCONT | `PUT /vm.pause` / `/vm.resume` |
| Stop+Snapshot | N/A (persistent pause) | `PUT /vm.snapshot` then kill |
| Cold restart | N/A (SIGCONT) | `PUT /vm.restore` + `/vm.resume` |
| Networking | gvproxy (in-process) | tap + iptables NAT |
| Ingress | gvproxy forward to 127.0.0.1 | router dials guest IP directly |
| Rootfs | directory | ext4 block image |
| Per-VM sidecar | vmm-worker | cloud-hypervisor + virtiofsd |

### Control Plane Communication

CH exposes a REST API over a unix socket. The backend talks to it via standard `net/http` — no cgo, no external SDK, no CH client library. All API calls are `PUT` requests with JSON bodies:

```
PUT /api/v1/vm.create   — configure VM (kernel, disks, net, vsock, virtiofs)
PUT /api/v1/vm.boot     — start the VM
PUT /api/v1/vm.pause    — freeze vCPUs
PUT /api/v1/vm.resume   — unfreeze vCPUs
PUT /api/v1/vm.snapshot — save memory to disk
PUT /api/v1/vm.restore  — load memory from disk
```

### Networking: Tap + NAT

Each VM gets a dedicated tap device and a /30 subnet from `172.16.0.0/12`:

```
Host (172.16.0.1/30) <--tap--> Guest (172.16.0.2/30)
```

**Egress:** Guest traffic is NATed via iptables MASQUERADE. The host enables `ip_forward` and adds FORWARD rules per tap device.

**Ingress:** No proxy layer. The guest has a routable IP from the host's perspective. The router calls `GetEndpoint()` which returns `guestIP:guestPort` (e.g. `172.16.0.2:80`), and dials it directly over the tap interface. This eliminates random backend ports, proxy lifecycle management, and stale-listener races.

Subnet allocation uses a monotonic counter. Each VM increments the counter and gets the next /30 block. The counter resets on daemon restart (tap devices are cleaned up on VM stop).

### Vsock

Same `{socket}_{port}` convention as other vsock-based VMMs. The host pre-creates a unix socket listener at `{vsock_socket}_5000` before booting the VM. The guest harness dials CID=2 port=5000 via AF_VSOCK. The existing `vsock_linux.go` in the harness works unchanged.

### Virtiofs Workspace

When a workspace is configured, the backend spawns a `virtiofsd` sidecar before VM boot:

```
virtiofsd --socket-path=/path/to/virtiofsd.sock --shared-dir=/host/workspace --cache=never
```

The CH `vm.create` payload references this socket via the `fs` config. Inside the guest, the harness mounts `workspace` at `/workspace` using the virtiofs filesystem type — identical to the libkrun path.

### Rootfs

CH requires ext4 block images (not directories). When the lifecycle manager detects a directory rootfs and the backend declares `RootFSBlockImage`, it converts via `mkfs.ext4 -d`. The base rootfs is built as `base-rootfs.ext4` on Linux (vs `base-rootfs/` directory on macOS).

## Snapshot/Restore

The lifecycle manager drives the snapshot lifecycle:

1. **Idle timeout fires** -> VM is paused via `PauseVM()`.
2. **Stop timer fires** -> lifecycle manager discovers `SnapshotVM` via structural type assertion, calls it with a per-instance directory under `~/.aegis/data/snapshots/{instance-id}/`.
3. **Snapshot** -> backend pauses if needed, calls `PUT /vm.snapshot`, saves memory + device state to disk.
4. **StopVM** -> kills CH process, destroys tap, removes NAT.
5. **Cold restart** -> `bootInstance` detects the snapshot directory, calls `SetSnapshotDir` on the backend, then `StartVM` takes the restore path: sets up tap/NAT/virtiofsd, starts CH, calls `PUT /vm.restore` + `PUT /vm.resume`, waits for harness reconnect.

**Stale snapshot cleanup:** If a process exits (crash or clean exit), `handleProcessExited` removes the snapshot directory — the snapshot is only valid for paused VMs, not for crashed ones.

### Harness Reconnect

After snapshot/restore, the vsock transport resets. The harness detects connection loss and enters a reconnect loop (only when vsock is available). It re-dials CID=2 port=5000 with the same retry logic as initial boot. The lifecycle manager sets up a new vsock listener before restore, accepts the reconnection, and wires up a new `ControlChannel`.

## Abstraction Boundaries

The implementation follows the VMM interface contract without leaking backend details into core:

| Layer | Knows about CH? | Mechanism |
|-------|-----------------|-----------|
| `vmm.VMM` interface | No | Backend-agnostic methods |
| `CloudHypervisorVMM` | Yes | Implements `vmm.VMM` |
| Lifecycle manager | No | Uses `vmm.VMM` + structural type assertions for `SnapshotVM` and `SetSnapshotDir` |
| Router | No | Dials `BackendAddr:HostPort` from `HostEndpoint` |
| Harness | No | Reads env vars, dials vsock, mounts virtiofs — all backend-agnostic |
| Config | Platform fields | Holds paths (`KernelPath`, `CloudHypervisorBin`, etc.) without CH logic |
| `cmd/aegisd` | Backend name only | `switch platform.Backend` instantiates the correct VMM |

**Structural type assertions** (same pattern as `DynamicExposePort` on libkrun):

```go
// Snapshot capability — lifecycle manager discovers at runtime
if snapshotter, ok := m.vmm.(interface {
    SnapshotVM(vmm.Handle, string) error
}); ok { ... }

// Snapshot restore — set before StartVM
if setter, ok := m.vmm.(interface {
    SetSnapshotDir(vmm.Handle, string) error
}); ok { ... }
```

This keeps snapshot/restore as an optional backend capability. macOS/libkrun has `PersistentPause` (OS manages swap). Linux/CH has `SnapshotVM` (explicit memory snapshot). Both achieve the same UX: instances remember state across stop/start cycles.

## Prerequisites

```bash
# KVM access (required)
sudo apt install qemu-kvm        # ensures /dev/kvm exists

# Virtiofs daemon
sudo apt install virtiofsd

# Cloud Hypervisor binary
make cloud-hypervisor             # downloads static binary from GitHub

# MicroVM kernel with virtiofs + vsock support
make kernel                       # builds vmlinux from Linux 6.1 source (~10 min, one-time)

# Build binaries (harness matches host arch on Linux)
make all

# Build base rootfs (ext4 image on Linux)
make base-rootfs
cp base/base-rootfs.ext4 ~/.aegis/base-rootfs.ext4
```

## Root Requirement

`aegisd` on Linux requires root or `CAP_NET_ADMIN` for tap device creation and iptables rules. The backend fails fast at initialization if `euid != 0`:

```
aegisd on Linux requires root or CAP_NET_ADMIN for tap networking
```

No rootless fallback. No slirp4netns. No partial functionality.

## Files

| File | Role |
|------|------|
| `internal/vmm/cloudhv.go` | `CloudHypervisorVMM` — VMM interface implementation |
| `internal/vmm/vmm.go` | `BackendAddr` field on `HostEndpoint` |
| `internal/config/config.go` | `KernelPath`, `CloudHypervisorBin`, `VirtiofsdBin`, `SnapshotsDir` |
| `internal/config/platform.go` | `"cloud-hypervisor"` backend selection for Linux |
| `cmd/aegisd/main.go` | Backend wiring |
| `internal/lifecycle/manager.go` | `dirToExt4`, snapshot/restore lifecycle, `BackendAddr` in `GetEndpoint` |
| `internal/harness/mount_linux.go` | `parseCmdlineEnv()` for kernel cmdline env vars |
| `internal/harness/main.go` | Vsock reconnect loop |
| `Makefile` | `cloud-hypervisor` and `kernel` targets, Linux-aware build |
| `base/Makefile` | Platform-aware Docker + ext4 output |
