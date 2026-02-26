# Agent Kit Tools Roadmap

**Status:** Active
**Updated:** 2026-02-26

---

## 1. Current State

### Built-in tools (11)

| Tool | What it does | Status |
|------|-------------|--------|
| `bash` | Execute shell commands (60s timeout, 10KB output) | Done |
| `read_file` | Read file contents, supports line ranges | Done |
| `write_file` | Create/overwrite files | Done |
| `edit_file` | Targeted edits via text match or line range, returns diff | Done |
| `list_files` | List directory contents | Done |
| `glob` | Find files by pattern (`**/*.go`), cap 200 results | Done |
| `grep` | Regex search file contents, cap 50 matches | Done |
| `web_fetch` | Fetch URL, strip HTML, extract text (10KB cap) | Done |
| `memory_store` | Store a persistent memory (with secret rejection) | Done |
| `memory_search` | Search memories by keyword/tag | Done |
| `memory_delete` | Delete a memory by ID | Done |

### MCP tools via aegis-mcp-guest (9)

| Tool | What it does | Status |
|------|-------------|--------|
| `instance_spawn` | Spawn child VM | Done |
| `instance_list` | List child instances | Done |
| `instance_stop` | Stop a child | Done |
| `self_info` | Get current VM info | Done |
| `self_restart` | Restart agent process (config reload) | Done |
| `expose_port` | Expose guest port on host | Done |
| `unexpose_port` | Remove port exposure | Done |
| `keepalive_acquire` | Prevent VM pause during long work | Done |
| `keepalive_release` | Release keepalive lease | Done |

### Agent configuration

- `/workspace/.aegis/agent.json` — model, max_tokens, context limits, system prompt, MCP servers, memory config
- Env var overrides: `AEGIS_MODEL`, `AEGIS_MAX_TOKENS`, `AEGIS_CONTEXT_CHARS`, `AEGIS_CONTEXT_TURNS`, `AEGIS_SYSTEM_PROMPT`
- MCP handshake with `protocolVersion` + `clientInfo` (Chrome DevTools MCP compatible)
- Self-management: agent can edit `agent.json` and call `self_restart` to load new MCP servers at runtime

### Memory

- JSONL-backed persistent memory (`/workspace/.aegis/memory/memories.jsonl`)
- Automatic context injection — relevant memories injected into system prompt before each LLM call
- Keyword-based relevance scoring with stopword filter and recency bonus
- Three injection modes: `relevant` (default), `recent_only`, `off`
- Secret rejection: API keys, tokens, high-entropy blobs blocked from storage
- Configurable via `agent.json`: `memory.inject_mode`, `memory.max_inject_chars`, `memory.max_inject_count`, `memory.max_total`

### LLM integration

- Claude + OpenAI, configurable model via `agent.json` or env var
- Streaming with tool calling
- Image support (ingress + egress)
- Configurable max tokens (default 4096)

### Session management

- JSONL persistence in `/workspace/sessions/`
- Configurable context window (default 50 turns / 24K chars)
- Tool chain preservation (never breaks assistant→tool→result)
- No compaction — old turns drop off the window silently

---

## 2. What's Done

| Phase | What | Status |
|-------|------|--------|
| Phase 0 | `agent.json` config + env var overrides | **Done** |
| Phase 0 | Configurable model/max_tokens for Claude + OpenAI | **Done** |
| Phase 0 | `self_restart` tool + harness restart endpoint | **Done** |
| Phase 1 | `edit_file` (text match + line range modes) | **Done** |
| Phase 1 | `read_file` partial reads (start_line/end_line) | **Done** |
| Phase 1 | `glob` with `**` recursive pattern support | **Done** |
| Phase 1 | `grep` with regex + include filter | **Done** |
| Phase 2 | `web_fetch` with HTML stripping | **Done** |
| — | Memory (store/search/delete + auto-injection) | **Done** |
| — | MCP protocol handshake fix (protocolVersion + clientInfo) | **Done** |

---

## 3. What's Next

### Tier 1 — High impact, ready to build

| Feature | Why it matters | Effort |
|---------|---------------|--------|
| **Cron / scheduled tasks** | Agent needs to run recurring work: health checks, polling, periodic reports, data collection. Currently requires user to send tether messages manually. | Medium |
| **Context compaction** | When the 24K window fills, old turns drop silently. The agent loses important early context. Compaction summarizes old turns via LLM call, preserving key info. | Medium |
| **Token/cost tracking** | Count request/response tokens per session, log to session file. Visibility into usage — no budget enforcement yet. | Small |

### Tier 2 — Medium impact

| Feature | Why it matters | Effort |
|---------|---------------|--------|
| **Skills** (markdown instructions) | Scan `/workspace/.aegis/skills/*.md`, inject relevant instructions into system prompt. Lets users define task-specific behavior without modifying code. | Medium |
| **Memory scope filtering** | Use the `scope` field on memories to filter injection (e.g., only `"user"` scope in new sessions). | Small |
| **Embedding-based memory search** | Optional enhancement: call embedding API, store vectors alongside text. Better relevance for semantic queries. | Large |

