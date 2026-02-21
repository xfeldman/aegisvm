# Guest Orchestration API

The Guest API allows processes running inside an Aegis VM to spawn and manage other Aegis instances. This enables patterns like a Telegram bot spawning work instances for heavy tasks.

## Quick Start

From inside a VM with spawn capabilities:

```bash
# Check your instance info
curl http://127.0.0.1:7777/v1/self

# Spawn a child instance
curl -X POST http://127.0.0.1:7777/v1/instances \
  -H "Content-Type: application/json" \
  -d '{"command":["python3","-m","http.server","8080"],"image_ref":"python:3.12","exposes":[8080]}'

# List your children
curl http://127.0.0.1:7777/v1/instances

# Stop a child
curl -X POST http://127.0.0.1:7777/v1/instances/{child_id}/stop
```

No authentication required — the harness handles it automatically.

## Environment Variables

Available inside every VM:

| Variable | Description |
|----------|-------------|
| `AEGIS_GUEST_API` | Guest API URL (always `http://127.0.0.1:7777`) |
| `AEGIS_INSTANCE_ID` | This instance's ID |

## Prerequisites: Capabilities

An instance can only spawn children if it was created with `capabilities`:

```bash
curl -s --unix-socket ~/.aegis/aegisd.sock -X POST http://aegis/v1/instances \
  -H "Content-Type: application/json" \
  -d '{
    "handle": "my-bot",
    "command": ["node", "bot.js"],
    "capabilities": {
      "spawn": true,
      "spawn_depth": 2,
      "max_children": 5,
      "allowed_images": ["node:22", "python:3.12", "alpine"],
      "max_memory_mb": 2048,
      "max_vcpus": 2,
      "max_expose_ports": 3
    }
  }'
```

Without `capabilities`, the instance can still call `/v1/self` and `/v1/self/keepalive`, but spawn requests will fail.

### Capability Fields

