# Aegis Kit System

## Overview

A kit is a reproducible execution environment and integration layer that runs on top of Aegis. Kits are integration accelerators -- pre-packaged agent runtime configurations that bundle image, secrets, routing, and resource defaults into a single installable manifest.

Kits are optional. Aegis is fully usable without any kit installed. You can build images, run tasks, serve apps, manage secrets, and route traffic with zero kits. Kits exist to reduce boilerplate when a common pattern emerges (e.g., "every Famiglia agent needs XMPP credentials, 2 vCPUs, and a healthcheck on port 8080").

Kits declare what goes inside the VM, required secrets, default resources, and routing config. They do not own any part of the VM lifecycle or host-side execution.

## Kit Manifest Schema

The full YAML manifest schema with all supported fields:

```yaml
name: famiglia                    # required, unique identifier
version: "1.0.0"                  # required, semver
description: "Team agents"        # optional
image: ghcr.io/kit/base:latest    # required, OCI image reference

config:
  secrets:
    required:
      - name: API_KEY
        description: "Required API key"
    optional:
      - name: EXTRA_KEY
        description: "Optional key"
  routing:
    default_port: 8080            # default exposed port
    healthcheck: /health          # readiness path hint (informational)
    headers:                      # custom headers added to proxied requests
      X-Kit-Name: famiglia
  networking:
    egress:                       # allowed egress hosts (informational in M3)
      - api.example.com
  policies:
    max_memory_mb: 4096           # max memory a kit app can request
    max_vcpus: 4                  # max vCPUs a kit app can request
  resources:
    memory_mb: 1024              # default memory for kit apps
    vcpus: 2                     # default vCPUs for kit apps
```

The `name` field is the kit's unique identifier across the system. The `image` field is the default OCI image reference used when an app is created under this kit without specifying its own image.

## What Kits Control

| Aspect | Kit controls | Aegis enforces |
|--------|-------------|----------------|
| Base image | Declares default OCI image | Pulls, caches, injects harness |
| Required secrets | Declares what secrets are needed | Encrypts, stores, injects |
| Resource defaults | Declares memory/CPU defaults | Enforces limits |
| Routing hints | Declares default port, healthcheck | Owns the router |
| Networking hints | Declares egress allowlist | Owns network topology |

Kits are declarative. They state intent and defaults. Aegis owns all enforcement, lifecycle, and runtime behavior.

## What Kits Cannot Do

Kits operate within a strict boundary. They cannot:

- Replace or bypass the router. All ingress flows through the Aegis router regardless of kit configuration.
- Control VM lifecycle (pause/resume/terminate). Lifecycle is owned entirely by Aegis.
- Override snapshot semantics. Snapshot behavior is a platform concern, not a kit concern.
- Bypass resource limits. The `policies` section declares maximums, but Aegis enforces them.
- Execute arbitrary host-side code. There are no host hooks in M3. Kits cannot run scripts on the host.
- Access the host filesystem outside workspace. The only host directory visible inside the VM is the workspace mount.

## Kit Hooks (Interface Defined, No-Op in M3)

The hooks interface is defined but uses `DefaultHooks` (pass-through) in M3. Real implementations come when Famiglia/OpenClaw kits are built.

| Hook | When called | Purpose |
|------|-------------|---------|
| `RenderEnv(app, secrets)` | Before VM boot | Transform secrets + config into final env map |
| `ValidateConfig(appConfig)` | On app creation | Validate app-specific configuration |
| `OnPublish(app, release)` | After publish | Post-publish actions (e.g., notify external service) |

`DefaultHooks` passes all inputs through unchanged. `RenderEnv` returns the secret map as-is, `ValidateConfig` always returns nil, and `OnPublish` is a no-op. This lets the rest of the system call hooks uniformly without checking whether a kit has a real implementation.

## Installing and Managing Kits

CLI commands for kit management:

```
aegis kit install manifest.yaml    # parse YAML, register via API
aegis kit list                     # table of installed kits
aegis kit info famiglia            # details
aegis kit uninstall famiglia       # remove
```

See [CLI.md](CLI.md) for full CLI reference and usage examples.

## CLI vs API Parsing

The CLI and API handle manifest parsing differently:

- `aegis kit install` parses only top-level scalar fields (name, version, image, description) from the YAML using a simple line-based parser. Nested `config:` sections are not sent.
- For full config registration, POST JSON directly to `POST /v1/kits` with the complete `config` object.
- Server-side (`internal/kit/manifest.go`) uses `gopkg.in/yaml.v3` for full manifest parsing including nested config.

This split exists because the CLI install path is intentionally simple -- it registers the kit identity and image, and the full config can be populated via the API. Most users will not need nested config in M3 since hooks are no-ops.

## Official Kits (Planned)

| Kit | Purpose | Status |
|-----|---------|--------|
| Famiglia | Team canvas agents with XMPP chat + data API | Spec complete, implementation M5+ |
| OpenClaw | Multi-agent autonomous runtime for SWE tasks | Spec complete, implementation M5+ |

Both kits are designed to validate the kit boundary. If they can be built without modifying Aegis core, the design is correct. Any change to Aegis internals required by a kit indicates a gap in the platform abstraction.
