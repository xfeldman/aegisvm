# AegisVM

*Give an AI agent a computer.*

A local, scale-to-zero microVM runtime for autonomous agent workloads.

AegisVM runs agent code inside isolated microVMs that boot in under a second, pause when idle, and wake on demand. It handles the hard infrastructure — VMs, networking, secrets, routing, lifecycle — so agent platforms don't have to.

## Why

Agent workloads don't fit containers or serverless. They run for minutes or hours, need real isolation (not just namespaces), maintain long-lived connections, expose services, and sit idle most of the time. AegisVM is built for exactly this shape of work.

**Scale to zero by default.** Nothing runs unless triggered. A paused VM resumes in ~35ms. A stopped VM cold-boots in ~500ms. The router accepts connections on declared ports, wakes the VM, and proxies traffic — no manual lifecycle management.

**Real isolation.** Each instance is a microVM with its own kernel, not a container sharing the host kernel. Code inside a VM cannot see the host filesystem, network, or processes.

**Kits extend the runtime.** Core AegisVM is a clean sandbox substrate. Kits add opinionated capabilities on top — like turning a VM into a messaging-driven LLM agent with wake-on-message and streaming Telegram responses.

| Path | Latency |
|------|---------|
| Cold boot (zero to process running) | ~500ms |
| Resume from pause | ~35ms |

| Alternative | Limitation |
|---|---|
| **Docker / Podman** | Shared host kernel — no real isolation. No scale-to-zero or wake-on-connect. |
| **E2B** | Cloud-hosted — your data leaves your machine, pay per-second. |
| **Firecracker / CH directly** | VMMs, not runtimes. No lifecycle, networking, port mapping, or guest agent. |
| **Lambda / Cloud Functions** | Stateless, second-scale cold starts, no persistent connections or ports. |
| **LXC / systemd-nspawn** | Shared kernel. No built-in networking or lifecycle for agents. |
| **Running on host** | No isolation, no resource limits, agents can read your files and credentials. |

---

# Agent Kit

*Turn a VM into an autonomous LLM agent.*

Agent Kit turns an AegisVM instance into a full-featured LLM agent — 19 built-in tools, persistent memory, scheduled tasks, web search, image generation, and multi-agent orchestration. All in a ~40MB idle footprint with scale-to-zero.

Unlike monolithic agent frameworks, Agent Kit is modular Go compiled into a single static binary. No Python/Node runtime overhead for core tools. MCP servers are optional add-ons for specialized capabilities (browser automation, custom integrations).

## What's included

| Category | Tools |
|----------|-------|
| **File ops** | bash, read/write/edit file, glob, grep |
| **Web** | web_search, image_search, web_fetch |
| **Images** | image_generate (DALL-E), respond_with_image |
| **Memory** | Persistent memory with auto-injection into LLM context |
| **Cron** | Scheduled tasks with scale-to-zero (gateway-side scheduler) |
| **Self-management** | self_restart (hot config reload), self_info, disabled_tools |
| **VM orchestration** | Spawn/manage child VMs, expose ports, keepalive |

All built-in tools are Go — zero runtime overhead. Any tool can be disabled and replaced with a custom MCP server via `agent.json`.

## Why not OpenClaw?

| | Agent Kit | OpenClaw |
|---|---|---|
| **Architecture** | Modular Go binary + optional MCP | Monolithic Python framework |
| **Idle footprint** | ~40MB | ~200MB+ |
| **Core tools** | 19 built-in (Go, zero overhead) | Python-based, runtime-dependent |
| **Extensibility** | MCP servers + `disabled_tools` config | Plugin system |
| **Memory** | Built-in with auto-injection | Requires external service |
| **Cron** | Built-in with scale-to-zero | Not included |
| **Image gen** | Built-in (OpenAI API, 0 overhead) | MCP or plugin |
| **Browser** | MCP add-on (when needed) | Built-in (always loaded) |
| **VM isolation** | Real microVM per agent | Container or process |
| **Scale-to-zero** | Native (pause/resume in ms) | Not supported |

Agent Kit is the right choice when you want lightweight, isolated agents that scale to zero. OpenClaw is the right choice when you need a batteries-included Python framework with a large plugin ecosystem.

---

# Getting Started

## Install

### macOS (Homebrew)

```bash
brew tap xfeldman/aegisvm
brew install aegisvm                # core runtime
brew install aegisvm-agent-kit      # agent kit (optional)
```

Requires Apple Silicon (M1+).

### Linux

```bash
curl -sSL https://raw.githubusercontent.com/xfeldman/aegisvm/main/install.sh | sh
```

Installs `aegisvm` + `aegisvm-agent-kit` and dependencies. Requires x86_64 or arm64 with KVM (`/dev/kvm`).

## Usage: Core

```bash
aegis up                                                    # start the daemon

aegis run -- echo "hello from aegisvm"                      # ephemeral VM
aegis run --expose 8080:80 -- python3 -m http.server 80     # with port mapping
aegis run --workspace ./myapp -- python3 /workspace/app.py  # with host directory mounted

# Persistent instances
aegis instance start --name web --expose 8080:80 -- python3 -m http.server 80
aegis exec web -- ls /workspace
aegis logs web --follow
aegis instance disable web                                  # stop VM
aegis instance start --name web                             # restart from stored config
aegis instance delete web                                   # remove entirely

aegis down                                                  # stop everything
```

