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

- `aegisd` -- the control plane daemon
- `aegis` -- the CLI
- `aegis-harness` -- the guest PID 1 (Linux ARM64, statically linked)

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
3. Inside the VM, `aegis-harness` (PID 1) received the command over vsock JSON-RPC.
4. The harness executed `echo "hello from aegis"` and streamed stdout back to the CLI.
5. The VM was shut down after the command completed.

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

- The `--expose 80` flag tells aegisd to route HTTP traffic to port 80 inside the VM.
- The embedded router in aegisd listens on `127.0.0.1:8099` and proxies requests into the VM.
- When the last HTTP connection closes and no new requests arrive for 60 seconds, the VM is paused (SIGSTOP). After 20 minutes idle, the VM is terminated entirely.
- The next incoming request cold-boots the VM again automatically (scale-to-zero).

Press Ctrl+C in the first terminal to stop.

## 7. Create an App

A full app lifecycle: create, set secrets, publish, serve, and call.

```bash
# Create the app definition
./bin/aegis app create --name demo --image python:3.12-alpine --expose 80 -- python -m http.server 80

# Inject a secret (available as an env var inside the VM)
./bin/aegis secret set demo API_KEY sk-test123

# Publish a release (snapshots the image + config into a release rootfs)
./bin/aegis app publish demo

# Serve the app (boots the VM and starts routing)
./bin/aegis app serve demo
```

In a second terminal:

```bash
curl http://127.0.0.1:8099/
```

When you are done, stop the app:

```bash
# Ctrl+C in the serve terminal, or:
./bin/aegis app stop demo
```

## 8. Where Is My Data?

All Aegis state lives under `~/.aegis/`:

| Path | Purpose |
|---|---|
| `~/.aegis/aegisd.sock` | Unix domain socket for CLI-to-daemon communication |
| `~/.aegis/data/aegisd.pid` | PID file for the running daemon |
| `~/.aegis/data/aegis.db` | SQLite database (apps, releases, metadata) |
| `~/.aegis/data/images/` | Cached OCI image layers |
| `~/.aegis/data/releases/` | Published release rootfs snapshots |
| `~/.aegis/data/workspaces/{appID}/` | Per-app workspace directories (persistent across reboots) |
| `~/.aegis/master.key` | Encryption key for secrets at rest |
| `~/.aegis/base-rootfs/` | Default Alpine rootfs built by `make base-rootfs` |

## 9. Clean Up

```bash
./bin/aegis down
```

This shuts down `aegisd` and all running VMs.

What is removed:

- All running microVM processes
- The daemon process
- The Unix socket (`~/.aegis/aegisd.sock`)
- The PID file

What is kept:

- Workspaces (`~/.aegis/data/workspaces/`)
- Cached images (`~/.aegis/data/images/`)
- The database (`~/.aegis/data/aegis.db`)
- Published releases (`~/.aegis/data/releases/`)
- The master key (`~/.aegis/master.key`)

To remove everything and start fresh:

```bash
./bin/aegis down
rm -rf ~/.aegis
```

## 10. Next Steps

- [AGENT_CONVENTIONS.md](AGENT_CONVENTIONS.md) -- conventions for building agents that run on Aegis
- [CLI.md](CLI.md) -- full CLI reference
- `examples/` -- sample apps and agent configurations
