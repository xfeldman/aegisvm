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
| `CMD` | Ignored. The command comes from `aegis run -- CMD` or `aegis app create -- CMD`. |
| `ENV` | Ignored. Environment is set by secrets + RPC params. |
| `EXPOSE` | Ignored. Ports are declared via `--expose` flag or kit manifest. |
| `VOLUME` | Ignored. Writable paths are fixed (`/workspace`, `/tmp`, `/run`, `/var`). |

One server process per VM. No `docker compose`. No restart supervisor -- if your
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
- libkrun shared library (`/opt/homebrew/lib/libkrun.dylib`, `/usr/local/lib/libkrun.dylib`, or `/usr/lib/libkrun.so`)
- e2fsprogs / `mkfs.ext4`
- Daemon status

When the daemon is running, also queries `/v1/status` and displays:

- Backend name
- Capabilities (Pause/Resume, Memory Snapshots, Boot from disk layers)
- Installed kit count

**Example:**

```
$ aegis doctor
Aegis Doctor
============

Go:       installed
krunvm:   found (optional, not used by Aegis)
libkrun:  found at /opt/homebrew/lib/libkrun.dylib
e2fsprogs: found

aegisd:   running

Backend:     libkrun
Capabilities:
  Pause/Resume:          no
  Memory Snapshots:      no
  Boot from disk layers: yes
Installed kits: 2
```

---

## Task / Serve Commands

### aegis run

Run a command inside a microVM. Behavior depends on whether `--expose` is provided.

```
aegis run [--expose PORT] [--image IMAGE] -- COMMAND [ARGS...]
```

**Flags:**

| Flag | Description |
|---|---|
| `--image IMAGE` | OCI image reference to pull and use as the rootfs (e.g., `alpine:3.21`, `python:3.12-alpine-alpine`). Without this flag, the base rootfs is used. |
| `--expose PORT` | Port to expose from the VM. Switches the command into serve mode. May be specified multiple times. |

**Task mode** (no `--expose`):

Creates a task via `POST /v1/tasks`, streams stdout/stderr via
`GET /v1/tasks/{id}/logs?follow=true`, and exits with the task's exit code.

**Serve mode** (with `--expose`):

Creates a long-running instance via `POST /v1/instances`, prints the router URL
(`http://127.0.0.1:8099`), and blocks until Ctrl+C. On interrupt, sends
`DELETE /v1/instances/{id}` to clean up.

**Examples:**

```
# Run a one-shot command (task mode)
$ aegis run -- echo "hello from aegis"
hello from aegis

# Run with a custom image
$ aegis run --image alpine:3.21 -- echo hello
hello

# Start a long-running server (serve mode)
$ aegis run --expose 80 -- python -m http.server 80
Serving on http://127.0.0.1:8099
Instance: inst-a1b2c3
Press Ctrl+C to stop

# Expose multiple ports
$ aegis run --expose 80 --expose 443 -- nginx
```

---

## App Commands

Manage apps -- packaged, publishable workloads with release history.

App references accept either the app name (e.g., `myapp`) or the app ID
(e.g., `app-173...`).

Run `aegis app help` to print app subcommand usage.

### aegis app create

Create a new app definition.

```
aegis app create --name NAME --image IMAGE [--expose PORT] -- COMMAND [ARGS...]
```

**Flags:**

| Flag | Required | Description |
|---|---|---|
| `--name NAME` | Yes | App name. |
| `--image IMAGE` | Yes | OCI image reference. |
| `--expose PORT` | No | Port to expose. May be specified multiple times. |

Everything after `--` is the command to run inside the VM.

**Example:**

```
$ aegis app create --name myapp --image python:3.12-alpine --expose 80 -- python -m http.server 80
App created: myapp (id=app-173f...)
```

---

### aegis app publish

Publish a new release for an app. Pulls the OCI image, creates a rootfs
overlay, and injects the harness.

```
aegis app publish APP_NAME [--label LABEL]
```

**Arguments:**

| Argument | Description |
|---|---|
| `APP_NAME` | App name or ID. |

**Flags:**

| Flag | Description |
|---|---|
| `--label LABEL` | Optional human-readable label for the release (e.g., `v1`, `staging`). |

**Example:**

```
$ aegis app publish myapp --label v1
Published release rel-8a4b... (label=v1)
```

---

### aegis app serve

Start serving an app from its latest published release.

```
aegis app serve APP_NAME
```

**Arguments:**

| Argument | Description |
|---|---|
| `APP_NAME` | App name or ID. |

Creates an instance, prints the router URL, and blocks until Ctrl+C. On
interrupt, cleans up the instance via `DELETE /v1/instances/{id}`.

**Example:**

