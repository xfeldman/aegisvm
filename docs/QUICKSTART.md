# Quickstart

Get from zero to a running agent inside an Aegis microVM in under 5 minutes.

---

## 1. Prerequisites

- macOS on Apple Silicon (M1 or later)
- [Homebrew](https://brew.sh)
- Go 1.23 or newer

Verify your setup:

```bash
uname -m        # must be arm64
go version       # must be 1.23+
brew --version
```

## 2. Install

Install the system dependencies:

```bash
brew tap slp/krun
brew install libkrun
brew install e2fsprogs
```

Clone the repo and build everything:

```bash
git clone <repo-url> aegis
cd aegis
make all
make base-rootfs
```

`make all` builds three binaries into `./bin/`:

- `aegisd` -- the infrastructure control plane daemon
- `aegis` -- the CLI
- `aegis-harness` -- the guest control agent (Linux ARM64, statically linked)

`make base-rootfs` creates an Alpine ARM64 ext4 root filesystem with the harness baked in. This is the default image used when you do not specify one.

## 3. Start Aegis

```bash
./bin/aegis up
```

This launches `aegisd` in the background. Verify it is running:

```bash
./bin/aegis status
```

You should see output confirming the daemon is listening on its Unix socket.

## 4. Run Your First Command

```bash
./bin/aegis run -- echo "hello from aegis"
```

Expected output:

```
hello from aegis
```

What happened behind the scenes:

1. The CLI sent a request to `aegisd` over `~/.aegis/aegisd.sock`.
2. `aegisd` booted a microVM using libkrun (Apple Hypervisor Framework).
3. Inside the VM, `aegis-harness` (PID 1) received the `run` RPC over vsock JSON-RPC.
4. The harness executed `echo "hello from aegis"` and streamed stdout back.
5. When the process exited, the harness sent a `processExited` notification.
6. The CLI received the exit, deleted the instance, and exited with code 0.

## 5. Run with a Custom Image

```bash
./bin/aegis run --image alpine:3.21 -- echo hello
```

When you specify `--image`, aegisd pulls the OCI image (if not already cached), extracts it into a rootfs, and boots the VM from that rootfs. Subsequent runs with the same image skip the pull and use the cached copy from `~/.aegis/data/images/`.

## 6. Start an HTTP Server

```bash
./bin/aegis run --expose 80 -- python3 -m http.server 80
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

## 7. Start a Named Instance

Start a long-lived instance with a handle:

```bash
# Start the instance
./bin/aegis instance start --name demo --expose 80 -- python3 -m http.server 80

# Set a workspace secret (available as env var inside VMs)
./bin/aegis secret set API_KEY sk-test123

# Curl the server
curl http://127.0.0.1:8099/
```

## 8. Exec Into a Running Instance

While the instance is running, exec commands inside the VM:

```bash
./bin/aegis exec demo -- echo "hello from inside"
```

Expected output:

```
hello from inside
```

This uses the existing control channel -- no SSH, no serial console. The instance
is auto-resumed if paused.

## 9. Stream Logs

View logs from a running instance:

```bash
./bin/aegis logs demo --follow
```

Logs are captured from the moment the VM starts and persisted to
`~/.aegis/data/logs/`. Without `--follow`, all buffered logs are printed and the
command exits. With `--follow`, new log entries stream live until Ctrl+C.

## 10. Inspect Instances

List all instances:

```bash
./bin/aegis instance list
```

Get detailed info about a specific instance:

```bash
./bin/aegis instance info demo
```

Stop the instance:

```bash
./bin/aegis instance stop demo
```

## 11. Where Is My Data?

All Aegis state lives under `~/.aegis/`:

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
| `~/.aegis/base-rootfs/` | Default Alpine rootfs built by `make base-rootfs` |

## 12. Clean Up

```bash
./bin/aegis down
```

This shuts down `aegisd` and all running VMs.

To remove everything and start fresh:

```bash
./bin/aegis down
rm -rf ~/.aegis
```

## 13. Next Steps

- [AGENT_CONVENTIONS.md](AGENT_CONVENTIONS.md) -- conventions for building agents that run on Aegis
- [CLI.md](CLI.md) -- full CLI reference
- `examples/` -- sample agents
