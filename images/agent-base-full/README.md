# agent-base-full

Full dev environment base image for Aegis agents. Includes both Python and Node.js runtimes plus common dev tools.

## What's included

- Python 3.12
- Node.js + npm
- git
- curl, wget
- jq
- openssh-client

Image size is approximately 200MB.

## Build

```bash
docker build -t agent-base:full .
```

## Usage

```bash
aegis run --name web --image agent-base:full --expose 80 -- python server.py
```

This image is suitable for agents that need both Python and Node.js, or that shell out to git, jq, or other tools during execution.

## Harness injection

You do not need to include `aegis-harness` in your image. Aegis injects the harness automatically when preparing the instance rootfs. The harness is always PID 1 inside the VM.

## Conventions

See `docs/AGENT_CONVENTIONS.md` for structured logging, workspace layout, and secrets access patterns.
