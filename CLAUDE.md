# CLAUDE.md

## Project Overview

Aegis is a local agent runtime platform that runs AI agent workloads inside isolated microVMs. This repo implements the Aegis platform core.

## Key Architecture

- **aegisd** (Go): local control plane daemon managing microVMs, written in Go
- **aegis** (Go): CLI tool
- **aegis-harness** (Go): guest PID 1 inside VMs, vsock JSON-RPC server
- **VMM Backend**: libkrun on macOS (Apple HVF), Firecracker on Linux (KVM)
- **Registry**: SQLite (M1+), in-memory for M0
- **Wire Protocol**: JSON-RPC 2.0 over vsock

## Current Milestone: M0

M0 = boot a VM on macOS ARM64, run a command, get output. No networking, no router, no kits, no secrets, no SQLite.

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
./bin/aegis down
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
- FAMIGLIA_KIT_SPEC.md — Famiglia kit
- OPENCLAW_KIT_SPEC.md — OpenClaw kit
