# AegisVM

Lightweight MicroVM sandbox runtime for agents.

AegisVM runs isolated processes inside microVMs that boot in under a second, pause when idle, and wake on demand. It handles VMs, networking, routing, and lifecycle so agent platforms don't have to.

AegisVM is not a PaaS, not a publish system, and not an agent framework. It is a clean sandbox substrate.

## Install

```bash
brew tap xfeldman/aegisvm
brew install aegisvm
```

Requires macOS ARM64 (Apple Silicon M1+). The formula installs all binaries and handles hypervisor entitlement signing automatically.

### From source

```bash
brew tap slp/krun && brew install libkrun e2fsprogs
git clone https://github.com/xfeldman/aegisvm.git && cd aegisvm
make all
```

Binaries are placed in `./bin/`.

## Quick start

```bash
# Start the daemon
aegis up

# Run a command in an ephemeral VM
aegis run -- echo "hello from aegisvm"

# Run a Python HTTP server on port 8080
aegis run --expose 8080:80 -- python3 -m http.server 80

# Random public port (OS-assigned)
aegis run --expose 80 -- python3 -m http.server 80

# Run a script from your host directory (mounted at /workspace/ in the VM)
aegis run --workspace ./myapp --expose 8080:80 -- python3 /workspace/server.py

# Stop the daemon
aegis down
```

## MCP (Claude Code integration)

AegisVM ships an MCP server (`aegis-mcp`) that lets LLMs drive sandboxed instances — start VMs, exec commands, read logs, manage secrets.

```bash
# Register with Claude Code (one-time setup)
aegis mcp install

# Or manually:
claude mcp add --transport stdio aegis -- aegis-mcp
```

Once registered, Claude can use tools like `instance_start`, `exec`, `logs`, `secret_set` to manage VMs directly.

## Instances

The only runtime object in AegisVM is an **instance** — a VM running a command with optional port exposure, workspace mount, and secret injection. No apps, no releases, no publish lifecycle.

```bash
# Ephemeral: run a command, collect output, instance deleted after
aegis run -- python analyze.py

# Ephemeral with exposed port (public 8080 → guest 80)
aegis run --expose 8080:80 -- python3 -m http.server 80

# Persistent instance with a handle
aegis instance start --name web --workspace myapp --expose 8080:80 -- python3 -m http.server 80
aegis exec web -- echo hello
aegis logs web --follow

# Disable — instance becomes unmanaged (no auto-wake, no listeners, VM off)
aegis instance disable web
aegis instance start --name web       # re-enables and boots from stored config

# Delete (remove entirely)
aegis instance delete web
```

Measured on macOS ARM64 (M1, libkrun backend):

| Path | Latency |
|------|---------|
| Cold boot (zero to process running) | ~500ms |
| Resume from pause (SIGCONT) | ~35ms |

## Lifecycle

```
STOPPED → STARTING → RUNNING ↔ PAUSED → STOPPED
```

| Operation | Result | Enabled? | In list? | Logs? | VM? |
|-----------|--------|----------|----------|-------|-----|
| Process exits naturally | STOPPED | Yes | Yes | Yes | No |
| Idle timeout | STOPPED | Yes | Yes | Yes | No |
| `aegis instance disable` | STOPPED | **No** | Yes | Yes | No |
| `aegis instance pause` | PAUSED | Yes | Yes | Yes | Suspended |
| `aegis instance delete` | Removed | - | No | No | No |
| `aegis run` exit/Ctrl+C | Deleted | - | No | No | No |

**Enabled** (default) means the instance is managed by aegisd: it auto-wakes on incoming connections, pauses when idle, and reboots from disk on demand. **Disabled** means the instance is a pure registry record — no listeners, no auto-wake, no implicit boot. Aegisd will not start a disabled instance under any circumstance. Only an explicit `instance start` re-enables it. Use `instance prune` to clean up old stopped instances.

## CLI

Daemon management:

```bash
aegis up / down / status / doctor
```

Runtime:

```bash
aegis run [options] -- <cmd>                        Ephemeral: start + follow + delete
aegis instance start [options] -- <cmd>             Start new or re-enable stopped/disabled instance
aegis instance list [--stopped|--running]            List instances
aegis instance info <name|id>                       Instance detail
aegis instance disable <name|id>                    Disable instance (stop VM, close listeners)
aegis instance delete <name|id>                     Remove instance + cleanup
aegis instance pause/resume <name|id>               SIGSTOP / SIGCONT
aegis instance prune --stopped-older-than <dur>     Remove stale stopped instances
aegis exec <name|id> -- <cmd>                       Execute in running instance
aegis logs <name|id> [--follow]                     Stream logs
aegis secret set/list/delete                        Manage secrets
aegis mcp install/uninstall                         Claude Code MCP integration
```

