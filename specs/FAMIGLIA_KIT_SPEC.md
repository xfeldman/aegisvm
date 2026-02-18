# Famiglia Kit for Aegis

**Aegis kit for team canvas agents with XMPP chat and Space data integration**

**Version:** v1.0 Draft
**Date:** 2026-02-17
**Depends on:** [AEGIS_PLATFORM_SPEC.md](AEGIS_PLATFORM_SPEC.md)

---

## 1. Summary

The Famiglia kit integrates Aegis microVMs with the Famiglia platform. It gives agents running inside Aegis sandboxes the ability to:

- Participate in room chat via XMPP (long-lived MUC connection)
- Read Space data via the Agent Data API (REST, read-only)
- Render interactive canvases served through Aegis router into Famiglia room tabs
- Be managed through the Famiglia UI (create, start, stop, configure)

Famiglia is the first official Aegis kit and serves as the proving ground for the kit API.

---

## 2. Kit Manifest

This is the `aegis-kit.yaml` for the Famiglia kit, following the manifest format defined in [AEGIS_PLATFORM_SPEC.md §5.5](AEGIS_PLATFORM_SPEC.md#55-kit-manifest--hooks-contract):

```yaml
# aegis-kit.yaml (aegis-kit-famiglia)
name: famiglia
version: 1.0.0
description: "Team canvas agents with XMPP chat and Space data integration"
homepage: https://github.com/famiglia/aegis-kit-famiglia

image:
  base: ghcr.io/famiglia/agent-base:latest

secrets:
  required:
    - name: FAMIGLIA_API_KEY
      description: "Scoped read-only API key for Agent Data API"
      scope: per_app
      generated: true
    - name: XMPP_USER
      description: "XMPP username for ejabberd"
      scope: per_app
      generated: true
    - name: XMPP_PASSWORD
      description: "XMPP password for ejabberd"
      scope: per_app
      generated: true
    - name: ANTHROPIC_API_KEY
      description: "AI provider API key"
      scope: per_workspace
      user_provided: true
      optional: true

routing:
  scheme: "/agent/{agentId}/{path}"
  session_stickiness: false
  websocket: true
  expose:
    - port: 80
      protocol: http
      ui: true                            # Canvas is user-facing

networking:
  egress: allow
  internal_hosts:
    - famiglia-web:3000       # Agent Data API (outbound HTTP)
    - ejabberd:5222           # XMPP (long-lived TCP)
    - ejabberd:5280           # XMPP WebSocket (alternative)
  long_lived_connections: true

policies:
  default_task: stateless
  default_serve: serve

resources:
  memory: 512mb
  cpu: 1
  disk: 1gb
  max_agents_per_group: 5
```

### 2.1 Kit Hooks

The Famiglia kit provides optional runtime hooks:

| Hook | Implementation |
|---|---|
| `render_env(canvas, secrets)` | Adds XMPP MUC JID, nick, domain, group/room IDs, and API URL to the env map |
| `validate_config(canvas_config)` | Verifies agent name uniqueness within group, validates XMPP slug format |
| `on_publish(canvas, version, artifacts)` | Notifies Famiglia backend to update AgentConfig status |

---

## 3. Kit Configuration Details

All secrets, networking, routing, policies, and resource defaults are declared in the kit manifest above (§2). Additional notes:

### 3.1 Secret Generation

Famiglia generates `FAMIGLIA_API_KEY`, `XMPP_USER`, and `XMPP_PASSWORD` on agent creation and registers them with Aegis for injection. Users can add arbitrary env vars (other AI API keys, MCP config, etc.) via the Famiglia UI — these are passed through as additional secrets.

### 3.2 Network Notes

- **Agent Data API** (`famiglia-web:3000`): Outbound HTTP from VM. Not ingress — no conflict with router-only ingress rule.
- **XMPP** (`ejabberd:5222/5280`): Long-lived TCP connection maintained by the agent SDK inside the VM. Aegis does not manage the XMPP connection — it only ensures the network route exists.
- **Internet egress**: Required for AI provider APIs (Anthropic, OpenAI, etc.) and MCP servers.

---

## 4. Base Image

The Famiglia kit provides a base rootfs layered on top of the Aegis base image:

```dockerfile
# ghcr.io/famiglia/agent-base
FROM aegis/base:latest

# Node.js runtime (agents are TypeScript by default)
RUN apk add --no-cache nodejs npm

# Agent SDK pre-installed
RUN npm install -g @famiglia/agent-sdk

# Working directory
WORKDIR /agent

# Default entrypoint: agent harness wraps the user's agent script
ENTRYPOINT ["famiglia-harness"]
```

The `famiglia-harness` wraps the user's agent script and handles:

- XMPP connection setup (connect to ejabberd, join MUC)
- Agent Data API client initialization
- Canvas directory setup (`/canvas`, `/workspace`)
- Heartbeat reporting to Famiglia
- Graceful shutdown on vsock `shutdown()` command

---

## 5. Canvas (Kit-Level)

Famiglia defines a **Canvas** as a room document tab with an agent-controlled UI. At the Aegis level, it maps to:

```
Famiglia Canvas = (AppID, ReleaseID, ServeTarget with port 80, ui: true)
```

| Famiglia concept | Aegis mapping |
|---|---|
| Canvas ID | `AppID` |
| Canvas version | `ReleaseID` |
| Canvas URL | ServeTarget expose port 80, `ui: true` |
| Canvas iframe in room tab | Kit UI embedding the Aegis router URL |

The canvas is rendered in the Famiglia UI as a sandboxed iframe (`sandbox="allow-scripts"`):

```html
<iframe src="http://localhost:{aegis_port}/agent/{appId}/index.html"
        sandbox="allow-scripts" />
```

Agents write HTML/SPA to the workspace volume. Aegis serves it via the router. Canvas updates use polling (version file) or WebSocket push.

---

## 6. Famiglia-Specific Integration

Everything below is Famiglia kit concern — none of it exists in Aegis core.

### 6.1 XMPP Chat Integration

#### Agent Identity

Each agent gets a service-type DID and corresponding XMPP credentials:

```
Agent DID:     did:web:s-photo-agent.famiglia.app
XMPP JID:     s-photo-agent@dev1.localhost
XMPP Nick:    @s:photo-agent@dev1.localhost
```

The `s-` prefix follows Famiglia's identifier strategy for service actors.

#### Agent Joins Room MUC

On VM start, the agent SDK connects to ejabberd and joins the room's MUC:

```typescript
// Inside agent VM — @famiglia/agent-sdk handles this
const xmpp = client({
  service: process.env.XMPP_SERVICE,    // ws://ejabberd:5280/ws
  username: process.env.XMPP_USER,      // s-photo-agent
  password: process.env.XMPP_PASSWORD,  // auto-generated
  domain: process.env.XMPP_DOMAIN,      // dev1.localhost
})

// MUC JID: {roomSlug}.{groupSlug}@conference.{host}
const mucJid = process.env.XMPP_MUC_JID
const nick = process.env.XMPP_NICK

await xmpp.send(xml('presence', { to: `${mucJid}/${nick}` },
  xml('x', { xmlns: 'http://jabber.org/protocol/muc' },
    xml('history', { maxstanzas: '0' })
  )
))
```

#### Message Flow

The agent participates in chat via the existing XMPP infrastructure. No custom bridge needed:

```
User types in Famiglia UI
    │
    ▼
Browser → Socket.IO → XMPP Worker → ejabberd MUC
    │
    ▼
ejabberd broadcasts to all MUC occupants (including agent VM)
    │
    ▼
Agent VM receives groupchat stanza (long-lived XMPP connection)
    │
    ▼
Agent processes message (calls AI, runs tools, etc.)
    │
    ▼
Agent sends groupchat stanza to MUC
    │
    ▼
ejabberd broadcasts → XMPP Worker → saves Message → Socket.IO → Browser
```

#### Document Tab Chat via XMPP Threads

Agent Document tab chat maps to XMPP threads (XEP-0201):

```xml
<!-- Agent sends to its document tab -->
<message to="photos.feldmans@conference.dev1.localhost" type="groupchat">
  <body>Found 234 photos! Building gallery...</body>
  <thread>clx_document_crdt_key</thread>
</message>

<!-- Agent sends to room's main chat (no thread) -->
<message to="photos.feldmans@conference.dev1.localhost" type="groupchat">
  <body>Gallery updated with 12 new photos</body>
</message>
```

#### Private Room Access

For `PARTICIPANTS_ONLY` rooms, Famiglia grants the agent MUC affiliation:

```typescript
await setMucAffiliation(mucJid, agentJid, 'member')
```

### 6.2 Agent Data API

Read-only REST API giving agents access to Space data. This API is served by Famiglia (not Aegis) and accessed by the agent over the network.

```
Base URL: http://famiglia-web:3000/api/agent
Auth: Authorization: Bearer {FAMIGLIA_API_KEY}
```

#### Endpoints

| Endpoint | Description |
|---|---|
| `GET /api/agent/group` | Group metadata, member list |
| `GET /api/agent/room` | Current room details |
| `GET /api/agent/rooms` | All rooms in the group |
| `GET /api/agent/feed?limit=50&type=image` | Group feed posts (filterable) |
| `GET /api/agent/feed/:postId` | Single post |
| `GET /api/agent/media/:mediaId` | Proxy media files from storage |
| `GET /api/agent/chat/history?roomId=xxx` | Message history |
| `GET /api/agent/documents?roomId=xxx` | Room documents |
| `GET /api/agent/files?q=search` | File search |
| `GET /api/agent/apps` | App schemas |
| `GET /api/agent/apps/:appId/records` | App records |

#### Authentication

Each agent gets a unique API key that is:

- Group-scoped (can read any data in the group)
- Read-only (no mutations)
- Revocable (admin can regenerate)
- Identified in audit logs

Rate limit: 100 requests/minute per agent.

#### Pagination Convention

All list endpoints:

```
?limit=50        (default 50, max 200)
&offset=0        (default 0)
```

Response:

```json
{
  "data": [...],
  "total": 234,
  "hasMore": true
}
```

### 6.3 Canvas as Room Tab

In Famiglia, an agent's canvas is rendered as a **room document tab**:

```
Room: #photos
[Chat] [Photo Library Agent] [Notes] [+]
         ^
         Agent canvas served by Aegis router,
         embedded in Famiglia UI as iframe
```

Layout:

```
┌────────────────────────────────────────────┐
│  CANVAS (iframe → Aegis router → VM)       │
│  ┌──────────────────────────────────────┐  │
│  │  Agent-controlled UI                 │  │
│  │  (photo gallery, dashboard, etc.)    │  │
│  └──────────────────────────────────────┘  │
├────────────────────────────────────────────┤
│  CHAT (XMPP via existing Famiglia chat)    │
│  ┌──────────────────────────────────────┐  │
│  │ You: Build me a photo library       │  │
│  │ Agent: Found 234 photos!            │  │
│  └──────────────────────────────────────┘  │
└────────────────────────────────────────────┘
```

The iframe is sandboxed with `sandbox="allow-scripts"`:

- JavaScript execution allowed (agent UI needs it)
- No same-origin access (can't read Famiglia cookies, localStorage)
- No form submission, popups, top navigation, plugins

#### Canvas URL

```
<iframe src="http://localhost:{aegis_port}/agent/{agentId}/index.html" />
```

Aegis router resolves the agent ID to the running VM instance and proxies the request.

---

## 7. Data Model (Famiglia-Side)

These models live in Famiglia's database, not in Aegis.

### 7.1 DocumentType Extension

```prisma
enum DocumentType {
  EDITOR    // TipTap collaborative editing (existing)
  APP       // Conversational app with schema + records (existing)
  AGENT     // Aegis-backed AI agent with canvas (NEW)
}
```

### 7.2 AgentConfig

```prisma
model AgentConfig {
  id              String       @id @default(cuid())
  documentId      String       @unique

  // Agent identity
  name            String       // "Photo Library Agent"
  description     String?

  // Aegis integration
  aegisAppId      String?      // AppID in Aegis registry
  aegisInstanceId String?      // Current InstanceID (transient)
  status          AgentStatus  @default(STOPPED)

  // Auth — Data API
  apiKey          String       @unique

  // Auth — XMPP
  xmppUser        String       @unique
  xmppPassword    String

  // Configuration
  env             Json?        // Extra env vars (AI API keys, MCP config)
  canvasEnabled   Boolean      @default(true)

  // Lifecycle
  lastHeartbeat   DateTime?
  startedAt       DateTime?
  stoppedAt       DateTime?
  createdAt       DateTime     @default(now())
  updatedAt       DateTime     @updatedAt

  document        RoomDocument @relation(...)
}

enum AgentStatus {
  STOPPED
  STARTING
  RUNNING
  PAUSED       // NEW: Aegis hot-start pause state
  ERROR
}
```

### 7.3 Famiglia-to-Aegis Lifecycle Mapping

| Famiglia Action | Aegis API Call |
|---|---|
| User creates Agent Document | `POST /v1/apps` (register app in Aegis) |
| User clicks "Start Agent" | `POST /v1/instances/ensure` → VM boots, agent SDK connects XMPP |
| User opens agent tab | `POST /v1/instances/ensure` (if paused, resumes instantly) |
| User leaves room | (no action — Aegis idle timers handle pause/terminate) |
| User returns to room | `POST /v1/instances/ensure` (resume from pause or restore from snapshot) |
| User clicks "Stop Agent" | `POST /v1/instances/{id}/terminate` |
| Agent crashes | Aegis detects via harness health check → Famiglia sets status to ERROR |

---

## 8. Agent SDK (`@famiglia/agent-sdk`)

TypeScript SDK that runs inside the Aegis VM. Handles all Famiglia-specific integration.

### 8.1 Usage

```typescript
import { FamigliaAgent } from '@famiglia/agent-sdk'

const agent = new FamigliaAgent()

// React to user messages (received via XMPP MUC)
agent.onMessage(async (msg) => {
  // msg.from: '@u:alice@dev1.localhost'
  // msg.body: 'Build me a photo library from all group photos'
  // msg.thread: 'clx...' (document tab) or null (room chat)

  // Read group data via Agent Data API
  const photos = await agent.api.feed({ type: 'image', limit: 100 })

  // Update canvas (write files to /canvas → Aegis serves them)
  await agent.canvas.write('index.html', `
    <html>
    <body>
      <h1>Photo Library (${photos.total} photos)</h1>
      <div class="grid">
        ${photos.data.map(p => `
          <img src="${p.media[0].url}" alt="${p.content.text}" />
        `).join('')}
      </div>
    </body>
    </html>
  `)

  // Respond in chat (via XMPP)
  await agent.say(`Found ${photos.total} photos! Gallery is ready.`)
})

agent.start()
```

### 8.2 SDK API Surface

```typescript
class FamigliaAgent {
  // Chat (XMPP)
  onMessage(handler: (msg: AgentMessage) => Promise<void>): void
  say(text: string): Promise<void>          // Respond in document tab
  announce(text: string): Promise<void>     // Send to room's main chat

  // Data API
  api: {
    group(): Promise<GroupInfo>
    room(): Promise<RoomInfo>
    rooms(): Promise<RoomInfo[]>
    feed(opts?: FeedQuery): Promise<Paginated<Post>>
    media(mediaId: string): Promise<Buffer>
    chatHistory(opts?: ChatQuery): Promise<Paginated<Message>>
    documents(roomId: string): Promise<Document[]>
    files(query: string): Promise<FileResult[]>
    apps(): Promise<App[]>
    appRecords(appId: string, opts?: RecordQuery): Promise<Paginated<Record>>
  }

  // Canvas
  canvas: {
    write(path: string, content: string | Buffer): Promise<void>
    read(path: string): Promise<string | Buffer>
    delete(path: string): Promise<void>
    bumpVersion(): Promise<void>    // Signal canvas update
  }

  // Lifecycle
  start(): Promise<void>
  stop(): Promise<void>
}
```

---

## 9. Implementation Phases

### Phase 1: Aegis Core + Famiglia Kit MVP

**Aegis side:**
- [ ] aegisd with VMM abstraction (Firecracker on Linux, libkrun on macOS)
- [ ] Base snapshot creation and restore
- [ ] Workspace volume mounting
- [ ] Task execution (Mode A)
- [ ] Basic networking (TAP, NAT, egress)
- [ ] Lima macOS integration
- [ ] CLI: `aegis up`, `aegis run`, `aegis status`
- [ ] Registry (SQLite)

**Famiglia kit side:**
- [ ] Kit registration and configuration
- [ ] Agent Data API endpoints (group, room, feed, media, chat history)
- [ ] API key auth middleware
- [ ] XMPP agent registration (ejabberd user + MUC affiliation)
- [ ] AgentConfig model + migration
- [ ] UI: Agent Document tab (create, start, stop)
- [ ] Base image with agent SDK

**Deliverable:** User can create an Agent Document in Famiglia, agent runs in Aegis microVM, chats via XMPP, reads Space data via API.

### Phase 2: Serve Mode + Canvas

**Aegis side:**
- [ ] Router with instance resolution and wake-on-connect
- [ ] App versioning and publishing (releases)
- [ ] Hybrid hot-start (pause/resume/terminate)
- [ ] Snapshot tiers (base + release overlays)
- [ ] Serve mode with scale-to-zero
- [ ] CLI: `aegis app publish/serve/releases`

**Famiglia kit side:**
- [ ] Canvas iframe integration in room tabs
- [ ] Canvas update signaling (XMPP or polling)
- [ ] Agent template system
- [ ] Template picker in Create Agent dialog

**Deliverable:** Agents render interactive canvases in room tabs. Instant resume when users switch tabs.

### Phase 3: Production Hardening

**Aegis side:**
- [ ] Snapshot GC with retention policies
- [ ] Resource quota enforcement
- [ ] Secret injection security hardening
- [ ] Warm VM pool for instant task execution
- [ ] CLI: `aegis kit install/list/info`
- [ ] Observability (metrics, structured logs)

**Famiglia kit side:**
- [ ] Agent status indicators in room tab bar
- [ ] Container logs viewer (admin debug tool)
- [ ] SPA canvas template (React starter)
- [ ] Agent-to-agent communication (multi-agent rooms)
- [ ] Mobile-responsive canvas layout

**Deliverable:** Production-ready Aegis platform with mature Famiglia integration.

---

## 10. Open Questions

1. **Agent image registry** — does Famiglia run its own, or use GHCR exclusively?
2. **Hot reload** — should agent code changes auto-restart the VM, or require manual restart?
3. **Multi-agent rooms** — can a room have multiple Agent Documents? (Probably yes, they're just tabs)
4. **Agent-initiated canvas** — should agents be able to open their canvas tab proactively?
5. **Shared canvas** — should multiple users see the same canvas state, or per-user views?
6. **Famiglia assistant integration** — how does the existing room assistant (ASSISTANT_ARCHITECTURE_V2) interact with Agent Documents?

---

## Related Documents

- [AEGIS_PLATFORM_SPEC.md](AEGIS_PLATFORM_SPEC.md) — Aegis platform specification
- [AGENT_DOCUMENT_SPEC.md](../apps/AGENT_DOCUMENT_SPEC.md) — Original Agent Document spec (superseded by this document)
- [ROOM_DOCUMENTS_APPS_SPEC.md](../apps/ROOM_DOCUMENTS_APPS_SPEC.md) — App Document system
- [AI_AGENT_TOOLCHAIN_ADDENDUM.md](../apps/AI_AGENT_TOOLCHAIN_ADDENDUM.md) — AI toolchain
- [ASSISTANT_ARCHITECTURE_V2.md](../groups/ASSISTANT_ARCHITECTURE_V2.md) — Room assistant
