# AegisVM

A local, scale-to-zero microVM runtime for autonomous agent workloads.

AegisVM runs agent code inside isolated microVMs that boot in under a second, pause when idle, and wake on demand. It handles the hard infrastructure — VMs, networking, secrets, routing, lifecycle — so agent platforms don't have to.

## Why

Agent workloads don't fit containers or serverless. They run for minutes or hours, need real isolation (not just namespaces), maintain long-lived connections, expose services, and sit idle most of the time. AegisVM is built for exactly this shape of work.

**Scale to zero by default.** Nothing runs unless triggered. A paused VM resumes in ~35ms. A stopped VM cold-boots in ~500ms. The router accepts connections on declared ports, wakes the VM, and proxies traffic — no manual lifecycle management.

**Real isolation.** Each instance is a microVM with its own kernel, not a container sharing the host kernel. Code inside a VM cannot see the host filesystem, network, or processes.

**Kits extend the runtime.** Core AegisVM is a clean sandbox substrate. Kits add opinionated capabilities on top — like turning a VM into a messaging-driven LLM agent with wake-on-message and streaming Telegram responses.

## Install

```bash
brew tap xfeldman/aegisvm
brew install aegisvm
```

Requires macOS ARM64 (Apple Silicon M1+).

## Quick start

```bash
aegis up                                                    # start the daemon

aegis run -- echo "hello from aegisvm"                      # ephemeral VM
aegis run --expose 8080:80 -- python3 -m http.server 80     # with port mapping
aegis run --workspace ./myapp -- python3 /workspace/app.py  # with host directory mounted

aegis down                                                  # stop everything
```

## Agent Kit

Agent Kit turns AegisVM into a messaging-driven agent platform. The agent VM consumes zero CPU when idle — a new Telegram message wakes it in milliseconds, the LLM responds with streaming text, and the VM goes back to sleep.

```bash
brew install aegisvm-agent-kit

aegis secret set OPENAI_API_KEY sk-...
aegis secret set TELEGRAM_BOT_TOKEN 123456:ABC-...

# Start the agent — kit provides command, image, and capabilities
aegis instance start --kit agent --name my-agent --secret OPENAI_API_KEY

# Connect to Telegram
mkdir -p ~/.aegis/kits/my-agent
echo '{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","allowed_chats":["*"]}}' \
  > ~/.aegis/kits/my-agent/gateway.json
```

The gateway runs on the host alongside the instance, staying alive while the VM sleeps to catch incoming messages. Each agent instance gets its own gateway — run multiple bots independently.

See [Agent Kit docs](docs/AGENT_KIT.md) for the full guide. See [Kits](docs/KITS.md) for how kits work.

## MCP (Claude Code integration)

AegisVM ships an MCP server that lets LLMs drive sandboxed VMs directly — start instances, exec commands, read logs, manage secrets, use kits.

```bash
aegis mcp install
```

Once registered, Claude can spin up isolated VMs, run code, and tear them down — all through MCP tools.

## How it works

The only runtime object is an **instance** — a VM running a command. No apps, no releases, no deploy lifecycle.

```bash
# Ephemeral: run, collect output, done
aegis run -- python analyze.py

# Persistent: named instance with port exposure
aegis instance start --name web --expose 8080:80 -- python3 -m http.server 80
aegis exec web -- ls /workspace
aegis logs web --follow

# Lifecycle
aegis instance disable web     # stop VM, close listeners, prevent auto-wake
aegis instance start --name web  # re-enable from stored config
aegis instance delete web      # remove entirely
```

| Path | Latency |
|------|---------|
| Cold boot (zero to process running) | ~500ms |
| Resume from pause | ~35ms |

**Port mapping.** `--expose 8080:80` maps public 8080 to guest 80. All ports go through the router with wake-on-connect — paused and stopped VMs wake automatically on incoming connections.

**Workspaces.** Host directories mounted at `/workspace` inside the VM. Durable storage that survives VM termination. Named workspaces (`--workspace myapp`) or host paths (`--workspace ./code`).

**Secrets.** AES-256-GCM encrypted store. Explicit injection only (`--secret API_KEY`). Default: inject nothing.

**OCI images.** Use any Docker image as the VM filesystem: `--image python:3.12-alpine`, `--image node:20`. The VM's ENTRYPOINT/CMD are ignored — AegisVM controls the process.

## CLI

```bash
aegis up / down / status / doctor
aegis run [options] -- <cmd>                        # ephemeral instance
aegis instance start [options] -- <cmd>             # persistent instance
aegis instance list / info / disable / delete       # manage instances
aegis instance pause / resume                       # SIGSTOP / SIGCONT
aegis exec <name> -- <cmd>                          # run command in instance
aegis logs <name> [--follow]                        # stream logs
aegis secret set / list / delete                    # manage secrets
aegis kit list                                      # list installed kits
aegis mcp install / uninstall                       # Claude Code integration
```

Common flags: `--name`, `--expose`, `--env K=V`, `--secret KEY`, `--workspace PATH`, `--image REF`, `--kit KIT`.

Full reference: [CLI docs](docs/CLI.md).

## Architecture

```
Host
├── aegisd          daemon: API, lifecycle, router, VMM backend
├── aegis           CLI
├── aegis-mcp       MCP server for LLMs (host-side)
└── VMM (libkrun / Firecracker)
    ├── VM 1: aegis-harness (PID 1) → user command
    ├── VM 2: aegis-harness (PID 1) → user command
    └── ...
```

**libkrun** on macOS (Apple Hypervisor.framework), **Firecracker** on Linux (KVM). Same daemon, same harness, same CLI.

## What AegisVM is not

- **Not a container runtime.** Containers share the host kernel. Aegis VMs don't.
- **Not a cloud platform.** Single-host, local-first. No regions, no multi-tenancy.
- **Not an agent framework.** No prompt templates, no tool definitions, no LLM abstractions.
- **Not a workflow engine.** No DAGs, no step definitions, no retry policies.

## Documentation

- [Quickstart](docs/QUICKSTART.md) — zero to running in 5 minutes
- [Kits](docs/KITS.md) — optional add-on bundles, instance daemons
- [Agent Kit](docs/AGENT_KIT.md) — Telegram bot with wake-on-message
- [CLI Reference](docs/CLI.md) — complete command reference
- [Guest API](docs/GUEST_API.md) — spawn and manage instances from inside a VM
- [Workspaces](docs/WORKSPACES.md) — persistent volumes
- [Secrets](docs/SECRETS.md) — encryption, injection
- [Router](docs/ROUTER.md) — wake-on-connect, idle behavior
- [Troubleshooting](docs/TROUBLESHOOTING.md) — common issues

## Tests

```bash
make test                 # unit tests (no VM, fast)
make integration          # boots real VMs, full suite
```

## License

[Apache License 2.0](LICENSE)
