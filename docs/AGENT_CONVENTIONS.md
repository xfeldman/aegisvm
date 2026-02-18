# Aegis Agent Conventions

Canonical reference for writing agents that run on the Aegis platform.

If you have built containers before, read the section "Aegis Is Not Docker" first.
Everything else follows from that distinction.

---

## Table of Contents

1. [Aegis Is Not Docker](#aegis-is-not-docker)
2. [Execution Model](#execution-model)
3. [Filesystem Layout](#filesystem-layout)
4. [Environment Variables and Secrets](#environment-variables-and-secrets)
5. [Logging](#logging)
6. [Readiness (Serve Mode)](#readiness-serve-mode)
7. [Lifecycle and Signals](#lifecycle-and-signals)
8. [Networking](#networking)
9. [Resource Defaults](#resource-defaults)
10. [Workspace Conventions](#workspace-conventions)
11. [Complete Examples](#complete-examples)

---

## Aegis Is Not Docker

Aegis uses OCI images for the filesystem only. Every other OCI/Docker concept is
ignored at runtime:

| Docker concept    | Aegis behavior                                      |
|-------------------|-----------------------------------------------------|
| `ENTRYPOINT`      | Ignored. PID 1 is always `aegis-harness`.           |
| `CMD`             | Ignored. Agent command comes from the RPC request.  |
| `EXPOSE`          | Ignored. Ports are declared in the kit manifest.    |
| `ENV`             | Ignored. Environment is set by the harness and RPC. |
| `VOLUME`          | Ignored. Writable paths are fixed (see Filesystem). |
| Layer caching     | Not applicable. No build cache.                     |
| `docker compose`  | Not applicable. One agent per VM.                   |

Your Dockerfile installs dependencies and copies code. Nothing more. The harness
decides what runs, when, and how.

```dockerfile
FROM python:3.12-slim
RUN pip install --no-cache-dir fastapi uvicorn
COPY ./agent /app
# No ENTRYPOINT. No CMD. Aegis ignores them.
```

---

## Execution Model

Every Aegis microVM boots with `aegis-harness` as PID 1. The harness:

1. Mounts the guest filesystem (see below).
2. Connects to the host control plane over TCP (tunneled via vsock/TSI).
3. Waits for JSON-RPC 2.0 commands from the host.

Your agent code runs as a **child process** of the harness, started by one of
two RPC methods:

- **`runTask`** -- runs a command to completion, returns the exit code. Used for
  one-shot jobs (data processing, code execution, batch tasks).
- **`startServer`** -- starts a long-lived process, polls a readiness port, and
  sends a `serverReady` notification when the port accepts TCP connections.
  Used for HTTP servers, API agents, and anything that listens on a port.

There is no process supervisor, no automatic restart, and no init system. If
your process exits, the instance transitions to `STOPPED`. If you need retry
logic, build it into your agent.

---

## Filesystem Layout

The harness sets up the following mount structure before running any agent code:

| Path         | Type     | Writable | Persistent | Notes                                         |
|--------------|----------|----------|------------|-----------------------------------------------|
| `/`          | virtiofs | No       | No         | Root filesystem from OCI image. Remounted read-only by the harness. |
| `/workspace` | virtiofs | Yes      | Yes        | Host-backed persistent storage. Only present when the app has a workspace configured (`AEGIS_WORKSPACE=1`). Survives VM termination. |
| `/tmp`       | tmpfs    | Yes      | No         | Ephemeral scratch space. Lost on VM shutdown.  |
| `/run`       | tmpfs    | Yes      | No         | Ephemeral. Convention: runtime sockets, PID files. |
| `/var`       | tmpfs    | Yes      | No         | Ephemeral. Convention: logs, transient state.  |
| `/proc`      | procfs   | --       | --         | Standard Linux procfs.                         |

Key points:

- The root filesystem is **read-only**. You cannot write to `/usr`, `/etc`,
  `/home`, or any path outside the writable mounts listed above. Install
  everything you need at image build time.
- `/workspace` is the only persistent writable location. If your agent produces
  artifacts, checkpoints, or state that must survive restarts, write them here.
- If no workspace is configured for the instance, `/workspace` does not exist.
  Your agent should check before writing.

---

## Environment Variables and Secrets

Secrets are injected as environment variables at process start via `execve`.
They are **never written to disk**. There is no secrets file, no mounted volume,
no sidecar. Read them from the environment like any other variable.

The harness starts with a minimal base environment inherited from the kernel
command line:

| Variable          | Value                | Description                          |
|-------------------|----------------------|--------------------------------------|
| `PATH`            | Standard Linux PATH  | Search path for executables.         |
| `HOME`            | `/root`              | Home directory.                      |
| `TERM`            | `linux`              | Terminal type.                       |
| `AEGIS_HOST_ADDR` | `host:port`          | Internal. Used by the harness only.  |

When the host sends a `runTask` or `startServer` RPC, it can include an `env`
field -- a map of key-value pairs. The harness merges these on top of the base
environment before calling `execve`. This is how secrets reach your agent.

Your agent code reads secrets the same way it reads any environment variable:

**Python:**
```python
import os

api_key = os.environ["OPENAI_API_KEY"]
db_url = os.environ.get("DATABASE_URL", "sqlite:///default.db")
```

**Node.js:**
```javascript
const apiKey = process.env.OPENAI_API_KEY;
const dbUrl = process.env.DATABASE_URL || "sqlite:///default.db";
```

Do not log secrets. Do not write them to `/workspace` or `/tmp`. They exist only
in your process's memory for the duration of execution.

---

## Logging

The harness captures your process's stdout and stderr line by line using a
`bufio.Scanner`. Each line is sent to the host as a JSON-RPC notification:

```json
{"jsonrpc":"2.0","method":"log","params":{"stream":"stdout","line":"..."}}
```

One printed line equals one log entry. Multi-line output (tracebacks, pretty-printed
JSON) is split into separate notifications, one per `\n`-delimited line.

**Plain text works.** There is no enforced format. But if you want structured,
machine-parseable logs, the recommended convention is one JSON object per line
on stdout:

**Python:**
```python
import json, sys, time

def log(level, msg, **extra):
    entry = {"level": level, "msg": msg, "ts": time.time(), **extra}
    print(json.dumps(entry), flush=True)

log("info", "agent started", version="1.0.0")
log("error", "connection failed", host="db.example.com", retries=3)
```

**Node.js:**
```javascript
function log(level, msg, extra = {}) {
  const entry = { level, msg, ts: Date.now() / 1000, ...extra };
  console.log(JSON.stringify(entry));
}

log("info", "agent started", { version: "1.0.0" });
log("error", "connection failed", { host: "db.example.com", retries: 3 });
```

Guidelines:

- Always flush stdout. In Python, use `flush=True` on `print()` or set
  `PYTHONUNBUFFERED=1`. In Node.js, `console.log` flushes automatically.
- Write diagnostic/error output to stderr. The harness captures both streams
  and tags them separately (`"stream":"stdout"` vs `"stream":"stderr"`).
- Do not write binary data to stdout or stderr. The scanner reads
  newline-delimited text.

---

## Readiness (Serve Mode)

When the host sends a `startServer` RPC, it includes a `readiness_port` field.
The harness polls this port via TCP connect attempts:

- **Interval:** 200ms between attempts.
- **Timeout:** 30 seconds total.
- **Success condition:** The port accepts a TCP connection.

When your server binds the port and accepts connections, the harness sends a
`serverReady` notification to the host, which then begins routing traffic to
your instance.

If the port does not accept connections within 30 seconds, the harness sends a
`serverFailed` notification and the instance is marked as unhealthy.

Your agent must:

1. Bind the declared readiness port.
2. Accept TCP connections on that port within 30 seconds of process start.

This is a TCP check, not an HTTP health check. The harness does not send an HTTP
request -- it only needs the `connect()` syscall to succeed. If your framework
binds the port before the application is fully initialized, that is fine from
the harness's perspective. If you need application-level readiness gating, bind
the port only after initialization is complete.

**Python (FastAPI/Uvicorn):**
```python
# Uvicorn binds the port when it is ready to accept connections.
# No special readiness logic needed.
import uvicorn
from fastapi import FastAPI

app = FastAPI()

@app.get("/health")
def health():
    return {"status": "ok"}

if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8000)
```

**Node.js (Express):**
```javascript
const express = require("express");
const app = express();
const PORT = 8000;

app.get("/health", (req, res) => res.json({ status: "ok" }));

// The listen callback fires after the port is bound.
app.listen(PORT, () => {
  console.log(JSON.stringify({ level: "info", msg: `listening on ${PORT}` }));
});
```

---

## Lifecycle and Signals

The harness handles `SIGTERM` and `SIGINT` for its own graceful shutdown. What
your agent receives depends on the execution mode:

**Task mode (`runTask`):** Your process runs to completion. The harness waits
for it to exit and returns the exit code. If the VM shuts down while your task
is running, the context is cancelled and the process is terminated.

**Serve mode (`startServer`):** Your server process runs indefinitely. On
shutdown, the harness currently sends `SIGKILL` to server processes (immediate
termination, no grace period). Future versions will send `SIGTERM` with a
5-second grace period before `SIGKILL`.

Regardless of the current behavior, **write your agent to handle `SIGTERM`**.
When the graceful shutdown path is enabled, agents that already handle `SIGTERM`
will benefit immediately. Agents that ignore it will be forcefully killed after
the grace period.

**Python:**
```python
import signal, sys

def shutdown(signum, frame):
    print(json.dumps({"level": "info", "msg": "shutting down"}), flush=True)
    # Close DB connections, flush buffers, etc.
    sys.exit(0)

signal.signal(signal.SIGTERM, shutdown)
```

**Node.js:**
```javascript
process.on("SIGTERM", () => {
  console.log(JSON.stringify({ level: "info", msg: "shutting down" }));
  // Close DB connections, flush buffers, etc.
  server.close(() => process.exit(0));
});
```

There is no automatic restart. If your process crashes, the instance stops. If
you need crash recovery, implement it inside your agent (e.g., a top-level
try/except with retry logic).

---

## Networking

**Egress (outbound):** Allowed by default. Your agent can reach the internet,
resolve DNS, and call external APIs. On macOS with libkrun, egress uses TSI
(Transparent Socket Impersonation) -- your code uses standard sockets and the
VMM tunnels traffic transparently. No special configuration needed.

**Ingress (inbound):** Traffic reaches your agent only through the Aegis router
on ports declared in your kit manifest. There is no direct host-to-VM access.
You cannot `curl localhost:8000` from the host to reach a VM -- all inbound
traffic flows through the router.

**No host access:** The VM cannot reach host services directly. `127.0.0.1`
inside the VM refers to the VM's own loopback, not the host. (The one exception
is `AEGIS_HOST_ADDR`, used internally by the harness for the control channel.)

---

## Resource Defaults

| Resource | Default | Configurable |
|----------|---------|--------------|
| Memory   | 512 MB  | Yes, per-kit or per-run (max 4 GB) |
| CPU      | 1 vCPU  | Yes, per-kit or per-run (max 4 vCPU) |

These defaults apply when the kit manifest does not specify resource overrides.
For most agents, the defaults are sufficient. If your agent loads large models,
processes large datasets, or runs memory-intensive computations, declare higher
limits in the kit manifest.

---

## Workspace Conventions

When a workspace is configured, `/workspace` is a persistent, writable virtiofs
mount backed by the host filesystem. Use the following directory structure:

| Path                  | Purpose                                              |
|-----------------------|------------------------------------------------------|
| `/workspace/data/`    | Persistent application data (databases, state files). |
| `/workspace/output/`  | Artifacts and exports (reports, generated files).     |
| `/workspace/.cache/`  | Caches (pip, npm, model weights). Safe to delete.     |

These paths are conventions, not enforced by the harness. You can write anywhere
under `/workspace`. But following this structure makes it easier to reason about
what is essential state vs. what is a reproducible cache.

**Python:**
```python
import os, json

WORKSPACE = "/workspace"
DATA_DIR = os.path.join(WORKSPACE, "data")
OUTPUT_DIR = os.path.join(WORKSPACE, "output")

def ensure_workspace():
    """Create workspace subdirectories if workspace is available."""
    if not os.path.isdir(WORKSPACE):
        return False
    os.makedirs(DATA_DIR, exist_ok=True)
    os.makedirs(OUTPUT_DIR, exist_ok=True)
    return True

def save_state(state):
    path = os.path.join(DATA_DIR, "state.json")
    with open(path, "w") as f:
        json.dump(state, f)

def load_state():
    path = os.path.join(DATA_DIR, "state.json")
    if not os.path.exists(path):
        return None
    with open(path) as f:
        return json.load(f)
```

**Node.js:**
```javascript
const fs = require("fs");
const path = require("path");

const WORKSPACE = "/workspace";
const DATA_DIR = path.join(WORKSPACE, "data");
const OUTPUT_DIR = path.join(WORKSPACE, "output");

function ensureWorkspace() {
  if (!fs.existsSync(WORKSPACE)) return false;
  fs.mkdirSync(DATA_DIR, { recursive: true });
  fs.mkdirSync(OUTPUT_DIR, { recursive: true });
  return true;
}

function saveState(state) {
  fs.writeFileSync(
    path.join(DATA_DIR, "state.json"),
    JSON.stringify(state, null, 2)
  );
}

function loadState() {
  const p = path.join(DATA_DIR, "state.json");
  if (!fs.existsSync(p)) return null;
  return JSON.parse(fs.readFileSync(p, "utf-8"));
}
```

If no workspace is configured, `/workspace` does not exist. Your agent should
degrade gracefully -- use `/tmp` for ephemeral scratch work, and skip
persistence features.

---

## Complete Examples

### Task Agent (Python)

A one-shot agent that fetches data, processes it, and writes output to the
workspace.

```python
#!/usr/bin/env python3
"""Example Aegis task agent."""

import json, os, sys, time

def log(level, msg, **kw):
    print(json.dumps({"level": level, "msg": msg, "ts": time.time(), **kw}), flush=True)

def main():
    log("info", "task starting")

    # Read secrets from environment
    api_key = os.environ.get("API_KEY")
    if not api_key:
        log("error", "API_KEY not set")
        sys.exit(1)

    # Do work (placeholder)
    result = {"processed": 42, "status": "complete"}

    # Write output to workspace if available
    output_dir = "/workspace/output"
    if os.path.isdir("/workspace"):
        os.makedirs(output_dir, exist_ok=True)
        out_path = os.path.join(output_dir, "result.json")
        with open(out_path, "w") as f:
            json.dump(result, f)
        log("info", "result written", path=out_path)
    else:
        # No workspace -- print to stdout
        log("info", "no workspace, printing result", result=result)

    log("info", "task complete")

if __name__ == "__main__":
    main()
```

### Server Agent (Node.js)

A long-lived HTTP agent with structured logging, graceful shutdown, and
workspace persistence.

```javascript
#!/usr/bin/env node
"use strict";

const express = require("express");
const fs = require("fs");
const path = require("path");

const PORT = 8000;
const WORKSPACE = "/workspace";
const DATA_DIR = path.join(WORKSPACE, "data");

function log(level, msg, extra = {}) {
  const entry = { level, msg, ts: Date.now() / 1000, ...extra };
  console.log(JSON.stringify(entry));
}

// Read secrets from environment
const apiKey = process.env.API_KEY;
if (!apiKey) {
  log("error", "API_KEY not set");
  process.exit(1);
}

// Set up workspace if available
const hasWorkspace = fs.existsSync(WORKSPACE);
if (hasWorkspace) {
  fs.mkdirSync(DATA_DIR, { recursive: true });
  log("info", "workspace available", { path: WORKSPACE });
}

const app = express();
app.use(express.json());

app.get("/health", (req, res) => {
  res.json({ status: "ok" });
});

app.post("/process", (req, res) => {
  log("info", "processing request");
  const result = { echo: req.body, processed_at: new Date().toISOString() };

  // Persist to workspace if available
  if (hasWorkspace) {
    const file = path.join(DATA_DIR, `${Date.now()}.json`);
    fs.writeFileSync(file, JSON.stringify(result));
  }

  res.json(result);
});

const server = app.listen(PORT, () => {
  log("info", "server listening", { port: PORT });
});

// Graceful shutdown on SIGTERM
process.on("SIGTERM", () => {
  log("info", "received SIGTERM, shutting down");
  server.close(() => {
    log("info", "server closed");
    process.exit(0);
  });

  // Force exit after 4 seconds if graceful close hangs
  setTimeout(() => {
    log("warn", "forcing exit after timeout");
    process.exit(1);
  }, 4000);
});
```

---

## Checklist

Before deploying an agent to Aegis, verify:

- [ ] Dockerfile installs all dependencies. No reliance on ENTRYPOINT, CMD, ENV, or VOLUME.
- [ ] Agent reads secrets from environment variables, not files.
- [ ] Agent writes persistent data only to `/workspace` (and checks it exists first).
- [ ] Agent does not attempt to write to `/usr`, `/etc`, or other root filesystem paths.
- [ ] stdout output is newline-delimited. Ideally structured JSON.
- [ ] stderr is used for errors and diagnostics.
- [ ] Server agents bind the readiness port within 30 seconds.
- [ ] Agent handles SIGTERM for graceful shutdown.
- [ ] No secrets are logged or written to disk.
