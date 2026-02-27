# AegisVM CLI Reference

The `aegis` command is the primary interface to the AegisVM agent runtime platform.
All commands communicate with the `aegisd` daemon over a Unix socket at
`~/.aegis/aegisd.sock`. The daemon PID file is stored at `~/.aegis/data/aegisd.pid`.

```
aegis <command> [options]
```

Run `aegis help` to print top-level usage.

---

## AegisVM Is Not Docker

AegisVM uses OCI images for the **filesystem only**. If you are coming from Docker,
these differences matter:

| Docker concept | AegisVM behavior |
|----------------|----------------|
| `ENTRYPOINT` | Ignored. PID 1 is always `aegis-harness`. |
| `CMD` | Ignored. The command comes from `aegis run -- CMD` or `aegis instance start -- CMD`. |
| `ENV` | Ignored. Environment is set by secrets + RPC params. |
| `EXPOSE` | Ignored. Ports are declared via `--expose` flag (Docker-style port mapping). |
| `VOLUME` | Ignored. Writable paths are fixed (`/workspace`, `/tmp`, `/run`, `/var`). |

One process per VM. No `docker compose`. No restart supervisor -- if your
process exits, the instance stops. See `docs/AGENT_CONVENTIONS.md` for the full
guest environment contract.

---

## Platform Commands

### aegis up

Start the aegisd daemon.

```
aegis up
```

Locates the `aegisd` binary next to the `aegis` binary, starts it as a
subprocess, and waits up to 2 seconds for the PID file to appear. If the
daemon is already running, prints a message and exits without error.

Kit daemons (e.g., `aegis-gateway`) are managed per-instance by aegisd, not
by `aegis up`. When an instance with a kit that declares `instance_daemons`
is created or enabled, aegisd spawns the daemon automatically. See
[Agent Kit docs](AGENT_KIT.md) for details.

If no base rootfs is installed at `~/.aegis/base-rootfs/`, automatically
downloads the default variant (`python` — Alpine + Python 3.12) before
starting the daemon. See `aegis rootfs` for managing rootfs variants.

**Example:**

```
$ aegis up
aegis v0.4.0
aegisd: started
```

---

### aegis down

Stop the aegisd daemon.

```
aegis down
```

Stops aegisd, which in turn stops all per-instance kit daemons and VMs. Sends
SIGTERM and waits up to 5 seconds for the process to exit. If the daemon is
not running, prints a message and exits without error.

**Example:**

```
$ aegis down
aegisd stopping (pid 48201)
aegisd stopped
```

---

### aegis status

Show daemon status.

```
aegis status
```

Checks whether the daemon is running, then queries `GET /v1/status` to display
the backend name.

**Example:**

```
$ aegis status
aegisd: running
backend: libkrun
```

---

### aegis doctor

Diagnose the local environment.

```
aegis doctor
```

Checks for the presence of required tools and libraries:

- Go
- krunvm (optional -- libkrun CLI, not used by AegisVM directly)
- libkrun shared library
- e2fsprogs / `mkfs.ext4`
- Daemon status and capabilities

---

## aegis run

Ephemeral command: creates an instance, follows logs, waits for exit, deletes
the instance. If `--workspace` is omitted, a temporary workspace is allocated
and deleted after. If `--workspace` is provided, that workspace is preserved.

```
aegis run [--expose [PUBLIC:]GUEST[/PROTO]] [--name NAME] [--image IMAGE] [--kit KIT] [--secret KEY] [--workspace NAME_OR_PATH] -- COMMAND [ARGS...]
```

**Flags:**

| Flag | Description |
|---|---|
| `--image IMAGE` | OCI image reference (e.g., `alpine:3.21`). Without this, the base rootfs is used. |
| `--expose [PUBLIC:]GUEST[/PROTO]` | Port to expose. `8080:80` maps public 8080 to guest 80. `80` assigns a random public port. Optional `/tcp` or `/http` protocol hint. May be specified multiple times. |
| `--name NAME` | Handle alias for the instance. |
| `--kit KIT` | Kit preset name (e.g., `agent`). Supplies defaults for command, image, and capabilities from the kit manifest. Explicit flags override kit defaults. See `aegis kit list`. |
| `--secret KEY` | Secret to inject as env var. Use `--secret '*'` for all secrets. May be specified multiple times. Default: none. |
| `--workspace NAME_OR_PATH` | Named workspace (e.g., `claw`) or host path (e.g., `./myapp`). Named workspaces resolve to `~/.aegis/data/workspaces/<name>`. |

