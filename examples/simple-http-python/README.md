# simple-http-python

Demonstrates a serve-mode Aegis agent: a long-running HTTP server written in Python using only the standard library.

## What this example shows

- **Serve mode**: the server runs indefinitely, handling requests via the Aegis router.
- **Secrets injection**: reads `API_KEY` from environment variables.
- **Workspace listing**: displays files present in `/workspace` on the response page.
- **Structured logging**: writes JSON log lines to stdout for each request.
- **Router access**: the Aegis router forwards external HTTP traffic to port 80 inside the VM.

## Running

```bash
aegis up
aegis app create --name http-demo --image python:3.12-alpine --expose 80 -- python server.py
aegis secret set http-demo API_KEY sk-test123
aegis app publish http-demo
aegis app serve http-demo
```

In another terminal:

```bash
curl http://127.0.0.1:8099/
```

## Conventions used

This example follows the patterns described in `docs/AGENT_CONVENTIONS.md`:

- Structured JSON logging to stdout
- Listens on port 80 (the conventional exposed port)
- Secrets read from environment variables
- Workspace at `/workspace`
