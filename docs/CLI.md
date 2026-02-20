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
| `EXPOSE` | Ignored. Ports are declared via `--expose` flag (Docker-style static mapping). |
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

If no base rootfs is installed at `~/.aegis/base-rootfs/`, automatically
downloads the default variant (`python` — Alpine + Python 3.12) before
starting the daemon. See `aegis rootfs` for managing rootfs variants.

**Example:**

```
$ aegis up
aegisd started (pid 48201)
```

---

### aegis down

Stop the aegisd daemon.

```
aegis down
```

Reads the PID file, sends SIGTERM, and waits up to 5 seconds for the process
to exit. If the daemon is not running, prints a message and exits without error.

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
aegis run [--expose PORT[:PROTO]] [--name NAME] [--image IMAGE] [--env K=V] [--secret KEY] [--workspace NAME_OR_PATH] -- COMMAND [ARGS...]
```

**Flags:**

| Flag | Description |
|---|---|
| `--image IMAGE` | OCI image reference (e.g., `alpine:3.21`). Without this, the base rootfs is used. |
| `--expose PORT[:PROTO]` | Port to expose, with optional protocol (`http`, `tcp`). Default: `http`. May be specified multiple times. |
| `--name NAME` | Handle alias for the instance. |
| `--env K=V` | Environment variable to inject. May be specified multiple times. |
| `--secret KEY` | Secret to inject (by name). May be specified multiple times. Use `--secret '*'` for all. Default: none. |
| `--workspace NAME_OR_PATH` | Named workspace (e.g., `claw`) or host path (e.g., `./myapp`). Named workspaces resolve to `~/.aegis/data/workspaces/<name>`. |

Creates an instance via `POST /v1/instances`, streams logs, and watches for
process exit. On Ctrl+C or process exit, sends `DELETE /v1/instances/{id}` to
clean up.

Secrets are **not injected by default**. You must explicitly name which secrets
an instance receives via `--secret`. This prevents accidental leakage.

**Examples:**

```
# Run a one-shot command (no secrets)
$ aegis run -- echo "hello from aegis"
hello from aegis

# Run with specific secrets
$ aegis run --secret API_KEY --expose 80 -- python app.py

# Run with all secrets
$ aegis run --secret '*' -- python agent.py

# Run with exposed ports (no secrets needed)
$ aegis run --expose 80 -- python -m http.server 80
Serving on http://127.0.0.1:8099
```

---

## Instance Commands

Manage instances -- the core runtime object in AegisVM.

Run `aegis instance help` to print subcommand usage.

### aegis instance start

Start a new instance, or restart a stopped instance by handle.

```
aegis instance start [--name NAME] [--expose PORT[:PROTO]] [--image IMAGE] [--env K=V] [--secret KEY] [--workspace NAME_OR_PATH] -- COMMAND [ARGS...]
aegis instance start --name NAME                          (restart stopped instance)
```

Idempotent on `--name`: if the named instance exists and is STOPPED, it is
restarted using stored config. If it is RUNNING or STARTING, returns 409. If
not found, creates a new instance.

**Flags:**

| Flag | Description |
|---|---|
| `--name NAME` | Handle alias (used for routing, exec, logs, restart). |
| `--expose PORT[:PROTO]` | Port to expose with optional protocol. Default: `http`. May be specified multiple times. |
| `--image IMAGE` | OCI image reference. |
| `--env K=V` | Environment variable. May be specified multiple times. |
| `--secret KEY` | Secret to inject. May be specified multiple times. `'*'` for all. Default: none. |
| `--workspace NAME_OR_PATH` | Named workspace or host path. Named workspaces resolve to `~/.aegis/data/workspaces/<name>`. |

**Examples:**

```
$ aegis instance start --name web --expose 80 --workspace myapp -- python3 -m http.server 80
Instance started: inst-173f...
Handle: web
Router: http://127.0.0.1:8099

$ aegis instance stop web
$ aegis instance start --name web
Instance restarted: inst-173f...
```

---

### aegis instance list

List instances, optionally filtered by state.

```
aegis instance list [--stopped | --running]
```

Columns: ID, HANDLE, STATE, STOPPED AT.

**Examples:**

```
$ aegis instance list
ID                             HANDLE          STATE      STOPPED AT
inst-1739893456789012345       web             running    -
inst-1739893456789054321       worker          stopped    2026-02-19T10:30:00Z

$ aegis instance list --stopped
ID                             HANDLE          STATE      STOPPED AT
inst-1739893456789054321       worker          stopped    2026-02-19T10:30:00Z
```

---

### aegis instance info

Show detailed information about an instance.

```
aegis instance info HANDLE_OR_ID
```

Accepts either a handle alias or instance ID.

**Example:**

```
$ aegis instance info web
ID:          inst-1739893456789012345
State:       running
Handle:      web
Command:     python3 -m http.server 80
Ports:       80
Endpoints:
  :80 → :49152 (http)
Connections: 0
Created:     2026-02-19T10:30:00Z
Last Active: 2026-02-19T10:35:00Z
```

---

### aegis instance stop

Stop an instance's VM. The instance remains in the list with state STOPPED and
logs are preserved. Use `aegis instance delete` to remove entirely.

```
aegis instance stop HANDLE_OR_ID
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

| Operation | Result | In list? | Logs kept? | VM running? |
|-----------|--------|----------|------------|-------------|
| Process exits naturally | STOPPED | Yes | Yes | No |
| `aegis instance stop` | STOPPED | Yes | Yes | No |
| Idle timeout (StopAfterIdle) | STOPPED | Yes | Yes | No |
| `aegis instance pause` | PAUSED | Yes | Yes | Suspended |
| `aegis instance delete` | Removed | No | No | No |
| `aegis run` + Ctrl+C | Deleted | No | No | No |

STOPPED instances retain their config and can be restarted via
`aegis instance start --name <handle>` or router wake-on-connect. Use
`aegis instance prune` to clean up old stopped instances. `delete` is
permanent — the instance and its logs are gone.

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
Returns 409 if the instance is STOPPED.

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
`--secret KEY` or `--secret '*'`. Default: none injected. Values are never
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
| `aegis up` | Start the daemon |
| `aegis down` | Stop the daemon |
| `aegis status` | Show daemon status |
| `aegis doctor` | Diagnose environment |
| `aegis run [...] -- CMD` | Ephemeral: start + follow + delete |
| `aegis instance start` | Start new or restart stopped instance |
| `aegis instance list` | List instances (`--stopped`, `--running`) |
| `aegis instance info` | Show instance details |
| `aegis instance stop` | Stop an instance (record kept for restart) |
| `aegis instance delete` | Delete an instance (removed entirely) |
| `aegis instance pause` | Pause an instance |
| `aegis instance resume` | Resume an instance |
| `aegis instance prune` | Remove stale stopped instances |
| `aegis exec` | Execute command in instance |
| `aegis logs` | Stream instance logs |
| `aegis secret set` | Set a secret |
| `aegis secret list` | List secret names |
| `aegis secret delete` | Delete a secret |
| `aegis rootfs list` | Show available rootfs variants |
| `aegis rootfs pull` | Download a rootfs variant |
| `aegis mcp install` | Register MCP server in Claude Code |
| `aegis mcp uninstall` | Remove MCP server from Claude Code |
