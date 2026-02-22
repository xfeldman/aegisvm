# CLAUDE.md

## Project Overview

AegisVM is a local agent runtime platform that runs AI agent workloads inside isolated microVMs. This repo implements the AegisVM platform core. The project brand is "AegisVM" but CLI binaries are named `aegis`, `aegisd`, `aegis-mcp`, etc.

## Key Architecture

Two-layer control model:

- **Infrastructure control plane (aegisd)**: VM lifecycle, port mapping, secrets, images, registry
- **Guest control agent (aegis-harness)**: PID 1 inside VMs, process management, exec, log streaming
- **Userland**: serving semantics, readiness, versioning — lives inside the VM

Components:

- **aegisd** (Go): infrastructure control plane daemon
- **aegis** (Go): CLI tool
- **aegis-harness** (Go): guest control agent (PID 1 inside VMs, JSON-RPC server)
- **aegis-mcp** (Go): MCP server for LLM integration (stdio JSON-RPC, talks to aegisd via unix socket)
- **VMM Backend**: libkrun on macOS (Apple HVF), Firecracker on Linux (KVM)
- **Registry**: SQLite for persistent state (instances, secrets)
- **Wire Protocol**: JSON-RPC 2.0 over vsock (control channel)
- **Ingress**: Docker-style static port mapping (`--expose`), infrastructure config not app semantics

## Current State: Instance-centric (post-pivot)

After the architectural pivot, AegisVM manages **instances** — a VM running a command with optional port exposure. No app, release, or task objects in core. The harness uses a single `run` RPC for all workloads. The `run` RPC is the handoff point — infrastructure hands off to guest control.

## Build

```bash
make all          # Build everything
make aegisd       # Daemon
make aegis        # CLI
make harness      # Guest harness (GOOS=linux GOARCH=arm64)
make mcp          # MCP server for LLM integration
make base-rootfs  # Alpine ARM64 ext4 with harness
```

## Dev Loop

```bash
brew install libkrun e2fsprogs
make all
./bin/aegisd &
./bin/aegis run -- echo hello
./bin/aegis run --expose 8080:80 -- python3 -m http.server 80
./bin/aegis instance list
./bin/aegis exec <handle> -- echo hello
./bin/aegis logs <handle>
./bin/aegis down
```

## CLI Commands

```
aegis up / down / status / doctor
aegis run [--expose [PUB:]GUEST[/proto]] [--env K=V] [--secret KEY] [--name H] [--workspace W] [--kit KIT] -- CMD
aegis instance start [--name H] [--expose [PUB:]GUEST[/proto]] [--env K=V] [--secret KEY] [--workspace W] [--image REF] [--kit KIT] -- CMD
aegis instance start --name H                              (restart stopped instance)
aegis instance list [--stopped | --running] / info / disable / delete / pause / resume
aegis instance prune --stopped-older-than <dur>
aegis exec <handle|id> -- CMD
aegis logs <handle|id> [--follow]
aegis secret set KEY VALUE / list / delete KEY
aegis kit list                                                       (list installed kits)
aegis mcp install / uninstall                                        (Claude Code MCP integration)
```

## Conventions

- Go 1.23+
- ARM64-only for M0-M5
- No conditional platform logic in core — all behind VMM interface
- VMM interface: CreateVM, StartVM, PauseVM, ResumeVM, StopVM, HostEndpoints, Capabilities
- Harness is statically linked (CGO_ENABLED=0 for harness)
- aegisd uses cgo (for libkrun bindings)

## Specs

Full specs live in the famiglia repo at `specs/aegis/`:
- AEGIS_v3_PLATFORM_SPEC.md — current platform spec (instance-centric, post-pivot)
- aegis_architectural_pivot_spec.md — pivot spec (instance-centric)
- AEGIS_PLATFORM_SPEC.md — original platform spec (pre-pivot, historical)
