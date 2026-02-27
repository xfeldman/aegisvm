# Agent Kit Tools Roadmap

**Status:** Active
**Updated:** 2026-02-27

---

## 1. Current State

### Built-in tools (19)

All built-in tools can be disabled per-instance via `disabled_tools` in `agent.json`.

| Tool | What it does |
|------|-------------|
| `bash` | Execute shell commands (60s timeout, 10KB output) |
| `read_file` | Read file contents, supports line ranges (start_line/end_line) |
| `write_file` | Create/overwrite files, auto-creates parent directories |
| `edit_file` | Targeted edits via text match or line range, returns diff |
| `list_files` | List directory contents |
| `glob` | Find files by pattern (`**/*.go`), cap 200 results |
| `grep` | Regex search file contents with include filter, cap 50 matches |
| `web_fetch` | Fetch URL, strip HTML, extract text (10KB cap) |
| `web_search` | Search the web via Brave Search API (requires BRAVE_SEARCH_API_KEY) |
| `image_search` | Search for images via Brave Image Search API, returns direct URLs |
| `image_generate` | Generate images via OpenAI Images API (DALL-E / gpt-image-1) |
| `respond_with_image` | Attach a downloaded image to the response (user sees it in Telegram etc.) |
| `memory_store` | Store a persistent memory (with secret rejection, max 500 chars) |
| `memory_search` | Search memories by keyword and/or tag |
| `memory_delete` | Delete a memory by ID |
| `cron_create` | Create a scheduled recurring task |
| `cron_list` | List all cron entries |
| `cron_delete` | Delete a cron entry |
| `cron_enable` / `cron_disable` | Toggle cron entries without deleting |
| `self_info` | Get VM instance info (ID, handle, state, endpoints) |
| `self_restart` | Restart agent process cleanly (config reload, no data loss) |

### MCP tools via aegis-mcp-guest (7)

VM orchestration — always loaded, not configurable via `agent.json`.

| Tool | What it does |
|------|-------------|
| `instance_spawn` | Spawn child VM instance |
| `instance_list` | List child instances |
| `instance_stop` | Stop a child instance |
| `expose_port` | Expose guest port on host |
| `unexpose_port` | Remove port exposure |
| `keepalive_acquire` | Prevent VM pause during long work |
| `keepalive_release` | Release keepalive lease |

### Agent configuration

`/workspace/.aegis/agent.json`:

```json
{
  "model": "openai/gpt-5.2",
  "max_tokens": 4096,
  "context_chars": 24000,
  "context_turns": 50,
  "system_prompt": "Custom prompt...",
  "disabled_tools": ["image_generate", "web_search"],
  "mcp": {
    "my-server": {"command": "npx", "args": ["my-mcp-server@latest"]}
  },
  "memory": {
    "inject_mode": "relevant",
    "max_inject_chars": 2000,
    "max_inject_count": 10,
    "max_total": 500
  }
}
```

- Env var overrides: `AEGIS_MODEL`, `AEGIS_MAX_TOKENS`, `AEGIS_CONTEXT_CHARS`, `AEGIS_CONTEXT_TURNS`, `AEGIS_SYSTEM_PROMPT`
- `disabled_tools`: deny list of built-in tool names to disable (new tools auto-enabled)
- MCP: user-added servers. Core tools (aegis-mcp-guest) are always injected automatically.
- Agent can edit this file and call `self_restart` to apply changes at runtime.

### Memory

- JSONL-backed persistent memory (`/workspace/.aegis/memory/memories.jsonl`)
- Automatic context injection — relevant memories injected into system prompt before each LLM call
- Keyword-based relevance scoring with stopword filter and recency bonus (only on overlap)
- Three injection modes: `relevant` (default), `recent_only`, `off`
- Secret rejection: API keys, tokens, high-entropy blobs blocked from storage
- Scope field stored (`user`/`workspace`/`session`) for future filtering

### Cron (scheduled tasks)

- Scheduler runs in the gateway (host-side) — no keepalive needed, VM pauses freely
- Cron file: `/workspace/.aegis/cron.json` (host-mounted, gateway reads directly)
- Agent tools create/manage entries, gateway fires tether messages on schedule
- Wake-on-message wakes the VM, agent processes, VM goes idle again
- `on_conflict`: `skip` (default) or `queue` per entry
- Deduplicate fires per minute, host local time evaluation

### Image support

- Ingress: users can send images to the agent (Telegram photos, tether image blocks)
- Egress: `respond_with_image` attaches images to responses, `image_generate` creates AI images
- `image_search` finds existing images via Brave, agent downloads and sends them
- Blob storage: content-addressed in `/workspace/.aegis/blobs/`

### LLM integration

- OpenAI (GPT-5.2 default) + Anthropic Claude, configurable model via `agent.json` or env var
- Streaming with tool calling
- Image support (ingress + egress)
- Configurable max tokens (default 4096)
- GPT-5+ uses `max_completion_tokens` (auto-detected)
- Dynamic env var summary injected into system prompt (available API keys)

### Session management