---

## 4. Cron / Scheduled Tasks

### Problem

Agents can only react to tether messages. There's no way to say "check the server health every 5 minutes" or "poll the RSS feed hourly" or "send a daily digest at 9am". The user has to manually send messages to trigger recurring work.

### Design

A built-in cron scheduler in `aegis-agent` that fires synthetic user messages on a schedule. Cron entries are stored in `/workspace/.aegis/cron.json` and managed via tools.

**Cron entry:**
```json
{
  "id": "cron-1",
  "schedule": "*/5 * * * *",
  "message": "Check if the web server at http://localhost:8080 is responding. If not, restart it.",
  "session": "health-check",
  "enabled": true
}
```

Fields:
- `id` — auto-generated
- `schedule` — standard cron expression (minute hour dom month dow)
- `message` — the text injected as a synthetic user message
- `session` — session ID for the cron's conversation (isolates cron work from interactive sessions)
- `enabled` — can be paused without deleting

**Built-in tools:**
- `cron_create` — create a new scheduled task
- `cron_list` — list all cron entries
- `cron_delete` — delete by ID
- `cron_enable` / `cron_disable` — toggle without deleting

**Scheduler:** A goroutine in `aegis-agent` that checks cron entries once per minute, evaluates which ones should fire, and injects a synthetic `user.message` tether frame into the agent's own handler. The cron message goes to a dedicated session (per cron entry), keeping cron work separate from interactive conversations.

**Keepalive:** When cron entries exist and are enabled, the agent should acquire a keepalive lease to prevent the VM from being paused by the idle timer. Release when all cron entries are disabled/deleted.

### Implementation

| File | Changes |
|------|---------|
| `cmd/aegis-agent/cron.go` | New file — CronStore (load/save), CronScheduler (goroutine), cron expression parsing |
| `cmd/aegis-agent/tools.go` | Add cron_create, cron_list, cron_delete, cron_enable, cron_disable tools |
| `cmd/aegis-agent/main.go` | Start cron scheduler, wire keepalive |

### Cron expression parsing

Standard 5-field: `minute hour day-of-month month day-of-week`. Support:
- `*` (any), `*/N` (every N), `N` (exact), `N-M` (range), `N,M` (list)
- No need for seconds or year fields
- Implement in ~100 lines, no external deps

---

## 5. Context Compaction (Phase 3)

When `assembleContext` drops turns due to window limits:
1. Collect the dropped turns
2. If total dropped > 5 turns and no cached summary covers them: generate summary via LLM call
3. Write summary to session JSONL as a special entry: `{"role": "compaction", "content": "...", "covers_through_ts": "..."}`
4. Inject into assembled context as a system message: `"[Context summary from earlier: ...]"`
5. On subsequent calls, reuse cached compaction entry if it covers the same dropped range

---

## 6. Token Tracking (Phase 3b)

- Count input/output tokens from API response headers (both Anthropic and OpenAI return usage)
- Log per-turn: `{"role": "usage", "input_tokens": 1200, "output_tokens": 450, "model": "...", "ts": "..."}`
- Append to session JSONL (same file, special role)
- No budget enforcement — just visibility

---

## 7. Tool Limits & Defaults

| Limit | Default | Applies to |
|-------|---------|-----------|
| Tool output max | 10 KB text | bash, grep, web_fetch |
| File read max | 50 KB | read_file (whole file mode) |
| Glob results max | 200 files | glob |
| Grep matches max | 50 matches | grep |
| Bash timeout | 60 seconds | bash |
| Web fetch timeout | 30 seconds | web_fetch |
| Web fetch body max | 1 MB | web_fetch |
| Memory injection max | 2 KB / 10 entries | memory auto-inject |
| Memory total max | 500 entries | memory store |

---

## 8. Capability Summary

| Capability | Type | Status |
|-----------|------|--------|
| bash, read/write/edit file, glob, grep | Built-in | **Done** |
| web_fetch | Built-in | **Done** |
| Memory (store/search/delete + auto-inject) | Built-in | **Done** |
| Image support | Built-in | **Done** |
| `agent.json` config + env overrides | Agent internals | **Done** |
| VM orchestration + self_restart | MCP (`aegis-mcp-guest`) | **Done** |
| Browser | MCP (Chrome DevTools) | **Tested E2E** |
| Cron / scheduled tasks | Built-in | **Next** |
| Context compaction | Built-in | Planned |
| Token tracking | Built-in | Planned |
| Skills (markdown injection) | Built-in | Future |
| Memory scope filtering | Built-in | Future |
| GitHub, Slack, Jira, DB, etc. | MCP | User brings |
