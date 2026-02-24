# Quickstart

Get from zero to a running agent inside an AegisVM microVM in under 5 minutes.

---

## 1. Prerequisites

| | macOS | Linux |
|---|---|---|
| **Hardware** | Apple Silicon (M1+) | x86_64 or arm64 with KVM |
| **OS** | macOS 13+ | Ubuntu 22.04+ (or any distro with KVM) |
| **Root** | No | Yes (`sudo`) |

Verify your setup:

```bash
# macOS
uname -m        # must be arm64
brew --version

# Linux
uname -m        # x86_64 or aarch64
ls /dev/kvm     # must exist
```

## 2. Install

### macOS

#### Homebrew (recommended)

```bash
brew tap xfeldman/aegisvm
brew install aegisvm
```

This installs all binaries and handles hypervisor entitlement signing automatically. On first run, `aegis up` will download the default rootfs (Alpine + Python 3.12).

#### From source

Install system dependencies:

```bash
brew tap slp/krun
brew install libkrun e2fsprogs
```

Clone and build:

```bash
git clone https://github.com/xfeldman/aegisvm.git
cd aegisvm
make all
```

`make all` builds into `./bin/`: `aegisd`, `aegis`, `aegis-harness`, `aegis-mcp`, `aegis-vmm-worker`.

### Linux

#### Package install (recommended)

```bash
sudo apt install aegisvm    # when available
```

#### From source

Install system dependencies:

```bash
sudo apt install virtiofsd e2fsprogs
```

Clone and build:

```bash
git clone https://github.com/xfeldman/aegisvm.git
cd aegisvm
make cloud-hypervisor    # download Cloud Hypervisor static binary
make kernel              # download prebuilt microVM kernel (~30MB)
make all                 # build aegisd, aegis, aegis-harness, aegis-mcp
```

`make all` builds into `./bin/`: `aegisd`, `aegis`, `aegis-harness`, `aegis-mcp` (no vmm-worker on Linux).

> **Unsupported architecture?** If `make kernel` fails (e.g. RISC-V), use `make kernel-build` to compile from source. Requires `build-essential flex bison libelf-dev libssl-dev bc`.

## 3. Base Rootfs

AegisVM needs a base root filesystem to boot VMs. Three pre-built variants are available:

| Variant | Contents | Size |
|---------|----------|------|
| `alpine` | Minimal Alpine Linux (sh, coreutils) | ~8MB |
| `python` | Alpine + Python 3.12 | ~55MB |
| `full` | Alpine + Python 3.12 + Node.js + npm + git + curl + jq | ~90MB |

**Default:** `python` is auto-downloaded on first `aegis up` if no rootfs is installed.

Manage rootfs variants:

```bash
# See what's available and installed
aegis rootfs list

# Download a specific variant (replaces current base-rootfs)
aegis rootfs pull python
aegis rootfs pull alpine
aegis rootfs pull full
```

The active rootfs lives at `~/.aegis/base-rootfs` (macOS, directory) or `~/.aegis/base-rootfs.ext4` (Linux, ext4 image). Pulling a variant replaces it. This is the rootfs used when you run instances without `--image`.

**Building rootfs from source (optional):**

```bash
make base-rootfs    # requires Docker
# macOS: creates base/rootfs/ directory
# Linux: creates base/base-rootfs.ext4 image
```

Copy the result to `~/.aegis/`:

```bash
# macOS
cp -r base/rootfs ~/.aegis/base-rootfs

# Linux
cp base/base-rootfs.ext4 ~/.aegis/base-rootfs.ext4
```

## 4. Start AegisVM

```bash
aegis up
```

On Linux, you'll be prompted for your password (sudo is needed for tap networking). Subsequent runs use cached credentials.

This launches `aegisd` in the background. If no rootfs is installed, it downloads the default (`python`) first. Verify it is running:

```bash
aegis status
aegis doctor    # detailed platform + dependency check
```

## 5. Run Your First Command

```bash
aegis run -- echo "hello from aegisvm"
```

Expected output:

```
hello from aegisvm
```

What happened behind the scenes:

1. The CLI sent a request to `aegisd` over `~/.aegis/aegisd.sock`.
2. `aegisd` booted a microVM (libkrun on macOS, Cloud Hypervisor on Linux).
3. Inside the VM, `aegis-harness` (PID 1) received the `run` RPC over vsock.
4. The harness executed `echo "hello from aegisvm"` and streamed stdout back.
5. When the process exited, the harness sent a `processExited` notification.
6. The CLI received the exit, deleted the instance, and exited with code 0.

## 6. Run with a Custom Image

```bash
aegis run --image alpine:3.21 -- echo hello
```

When you specify `--image`, aegisd pulls the OCI image (if not already cached), extracts it into a rootfs, and boots the VM from that rootfs. Subsequent runs with the same image skip the pull and use the cached copy from `~/.aegis/data/images/`.

## 7. Start an HTTP Server

```bash
aegis run --expose 8080:80 -- python3 -m http.server 80
```

In a second terminal:

```bash
curl http://127.0.0.1:8080/
```

