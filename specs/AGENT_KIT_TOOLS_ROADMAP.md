# Agent Kit Tools Roadmap

**Status:** Draft
**Date:** 2026-02-26
**Context:** After evaluating OpenClaw integration (see `OPENCLAW_TETHER_KIT_SPEC.md` section 16), the decision is to invest in the native agent kit's tool ecosystem. This spec maps what we have, what's missing, and the implementation plan.

---

## 1. Current State

### Built-in tools (4)

| Tool | What it does | Limitations |
|------|-------------|-------------|
| `bash` | Execute shell commands | 60s timeout, 10KB output truncation |
| `read_file` | Read file contents | 50KB truncation, whole-file only |
| `write_file` | Create/overwrite files | Whole-file write only — no partial edits |
| `list_files` | List directory contents | Flat listing, no recursion |

### MCP tools via aegis-mcp-guest (8)

| Tool | What it does |
|------|-------------|
| `instance_spawn` | Spawn child VM |
| `instance_list` | List child instances |
| `instance_stop` | Stop a child |
| `self_info` | Get current VM info |
| `expose_port` | Expose guest port on host |
| `unexpose_port` | Remove port exposure |
| `keepalive_acquire` | Prevent VM pause during long work |
| `keepalive_release` | Release keepalive lease |

### LLM integration

- Claude (claude-sonnet-4) + OpenAI (gpt-4o)
- Streaming with tool calling
- Image support (ingress + egress plumbed)
- 4096 max tokens per response

### Session management

- JSONL persistence in `/workspace/sessions/`
- Context window: 50 turns / 24K chars
- Tool chain preservation (never breaks assistant→tool→result)
- No compaction — old turns drop off the window silently

---

## 2. Tool Architecture: Built-in vs MCP

Two categories of tools, serving different purposes:

### Built-in tools — zero-config, always available

Compiled into the `aegis-agent` binary. Available out of the box with no setup. These are the **core capabilities** every agent needs — file manipulation, code search, web access. They're fast (no IPC overhead), simple (no server process), and reliable (no startup dependencies).

**What belongs here:** Tools that 90%+ of agent tasks need. If an agent can't edit files or search code without extra setup, it's broken.

### MCP tools — pluggable, opt-in

External MCP servers configured in `/workspace/.aegis/agent.json`. Each server is a separate process (stdio JSON-RPC). The agent discovers tools at startup and offers them to the LLM alongside built-ins.

**What belongs here:** Specialized capabilities that not every agent needs. Browser control, database access, image generation, domain-specific APIs. Users plug in what they need, the agent binary stays small.

### The split

Kit and tools are orthogonal. Kit = VM packaging (image, command, daemons). Tools = agent capabilities. Same kit, different tool configs per instance.

| Concept | Where it lives | What controls it | Examples |
|---------|---------------|-----------------|----------|
| **Built-in tools** | Compiled into `aegis-agent` | Always on | bash, read/write/edit file, glob, grep, web_fetch |
| **Optional tools** | In `aegis-agent` binary, toggled by config | Feature flags in `agent.json` | browser, memory |
| **User MCP** | External servers, configured by user | MCP manifest in `agent.json` | Their GitHub, Slack, custom APIs |

### Agent config: `/workspace/.aegis/agent.json`

One file, one place. The agent reads it at startup.

```json
{
  "tools": {
    "browser": true,
    "memory": true
  },
  "mcp": {
    "my-github": {"command": "gh-mcp-server"},
    "my-jira": {"command": "jira-mcp", "args": ["--token", "$JIRA_TOKEN"]}
  }
}
```

**`tools`** — feature flags for optional capabilities shipped with the agent binary. The agent knows how to set them up (spawn the right MCP server, install dependencies if needed). User just flips the switch.

**`mcp`** — manifest of user-provided MCP servers. User brings their own integrations.

If `agent.json` doesn't exist, the agent runs with built-in tools only + `aegis-mcp-guest` (VM orchestration, always auto-discovered). Zero config needed for the common case.

### Built-in tools (always on, no config)

Compiled into the agent binary. Fast (no IPC), simple (no server process), reliable (no startup deps). Every agent needs these.

