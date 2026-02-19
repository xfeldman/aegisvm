# Aegis

A local, scale-to-zero microVM runtime for autonomous agent workloads.

Aegis runs agent code inside isolated microVMs that boot in under a second, hibernate when idle, and wake on demand. It handles the hard infrastructure — VMs, snapshots, networking, routing, lifecycle — so agent platforms don't have to.

## Why

Agent workloads don't fit containers or serverless. They run for minutes or hours, need real isolation (not just namespaces), maintain long-lived connections, expose HTTP and TCP services, and sit idle most of the time. Aegis is built for exactly this shape of work.

## Instances

Aegis manages **instances** — a VM running a command with optional port exposure. Everything is scale-to-zero by default.

```bash
# Run a command, collect output, done
aegis run -- python analyze.py

# Run with exposed ports (Docker-style static mapping)
aegis run --expose 80 -- python app.py

# Long-lived instance with a handle
aegis instance start --name myapp --expose 80 -- python3 -m http.server 80
aegis exec myapp -- echo hello
aegis logs myapp --follow
aegis instance stop myapp     # VM stopped, instance stays in list
aegis instance delete myapp   # removed entirely
```

Port exposure is infrastructure configuration — like CPU or memory. It configures VMM port forwarding at creation time. If nothing binds the port, the router returns 503. No readiness gating in core.

Measured on macOS ARM64 (M1, libkrun backend):

| Path | Latency |
|------|---------|
| Cold boot (zero to process running) | ~500ms |
| Resume from pause (SIGCONT) | ~35ms |

## Core mechanisms

**Docker-style static port mapping** is the ingress model. `--expose` configures port forwarding at instance creation. It does not enable a "mode," imply readiness, or affect lifecycle. The router proxies traffic, resumes paused instances on ingress, and returns 503 if the backend is unreachable.

**The snapshot rule** keeps lifecycle predictable: disk layers are canonical, memory snapshots are cache. Terminate always restores from disk layers. Pause retains RAM for fast resume but is never treated as durable state.

**Two-layer control model.** The infrastructure control plane (aegisd) manages VMs, port mapping, secrets, and images. The guest control agent (harness) manages the process inside the VM. Serving semantics, readiness, and versioning belong to the kit layer outside core.

**Firecracker and libkrun** are the microVM backbone. Firecracker on Linux (KVM), libkrun on macOS (Apple Hypervisor.framework). Same daemon, same harness, same CLI. The VMM is an implementation detail behind a narrow interface.

**Workspace volumes** give agents durable storage that survives VM termination.

## What Aegis is not

**Not a generic hypervisor.** Fixed base image, opinionated lifecycle, one kind of workload.

**Not a container replacement.** Containers share the host kernel. Aegis VMs don't.

**Not a cloud platform.** Single-host, local-first. No regions, no load balancers, no multi-tenancy.

**Not a workflow engine.** No DAGs, no step definitions, no retry policies.

**Not an agent framework.** No prompt templates, no tool definitions, no LLM abstractions.

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
./bin/aegis run --expose 80 -- python3 -m http.server 80
./bin/aegis down
```

## Architecture

```
┌──────────────────────────────────────────────┐
│  Host                                        │
│                                              │
│  aegisd (Go)          aegis CLI              │
│    ├── API server        ├── up / down       │
│    ├── lifecycle mgr     ├── run / instance  │
│    ├── router            ├── exec / logs     │
│    └── VMM backend       └── secret          │
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

**aegis-vmm-worker** — per-VM helper process. Configures the VMM backend and hands control to the hypervisor.

**aegis-harness** — guest control agent. PID 1 inside every VM. Handles JSON-RPC commands (`run`, `exec`, `health`, `shutdown`). Sends `processExited` and `log` notifications.

**aegis** — CLI. Talks to aegisd over the unix socket.

## Tests

Unit tests (no VM, fast):

```bash
make test
```

Integration tests (boots real VMs, requires built binaries + base rootfs at `~/.aegis/base-rootfs`):

```bash
make integration          # full suite (includes pause/resume)
make integration SHORT=1  # skip pause/resume test
```

The integration suite manages the daemon lifecycle automatically. Tests cover instance lifecycle (boot, run, exec, logs, pause/resume, stop) and backend conformance.

## Documentation

- [Quickstart](docs/QUICKSTART.md) — zero to running agent in 5 minutes
- [Agent Conventions](docs/AGENT_CONVENTIONS.md) — guest environment contract (filesystem, secrets, logging, signals)
- [CLI Reference](docs/CLI.md) — complete command reference
- [Router](docs/ROUTER.md) — handle-based routing, wake-on-connect, idle behavior
- [Workspaces](docs/WORKSPACES.md) — persistent volumes, host paths, lifecycle
- [Secrets](docs/SECRETS.md) — encryption, injection, threat model
- [Kits](docs/KITS.md) — kit boundary, what kits control vs what core owns
- [Troubleshooting](docs/TROUBLESHOOTING.md) — common issues and fixes

## Specs

- [Platform spec](specs/AEGIS_PLATFORM_SPEC.md) — architecture, lifecycle, APIs, security model
- [Implementation kickoff](specs/IMPLEMENTATION_KICKOFF.md) — engineering decisions, milestones, project structure
- [Implementation notes](specs/IMPLEMENTATION_NOTES.md) — M0-M3b post-implementation details and corrections
- [Architectural pivot](specs/aegis_architectural_pivot_spec.md) — instance-centric architecture
- [Expose model](specs/aegis_docker_style_expose_model.md) — Docker-style static port mapping
- [Kit boundary](specs/KIT_BOUNDARY_SPEC.md) — responsibility split between core and kits

## Status

**Post-pivot.** Instance-centric architecture. No app/task/kit objects in core.

| Milestone | Status | Adds |
|---|---|---|
| **M0** | **Done** | Boot + run. libkrun backend, VMM interface, harness, CLI. |
| **M1** | **Done** | Router with wake-on-connect, SIGSTOP/SIGCONT pause/resume, SQLite registry. |
| **M2** | **Done** | OCI images, overlays, workspace volumes. |
| **M3** | **Done** | Secrets, conformance test suite. |
| **M3b** | **Done** | Durable logs, exec into running VMs, instance list/info. |
| **Pivot** | **Done** | Instance-centric architecture. Removed app/task/kit from core. |
| M4 | Next | Firecracker on Linux. Both backends pass conformance. |
| M5 | — | Shared workspaces, network groups, warm pool, GC. |
