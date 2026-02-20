
# AEGIS v3  
## Lightweight MicroVM Sandbox Runtime

**Status:** Draft  
**Audience:** Core engineering  
**Scope:** Infrastructure runtime only  
**Non-goal:** Application platform, publishing system, or agent framework  

---

# 1. Philosophy

Aegis is a **local microVM runtime for isolated process execution**.

It is:

- A sandbox for agents
- A microVM lifecycle manager
- A port-aware reverse proxy with scale-to-zero
- A workspace-mounted execution environment
- A secrets injection primitive

It is NOT:

- An application platform
- A publish/version system
- A release registry
- A workflow engine
- An orchestrator

If it can be implemented in userland inside the VM, it does not belong in Aegis core.

---

# 2. Core Architecture

## 2.1 Host Components

Host
 ├─ aegisd (control plane)
 ├─ Router (HTTP/TCP reverse proxy)
 ├─ Registry (SQLite)
 ├─ VMM backend (Firecracker | libkrun)
 └─ CLI (aegis)

## 2.2 Guest Components

Guest VM
 ├─ Harness (PID 1)
 └─ User process (PID 2+)

No guest control plane abstraction.  
Harness is a process supervisor only.

---

# 3. Responsibility Boundary

## 3.1 Aegis Core Owns

- VM lifecycle
- Resource limits
- Workspace mounting
- Secrets storage + injection
- Port exposure + routing
- Scale-to-zero
- Log streaming
- Exec RPC
- Pause/Resume
- Terminate
- VMM abstraction

## 3.2 Aegis Core Does NOT Know

- What an app is
- What a version is
- What publish means
- What a release is
- What an agent does
- Business semantics

Those belong to userland inside the VM.

---

# 4. Instance Model

An **instance** is the only runtime object.

It consists of:

- MicroVM
- Root filesystem
- Mounted workspace
- Declared exposed ports
- Environment variables
- Command

No AppID.  
No ReleaseID.  
No publish state.

---

# 5. Lifecycle Model

STOPPED → STARTING → RUNNING ↔ PAUSED → STOPPED

- STOPPED — VM not running, config retained
- STARTING — VM booting
- RUNNING — VM active
- PAUSED — VM suspended (RAM retained)

Transitions:

| From | To | Trigger |
|------|----|---------|
| STOPPED | STARTING | User starts instance, or router receives ingress |
| STARTING | RUNNING | VM boots, `run` RPC succeeds |
| RUNNING | PAUSED | No connections for idle timeout |
| PAUSED | RUNNING | Ingress arrives, or user resumes |
| PAUSED | STOPPED | Extended idle timeout |
| RUNNING | STOPPED | Process exits, or user stops |

STOPPED is not a dead end. A stopped instance retains its config (command, ports, workspace, env) and can be restarted via `instance start --name` or router wake-on-connect.

Disk is canonical. Memory is ephemeral.

---

# 6. Execution Primitives

## 6.1 Run (Ephemeral)

```bash
aegis run --name web --workspace ./myapp --expose 80 --secret API_KEY -- python -m http.server 80
```

`aegis run` executes a command in an ephemeral instance. It is equivalent to: create instance → start → stream logs → wait → delete instance.

If `--workspace` is omitted, Aegis allocates a temporary workspace which is deleted at the end of the run. If `--workspace` is provided, that workspace is preserved.

## 6.2 Start Instance (Persistent)

```bash
aegis instance start --name web --workspace ./myapp --expose 80 --secret API_KEY -- python -m http.server 80
```

`instance start` is idempotent on `--name`:

- Name does not exist → create + start with provided config.
- Name exists and STOPPED → restart using stored config. Command/flags are ignored (instance already has its config).
- Name exists and RUNNING or STARTING → error (409 conflict).

This makes STOPPED a useful state: stop a workload, come back later, `instance start --name web` brings it back with the same config, workspace, ports, and handle.

## 6.3 Exec

```bash
aegis exec web -- echo hello
```

Non-interactive execution only (PTY later).

---

# 7. CLI Model

Daemon management:

```bash
aegis up                                          Start aegisd
aegis down                                        Stop aegisd
aegis status                                      Daemon status
aegis doctor                                      Diagnose environment
```

These are daemon utilities, not runtime primitives. `up`/`down` manage the aegisd process, not instances.

Runtime:

