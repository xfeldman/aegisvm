# simple-node-server

Demonstrates a serve-mode Aegis agent: a Node.js HTTP server returning JSON, using only the standard library (no Express or other dependencies).

## What this example shows

- **Serve mode**: the server runs indefinitely, handling requests via the Aegis router.
- **JSON API**: returns a structured JSON response with status, secret presence, request path, and timestamp.
- **Secrets injection**: reads `SECRET_KEY` from environment variables.
- **Structured logging**: writes JSON log lines to stdout for each request.

## Running

```bash
aegis up
aegis app create --name node-demo --image node:22-alpine --expose 80 -- node server.js
aegis app publish node-demo
aegis app serve node-demo
```

In another terminal:

```bash
curl http://127.0.0.1:8099/
```

Expected response:

```json
{
  "status": "ok",
  "secret_configured": false,
  "path": "/",
  "timestamp": "2026-02-18T12:00:00.000Z"
}
```

To inject a secret:

```bash
aegis secret set node-demo SECRET_KEY my-secret-value
```

## Conventions used

This example follows the patterns described in `docs/AGENT_CONVENTIONS.md`:

- Structured JSON logging to stdout
- Listens on port 80 (the conventional exposed port)
- Secrets read from environment variables
- No external dependencies required
