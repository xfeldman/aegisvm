# Aegis

A local, scale-to-zero microVM runtime for autonomous agent workloads.

Aegis runs agent code inside isolated microVMs that boot in under a second, hibernate when idle, and wake on demand. It handles the hard infrastructure — VMs, snapshots, networking, routing, lifecycle — so agent platforms don't have to.

## Why

Agent workloads don't fit containers or serverless. They run for minutes or hours, need real isolation (not just namespaces), maintain long-lived connections, expose HTTP and TCP services, and sit idle most of the time. Aegis is built for exactly this shape of work.

## Two execution modes

Everything is scale-to-zero by default. Nothing runs unless triggered.

**Task mode** — run a command, collect output, done. VM lifecycle is tied to task completion. Boot, execute, capture logs and artifacts, terminate. No state carried between runs.

```bash
aegis run -- python analyze.py    # boot VM, run, collect output, done
```

**Serve mode** — expose ports, wake on connection, hibernate on idle. VM lifecycle is tied to connection activity. The router accepts connections on any declared port, wakes the VM if it's paused or terminated, proxies traffic, tracks activity, and hibernates the VM when the last connection closes.

```bash
aegis run --expose 80:http --expose 5432:tcp -- python app.py
```

A web UI, a database sandbox, an MCP tool server, and a gRPC endpoint all use the same mechanism. Connection arrives → VM wakes → router proxies → idle timer starts on last disconnect → pause → terminate. Same router, same wake-on-connect, same scale-to-zero.

Measured on macOS ARM64 (M1, libkrun backend):

| Path | Latency |
|------|---------|
| Cold boot (zero to HTTP 200) | ~500ms |
| Resume from pause (SIGCONT to HTTP 200) | ~35ms |

## Core mechanisms

**ServeTargets** are the declarative object at the center of serve mode. A ServeTarget declares which ports a VM exposes, what protocol each uses (HTTP, TCP, gRPC, WebSocket), and how the router behaves during wake. The router is protocol-aware: HTTP ports get a loading page while the VM boots; TCP ports hold the connection silently; gRPC ports queue requests. All ports get wake-on-connect and scale-to-zero automatically.

**The snapshot rule** keeps lifecycle predictable: disk layers are canonical, memory snapshots are cache. Publishing a release produces disk artifacts only — never a memory snapshot. Terminate always restores from disk layers. Pause retains RAM for fast resume but is never treated as durable state. Every restore is reproducible and GC is safe.

**Kits** are the integration surface. A kit defines what goes inside the VM (base image, runtimes, SDKs), how the agent talks to external services, and how URLs are routed. Aegis core doesn't know about chat protocols, data APIs, or orchestration logic — kits do. If you can build a kit without modifying Aegis core, the abstraction is correct.

**Firecracker and libkrun** are the microVM backbone. Firecracker on Linux (KVM), libkrun on macOS (Apple Hypervisor.framework). Same daemon, same harness, same CLI. The VMM is an implementation detail behind a narrow interface with explicit capability reporting. Aegis semantics are backend-independent; if a backend can't do memory snapshots, it restores from disk layers instead. No feature degrades into incorrect behavior — only into slower recovery.

**Workspace volumes** give agents durable storage that survives VM termination. One volume per agent (isolated mode) or one volume shared across a group of agents collaborating on the same task (shared mode). Workspace data is never part of any snapshot — it's a separate mount with a separate lifecycle.

**Network groups** let agents in the same session reach each other over a private subnet while remaining invisible to everything else. A coordinator spawns five agents on a shared codebase — they can communicate. A different session's agents cannot. The router still handles all external ingress.

## What Aegis is not

**Not a generic hypervisor.** Fixed base image, opinionated lifecycle, one kind of workload. You don't pick your kernel or configure virtio devices.

**Not a container replacement.** Containers share the host kernel. Aegis VMs don't. The isolation boundary is the hypervisor, not namespaces.

**Not a cloud platform.** Single-host, local-first. No regions, no load balancers, no multi-tenancy.

**Not a workflow engine.** No DAGs, no step definitions, no retry policies. Orchestration belongs in kits or in the agent itself.

**Not an agent framework.** No prompt templates, no tool definitions, no LLM abstractions. Aegis doesn't know what an "agent" is. It knows what a microVM is.