- `bash`, `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `web_fetch`

### Optional tools (feature flags)

Shipped in the agent binary or as companion binaries, but only activated when the user enables them. The agent handles setup — spawning MCP servers, checking for dependencies.

| Flag | What it does | Delivery | Dependencies |
|------|-------------|----------|-------------|
| `browser` | Web browsing via Chrome DevTools or Playwright | Agent spawns MCP server process | Chromium in VM image |
| `memory` | Semantic search over workspace files | Agent spawns MCP server process | Embedding API (uses same LLM provider) |

When `"browser": true`, the agent:
1. Checks if Chromium is available
2. Starts [Chrome DevTools MCP](https://github.com/ChromeDevTools/chrome-devtools-mcp) or [Playwright MCP](https://github.com/microsoft/playwright-mcp) as a child process
3. Registers its tools alongside built-ins
4. User sees browser tools — never thinks about MCP

Same for memory. The MCP server is an implementation detail. The user sees a feature flag.

### User MCP (user's own integrations)

External MCP servers the user brings. Configured in `agent.json` under `mcp`. These are *their* tools for *their* workflows — not agent infrastructure.

The agent loads them at startup alongside built-ins and optional tools. All tools appear in one flat list to the LLM.

### Skills (future)

Claude Code distinguishes between tools (what the agent *can do*) and skills (how the agent *should think*). Tools have strict schemas. Skills are markdown instructions loaded contextually.

For our agent, this maps to:
- **Tools** → built-in + MCP (what we have)
- **Skills** → system prompt + workspace markdown files injected into context (future enhancement)

The agent already reads `AEGIS_SYSTEM_PROMPT` from env. A future extension could scan `/workspace/.aegis/skills/*.md` and inject relevant skill files into the system prompt based on task context. This is a Tier 4 enhancement — the tool layer comes first.

---

## 3. What's Missing

Prioritized by impact on agent usefulness.

### Tier 1 — High impact, small effort

| Feature | Why it matters | Effort |
|---------|---------------|--------|
| **edit_file** | Agents constantly need to modify existing files without rewriting them entirely. Current workaround (read + write entire file) wastes context and is error-prone for large files. | Small — new tool, ~100 lines |
| **glob** | Find files by pattern. `list_files` only shows one directory. Agents need to discover project structure. | Small — new tool, ~50 lines |
| **grep** | Search file contents. Agents need to find code, understand structure, locate definitions. | Small — new tool, ~50 lines |
| **web_fetch** | Download URLs and extract text. Agents need to read docs, APIs, web pages. No web access currently — `bash` + `curl` works but output is raw HTML, wastes context. | Small — new tool, ~100 lines. Fetch + html-to-text conversion. |

### Tier 2 — High impact, medium effort

| Feature | Why it matters | Effort |
|---------|---------------|--------|
| **Context compaction** | When the 24K window fills, old turns are silently dropped. The agent loses important early context. Compaction summarizes old turns into a condensed form, preserving key information. | Medium — ~200 lines. On window overflow: summarize dropped turns via LLM call, inject summary as first turn. |
| **Configurable model** | Hardcoded to claude-sonnet-4 / gpt-4o. Users should be able to pick the model via env var or config. | Small — ~30 lines. Read `AEGIS_MODEL` env var, parse `provider/model` format. |
| **Configurable max tokens** | Hardcoded to 4096. Some tasks need longer responses. | Tiny — ~5 lines. Read `AEGIS_MAX_TOKENS` env var. |

### Tier 3 — Medium impact, medium effort

| Feature | Why it matters | Effort |
|---------|---------------|--------|
| **Memory / workspace search** | Semantic search over workspace files. Lets the agent find relevant context without reading every file. Useful for large projects. | Medium-large — needs embedding API calls + simple vector store (SQLite with cosine similarity, or just BM25 keyword search). |
| **diff output** | Show what changed after file edits. Useful for the agent to verify its own changes and for the user to review. | Small — generate unified diff in `edit_file` response. |
| **Multi-file glob+read** | Read multiple files matching a pattern in one tool call. Reduces round trips for common "understand the codebase" patterns. | Small — combine glob + read into a single tool. |

### Tier 4 — Nice to have, larger effort

| Feature | Why it matters | Effort |
|---------|---------------|--------|
| **Browser control** | Headless Chrome via CDP. Navigate pages, fill forms, take screenshots. Powerful but heavy (Chromium dependency, ~300MB). | Large — needs Chromium in image, CDP client, screenshot handling. |
| **Image generation** | Call DALL-E or similar to produce images. Wire into `sendDoneWithImages`. | Medium — new tool + API client + blob store write. |
| **Cron / scheduled tasks** | Run tasks on a schedule (health checks, reports, etc.). | Medium — needs a scheduler, persistence, and tether notification on completion. |
| **Cost tracking** | Count tokens, track spend per session. Alert when approaching budget. | Medium — needs token counting per provider, budget config, session-level tracking. |

---

## 4. Implementation Plan

### Phase 1: Core file tools

Add to `cmd/aegis-agent/tools.go`:

**`edit_file`** — Apply a targeted edit to an existing file.
```
Parameters:
  path: string        — file path (under /workspace/)
  old_text: string    — exact text to find (must be unique in file)
  new_text: string    — replacement text
```
Reads file, finds `old_text`, replaces with `new_text`, writes back. Returns confirmation with line numbers. Fails if `old_text` not found or not unique.

**`glob`** — Find files matching a pattern.
```
Parameters:
  pattern: string     — glob pattern (e.g., "**/*.go", "src/**/*.ts")
  path: string        — base directory (optional, defaults to /workspace/)