Creates an instance via `POST /v1/instances`, streams logs, and watches for
process exit. On Ctrl+C or process exit, sends `DELETE /v1/instances/{id}` to
clean up.

Secrets are **not injected by default**. You must explicitly name which secrets
an instance receives via `--secret KEY`. This prevents accidental leakage.

**Examples:**

```
# Run a one-shot command
$ aegis run -- echo "hello from aegisvm"
hello from aegisvm

# Run with deterministic port (public 8080 → guest 80)
$ aegis run --expose 8080:80 -- python3 -m http.server 80

# Run with random public port
$ aegis run --expose 80 -- python3 -m http.server 80

# Run with secrets
$ aegis run --secret API_KEY --expose 8080:80 -- python app.py
```

---

## Instance Commands

Manage instances -- the core runtime object in AegisVM.

Run `aegis instance help` to print subcommand usage.

### aegis instance start

Start a new instance, or restart a stopped instance by handle.

```
aegis instance start [--name NAME] [--expose [PUBLIC:]GUEST[/PROTO]] [--image IMAGE] [--kit KIT] [--secret KEY] [--workspace NAME_OR_PATH] -- COMMAND [ARGS...]
aegis instance start --name NAME                          (restart stopped instance)
```

Idempotent on `--name`: if the named instance exists and is STOPPED (whether
enabled or disabled), it is re-enabled and restarted using stored config. If it
is RUNNING or STARTING, returns 409. If not found, creates a new instance.

**Flags:**

| Flag | Description |
|---|---|
| `--name NAME` | Handle alias (used for routing, exec, logs, restart). |
| `--expose [PUBLIC:]GUEST[/PROTO]` | Port to expose. `8080:80` maps public 8080 to guest 80. `80` assigns random. May be specified multiple times. |
| `--image IMAGE` | OCI image reference. |
| `--kit KIT` | Kit preset. Supplies defaults for command, image, and capabilities from the kit manifest. Explicit flags override. |
| `--secret KEY` | Secret to inject as env var. Use `--secret '*'` for all secrets. May be specified multiple times. Default: none. |
| `--workspace NAME_OR_PATH` | Named workspace or host path. Named workspaces resolve to `~/.aegis/data/workspaces/<name>`. |

**Examples:**

```
# Start with a kit (supplies command, image, capabilities from manifest)
$ aegis instance start --kit agent --name my-agent --secret OPENAI_API_KEY
Instance started: inst-173f...
Handle: my-agent

# Kit with command override (debug shell in a kit-configured VM)
$ aegis instance start --kit agent --name debug --secret OPENAI_API_KEY -- sh

# Start without a kit
$ aegis instance start --name web --expose 8080:80 --workspace myapp -- python3 -m http.server 80
Instance started: inst-173f...
Handle: web

# Restart a stopped instance
$ aegis instance disable web
$ aegis instance start --name web
Instance restarted: inst-173f...
```

---

### aegis instance list

List instances, optionally filtered by state.

```
aegis instance list [--stopped | --running]
```

Columns: ID, HANDLE, STATUS (enabled/disabled), STATE, STOPPED AT. Output is
color-coded: enabled/running in green, disabled in red, paused in yellow,
stopped in gray.

**Examples:**

```
$ aegis instance list
ID                             HANDLE          STATUS       STATE      STOPPED AT
inst-1739893456789012345       web             enabled      running    -
inst-1739893456789054321       worker          disabled     stopped    2026-02-19T10:30:00Z

$ aegis instance list --stopped
ID                             HANDLE          STATUS       STATE      STOPPED AT
inst-1739893456789054321       worker          disabled     stopped    2026-02-19T10:30:00Z
```

---

### aegis instance info

Show detailed information about an instance.

```
aegis instance info HANDLE_OR_ID
```

Accepts either a handle alias or instance ID.

**Examples:**

