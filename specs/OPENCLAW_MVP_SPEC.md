
# OpenClaw on Aegis — MVP Design Spec

## Status: Draft (Pre-Firecracker Phase)

This document defines how OpenClaw runs on top of Aegis v3 without modifying Aegis core.

---

# 1. Scope

Goal: Run OpenClaw inside an Aegis VM with:

- Workspace-mounted configuration
- Static exposed console port
- Secret injection
- No app lifecycle logic inside Aegis
- No publishing/versioning in core

OpenClaw remains a workload.

---

# 2. Architectural Model

OpenClaw runs as the primary process inside an Aegis instance.

Aegis provides:

- VM isolation
- Workspace mount
- Static port exposure
- Secrets injection
- Scale-to-zero

OpenClaw provides:

- Agent logic
- Web console
- Agent orchestration
- Canvas generation
- App logic

---

# 3. Instance Layout

Host:

~/.aegis/workspaces/claw/

Mounted inside guest as:

/workspace

OpenClaw config directory:

/workspace/.clawbot

No special Aegis integration required.

---

# 4. Startup Example

```
aegis instance start   --name claw   --workspace ./clawbot   --expose 3000   --secret OPENAI_API_KEY   -- python run_claw.py
```

Port 3000 is exposed statically at instance creation.

---

# 5. Configuration Model

Configuration resides entirely inside /workspace.

- No dynamic port declaration
- No runtime reconfiguration of Aegis
- No kit registration at host layer

OpenClaw controls its own behavior via its internal config files.

---

# 6. Canvas Generation Flow

User interacts with OpenClaw console.
OpenClaw generates application code inside /workspace.
Application runs inside the same VM.
If the generated app binds to an exposed port (e.g., 3000), router forwards traffic.

Aegis does not know this is an “app”.
It is simply a process inside the VM.

---

# 7. Limitations (MVP)

- No multi-instance orchestration
- No shared workspace across VMs
- No app version tracking in Aegis
- No dynamic port exposure
- No nested Aegis instance creation

---

# 8. Future Extensions

Possible future additions (not MVP):

- Aegis SDK inside guest
- Kit metadata reporting
- Multi-VM orchestration
- Shared workspace volumes
- Snapshot optimization

---

# 9. Design Principle

OpenClaw is a workload.

Aegis is the isolation substrate.

They are cleanly separated.

Aegis does not embed OpenClaw semantics.
OpenClaw does not assume Aegis platform logic.
