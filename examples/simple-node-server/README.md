# simple-node-server

Demonstrates a long-running AegisVM agent: a Node.js HTTP server returning JSON, using only the standard library (no Express or other dependencies).

## What this example shows

- **Long-lived instance**: the server runs indefinitely, handling requests via the AegisVM router.
- **JSON API**: returns a structured JSON response with status, secret presence, request path, and timestamp.
- **Secrets injection**: reads `SECRET_KEY` from environment variables.
- **Structured logging**: writes JSON log lines to stdout for each request.

## Running

```bash
aegis up
aegis secret set SECRET_KEY my-secret-value
aegis run --name node-demo --expose 80 --image node:22-alpine -- node server.js
```

In another terminal:

```bash
curl http://127.0.0.1:8099/
```

Expected response:

```json
{
  "status": "ok",
  "secret_configured": true,
  "path": "/",
  "timestamp": "2026-02-18T12:00:00.000Z"
}
```

## Conventions used

This example follows the patterns described in `docs/AGENT_CONVENTIONS.md`:

- Structured JSON logging to stdout
- Listens on port 80 (the exposed port)
- Secrets read from environment variables
- No external dependencies required