```
$ aegis app serve myapp
Serving myapp on http://127.0.0.1:8099
Instance: inst-d4e5f6
Press Ctrl+C to stop
```

---

### aegis app list

List all apps in table format.

```
aegis app list
```

Columns: NAME, IMAGE, ID.

**Example:**

```
$ aegis app list
NAME                 IMAGE                          ID
myapp                python:3.12-alpine                    app-173f...
frontend             node:20-alpine                 app-28a1...
```

---

### aegis app info

Show detailed information about an app and its releases.

```
aegis app info APP_NAME
```

**Arguments:**

| Argument | Description |
|---|---|
| `APP_NAME` | App name or ID. |

Displays: Name, ID, Image, Command, Ports, and a list of releases.

**Example:**

```
$ aegis app info myapp
Name:    myapp
ID:      app-173f...
Image:   python:3.12-alpine
Command: python -m http.server 80
Ports:   80

Releases (2):
  rel-8a4b... (v1)
  rel-c3d2...
```

---

### aegis app delete

Delete an app, all its releases, and stop any running instances.

```
aegis app delete APP_NAME
```

**Arguments:**

| Argument | Description |
|---|---|
| `APP_NAME` | App name or ID. |

**Example:**

```
$ aegis app delete myapp
App "myapp" deleted
```

---

## Secret Commands

Manage secrets scoped to an app or shared across the entire workspace. Secret
values are never displayed by any list command.

Run `aegis secret help` to print secret subcommand usage.

### aegis secret set

Set an app-scoped secret.

```
aegis secret set APP_NAME KEY VALUE
```

**Arguments:**

| Argument | Description |
|---|---|
| `APP_NAME` | App name or ID. |
| `KEY` | Secret name. |
| `VALUE` | Secret value. |

Sends `PUT /v1/apps/{name}/secrets/{key}`.

**Example:**

```
$ aegis secret set myapp API_KEY sk-test123
Secret API_KEY set for myapp
```

---

### aegis secret list

List secret names for an app. Values are never shown.

```
aegis secret list APP_NAME
```

**Arguments:**

| Argument | Description |
|---|---|
| `APP_NAME` | App name or ID. |

**Example:**

```
$ aegis secret list myapp
Secrets for myapp:
  API_KEY
  DB_PASSWORD
```

---

### aegis secret delete

Delete an app-scoped secret.

```
aegis secret delete APP_NAME KEY
```

**Arguments:**

| Argument | Description |
|---|---|
| `APP_NAME` | App name or ID. |
| `KEY` | Secret name to delete. |

**Example:**

```
$ aegis secret delete myapp API_KEY
Secret API_KEY deleted from myapp
```

---

### aegis secret set-workspace

Set a workspace-wide secret shared across all apps.

```
aegis secret set-workspace KEY VALUE
```

**Arguments:**

| Argument | Description |
|---|---|
| `KEY` | Secret name. |
| `VALUE` | Secret value. |

Sends `PUT /v1/secrets/{key}`.

**Example:**

```
$ aegis secret set-workspace GLOBAL_TOKEN abc123
Workspace secret GLOBAL_TOKEN set
```

---

### aegis secret list-workspace

List workspace-wide secret names. Values are never shown.

```
aegis secret list-workspace
```

**Example:**

```
$ aegis secret list-workspace
Workspace secrets:
  GLOBAL_TOKEN
```

---

## Kit Commands

Manage kits -- pre-packaged agent runtime configurations installed from YAML
manifests.

Run `aegis kit help` to print kit subcommand usage.

### aegis kit install

Install a kit from a YAML manifest file.

```
aegis kit install MANIFEST.yaml
```

**Arguments:**

| Argument | Description |
|---|---|
| `MANIFEST.yaml` | Path to a YAML file containing the kit manifest. Must include top-level `name`, `version`, and `image` fields. An optional `description` field is also supported. |

The CLI parses the YAML and sends a JSON payload to `POST /v1/kits`.

**Example manifest (`famiglia.yaml`):**

```yaml
name: famiglia
version: "1.0.0"
description: Famiglia agent kit
image: ghcr.io/aegis/famiglia:1.0.0
```

**Example:**

```
$ aegis kit install famiglia.yaml
Kit famiglia v1.0.0 installed
```

---

### aegis kit list

List installed kits in table format.

```
aegis kit list
```

Columns: NAME, VERSION, IMAGE.

**Example:**

```
$ aegis kit list
NAME                 VERSION         IMAGE
famiglia             1.0.0           ghcr.io/aegis/famiglia:1.0.0
openclaw             0.9.2           ghcr.io/aegis/openclaw:0.9.2
```

---

### aegis kit info

Show detailed information about an installed kit.

```
aegis kit info KIT_NAME
```

**Arguments:**

