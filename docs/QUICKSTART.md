# Quickstart

Get from zero to a running agent inside an AegisVM microVM in under 5 minutes.

---

## 1. Prerequisites

- macOS on Apple Silicon (M1 or later)
- [Homebrew](https://brew.sh)

Verify your setup:

```bash
uname -m        # must be arm64
brew --version
```

## 2. Install

### Option A: Homebrew (recommended)

```bash
brew tap xfeldman/aegisvm
brew install aegisvm
```

This installs all binaries and handles hypervisor entitlement signing automatically. On first run, `aegis up` will download the default rootfs (Alpine + Python 3.12).

### Option B: From source

Install system dependencies:

```bash
brew tap slp/krun
brew install libkrun e2fsprogs
```

Clone the repo and build:

```bash
git clone https://github.com/xfeldman/aegisvm.git
cd aegisvm
make all
```

`make all` builds five binaries into `./bin/`:

- `aegisd` -- the infrastructure control plane daemon
- `aegis` -- the CLI
- `aegis-harness` -- the guest control agent (Linux ARM64, statically linked)
- `aegis-mcp` -- MCP server for LLM integration
- `aegis-vmm-worker` -- per-VM VMM helper (cgo, libkrun)

#### Building rootfs from source (optional)

If you want to build the rootfs manually instead of downloading it:

```bash
make base-rootfs    # requires Docker
```

This creates an Alpine ARM64 rootfs with Python 3.12 and the harness baked in at `base/rootfs/`. Copy it to `~/.aegis/base-rootfs/` to use it.

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

The active rootfs lives at `~/.aegis/base-rootfs/`. Pulling a variant replaces it. This is the rootfs used when you run instances without `--image`.

## 4. Start AegisVM

```bash
aegis up
```

This launches `aegisd` in the background. If no rootfs is installed, it downloads the default (`python`) first. Verify it is running:

```bash
aegis status
```

You should see output confirming the daemon is listening on its Unix socket.

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
2. `aegisd` booted a microVM using libkrun (Apple Hypervisor Framework).
3. Inside the VM, `aegis-harness` (PID 1) received the `run` RPC over vsock JSON-RPC.
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
aegis run --expose 80 -- python3 -m http.server 80
```

In a second terminal:

```bash
curl http://127.0.0.1:8099/
```

You should see the directory listing from Python's HTTP server.

How this works:

- The `--expose 80` flag tells aegisd to map port 80 inside the VM to a host port (Docker-style static port mapping).
- The router in aegisd listens on `127.0.0.1:8099` and proxies requests into the VM.
- When idle for 60 seconds, the VM is paused (SIGSTOP). After 20 minutes idle, the VM is stopped entirely.
- The next incoming request wakes the VM automatically (scale-to-zero).

Press Ctrl+C in the first terminal to stop.

## 8. Run a Script from Your Host

Use `--workspace` to mount a host directory into the VM at `/workspace/`:

```bash
aegis run --workspace ./myapp --expose 80 -- python3 /workspace/server.py
```

This mounts `./myapp` from your host as `/workspace/` inside the VM. Files are available read-write and persist after the VM stops.

## 9. Start a Named Instance

Start a long-lived instance with a handle:

```bash
# Start the instance
aegis instance start --name demo --expose 80 -- python3 -m http.server 80

# Set a workspace secret (available as env var inside VMs)
aegis secret set API_KEY sk-test123

# Curl the server
curl http://127.0.0.1:8099/
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

Stop the instance (VM stops, instance stays in list with logs):

```bash
aegis instance stop demo
```

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
| `~/.aegis/master.key` | Encryption key for secrets at rest |
| `~/.aegis/base-rootfs/` | Active base rootfs (downloaded or built manually) |

## 15. Clean Up

```bash
aegis down
```

This shuts down `aegisd` and all running VMs.

To remove everything and start fresh:

```bash
aegis down
rm -rf ~/.aegis
```

## 16. Next Steps

- [AGENT_CONVENTIONS.md](AGENT_CONVENTIONS.md) -- conventions for building agents that run on AegisVM
- [CLI.md](CLI.md) -- full CLI reference
- `examples/` -- sample agents
