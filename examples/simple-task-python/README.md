# simple-task-python

Demonstrates a short-lived AegisVM agent written in Python. The agent runs once and exits.

## What this example shows

- **One-shot execution**: the agent runs to completion and exits with code 0.
- **Secrets**: reads the `GREETING` environment variable (injected by AegisVM secrets).
- **Workspace writes**: creates output files under `/workspace/output/`.
- **Structured logging**: writes JSON log lines to stdout with level, message, and timestamp.

## Running

```bash
aegis up
aegis run --image python:3.12-alpine -- python /path/to/agent.py
```

In practice, the agent code would be baked into a custom image or copied to the workspace before execution:

```bash
# Option 1: bake into image
docker build -t my-task-agent .
aegis run --image my-task-agent -- python agent.py

# Option 2: copy to workspace (future milestone)
aegis cp agent.py myvm:/workspace/agent.py
aegis run -- python /workspace/agent.py
```

To inject a secret:

```bash
aegis secret set GREETING "Hello, world"
```

## Conventions used

This example follows the patterns described in `docs/AGENT_CONVENTIONS.md`:

- Structured JSON logging to stdout
- Output written to `/workspace/output/`
- Secrets read from environment variables
- Clean exit with code 0 on success