You should see the directory listing from Python's HTTP server.

How this works:

- `--expose 8080:80` maps public port 8080 to guest port 80. You can also use `--expose 80` to let the OS assign a random public port.
- All exposed ports are owned by the router in aegisd -- traffic is proxied with wake-on-connect.
- When idle for 60 seconds, the VM is paused. After 5 minutes idle, the VM is stopped entirely.
- The next incoming connection wakes the VM automatically (scale-to-zero).

Press Ctrl+C in the first terminal to stop.

## 8. Run a Script from Your Host

Use `--workspace` to mount a host directory into the VM at `/workspace/`:

```bash
aegis run --workspace ./myapp --expose 8080:80 -- python3 /workspace/server.py
```

This mounts `./myapp` from your host as `/workspace/` inside the VM via virtiofs. Files are available read-write and persist after the VM stops.

## 9. Start a Named Instance

Start a long-lived instance with a handle:

```bash
# Start the instance (public port 8080 -> guest port 80)
aegis instance start --name demo --expose 8080:80 -- python3 -m http.server 80

# Set a workspace secret (available as env var inside VMs)
aegis secret set API_KEY sk-test123

# Curl the server
curl http://127.0.0.1:8080/
```

## 10. Exec Into a Running Instance

While the instance is running, exec commands inside the VM:

```bash
aegis exec demo -- echo "hello from inside"
```

Expected output:

```
hello from inside
```

This uses the existing control channel -- no SSH, no serial console. The instance
is auto-resumed if paused.

## 11. Stream Logs

View logs from a running instance:

```bash
aegis logs demo --follow
```

Logs are captured from the moment the VM starts and persisted to
`~/.aegis/data/logs/`. Without `--follow`, all buffered logs are printed and the
command exits. With `--follow`, new log entries stream live until Ctrl+C.

## 12. Inspect Instances

List all instances:

```bash
aegis instance list
```

Get detailed info about a specific instance:

```bash
aegis instance info demo
```

Disable the instance -- the instance becomes unmanaged. The VM is stopped, port
listeners are closed, and aegisd will not auto-start it for any reason (no
wake-on-connect, no implicit boot). The instance stays in the list as a registry
record with logs preserved:

```bash
aegis instance disable demo
```

The only way to bring it back is an explicit `aegis instance start --name demo`.

Delete the instance entirely (removes from list, cleans logs):

```bash
aegis instance delete demo
```

## 13. MCP (Claude Code Integration)

Register AegisVM as an MCP server so Claude can drive sandboxed instances:

```bash
aegis mcp install
```

Once registered, Claude can start VMs, exec commands, read logs, and manage secrets directly.

## 14. Where Is My Data?

All AegisVM state lives under `~/.aegis/`:

| Path | Purpose |
|---|---|
| `~/.aegis/aegisd.sock` | Unix domain socket for CLI-to-daemon communication |
| `~/.aegis/data/aegisd.pid` | PID file for the running daemon |
| `~/.aegis/data/aegis.db` | SQLite database (instances, secrets) |
| `~/.aegis/data/images/` | Cached OCI image layers |
| `~/.aegis/data/overlays/` | Instance rootfs overlays |
| `~/.aegis/data/workspaces/` | Workspace directories |
| `~/.aegis/data/logs/` | Per-instance NDJSON log files |
| `~/.aegis/data/snapshots/` | VM memory snapshots (Linux only) |
| `~/.aegis/kernel/vmlinux` | MicroVM kernel (Linux only) |
| `~/.aegis/master.key` | Encryption key for secrets at rest |
| `~/.aegis/base-rootfs/` | Active base rootfs directory (macOS) |
| `~/.aegis/base-rootfs.ext4` | Active base rootfs ext4 image (Linux) |

## 15. Platform Differences

| Behavior | macOS | Linux |
|---|---|---|
| Hypervisor | libkrun (Apple HVF) | Cloud Hypervisor (KVM) |
| Root required | No | Yes |
| Networking | gvproxy (in-process) | tap + NAT |
| Rootfs format | Directory | ext4 block image |
| Pause mechanism | SIGSTOP/SIGCONT | vm.pause/vm.resume API |
| Idle stop | Persistent pause (OS swap) | Snapshot to disk, then stop |
| Cold restart | SIGCONT (instant) | Restore from snapshot |
| Workspace | virtiofs (built-in) | virtiofsd sidecar |

From the user's perspective, the CLI commands are identical. The backend differences are handled transparently by aegisd.

## 16. Clean Up

```bash
aegis down
```

This shuts down `aegisd` and all running VMs.

To remove everything and start fresh:

```bash
aegis down
rm -rf ~/.aegis
```

## 17. Next Steps

- [AGENT_CONVENTIONS.md](AGENT_CONVENTIONS.md) -- conventions for building agents that run on AegisVM
- [CLI.md](CLI.md) -- full CLI reference
- [cloud-hypervisor-backend.md](cloud-hypervisor-backend.md) -- Linux backend architecture (for contributors)
- `examples/` -- sample agents
