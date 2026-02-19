# Aegis CLI Reference

The `aegis` command is the primary interface to the Aegis agent runtime platform.
All commands communicate with the `aegisd` daemon over a Unix socket at
`~/.aegis/aegisd.sock`. The daemon PID file is stored at `~/.aegis/data/aegisd.pid`.

```
aegis <command> [options]
```

Run `aegis help` to print top-level usage.

---

## Aegis Is Not Docker

Aegis uses OCI images for the **filesystem only**. If you are coming from Docker,
these differences matter:

| Docker concept | Aegis behavior |
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
- krunvm (optional -- libkrun CLI, not used by Aegis directly)
- libkrun shared library
- e2fsprogs / `mkfs.ext4`
- Daemon status and capabilities

---

## aegis run

Sugar command: creates an instance, follows logs, deletes on exit. Equivalent
to `aegis instance start` + `aegis logs --follow` + cleanup on Ctrl+C or
process exit.

```
aegis run [--expose PORT] [--name NAME] [--image IMAGE] [--env K=V] -- COMMAND [ARGS...]
```

**Flags:**

| Flag | Description |
|---|---|
| `--image IMAGE` | OCI image reference (e.g., `alpine:3.21`). Without this, the base rootfs is used. |
| `--expose PORT` | Port to expose from the VM. Docker-style static port mapping. May be specified multiple times. |
| `--name NAME` | Handle alias for the instance. |
| `--env K=V` | Environment variable to inject. May be specified multiple times. |

Creates an instance via `POST /v1/instances`, streams logs, and watches for
process exit. On Ctrl+C or process exit, sends `DELETE /v1/instances/{id}` to
clean up.

**Examples:**

```
# Run a one-shot command
$ aegis run -- echo "hello from aegis"
hello from aegis

# Run with a custom image
$ aegis run --image alpine:3.21 -- echo hello
hello

# Run with exposed ports
$ aegis run --expose 80 -- python -m http.server 80
Serving on http://127.0.0.1:8099

# Named instance with env vars
$ aegis run --name myapp --env API_KEY=sk-123 --expose 80 -- python app.py
```

---

## Instance Commands

Manage instances -- the core runtime object in Aegis.

Run `aegis instance help` to print subcommand usage.

### aegis instance start

Start a new instance.

```
aegis instance start [--name NAME] [--expose PORT] [--image IMAGE] [--env K=V] [--workspace PATH] -- COMMAND [ARGS...]
```

**Flags:**

| Flag | Description |
|---|---|
| `--name NAME` | Handle alias (used for routing, exec, logs). |
| `--expose PORT` | Port to expose. May be specified multiple times. |
| `--image IMAGE` | OCI image reference. |
| `--env K=V` | Environment variable. May be specified multiple times. |
| `--workspace PATH` | Host path for workspace volume. |

**Example:**

```
$ aegis instance start --name myapp --expose 80 -- python3 -m http.server 80
Instance started: inst-173f...
Handle: myapp
Router: http://127.0.0.1:8099
```

---

### aegis instance list

List all instances.

```
aegis instance list
```

Columns: ID, HANDLE, STATE, CONNS.

**Example:**

```
$ aegis instance list
ID                             HANDLE          STATE      CONNS
inst-1739893456789012345       myapp           running    0
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
$ aegis instance info myapp
ID:          inst-1739893456789012345
State:       running
Handle:      myapp
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
$ aegis exec myapp -- echo hello
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
$ aegis logs myapp --follow
Serving HTTP on 0.0.0.0 port 80 ...
127.0.0.1 - - [19/Feb/2026 10:35:12] "GET / HTTP/1.1" 200 -
```

---

## Secret Commands

Manage workspace secrets. Secret values are never displayed by any list command.

Run `aegis secret help` to print subcommand usage.

### aegis secret set

Set a workspace secret.

```
aegis secret set KEY VALUE
```

**Arguments:**

| Argument | Description |
|---|---|
| `KEY` | Secret name. |
| `VALUE` | Secret value. |

Sends `PUT /v1/secrets/{key}`.

**Example:**

```
$ aegis secret set API_KEY sk-test123
Secret API_KEY set
```

---

### aegis secret list

List workspace secret names. Values are never shown.

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

## Quick Reference

| Command | Description |
|---|---|
| `aegis up` | Start the daemon |
| `aegis down` | Stop the daemon |
| `aegis status` | Show daemon status |
| `aegis doctor` | Diagnose environment |
| `aegis run [...] -- CMD` | Run a command (sugar: start + follow + delete) |
| `aegis instance start` | Start a new instance |
| `aegis instance list` | List instances |
| `aegis instance info` | Show instance details |
| `aegis instance stop` | Stop an instance (VM stopped, stays in list) |
| `aegis instance delete` | Delete an instance (removed entirely) |
| `aegis instance pause` | Pause an instance |
| `aegis instance resume` | Resume an instance |
| `aegis exec` | Execute command in instance |
| `aegis logs` | Stream instance logs |
| `aegis secret set` | Set a workspace secret |
| `aegis secret list` | List workspace secrets |