- JSONL persistence in `/workspace/sessions/`
- Configurable context window (default 50 turns / 24K chars)
- Tool chain preservation (never breaks assistant→tool→result chains)
- Tail trimming: orphaned tool calls at end of history silently dropped (crash recovery)
- Sessions persist across VM restart (disable→start preserves workspace)
- Post-restart notification: agent sends "Restart complete" to the session that triggered restart

### OCI image ENV propagation

- OCI image ENV directives (PATH, GOPATH, etc.) extracted during image pull
- Stored as `.image-env.json` metadata alongside cached rootfs (not inside rootfs)
- Merged into instance env on boot via the run RPC (host-side, no guest hacks)
- PATH is prepended; explicit env wins over image defaults

---

## 2. What's Done

| What | Status |
|------|--------|
| `agent.json` config + env var overrides | **Done** |
| Configurable model/max_tokens for Claude + OpenAI (including GPT-5.2) | **Done** |
| `self_restart` + `self_info` (built-in, clean shutdown) | **Done** |
| `edit_file` (text match + line range modes) | **Done** |
| `read_file` partial reads (start_line/end_line) | **Done** |
| `glob` with `**` recursive pattern support | **Done** |
| `grep` with regex + include filter | **Done** |
| `web_fetch` with HTML stripping | **Done** |
| `web_search` (Brave Search API) | **Done** |
| `image_search` (Brave Image Search API) | **Done** |
| `image_generate` (OpenAI Images API, b64_json + URL) | **Done** |
| `respond_with_image` (blob store + tether egress) | **Done** |
| Memory (store/search/delete + auto-injection + secret rejection) | **Done** |
| Cron (agent tools + gateway scheduler + dedupe + on_conflict) | **Done** |
| `disabled_tools` config for disabling/replacing built-in tools | **Done** |
| MCP protocol handshake (protocolVersion + clientInfo) | **Done** |
| MCP stdout banner tolerance (skip non-JSON lines) | **Done** |
| OCI image ENV propagation (host-side, via run RPC) | **Done** |
| Auto-workspace (`~/.aegis/data/workspaces/{handle}/`) | **Done** |
| Post-restart notification (marker file + tether send) | **Done** |
| Gateway: unsolicited message delivery (restart notifications, future agent-push) | **Done** |
| Gateway: unconditional egress subscription (serves all channels) | **Done** |

---

## 3. What's Next

### Tier 1 — High impact

| Feature | Why it matters | Effort |
|---------|---------------|--------|
| **Context compaction** | Old turns drop silently. Compaction summarizes via LLM call. | Medium |
| **Token/cost tracking** | Count tokens per session, log to JSONL. Visibility, no enforcement. | Small |
| **Agent-initiated messages** | `send_message(channel, chat_id, text)` for cross-channel push (cron→Telegram). | Medium |

### Tier 2 — Medium impact

| Feature | Why it matters | Effort |
|---------|---------------|--------|
| **Skills** (markdown injection) | Scan `/workspace/.aegis/skills/*.md`, inject into system prompt. | Medium |
| **Memory scope filtering** | Filter injection by scope (user/workspace/session). | Small |
| **Workspace persistence on delete** | Keep workspace on `instance delete`, add `--purge` flag. | Small |

---

## 4. Tool Limits & Defaults

| Limit | Default | Applies to |
|-------|---------|-----------|
| Tool output max | 10 KB text | bash, grep, web_fetch |
| File read max | 50 KB | read_file (whole file mode) |
| Glob results max | 200 files | glob |
| Grep matches max | 50 matches | grep |
| Bash timeout | 60 seconds | bash |
| Web fetch timeout | 30 seconds | web_fetch |
| Web fetch body max | 1 MB | web_fetch |
| Web/image search timeout | 15 seconds | web_search, image_search |
| Image generate timeout | 60 seconds | image_generate |
| Memory injection max | 2 KB / 10 entries | memory auto-inject |
| Memory total max | 500 entries | memory store |
| Cron max entries | 20 | cron_create |

---

## 5. Capability Summary

| Capability | Type | Status |
|-----------|------|--------|
| File ops (bash, read/write/edit, glob, grep) | Built-in | **Done** |
| Web access (fetch, search, image search) | Built-in | **Done** |
| Image generation (DALL-E / gpt-image-1) | Built-in | **Done** |
| Image send (respond_with_image) | Built-in | **Done** |
| Memory (store/search/delete + auto-inject) | Built-in | **Done** |
| Cron (scheduled tasks via gateway) | Built-in + Gateway | **Done** |
| Self-management (restart, info) | Built-in | **Done** |
| Tool disable/replace | Config (`disabled_tools`) | **Done** |
| VM orchestration | MCP (`aegis-mcp-guest`) | **Done** |
| Browser automation | MCP (Playwright/Chrome DevTools) | User adds |
| Context compaction | Built-in | Planned |
| Token tracking | Built-in | Planned |
| Agent-initiated messages | Built-in | Planned |
| GitHub, Slack, Jira, DB, etc. | MCP | User brings |