```
$ aegis instance info my-agent
Handle:      my-agent
ID:          inst-1739893456789012345
State:       running
Enabled:     true
Image:       python:3.12-alpine
Kit:         agent
Command:     aegis-agent
Gateway:     running
Connections: 0
Created:     2026-02-19T10:30:00Z
Last Active: 2026-02-19T10:35:00Z

$ aegis instance info web
Handle:      web
ID:          inst-1739893456789054321
State:       running
Enabled:     true
Command:     python3 -m http.server 80
Endpoints:
  http://127.0.0.1:49152 → vm:80
Connections: 0
Created:     2026-02-19T10:30:00Z
Last Active: 2026-02-19T10:35:00Z
```

If a kit has been uninstalled but the instance still references it:

```
Kit:         agent (not installed)
```

---

### aegis instance disable

Disable an instance. Sets Enabled=false, stops the VM, closes all port
listeners, and prevents wake-on-connect. The instance remains in the list with
state STOPPED and logs are preserved. Use `aegis instance start --name` to
re-enable, or `aegis instance delete` to remove entirely.

A disabled instance is completely unreachable — no ingress, no auto-wake, no
exec, no log streaming.

```
aegis instance disable HANDLE_OR_ID
```

---

### aegis instance delete

Delete an instance entirely. Stops the VM (if running), removes from the
instance list, and cleans up logs.

```
aegis instance delete HANDLE_OR_ID
```

---

### Instance Lifecycle States

| Operation | Result | Enabled? | In list? | Logs kept? | VM running? |
|-----------|--------|----------|----------|------------|-------------|
| Process exits naturally | STOPPED | Yes | Yes | Yes | No |
| Idle timeout (StopAfterIdle) | STOPPED | Yes | Yes | Yes | No |
| `aegis instance disable` | STOPPED | **No** | Yes | Yes | No |
| `aegis instance pause` | PAUSED | Yes | Yes | Yes | Suspended |
| `aegis instance delete` | Removed | - | No | No | No |
| `aegis run` + Ctrl+C | Deleted | - | No | No | No |

**Enabled** instances retain their config and auto-wake on incoming connections
(wake-on-connect). **Disabled** instances are unreachable — no ingress, no
auto-wake, no exec, no logs. Use `aegis instance start --name <handle>` to
re-enable a disabled instance. Use `aegis instance prune` to clean up old
stopped instances. `delete` is permanent — the instance and its logs are gone.

---

### aegis instance pause

Pause a running instance (SIGSTOP).

```
aegis instance pause HANDLE_OR_ID
```

---

### aegis instance resume

Resume a paused instance (SIGCONT).

```
aegis instance resume HANDLE_OR_ID
```

---

### aegis instance prune

Remove stopped instances older than a threshold. User-invoked cleanup —
no background GC. Workspaces are never deleted by prune.

```
aegis instance prune --stopped-older-than DURATION
```

Duration supports `h` (hours) and `d` (days). Default: `7d`.

**Example:**

```
$ aegis instance prune --stopped-older-than 7d
Pruned 3 stopped instance(s)
```

---

## Exec Command

Execute a command inside a running instance. No SSH required -- uses the
existing control channel.

### aegis exec

```
aegis exec HANDLE_OR_ID -- COMMAND [ARGS...]
```

**Arguments:**

| Argument | Description |
|---|---|
| `HANDLE_OR_ID` | Instance handle or ID. |

Everything after `--` is the command to execute inside the VM.

Valid in RUNNING, PAUSED (auto-resume), and STARTING (waits for ready).
Returns 409 if the instance is STOPPED or DISABLED.

**Examples:**

```
# Exec by handle
$ aegis exec web -- echo hello
hello

# Exec by instance ID
$ aegis exec inst-1739893456789012345 -- ls /workspace
data/
output/
```

---

## Log Commands

Stream instance logs. Logs are captured from the moment the VM starts
and persisted to `~/.aegis/data/logs/`.

### aegis logs

```
aegis logs HANDLE_OR_ID [--follow]
```

**Arguments:**

| Argument | Description |
|---|---|
| `HANDLE_OR_ID` | Instance handle or ID. |

**Flags:**

| Flag | Description |
|---|---|
| `--follow`, `-f` | Stream live logs (blocks until Ctrl+C). |

