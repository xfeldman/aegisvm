# CLAUDE.md

## Project Overview

Aegis is a local agent runtime platform that runs AI agent workloads inside isolated microVMs. This repo implements the Aegis platform core.

## Key Architecture

Two-layer control model:

- **Infrastructure control plane (aegisd)**: VM lifecycle, port mapping, secrets, images, registry
- **Guest control agent (aegis-harness)**: PID 1 inside VMs, process management, exec, log streaming
- **Application control plane**: kit layer (versioning, readiness, routing policy) — lives outside core

Components:

- **aegisd** (Go): infrastructure control plane daemon
- **aegis** (Go): CLI tool
- **aegis-harness** (Go): guest control agent (PID 1 inside VMs, JSON-RPC server)
- **VMM Backend**: libkrun on macOS (Apple HVF), Firecracker on Linux (KVM)
- **Registry**: SQLite for persistent state (instances, secrets)
- **Wire Protocol**: JSON-RPC 2.0 over vsock (control channel)
- **Ingress**: Docker-style static port mapping (`--expose`), infrastructure config not app semantics

## Current State: Instance-centric (post-pivot)

After the architectural pivot, Aegis manages **instances** — a VM running a command with optional port exposure. No app, release, task, or kit objects in core. The harness uses a single `run` RPC for all workloads. The `run` RPC is the handoff point — infrastructure hands off to guest control.

## Build

```bash
make all          # Build everything
make aegisd       # Daemon
make aegis        # CLI
make harness      # Guest harness (GOOS=linux GOARCH=arm64)
make base-rootfs  # Alpine ARM64 ext4 with harness
```

## Dev Loop

```bash
brew install libkrun e2fsprogs
make all
./bin/aegisd &
./bin/aegis run -- echo hello
./bin/aegis run --expose 80 -- python3 -m http.server 80
./bin/aegis instance list
./bin/aegis exec <handle> -- echo hello
./bin/aegis logs <handle>
./bin/aegis down
```

## CLI Commands

```
aegis up / down / status / doctor
aegis instance start [--name H] [--expose P] [--env K=V] [--secret KEY] [--image REF] -- CMD
aegis instance list / info / stop / delete / pause / resume
aegis exec <handle|id> -- CMD
aegis logs <handle|id> [--follow]
aegis run [--expose P] [--env K=V] [--secret KEY] [--name H] -- CMD   (sugar: start + follow + delete)
aegis secret set KEY VALUE / list / delete KEY
```

## Conventions

- Go 1.23+
- ARM64-only for M0-M5
- No conditional platform logic in core — all behind VMM interface
- VMM interface is frozen — do not modify without explicit approval
- Harness is statically linked (CGO_ENABLED=0 for harness)
- aegisd uses cgo (for libkrun bindings)

## Specs

Full specs live in the famiglia repo at `specs/aegis/`:
- AEGIS_PLATFORM_SPEC.md — platform spec
- IMPLEMENTATION_KICKOFF.md — engineering decisions + milestones
- aegis_architectural_pivot_spec.md — pivot spec (instance-centric)
- KIT_BOUNDARY_SPEC.md — kit boundary
