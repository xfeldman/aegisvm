# Aegis Kit System

## Overview

A kit is a reproducible execution environment and integration layer that runs on top of Aegis. Kits are integration accelerators -- pre-packaged agent runtime configurations that bundle image, secrets, routing, and resource defaults into a single declarative manifest.

Kits are optional. Aegis is fully usable without any kit installed. Kits exist to reduce boilerplate when a common pattern emerges (e.g., "every Famiglia agent needs XMPP credentials, 2 vCPUs, and specific exposed ports").

## Kit Boundary

Kits are optional declarative configuration layers consumed by Aegis. In v1 they contain no executable host code -- no hooks, no plugins, no host-side scripts.

- **Aegis core** (aegisd) owns: VM lifecycle, port mapping, secrets storage, image pulling, routing, pause/resume, snapshots.
- **Kits** declare: default image, required secrets, default resources, default exposed ports, egress hints.
- **Guest code** (harness + kit agent inside VM) owns: serving semantics, readiness logic, versioning, application-level behavior.

If a kit can be built without modifying Aegis core, the abstraction is correct.

## Kit Manifest Schema (v1)

Kits are purely declarative. The manifest describes defaults and requirements:

```yaml
name: famiglia
version: "1.0.0"
description: "Team agents"
image: ghcr.io/kit/base:latest

secrets:
  required:
    - name: API_KEY
      description: "Required API key"
  optional:
    - name: EXTRA_KEY
      description: "Optional key"

exposes:
  - port: 8080

resources:
  memory_mb: 1024
  vcpus: 2
  max_memory_mb: 4096
  max_vcpus: 4

egress:
  - api.example.com
```

Aegis interprets this manifest at instance creation time. No code runs on the host.

## What Kits Control

| Aspect | Kit declares | Aegis enforces |
|--------|-------------|----------------|
| Base image | Default OCI image | Pulls, caches, injects harness |
| Required secrets | What secrets are needed | Encrypts, stores, injects as env |
| Default exposed ports | Port numbers | Configures VMM port mapping |
| Resource defaults | Memory/CPU defaults and maximums | Enforces limits |
| Egress hints | Allowed outbound hosts | Owns network topology |

Kits are declarative. They state intent and defaults. Aegis owns all enforcement, lifecycle, and runtime behavior.

## What Kits Cannot Do

- Replace or bypass the router. All ingress flows through the Aegis router.
- Control VM lifecycle (pause/resume/stop). Lifecycle is owned entirely by Aegis.
- Override snapshot semantics. Aegis owns the snapshot mechanism and lifecycle. Kits may only suggest guest-side layout conventions (e.g., "put cache in /tmp so it won't persist," "put durable state in /workspace").
- Bypass resource limits. Kits declare maximums, but Aegis enforces them.
- Execute host-side code. There are no hooks, plugins, or host scripts in v1.
- Access the host filesystem outside workspace.
- Perform readiness checks from the host. Readiness is entirely guest-side -- the kit's agent code inside the VM decides when it's ready. Aegis core does not healthcheck.

## Readiness

Readiness logic belongs entirely to the guest (kit agent inside the VM). Aegis core does not perform health checks, readiness probes, or port polling.

If the exposed port is not yet bound, the router returns 503. The kit agent inside the VM is responsible for binding ports, warming up, and handling requests. This keeps core simple and avoids coupling infrastructure to application semantics.

## Official Kits (Planned)

| Kit | Purpose | Status |
|-----|---------|--------|
| Famiglia | Team canvas agents with XMPP chat + data API | Spec complete, implementation future |
| OpenClaw | Multi-agent autonomous runtime for SWE tasks | Spec complete, implementation future |

Both kits are designed to validate the kit boundary. If they can be built without modifying Aegis core, the design is correct.

## Reference Specs

- [KIT_BOUNDARY_SPEC.md](../specs/KIT_BOUNDARY_SPEC.md) -- formal responsibility split between core and kits
- [FAMIGLIA_KIT_SPEC.md](../specs/FAMIGLIA_KIT_SPEC.md) -- Famiglia kit specification
- [OPENCLAW_KIT_SPEC.md](../specs/OPENCLAW_KIT_SPEC.md) -- OpenClaw kit specification