```bash
aegis run [options] -- <cmd> [args...]             Ephemeral: start + follow + delete

aegis instance start [options] -- <cmd> [args...]  Start new or restart stopped instance
aegis instance list [--stopped|--running]           List instances
aegis instance info <name|id>                      Instance detail
aegis instance stop <name|id>                      Stop VM (keep record)
aegis instance delete <name|id>                    Remove instance + cleanup
aegis instance pause <name|id>                     Pause (SIGSTOP, RAM retained)
aegis instance resume <name|id>                    Resume (SIGCONT)
aegis instance prune --stopped-older-than <dur>    Remove stale stopped instances

aegis exec <name|id> -- <cmd> [args...]            Execute in running instance
aegis logs <name|id> [--follow]                    Stream logs

aegis secret set KEY VALUE                         Set secret
aegis secret list                                  List secret names
aegis secret delete KEY                            Delete secret
```

## 7.1 `aegis run` vs `aegis instance start`

Two entry points, two intents:

- **`run`** — ephemeral. Create instance, stream logs, wait for exit, delete instance. No leftovers. If `--workspace` is omitted, a temporary workspace is allocated and deleted. If `--workspace` is provided, it is preserved (user-owned).
- **`instance start`** — persistent. Instance stays until explicitly stopped/deleted. For long-running workloads, servers, agents. Idempotent on `--name`: if the instance is STOPPED, restarts it from stored config.

Both use the same flags and the same instance primitive. The difference is lifecycle ownership: `run` owns cleanup, `instance start` leaves it to the user.

## 7.2 `stop` vs `delete`

Two separate intents:

- **stop** — turn off the VM, keep the instance record (logs, config, handle reservation). Allows inspection and restart.
- **delete** — forget the instance completely. Remove record, logs, cleanup.

No background GC. Stopped instances accumulate until explicitly deleted or pruned. `instance list` shows `STOPPED` state and `stopped_at` timestamp for visibility. `instance prune --stopped-older-than 7d` is the user-invoked broom — removes stopped instance records and logs older than the threshold. Workspaces are never deleted by prune.

---

# 8. Workspace Model

Mounted at:

/workspace

## Named Workspace

--workspace claw  
~/.aegis/workspaces/claw

## Path Workspace

--workspace ./myagent

Always RW.  
Never copied.  
Never part of VM snapshot.

---

# 9. Secrets Model

Flat encrypted store:

- id  
- name (unique)  
- value  
- created_at  

Injection is explicit:

--secret API_KEY  
--secret '*'

Default: inject nothing.

Secrets:

- Injected as env vars
- Not written to disk
- Re-injected on every boot

---

# 10. Port Exposure Model

Docker-style static exposure:

--expose 80  
--expose 8080:tcp  
--expose 80:http  

Router:

- Wake on connect
- Pause on idle
- Reverse proxy traffic

Guest does not declare ports dynamically.

---

# 11. Control Plane

Only one control plane exists:

**aegisd**

Harness is a guest supervisor only.

---

# 12. VMM Abstraction

Backends:

- Firecracker (Linux)
- libkrun (macOS ARM)

Core interface:

- CreateVM
- StartVM
- PauseVM
- ResumeVM
- StopVM
- HostEndpoints
- Capabilities

Kernel selection is backend-internal. Core does not expose kernel path or kernel args — each backend resolves its own kernel (libkrun uses its built-in kernel; Firecracker will source from build-time defaults or image metadata).

Snapshotting is not part of the core VMM interface. If added later, it will be expressed as an optional extension interface that backends may implement. Capability discovery uses interface assertion, not a capability flag.

---

# 13. Logging

Per-instance:

- In-memory ring buffer
- NDJSON persistent file

Example:

```json
{
  "ts": "...",
  "stream": "stdout|stderr|system",
  "line": "...",
  "instance_id": "...",
  "exec_id": "optional"
}
```

---

# 14. Networking

- Egress allowed by default
- Ingress only via router
- No direct LAN access to VM
- Inter-VM networking disabled (future extension)

---

# 15. Out of Scope

Not part of Aegis v3:

- App lifecycle
- Versioning
- Publishing
- Release overlays
- Routing schemes
- Agent orchestration

Belongs to userland inside the VM or external tooling.

---

# 16. Design Principles

1. Disk is canonical.
2. Memory is ephemeral.
3. Workspace is separate.
4. Secrets never persist.
5. Expose is static.
6. Instance is the only runtime object.
7. Control plane exists only on host.
8. Guest logic is workload.
9. Simplicity over feature density.

---

# 17. Example: OpenClaw in Sandbox

```bash
aegis run --name claw --workspace ./clawbot --expose 3000 --secret OPENAI_API_KEY -- python run_claw.py
```

Aegis does not know what Claw is.
Claw is just a process.

---

# Final Summary

Aegis v3 is a microVM runtime for isolated process execution with workspace mount and port exposure.

It is not a PaaS, release manager, or agent framework.