Common flags: `--name`, `--expose [PUBLIC:]GUEST[/proto]`, `--env K=V`, `--secret KEY`, `--workspace NAME_OR_PATH`, `--image REF`.

## Core mechanisms

**Docker-style port mapping.** `--expose 8080:80` maps public port 8080 to guest port 80. `--expose 80` assigns a random public port. All ports are owned by the router — traffic is proxied through aegisd with wake-on-connect, so paused and stopped VMs wake automatically on incoming connections.

**Disk is canonical.** Stop terminates the VM and restores from disk layers on next boot. Pause (SIGSTOP) retains RAM for fast resume but is never treated as durable state. Memory is ephemeral, workspace is persistent.

**Workspace volumes** give agents durable storage that survives VM termination. Named workspaces (`--workspace claw` resolves to `~/.aegis/data/workspaces/claw`) or host paths (`--workspace ./myapp`). Always RW, mounted at `/workspace`.

**Secrets** are a flat AES-256-GCM encrypted store. Injection is explicit (`--secret API_KEY` or `--secret '*'`). Default: inject nothing.

**Two-layer control model.** The infrastructure control plane (aegisd) manages VMs, port mapping, secrets, and images. The guest control agent (harness) manages the process inside the VM. Serving semantics, readiness, and versioning belong to userland inside the VM.

**Firecracker and libkrun** are the microVM backbone. Firecracker on Linux (KVM), libkrun on macOS (Apple Hypervisor.framework). Same daemon, same harness, same CLI.

## Architecture

```
┌──────────────────────────────────────────────┐
│  Host                                        │
│                                              │
│  aegisd (Go)          aegis CLI              │
│    ├── API server        ├── up / down       │
│    ├── lifecycle mgr     ├── run / instance  │
│    ├── router            ├── exec / logs     │
│    └── VMM backend       └── secret / mcp    │
│         │                                    │
│    ┌────┴────────────────────────────┐       │
│    │  VMM (libkrun / Firecracker)    │       │
│    │  ┌─────────┐  ┌─────────┐      │       │
│    │  │  VM 1   │  │  VM 2   │ ...  │       │
│    │  │ harness │  │ harness │      │       │
│    │  └─────────┘  └─────────┘      │       │
│    └─────────────────────────────────┘       │
└──────────────────────────────────────────────┘
```

**aegisd** — infrastructure control plane. Manages instance lifecycle, serves the HTTP API on a unix socket, runs the router.

**aegis-harness** — guest control agent. PID 1 inside every VM. Handles JSON-RPC commands (`run`, `exec`, `health`, `shutdown`).

**aegis** — CLI. Talks to aegisd over the unix socket.

**aegis-mcp** — MCP server. Exposes aegisd as tools for LLMs over stdio JSON-RPC.

## What AegisVM is not

- **Not a generic hypervisor.** Fixed base image, opinionated lifecycle, one kind of workload.
- **Not a container replacement.** Containers share the host kernel. Aegis VMs don't.
- **Not a cloud platform.** Single-host, local-first. No regions, no load balancers, no multi-tenancy.
- **Not a workflow engine.** No DAGs, no step definitions, no retry policies.
- **Not an agent framework.** No prompt templates, no tool definitions, no LLM abstractions.

## Design principles

1. Disk is canonical.
2. Memory is ephemeral.
3. Workspace is separate from rootfs.
4. Secrets never persist in the VM.
5. Expose is static.
6. Instance is the only runtime object.
7. Control plane lives on the host only.
8. Guest logic is workload, not platform.
9. Simplicity over feature density.

## Tests

```bash
make test                 # unit tests (no VM, fast)
make integration          # boots real VMs, full suite
make integration SHORT=1  # skip pause/resume test
```

## Documentation

- [Quickstart](docs/QUICKSTART.md) — zero to running agent in 5 minutes
- [CLI Reference](docs/CLI.md) — complete command reference
- [Agent Conventions](docs/AGENT_CONVENTIONS.md) — guest environment contract
- [Router](docs/ROUTER.md) — always-proxy ingress, wake-on-connect, idle behavior
- [Workspaces](docs/WORKSPACES.md) — persistent volumes, lifecycle
- [Secrets](docs/SECRETS.md) — encryption, injection, threat model
- [Troubleshooting](docs/TROUBLESHOOTING.md) — common issues and fixes

## Specs

- [v3 Platform Spec](specs/AEGIS_v3_PLATFORM_SPEC.md) — current spec (instance-centric, post-pivot)
- [Architectural Pivot](specs/aegis_architectural_pivot_spec.md) — pivot from app-centric to instance-centric
- [Platform Spec (pre-pivot)](specs/AEGIS_PLATFORM_SPEC.md) — original architecture

## License

[Apache License 2.0](LICENSE)
