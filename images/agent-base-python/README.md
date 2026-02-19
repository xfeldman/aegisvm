# agent-base-python

Minimal Python base image for Aegis agents.

## What's included

- Python 3.12 (Alpine-based)
- curl, wget
- Working directory set to `/workspace`

Image size is approximately 80MB.

## Build

```bash
docker build -t agent-base:python .
```

## Usage

```bash
# Long-lived instance with exposed port
aegis run --name myapp --image agent-base:python --expose 80 -- python server.py

# One-shot command
aegis run --image agent-base:python -- python agent.py
```

## Harness injection

You do not need to include `aegis-harness` in your image. Aegis injects the harness automatically when preparing the instance rootfs. The harness is always PID 1 inside the VM. Aegis ignores any `ENTRYPOINT` or `CMD` set in the Dockerfile.

## Conventions

See `docs/AGENT_CONVENTIONS.md` for structured logging, workspace layout, and secrets access patterns.
