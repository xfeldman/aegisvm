
# Aegis

**Lightweight MicroVM Sandbox Runtime for Agents**

Aegis is a local-first microVM runtime for running isolated processes and agent workloads with:

- VM-level isolation
- Static port exposure (Docker-style)
- Scale-to-zero via pause/restore
- Persistent workspace mount
- Explicit secret injection
- Firecracker (Linux) and libkrun (macOS ARM) backends

Aegis is NOT a PaaS, not a publish system, and not an agent framework.
It is a clean sandbox substrate.

---

# Architecture

```
                        ┌────────────────────────────┐
                        │           Host              │
                        │                            │
                        │  ┌───────────────┐         │
CLI (aegis) ───────────▶│  │   aegisd     │         │
                        │  │ (control plane)│         │
                        │  └───────┬───────┘         │
                        │          │                 │
                        │   ┌──────▼──────┐          │
                        │   │   Router    │          │
                        │   │ (HTTP/TCP)  │          │
                        │   └──────┬──────┘          │
                        │          │                 │
                        │   ┌──────▼────────────────┐│
                        │   │   VMM Backend         ││
                        │   │ (Firecracker/libkrun) ││
                        │   └──────┬────────────────┘│
                        └──────────│──────────────────┘
                                   │
                          ┌────────▼────────┐
                          │      Guest VM    │
                          │                  │
                          │  Harness (PID 1) │
                          │        │         │
                          │  User Process    │
                          │   (Agent/App)    │
                          └──────────────────┘
```

---

# Core Model

The only runtime object in Aegis is an **Instance**.

An instance consists of:

- MicroVM
- Root filesystem
- Mounted workspace (/workspace)
- Static exposed ports
- Environment variables
- Command

No AppID. No ReleaseID. No publish lifecycle.

---

# Example

Run a web server in an isolated VM:

```
aegis instance start   --name web   --workspace ./app   --expose 80   --secret API_KEY   -- python -m http.server 80
```

---

# Design Principles

1. Disk is canonical.
2. Memory is ephemeral.
3. Secrets never persist.
4. Workspace is separate from rootfs.
5. Expose is static.
6. Instance is the only runtime object.
7. Control plane lives on the host only.
8. Kits are optional and external to core.

---

See `AEGIS_v3_PLATFORM_SPEC.md` for full specification.
