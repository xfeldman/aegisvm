# Kits

Kits are optional add-on bundles that extend AegisVM with specific capabilities. Core AegisVM provides the VM runtime — instances, networking, secrets, lifecycle. Kits provide opinionated workloads that run on top.

## Install

```bash
brew tap xfeldman/aegisvm
brew install aegisvm-agent-kit    # example: Agent Kit
```

List installed kits:

```bash
aegis kit list
```

## Using a kit

The `--kit` flag on `aegis instance start` is a preset. It supplies default command, image, and capabilities from the kit manifest. Explicit flags override kit defaults.

```bash
# Create an instance using kit defaults
aegis instance start --kit agent --name my-agent --env OPENAI_API_KEY

# Override the command (e.g. debug shell in a kit-configured VM)
aegis instance start --kit agent --name debug -- sh

# Restart a stopped kit instance (no --kit needed — config is stored)
aegis instance start --name my-agent
```

## Kit manifests

Each kit ships a manifest file (`agent.json`, etc.) that declares:

- **image** — base OCI image and binaries to inject into the VM
- **defaults** — default command and capabilities
- **instance_daemons** — host-side processes to run alongside each instance

Manifests are installed to `~/.aegis/kits/` (from source) or discovered from the Homebrew prefix (from brew).

## Instance daemons

Kits can declare host-side daemon processes that run alongside each instance. These are declared in the manifest's `instance_daemons` field.

**Lifecycle:**

- **Start** when the instance is created or re-enabled
- **Stop** when the instance is disabled or deleted
- **Survive** VM pause and stop — the daemon stays running even when the VM is idle, enabling features like wake-on-message
- **Restart** automatically on crash (with backoff)
- **Restore** on daemon boot for all enabled kit instances

Each instance gets its own daemon process. Multiple kit instances run independent daemons.

**Per-instance config:**

Instance daemons read their config from `~/.aegis/kits/{handle}/`. The directory name matches the instance handle. Config changes are picked up automatically (hot-reload).

```
~/.aegis/kits/my-agent/gateway.json     # config for instance "my-agent"
~/.aegis/kits/my-bot/gateway.json       # config for instance "my-bot"
```

Check daemon status:

```bash
aegis instance info my-agent
# ...
# Gateway:     running
```

## Available kits

- **[Agent Kit](AGENT_KIT.md)** — messaging-driven LLM agent with Telegram integration