**Port mapping.** `--expose 8080:80` maps public 8080 to guest 80. All ports go through the router with wake-on-connect — paused and stopped VMs wake automatically on incoming connections.

**Workspaces.** Host directories mounted at `/workspace` inside the VM. Auto-created if not specified. Named workspaces (`--workspace myapp`) or host paths (`--workspace ./code`).

**Secrets.** AES-256-GCM encrypted store. Explicit injection only (`--env API_KEY`). Default: inject nothing.

**OCI images.** Use any Docker image as the VM filesystem: `--image python:3.12-alpine`, `--image node:20`. OCI image ENV vars (PATH, GOPATH, etc.) are automatically propagated.

**MCP.** AegisVM ships an MCP server that lets LLMs drive sandboxed VMs directly — start instances, exec commands, read logs, manage secrets, use kits.

```bash
aegis mcp install     # register with Claude Code
```

## Usage: Agent Kit

### Start an agent

```bash
aegis secret set OPENAI_API_KEY sk-...
aegis instance start --kit agent --name my-agent --env OPENAI_API_KEY
```

### Talk to it (from Claude Code via MCP)

```
Claude: ⏺ aegis — tether_send (instance="my-agent", text="Research the top 5 ML frameworks")
        ⏺ aegis — tether_read (instance="my-agent", wait_ms=30000)
        The agent responded with a detailed comparison...
```

### Heavy profile (browser MCP, Node MCP servers)

```bash
aegis instance start --kit agent --name browser-agent \
  --image node:22-alpine --memory 2048 --env OPENAI_API_KEY
```

### Connect to Telegram

```bash
aegis secret set TELEGRAM_BOT_TOKEN 123456:ABC-...
aegis instance start --kit agent --name my-agent \
  --env OPENAI_API_KEY --env TELEGRAM_BOT_TOKEN

mkdir -p ~/.aegis/kits/my-agent
echo '{"telegram":{"allowed_chats":["*"]}}' \
  > ~/.aegis/kits/my-agent/gateway.json
# Gateway picks up config within seconds — send a message to your bot
```

### Add web search and image generation

```bash
aegis secret set BRAVE_SEARCH_API_KEY BSA...
aegis instance start --kit agent --name my-agent \
  --env OPENAI_API_KEY --env BRAVE_SEARCH_API_KEY
```

The agent can now search the web, find images, and generate AI images — all built-in.

## Architecture

```
Host
├── aegisd              daemon: API, lifecycle, router, VMM backend
├── aegis               CLI
├── aegis-mcp           MCP server for host LLMs (Claude Code integration)
├── aegis-gateway       per-instance host daemon (Telegram bridge, cron scheduler)
│
└── VMM (libkrun / Cloud Hypervisor)
    ├── VM 1: aegis-harness (PID 1) → user command
    ├── VM 2: aegis-harness (PID 1) → aegis-agent (Agent Kit)
    │         ├── 19 built-in tools (Go, compiled in)
    │         ├── aegis-mcp-guest (VM orchestration)
    │         ├── memory, cron, sessions (workspace-backed)
    │         └── LLM API (OpenAI / Anthropic)
    └── ...
```

**libkrun** on macOS (Apple Hypervisor.framework), **Cloud Hypervisor** on Linux (KVM). Same daemon, same harness, same CLI.

**Agent Kit fits on top of core** — it's a kit binary (`aegis-agent`) injected into the VM's OCI image overlay. The harness starts it as the primary process. The gateway runs on the host alongside aegisd, bridging messaging apps and cron to the agent via tether. Everything else (VM lifecycle, networking, secrets, port mapping) is core AegisVM.

## Tether

Tether is the bidirectional message channel between host and VM agents. Everything flows through it — Claude Code delegation, Telegram messages, cron-scheduled tasks, multi-agent orchestration.

```
Host (Claude Code) ──tether──► Agent VM ──tether──► Child Agent VM
Telegram ──gateway──► tether ──┘
Cron     ──gateway──► tether ──┘
```

**Wake-on-message.** Sending a tether frame to a paused VM wakes it in ~35ms. The gateway stays running while VMs sleep — that's what enables wake-on-message for Telegram and cron.

**Sessions.** Each conversation gets an independent session (`channel:session_id`). A Telegram chat, a Claude delegation task, and a cron job each have their own history. Sessions persist across VM restarts.

**Async.** Send a message and read responses later — no blocking. Long-poll support for real-time streaming.

See [Tether docs](docs/TETHER.md) for the full protocol reference, frame types, and API endpoints.

## CLI Reference

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

Common flags: `--name`, `--expose`, `--env KEY|K=V|K=secret.name`, `--workspace PATH`, `--image REF`, `--kit KIT`, `--memory MB`.

Full reference: [CLI docs](docs/CLI.md).

## Documentation

- [Quickstart](docs/QUICKSTART.md) — zero to running in 5 minutes
- [Agent Kit](docs/AGENT_KIT.md) — full guide: all tools, config, profiles, sessions
- [Tether](docs/TETHER.md) — host-to-agent messaging, delegation, long-poll
- [Kits](docs/KITS.md) — optional add-on bundles, instance daemons
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
