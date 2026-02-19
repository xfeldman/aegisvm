# Aegis Ingress Model: Docker-Style Static Expose

## Context

As part of the architectural pivot, Aegis core no longer models “serve mode” or an “app plane.”
Instances are the only first-class runtime object. Serving semantics belong to the harness/SDK (kits).

However, libkrun and Firecracker require inbound port forwarding to be configured at VM creation time.
This imposes a physical constraint: we cannot dynamically add inbound mappings after the VM starts.

This document defines the ingress model that reconciles:

- The clean pivot (no serve semantics in core)
- VMM constraints (static port mapping)
- Operational clarity (no readiness coupling)

---

## Core Principle

Aegis uses **Docker-style static port mapping**.

Ingress is infrastructure configuration — not lifecycle semantics.

`--expose` configures port forwarding at instance creation.
It does **not**:

- Enable “serve mode”
- Imply readiness
- Create an “app” object
- Affect versioning
- Modify lifecycle state transitions

It is equivalent to CPU/memory configuration.

---

## CLI Semantics

Example:

```
aegis instance start   --image ghcr.io/org/app@sha256:...   --name my-instance   --expose 8080:7777
```

Meaning:

- Host ingress (via Aegis router) forwards traffic to guest port 7777.
- The instance may or may not bind that port.
- If nothing listens, the router returns 503.
- No readiness gating occurs in Aegis core.

This mirrors:

```
docker run -p 8080:80 nginx
```

Infrastructure-level port forwarding only.

---

## Router Behavior

Router responsibilities:

- Route based on expose configuration.
- Resume paused instance on ingress (if enabled).
- Retry proxy with bounded timeout.
- Return 503 + Retry-After if upstream unavailable.

Router does NOT:

- Wait for readiness events.
- Inspect guest port state.
- Infer serve state.
- Track “app plane.”

---

## Harness / Kit Responsibility

Serving semantics belong to the guest (harness/SDK).

Kits may:

- Delay binding until ready.
- Return 503 internally while warming up.
- Multiplex multiple logical apps behind a single exposed port.
- Emit logs indicating readiness.

Aegis core remains unaware of these details.

---

## Mutability Rules (v1 Recommendation)

To keep semantics simple in v1:

- `--expose` is configured at instance creation.
- Expose mappings are immutable for the lifetime of the instance.
- Changing ingress requires creating a new instance.

This avoids:

- VMM reconfiguration complexity
- Router race conditions
- Dynamic security surface changes

Future versions may relax this constraint once backends fully support safe reconfiguration.

---

## Design Benefits

- Aligns with Docker mental model.
- Preserves architectural pivot purity.
- Avoids tunneling HTTP over control channel.
- Avoids dynamic VMM mutation.
- Keeps Aegis core minimal and infrastructure-focused.
- Keeps versioning entirely in kit layer.

---

## Non-Goals

Aegis does not:

- Implement application publishing.
- Implement release management.
- Track readiness or health for routing.
- Distinguish serve vs task modes.

Ingress is plumbing — not platform behavior.

---

## Final Statement

Aegis provides:

> A local scale-to-zero microVM runtime with static ingress mapping and control channel.

Serving remains:

> A harness/kit capability layered on top of infrastructure.