## Quick start

```bash
# Dependencies (macOS ARM64)
brew tap slp/krun && brew install libkrun

# Build
make all
make base-rootfs    # requires Docker

# Run
./bin/aegisd &
./bin/aegis run -- echo "hello from aegis"
./bin/aegis down
```

## Architecture

```
┌──────────────────────────────────────────────┐
│  Host                                        │
│                                              │
│  aegisd (Go)          aegis CLI              │
│    ├── API server        ├── up / down       │
│    ├── task manager      ├── run             │
│    ├── router (M1+)      ├── status          │
│    └── VMM backend       └── doctor          │
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

**aegisd** — daemon. Manages VM lifecycle, serves the HTTP API on a unix socket, runs the connection-aware router. Pure Go, no cgo.

**aegis-vmm-worker** — per-VM helper process. Configures the VMM backend and hands control to the hypervisor. Separate process because `krun_start_enter()` takes over the caller.

**aegis-harness** — guest PID 1. Statically linked Go binary (linux/arm64) inside every VM. Connects back to aegisd, handles JSON-RPC commands (`runTask`, `startServer`, `exec`, `health`, `shutdown`).

**aegis** — CLI. Talks to aegisd over the unix socket.

## Tests

Unit tests (no VM, fast):

```bash
make test
```

Integration tests (boots real VMs, requires built binaries + base rootfs installed at `~/.aegis/base-rootfs`):

```bash
make integration          # full suite (includes pause/resume)
make integration SHORT=1  # skip pause/resume test
make test-m3              # M3 + conformance tests only
```

The integration suite manages the daemon lifecycle automatically. Tests cover M0 (task mode), M1 (serve + pause/resume), M2 (images + apps + releases), M3 (secrets + kits + conformance), and M3b (logs + exec + instance inspect).

Python SDK tests:

```bash
cd sdk/python && python3 -m venv .venv && .venv/bin/pip install pytest
.venv/bin/python -m pytest tests/ -v
```

## Documentation

- [Quickstart](docs/QUICKSTART.md) — zero to running agent in 5 minutes
- [Agent Conventions](docs/AGENT_CONVENTIONS.md) — guest environment contract (filesystem, secrets, logging, signals)
- [CLI Reference](docs/CLI.md) — complete command reference
- [Router](docs/ROUTER.md) — app resolution, wake-on-connect, idle behavior
- [Workspaces](docs/WORKSPACES.md) — persistent volumes, host paths, lifecycle
- [Secrets](docs/SECRETS.md) — encryption, scopes, injection, threat model
- [Kits](docs/KITS.md) — manifest schema, hooks, kit boundary
- [Troubleshooting](docs/TROUBLESHOOTING.md) — common issues and fixes

## Specs

- [Platform spec](specs/AEGIS_PLATFORM_SPEC.md) — architecture, lifecycle, APIs, security model
- [Implementation kickoff](specs/IMPLEMENTATION_KICKOFF.md) — engineering decisions, milestones, project structure
- [Implementation notes](specs/IMPLEMENTATION_NOTES.md) — M0-M3b post-implementation details and corrections
- [Famiglia kit](specs/FAMIGLIA_KIT_SPEC.md) — team agents with chat and data integration
- [OpenClaw kit](specs/OPENCLAW_KIT_SPEC.md) — multi-agent autonomous runtime

## Status

**M3b complete.** Durable logs, exec into running VMs, instance inspect — full operational visibility.

| Milestone | Status | Adds |
|---|---|---|
| **M0** | **Done** | Boot + run. libkrun backend, VMM interface, harness, CLI. |
| **M1** | **Done** | Serve mode, router with wake-on-connect, SIGSTOP/SIGCONT pause/resume, SQLite registry. |
| **M2** | **Done** | Releases, publishing, OCI images, overlays, workspace volumes. |
| **M3** | **Done** | Kits, secrets, conformance test suite. |
| **M3a** | **Done** | Agent conventions, Python SDK, CLI docs, base images, examples. |
| **M3b** | **Done** | Durable logs, exec into running VMs, instance list/info. |
| M4 | Next | Firecracker on Linux. Both backends pass conformance. |
| M5 | — | Shared workspaces, network groups, warm pool, GC. |
