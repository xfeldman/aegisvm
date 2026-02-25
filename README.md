# AegisVM

*Give an AI agent a computer.*

A local, scale-to-zero microVM runtime for autonomous agent workloads.

AegisVM runs agent code inside isolated microVMs that boot in under a second, pause when idle, and wake on demand. It handles the hard infrastructure — VMs, networking, secrets, routing, lifecycle — so agent platforms don't have to.

## Why

Agent workloads don't fit containers or serverless. They run for minutes or hours, need real isolation (not just namespaces), maintain long-lived connections, expose services, and sit idle most of the time. AegisVM is built for exactly this shape of work.

**Scale to zero by default.** Nothing runs unless triggered. A paused VM resumes in ~35ms. A stopped VM cold-boots in ~500ms. The router accepts connections on declared ports, wakes the VM, and proxies traffic — no manual lifecycle management.

**Real isolation.** Each instance is a microVM with its own kernel, not a container sharing the host kernel. Code inside a VM cannot see the host filesystem, network, or processes.

**Kits extend the runtime.** Core AegisVM is a clean sandbox substrate. Kits add opinionated capabilities on top — like turning a VM into a messaging-driven LLM agent with wake-on-message and streaming Telegram responses.

## Install

### macOS (Homebrew)

```bash
brew tap xfeldman/aegisvm
brew install aegisvm
```

Requires Apple Silicon (M1+).

### Linux

```bash
curl -sSL https://raw.githubusercontent.com/xfeldman/aegisvm/main/install.sh | sh
```

Installs `aegisvm` + `aegisvm-agent-kit` and dependencies. Requires x86_64 or arm64 with KVM (`/dev/kvm`).

## Quick start

```bash
aegis up                                                    # start the daemon

aegis run -- echo "hello from aegisvm"                      # ephemeral VM
aegis run --expose 8080:80 -- python3 -m http.server 80     # with port mapping
aegis run --workspace ./myapp -- python3 /workspace/app.py  # with host directory mounted

aegis down                                                  # stop everything
```

## Agent Kit

Agent Kit adds an LLM agent to AegisVM instances. Each agent runs in its own isolated VM — delegate tasks from Claude Code, connect messaging bots, or build multi-agent pipelines where agents spawn sub-agents.

**Pair debugging across host + VM.** Run your backend inside a VM while a host-side agent and the in-VM agent collaborate over tether. You get live, interactive debugging with full isolation: the host agent can orchestrate, the VM agent can inspect the sandboxed runtime, and the VM still scale-to-zero when idle.

```bash
# macOS (Linux: already included by install.sh)
brew install aegisvm-agent-kit
```

```bash
aegis secret set OPENAI_API_KEY sk-...
aegis instance start --kit agent --name my-agent --secret OPENAI_API_KEY
```

The agent is immediately reachable — Claude Code can delegate tasks, read streaming responses, and orchestrate multiple agents:

```
You: Start a new aegis instance with Agent kit using OpenAI, and try to contact its agent.

Claude: Let me spin up an agent instance and ping it.
        ⏺ aegis — instance_start (kit="agent", name="my-agent", secrets=["OPENAI_API_KEY"])
        Instance is starting. Let me send it a message.
        ⏺ aegis — tether_send (instance="my-agent", text="Hello, are you there?")
        Message sent. Let me read the response.
        ⏺ aegis — tether_read (instance="my-agent", after_seq=1, wait_ms=15000)
        It's alive and responding! The agent replied:
          "Hello! I'm an AI assistant running inside an Aegis VM. I can help with
           executing shell commands, reading and writing files, and managing
           resources within the workspace. Let me know what you need!"
```

Agents can also spawn sub-agents via the Guest API, and optionally connect to messaging apps for conversational AI with wake-on-message and scale-to-zero:

```bash
# Connect to Telegram (optional)
aegis secret set TELEGRAM_BOT_TOKEN 123456:ABC-...
mkdir -p ~/.aegis/kits/my-agent
echo '{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","allowed_chats":["*"]}}' \
  > ~/.aegis/kits/my-agent/gateway.json
# Gateway picks up config within seconds — send a message to your bot
```

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

**libkrun** on macOS (Apple Hypervisor.framework), **Cloud Hypervisor** on Linux (KVM). Same daemon, same harness, same CLI.

## Why not...

**Docker / Podman** — Containers share the host kernel. A malicious or buggy agent can escape via kernel exploits, mount the host filesystem, or interfere with other containers. AegisVM runs each workload in its own microVM with a separate kernel — true isolation, not namespace tricks. Docker also has no concept of scale-to-zero, wake-on-connect, or idle hibernation. You manage container lifecycle yourself.

**E2B** — Cloud-hosted sandboxes. Great if you want managed infrastructure, but your code runs on someone else's machines, your data leaves your network, and you pay per-second. AegisVM runs locally on your own hardware — zero latency to your local files, no API keys leaving the machine, no cloud bills. You own the box.

**Firecracker directly** — Firecracker is a VMM, not a runtime. It gives you a VM. You still need to build rootfs images, manage networking, handle lifecycle, implement port mapping, build a control plane, and write a guest agent. AegisVM does all of that and gives you a single CLI.

**AWS Lambda / Cloud Functions** — Designed for stateless request-response, not long-running agents. Cold starts are seconds, not milliseconds. No persistent connections, no exposed ports, no local filesystem. Agent workloads need to maintain state, run for minutes or hours, and wake on various triggers — not just HTTP.

**LXC / systemd-nspawn** — Lightweight, but still container-based (shared kernel). No hardware-level isolation. No built-in networking, port mapping, or lifecycle management for agent workloads. AegisVM provides all of this out of the box with microVM-grade isolation.

**Running agents directly on the host** — Works until it doesn't. No isolation between agents, no resource limits, no cleanup on crash, agents can read your files and credentials. One misbehaving agent affects everything else. AegisVM gives each agent its own isolated VM with explicit secret injection — nothing leaks unless you allow it.

## Documentation

- [Quickstart](docs/QUICKSTART.md) — zero to running in 5 minutes
- [Tether](docs/TETHER.md) — host-to-agent messaging, delegation, long-poll
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