```
Uses `filepath.Glob` or `doublestar` library for `**` support. Returns list of matching paths.

**`grep`** — Search file contents.
```
Parameters:
  pattern: string     — search pattern (literal string or regex)
  path: string        — file or directory to search (defaults to /workspace/)
  include: string     — file glob filter (optional, e.g., "*.go")
```
Walks files, matches pattern, returns filename:line:content for each match. Output truncated at 10KB.

### Phase 2: Web access

**`web_fetch`** — Fetch a URL and return its text content.
```
Parameters:
  url: string         — URL to fetch
  prompt: string      — optional: what to extract (used for summary)
```
HTTP GET, convert HTML to plain text (strip tags, extract readable content), truncate to 10KB. For non-HTML content (JSON, plain text), return as-is.

Minimal HTML-to-text: strip `<script>`, `<style>`, `<nav>`, `<footer>`, then extract text content from remaining elements. No headless browser needed.

### Phase 3: Context compaction

Modify `cmd/aegis-agent/session.go`:

When `assembleContext` drops turns due to window limits:
1. Collect the dropped turns
2. If total dropped > 5 turns: generate a summary via LLM call
3. Inject summary as a `system` message after the system prompt: `"[Context summary from earlier in this conversation: ...]"`
4. Cache the summary in the session (avoid re-summarizing on every turn)

Compaction LLM call uses a small, cheap prompt: "Summarize the key points from this conversation so far in 2-3 paragraphs. Focus on decisions made, files modified, and current task state."

### Phase 4: Configuration + `agent.json`

Implement `/workspace/.aegis/agent.json` loading and env var overrides in `cmd/aegis-agent/main.go`.

**`agent.json`** (optional — agent works without it):
```json
{
  "model": "anthropic/claude-sonnet-4-20250514",
  "max_tokens": 8192,
  "context_chars": 48000,
  "context_turns": 100,
  "system_prompt": "You are a coding assistant...",
  "tools": {
    "browser": true,
    "memory": true
  },
  "mcp": {
    "my-github": {"command": "gh-mcp-server"}
  }
}
```

**Env vars override `agent.json`** (for quick per-instance tweaks without editing the file):

| Env var | `agent.json` key | Default |
|---------|-----------------|---------|
| `AEGIS_MODEL` | `model` | Auto-detect from API key |
| `AEGIS_MAX_TOKENS` | `max_tokens` | `4096` |
| `AEGIS_CONTEXT_CHARS` | `context_chars` | `24000` |
| `AEGIS_CONTEXT_TURNS` | `context_turns` | `50` |
| `AEGIS_SYSTEM_PROMPT` | `system_prompt` | Built-in default |

Precedence: env var > `agent.json` > default.

---

## 5. Capability Summary

| Capability | Type | Status | Config |
|-----------|------|--------|--------|
| bash, read/write file | Built-in | Done | Always on |
| edit_file, glob, grep | Built-in | Phase 1 | Always on |
| web_fetch | Built-in | Phase 2 | Always on |
| Context compaction | Built-in | Phase 3 | Always on |
| Image support | Built-in | Done | Always on |
| VM orchestration | Auto MCP (`aegis-mcp-guest`) | Done | Always on |
| Browser | Optional tool | Future | `"tools": {"browser": true}` |
| Semantic memory | Optional tool | Future | `"tools": {"memory": true}` |
| GitHub, Slack, Jira, DB, etc. | User MCP | User brings | `"mcp": {"name": {"command": "..."}}` |

---

## 6. Verification

After each phase:

```bash
make all                          # clean build
go test ./cmd/aegis-agent/...     # if tests exist

# Manual test via MCP tether
./bin/aegis instance start --kit agent --name tool-test --secret OPENAI_API_KEY --workspace /tmp/tool-test
# tether_send: "Create a file hello.go, then edit it to add a main function"
# tether_send: "Search the workspace for all .go files"
# tether_send: "Fetch https://example.com and summarize it"
```