| Field | Type | Description |
|-------|------|-------------|
| `spawn` | bool | Allow spawning children |
| `spawn_depth` | int | Nesting depth (1 = children can't spawn grandchildren) |
| `max_children` | int | Maximum concurrent children |
| `allowed_images` | []string | OCI images children can use (`"*"` = any) |
| `max_memory_mb` | int | Per-child memory ceiling |
| `max_vcpus` | int | Per-child vCPU ceiling |
| `max_expose_ports` | int | Maximum exposed ports per child |

Child instances inherit capabilities with the same or stricter limits — no escalation is possible.

## API Reference

### `GET /v1/self`

Returns info about this instance. No capabilities required.

**Response:**
```json
{
  "id": "inst-1234567890",
  "handle": "my-bot",
  "state": "running",
  "image": "node:22",
  "parent_id": ""
}
```

### `POST /v1/instances`

Spawn a child instance. Requires `spawn` capability.

**Request:**
```json
{
  "command": ["python3", "build.js"],
  "handle": "work-1",
  "image_ref": "node:22",
  "workspace": "/Users/me/projects/my-site",
  "exposes": [8080],
  "memory_mb": 2048,
  "env": {"NODE_ENV": "production"}
}
```

**Response:**
```json
{
  "id": "inst-9876543210",
  "handle": "work-1",
  "state": "starting",
  "parent_id": "inst-1234567890"
}
```

The child boots in the background. Poll `/v1/instances` to check when it's running.

### `GET /v1/instances`

List children of this instance.

**Response:**
```json
[
  {"id": "inst-9876543210", "handle": "work-1", "state": "running"},
  {"id": "inst-1111111111", "handle": "work-2", "state": "stopped"}
]
```

### `POST /v1/instances/{id}/stop`

Stop a child instance. Only works on your own children.

**Response:**
```json
{"status": "stopped"}
```

### `POST /v1/self/keepalive`

Acquire a keepalive lease (prevents idle pause).

**Request:**
```json
{"ttl_ms": 30000, "reason": "building"}
```

### `DELETE /v1/self/keepalive`

Release the keepalive lease.

## Security Model

- **No auth header needed** — the harness attaches the capability token automatically
- **Token never exposed** — stays in harness memory, not in environment variables
- **Host validates everything** — the harness forwards requests without checking; aegisd validates the token signature, capabilities, and resource ceilings
- **No escalation** — children inherit parent's caps (same or stricter)
- **No upward visibility** — children can't see or manage the parent
- **Cascade stop** — when a parent stops, all children are stopped automatically

## Parent-Child Lifecycle

```
Parent creates child → child boots → child runs workload
                                        ↓
                                    child idles → child pauses → child stops

Parent stops → ALL children stopped (cascade)
```

Children are independent instances with their own idle/pause/stop lifecycle. They persist across parent restarts (the parent_id link is stored in the registry).

## Example: Bot Spawning Work Instance

```javascript
// Inside a Telegram bot running in Aegis
const AEGIS_API = process.env.AEGIS_GUEST_API;

async function handleBuildRequest(chatId, repoUrl) {
  // Spawn a work instance
  const resp = await fetch(`${AEGIS_API}/v1/instances`, {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      command: ['sh', '-c', `git clone ${repoUrl} /workspace/repo && cd /workspace/repo && npm install && npm run build`],
      image_ref: 'node:22',
      handle: `build-${chatId}`,
      memory_mb: 2048,
      exposes: [3000],
    }),
  });

  const child = await resp.json();
  sendTelegram(chatId, `Build started: ${child.id}`);

  // Poll until done
  while (true) {
    await sleep(5000);
    const info = await fetch(`${AEGIS_API}/v1/instances`).then(r => r.json());
    const c = info.find(i => i.id === child.id);
    if (!c || c.state === 'stopped') {
      sendTelegram(chatId, 'Build complete!');
      break;
    }
  }
}
```

## MCP Server for LLM Agents

For LLM agents (Claude, OpenClaw, etc.) running inside a VM, the Guest API is also available as an MCP server: `aegis-mcp-guest`.

This binary is pre-installed at `/usr/bin/aegis-mcp-guest` in every Aegis VM. It communicates over stdio (JSON-RPC 2.0) and calls the Guest API on localhost:7777.

### Available MCP Tools

| Tool | Description |
|------|-------------|
| `instance_spawn` | Spawn a child VM with command, image, workspace, exposed ports |
| `instance_list` | List children of this instance (only your children, not all instances) |
| `instance_stop` | Stop a child instance |
| `self_info` | Get this instance's ID, handle, state, endpoints |
| `keepalive_acquire` | Prevent idle pause during long work (with TTL) |
| `keepalive_release` | Release keepalive, allow idle pause |

### Registering with Claude Code

To use the Guest API from Claude Code running inside an Aegis VM, add the MCP server to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "aegis": {
      "command": "/usr/bin/aegis-mcp-guest",
      "args": []
    }
  }
}
```

Or register it via the CLI:

```bash
claude mcp add aegis /usr/bin/aegis-mcp-guest
```

Claude will then have access to all 6 tools and can spawn child instances, monitor their state, and manage their lifecycle.

### Registering with OpenClaw

For OpenClaw agents, add the MCP server as a tool provider in the agent config. The exact configuration depends on OpenClaw's MCP tool integration — consult OpenClaw's documentation for registering external MCP servers.

### Example: Claude Spawning a Build Instance

When Claude Code runs inside an Aegis VM with spawn capabilities, it can use the MCP tools directly:

```
User: Build and serve my React app at /workspace/my-app

Claude: I'll spawn a dedicated build instance with Node.js.

[Calls instance_spawn with command=["sh", "-c", "cd /workspace/my-app && npm install && npm run build && npx serve -s build -l 3000"], image_ref="node:22", workspace="/Users/me/my-app", exposes=[3000], handle="react-build"]

The build instance is starting. Let me check its status.

[Calls instance_list]

It's running. The app will be available at the public port shown in the response once the build completes.
```

### Security Note

`instance_list` only returns instances spawned by the calling VM — it does NOT show all Aegis instances. Each VM can only see and manage its own children. This prevents information leakage between unrelated instances.