Log entries are color-coded by source: `[exec]` in cyan, `[system]` in yellow,
primary process output has no prefix.

**Example:**

```
$ aegis logs web --follow
Serving HTTP on 0.0.0.0 port 80 ...
127.0.0.1 - - [19/Feb/2026 10:35:12] "GET / HTTP/1.1" 200 -
```

---

## Secret Commands

Manage secrets. Secrets are a flat key-value store with AES-256 encryption at
rest. Secrets are injected as env vars only when explicitly requested via
`--secret KEY` or `--secret '*'` (all secrets). Default: none injected. Values are never
displayed by any list command.

Run `aegis secret help` to print subcommand usage.

### aegis secret set

Set a secret (creates or updates).

```
aegis secret set KEY VALUE
```

Sends `PUT /v1/secrets/{key}`.

**Example:**

```
$ aegis secret set API_KEY sk-test123
Secret API_KEY set
```

---

### aegis secret list

List secret names. Values are never shown.

```
aegis secret list
```

**Example:**

```
$ aegis secret list
Secrets:
  API_KEY
  DB_PASSWORD
```

---

### aegis secret delete

Delete a secret.

```
aegis secret delete KEY
```

Sends `DELETE /v1/secrets/{key}`.

**Example:**

```
$ aegis secret delete API_KEY
Secret API_KEY deleted
```

---

## Kit Commands

Manage kits — optional add-on bundles that extend AegisVM with specific
capabilities. Kit manifests live at `~/.aegis/kits/<name>.json`.

Run `aegis kit help` to print subcommand usage.

### aegis kit list

List installed kits with validation status.

```
aegis kit list
```

Shows each kit's name, version, status, and description. Status is `ok` if all
required binaries (daemons + inject) are present, or `broken` with a list of
missing binaries.

**Examples:**

```
$ aegis kit list
NAME         VERSION    STATUS     DESCRIPTION
agent        v0.4.0     ok         Messaging-driven LLM agent with Telegram integration

$ aegis kit list
NAME         VERSION    STATUS     DESCRIPTION
agent        v0.4.0     broken     Messaging-driven LLM agent (missing: aegis-agent)
```

No kits installed:

```
$ aegis kit list
No kits installed.
```

---

## MCP Commands

Manage the MCP (Model Context Protocol) server integration with Claude Code.

### aegis mcp install

Register `aegis-mcp` as an MCP server in Claude Code.

```
aegis mcp install [--project]
```

By default, registers with `user` scope (available across all projects). Pass
`--project` to register with project scope (shared via `.mcp.json`).

**Example:**

```
$ aegis mcp install
aegis MCP server registered in Claude Code
  binary: /opt/homebrew/bin/aegis-mcp
  scope:  user
```

---

### aegis mcp uninstall

Remove the `aegis-mcp` server from Claude Code.

```
aegis mcp uninstall
```

---

## Quick Reference

| Command | Description |
|---|---|
| `aegis up` | Start daemon (kit daemons are per-instance, managed by aegisd) |
| `aegis down` | Stop daemon (stops all kit daemons and VMs) |
| `aegis status` | Show daemon status |
| `aegis doctor` | Diagnose environment |
| `aegis run [...] -- CMD` | Ephemeral: start + follow + delete |
| `aegis instance start` | Start new or re-enable stopped/disabled instance |
| `aegis instance list` | List instances (`--stopped`, `--running`) |
| `aegis instance info` | Show instance details |
| `aegis instance disable` | Disable an instance (stop VM, close listeners, prevent auto-wake) |
| `aegis instance delete` | Delete an instance (removed entirely) |
| `aegis instance pause` | Pause an instance |
| `aegis instance resume` | Resume an instance |
| `aegis instance prune` | Remove stale stopped instances |
| `aegis exec` | Execute command in instance |
| `aegis logs` | Stream instance logs |
| `aegis secret set` | Set a secret |
| `aegis secret list` | List secret names |
| `aegis secret delete` | Delete a secret |
| `aegis kit list` | List installed kits |
| `aegis mcp install` | Register MCP server in Claude Code |
| `aegis mcp uninstall` | Remove MCP server from Claude Code |