| Argument | Description |
|---|---|
| `KIT_NAME` | Name of the kit. |

Displays: Name, Version, Description (if set), Image, and Installed date.

**Example:**

```
$ aegis kit info famiglia
Name:        famiglia
Version:     1.0.0
Description: Famiglia agent kit
Image:       ghcr.io/aegis/famiglia:1.0.0
Installed:   2026-02-18T10:30:00Z
```

---

### aegis kit uninstall

Remove an installed kit.

```
aegis kit uninstall KIT_NAME
```

**Arguments:**

| Argument | Description |
|---|---|
| `KIT_NAME` | Name of the kit to remove. |

**Example:**

```
$ aegis kit uninstall famiglia
Kit "famiglia" uninstalled
```

---

## Instance Commands

Inspect and interact with running instances.

### aegis instance list

List all active instances.

```
aegis instance list
```

Columns: ID, STATE, APP, CONNS.

**Example:**

```
$ aegis instance list
ID                             STATE      APP                  CONNS
inst-1739893456789012345       running    app-173f...          0
```

---

### aegis instance info

Show detailed information about an instance.

```
aegis instance info INSTANCE_ID
```

**Arguments:**

| Argument | Description |
|---|---|
| `INSTANCE_ID` | Instance ID. |

Displays: ID, State, App ID, Release ID, Command, Ports, Connections, Created
time, Last Active time.

**Example:**

```
$ aegis instance info inst-1739893456789012345
ID:          inst-1739893456789012345
State:       running
App:         app-173f...
Release:     rel-8a4b...
Command:     python -m http.server 80
Ports:       80
Connections: 0
Created:     2026-02-19T10:30:00Z
Last Active: 2026-02-19T10:35:00Z
```

---

## Exec Command

Execute a command inside a running instance. No SSH required -- uses the
existing ControlChannel.

### aegis exec

```
aegis exec TARGET -- COMMAND [ARGS...]
```

**Arguments:**

| Argument | Description |
|---|---|
| `TARGET` | Instance ID or app name. If an app name is given, resolves to the running instance for that app. |

Everything after `--` is the command to execute inside the VM.

Valid in RUNNING, PAUSED (auto-resume), and STARTING (waits for ready).
Returns 409 if the instance is STOPPED. If the instance is stopped while an
exec is in flight, the command exits with code `-1`.

**Examples:**

```
# Exec by app name
$ aegis exec myapp -- echo hello
hello

# Exec by instance ID
$ aegis exec inst-1739893456789012345 -- ls /workspace
data/
output/

# Run a diagnostic command
$ aegis exec myapp -- cat /etc/os-release
```

---

## Log Commands

Stream instance logs. Logs are captured from the moment the VM channel opens
(no boot-log gap) and persisted to `~/.aegis/data/logs/`.

### aegis logs

Short alias for `aegis app logs`.

```
aegis logs APP_NAME [--follow]
```

**Arguments:**

| Argument | Description |
|---|---|
| `APP_NAME` | App name or instance ID. |

**Flags:**

| Flag | Description |
|---|---|
| `--follow`, `-f` | Stream live logs (blocks until Ctrl+C). |

**Example:**

```
$ aegis logs myapp --follow
Serving HTTP on 0.0.0.0 port 80 ...
127.0.0.1 - - [19/Feb/2026 10:35:12] "GET / HTTP/1.1" 200 -
```

---

### aegis app logs

Stream logs for an app's running instance.

```
aegis app logs APP_NAME [--follow]
```

Identical to `aegis logs` -- resolves the app name to its running instance and
streams the instance logs.

---

## Quick Reference

| Command | Description |
|---|---|
| `aegis up` | Start the daemon |
| `aegis down` | Stop the daemon |
| `aegis status` | Show daemon status |
| `aegis doctor` | Diagnose environment |
| `aegis run [...] -- CMD` | Run a command in a microVM |
| `aegis app create` | Create an app |
| `aegis app publish` | Publish a release |
| `aegis app serve` | Serve an app |
| `aegis app list` | List apps |
| `aegis app info` | Show app details |
| `aegis app logs` | Stream app logs |
| `aegis app delete` | Delete an app |
| `aegis instance list` | List instances |
| `aegis instance info` | Show instance details |
| `aegis exec` | Execute command in instance |
| `aegis logs` | Stream instance logs |
| `aegis secret set` | Set an app secret |
| `aegis secret list` | List app secrets |
| `aegis secret delete` | Delete an app secret |
| `aegis secret set-workspace` | Set a workspace secret |
| `aegis secret list-workspace` | List workspace secrets |
| `aegis kit install` | Install a kit |
| `aegis kit list` | List kits |
| `aegis kit info` | Show kit details |
| `aegis kit uninstall` | Remove a kit |
