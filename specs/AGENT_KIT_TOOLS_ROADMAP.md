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
| **MCP tools** | External servers, configured in `agent.json` | User manages | VM orchestration, browser, memory, GitHub, Slack |

### Agent config: `/workspace/.aegis/agent.json`

One file, one place. The agent reads it at startup.

```json
{
  "mcp": {
    "aegis": {"command": "aegis-mcp-guest"},
    "browser": {"command": "npx", "args": ["@anthropic-ai/chrome-devtools-mcp@latest"]},
    "my-github": {"command": "gh-mcp-server"}
  }
}
```

All MCP servers are visible and manageable. The user can see what's configured, swap implementations (Chrome DevTools vs Playwright), add their own, or remove what they don't need. No hidden magic.

If `agent.json` doesn't exist, the agent runs with built-in tools only + `aegis-mcp-guest` (auto-discovered from rootfs). Zero config for the common case.

### Built-in tools (always on, no config)

Compiled into the agent binary. Fast (no IPC), simple (no server process), reliable (no startup deps). Every agent needs these. Should work in an empty VM with no setup.

- `bash`, `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `web_fetch`

### MCP tools (visible, manageable)

All MCP servers live in `agent.json` under `mcp`. The user sees and controls all of them.

**Pre-bundled** (shipped with the kit, in default `agent.json`):

| Server | What it provides | Swappable? |
|--------|-----------------|-----------|
| `aegis-mcp-guest` | VM orchestration (spawn, list, stop, expose ports) | No — core infrastructure |
| Browser MCP | Web browsing, screenshots, DOM inspection | Yes — swap Chrome DevTools ↔ Playwright, or remove |
| Memory MCP | Semantic search over workspace (future) | Yes — remove if not needed |

**User-added** (user configures for their workflows):

| Server | Examples |
|--------|---------|
| GitHub MCP | PR review, issue management |
| Slack MCP | Channel messaging |
| Database MCP | Query Postgres, MySQL |
| Custom | Anything the user builds or installs |

All MCP tools appear in one flat list to the LLM alongside built-ins. No distinction at runtime.

### Self-management: agent installs its own MCP servers

The agent can modify its own configuration at runtime:

1. Agent uses `bash` to install an MCP package: `npm install -g @anthropic-ai/chrome-devtools-mcp`
2. Agent uses `edit_file` to add the entry to `/workspace/.aegis/agent.json`
3. Agent calls `self_restart` (via `aegis-mcp-guest`) to restart itself
4. Harness restarts the main process → new agent loads updated `agent.json` → new MCP tools available
5. Session JSONL is intact — conversation resumes with new tools

**Prerequisite:** Add `self_restart` to `aegis-mcp-guest`:

```
self_restart — Restart the main process. Workspace and session state are preserved.
               Use after modifying /workspace/.aegis/agent.json to load new MCP servers
               or apply configuration changes.
```

The harness already restarts the main process on exit. `self_restart` just triggers a clean exit via RPC. No new agent code needed — it's a harness/infrastructure concern, implemented in `aegis-mcp-guest` alongside the existing `self_info`, `keepalive_acquire/release` lifecycle tools.

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

| Feature | Why it matters |
|---------|---------------|
| **edit_file** | Agents constantly need to modify existing files without rewriting them entirely. Current workaround (read + write entire file) wastes context and is error-prone for large files. |
| **read_file partials** | Current `read_file` returns the whole file (truncated at 50KB). For large files the agent needs to read specific line ranges — especially before editing. |
| **glob** | Find files by pattern. `list_files` only shows one directory. Agents need to discover project structure. |
| **grep** | Search file contents. Agents need to find code, understand structure, locate definitions. |
| **web_fetch** | Download URLs and extract text. `bash` + `curl` works but output is raw HTML, wastes context. |

### Tier 2 — High impact, medium effort

| Feature | Why it matters |
|---------|---------------|
| **Context compaction** | When the 24K window fills, old turns are silently dropped. The agent loses important early context. Compaction summarizes old turns, preserving key information. |
| **Token/cost tracking** | Minimal version: count request/response tokens per session, log to session file. No budget enforcement yet — just visibility. |
| **Configuration** | Hardcoded model, max tokens, context limits. Should be configurable via `agent.json` and env vars. |

### Tier 3 — Medium impact, larger effort

| Feature | Why it matters |
|---------|---------------|
| **Memory / workspace search** | Semantic search over workspace files. Optional tool flag. |
| **Browser control** | Optional tool flag. Delivered via Chrome DevTools MCP or Playwright MCP. |

---

## 4. Implementation Plan

### Phase 1: Core tools

Add to `cmd/aegis-agent/tools.go`. All results are structured JSON (not raw text) so the LLM can reason without re-parsing.

**`edit_file`** — Apply a targeted edit to an existing file.
```json
{
  "name": "edit_file",
  "parameters": {
    "path":       "string  — file path (under /workspace/)",
    "old_text":   "string  — exact text to find and replace (optional if using line range)",
    "new_text":   "string  — replacement text",
    "start_line": "integer — first line of range to replace (optional, 1-indexed)",
    "end_line":   "integer — last line of range to replace (optional, inclusive)",
    "occurrence": "integer — which occurrence to replace when old_text is not unique (optional, default 1)"
  }
}
```

Two modes:
- **Text match** (`old_text` + `new_text`): find exact text, replace. If not unique and no `occurrence` given, return error listing all occurrences with line numbers so the agent can retry with `occurrence` or `start_line`/`end_line`.
- **Line range** (`start_line` + `end_line` + `new_text`): replace lines in range. Safe for repeated patterns.

Returns: `{"ok": true, "path": "...", "lines_changed": [12, 13, 14], "diff": "...unified diff snippet..."}`.

Diff output in the response lets the agent verify its own edit.

**`read_file`** — Enhanced with line range support.
```json
{
  "name": "read_file",
  "parameters": {
    "path":       "string  — file path",
    "start_line": "integer — first line to read (optional, 1-indexed)",
    "end_line":   "integer — last line to read (optional, inclusive)"
  }
}
```

When range is specified, returns only those lines (with line numbers). Without range, returns whole file (existing behavior, truncated at 50KB). Returns: `{"path": "...", "total_lines": 150, "content": "...", "truncated": false}`.

**`glob`** — Find files matching a pattern.
```json
{
  "name": "glob",
  "parameters": {
    "pattern": "string — glob pattern (e.g. '**/*.go', 'src/**/*.ts')",
    "path":    "string — base directory (optional, defaults to /workspace/)"
  }
}
```

Returns: `{"pattern": "...", "count": 12, "files": ["src/main.go", "src/lib.go", ...]}`. Sorted by path. Capped at 200 results.

**`grep`** — Search file contents.
```json
{
  "name": "grep",
  "parameters": {
    "pattern": "string — search pattern (literal or regex)",
    "path":    "string — file or directory to search (defaults to /workspace/)",
    "include": "string — file glob filter (optional, e.g. '*.go')"
  }
}
```

Returns structured results:
```json
{
  "pattern": "...",
  "count": 5,
  "matches": [
    {"file": "src/main.go", "line": 42, "text": "func main() {"},
    {"file": "src/lib.go", "line": 15, "text": "func helper() {"}
  ],
  "truncated": false
}
```

Capped at 50 matches. Each match includes file, line number, matched line text.

### Phase 2: Web access

**`web_fetch`** — Fetch a URL and return text content.
```json
{
  "name": "web_fetch",
  "parameters": {
    "url": "string — URL to fetch"
  }
}
```

Returns:
```json
{
  "url": "...",
  "status": 200,
  "content_type": "text/html",
  "text": "...clean extracted text...",
  "raw": "...first 2KB of raw response for debugging...",
  "truncated": false
}
```

- HTTP GET with 30s timeout, follow redirects, 1MB max response body
- HTML: strip `<script>`, `<style>`, `<nav>`, `<footer>`, extract readable text. Return both `text` (clean) and `raw` (truncated) so the agent can debug extraction issues without another round trip.
- JSON/plain text: return as-is in `text`, `raw` omitted
- Text output capped at 10KB
- Network policy: unrestricted egress (VM is already isolated)

### Phase 3: Context compaction

Modify `cmd/aegis-agent/session.go`. Key design: **raw JSONL is append-only, never rewritten. Compaction artifacts are separate entries.**

When `assembleContext` drops turns due to window limits:
1. Collect the dropped turns
2. If total dropped > 5 turns and no cached summary covers them: generate summary via LLM call
3. Write summary to session JSONL as a special entry: `{"role": "compaction", "content": "...", "covers_through_ts": "...", "source_hash": "..."}`
4. Inject into assembled context as a system message after the prompt: `"[Context summary from earlier in this conversation: ...]"`
5. On subsequent calls, reuse cached compaction entry if it covers the same dropped range (compare `covers_through_ts`)

The `source_hash` field hashes the covered turns — if turns change (shouldn't happen in append-only), the compaction is invalidated. This prevents the agent from gaslighting itself with stale summaries.

Compaction LLM call uses a small, cheap prompt: "Summarize the key points from this conversation so far in 2-3 paragraphs. Focus on decisions made, files modified, and current task state."

### Phase 3b: Token tracking (minimal)

Add to LLM response handling:
- Count input/output tokens from API response headers (both Anthropic and OpenAI return usage)
- Log per-turn: `{"role": "usage", "input_tokens": 1200, "output_tokens": 450, "model": "...", "ts": "..."}`
- Append to session JSONL (same file, special role)
- No budget enforcement — just visibility. Per-session totals available via `self_info` or a future dashboard.

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
  "mcp": {
    "aegis": {"command": "aegis-mcp-guest"},
    "browser": {"command": "npx", "args": ["@anthropic-ai/chrome-devtools-mcp@latest"]},
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

## 5. Tool Limits & Defaults

Consistent limits across all tools. Configured once, not per-tool.

| Limit | Default | Applies to |
|-------|---------|-----------|
| Tool output max | 10 KB text | bash, grep, web_fetch |
| File read max | 50 KB | read_file (whole file mode) |
| Glob results max | 200 files | glob |
| Grep matches max | 50 matches | grep |
| Bash timeout | 60 seconds | bash |
| Web fetch timeout | 30 seconds | web_fetch |
| Web fetch body max | 1 MB | web_fetch |

---

## 6. Capability Summary

| Capability | Type | Status | Config |
|-----------|------|--------|--------|
| bash, read/write file | Built-in | Done | Always on |
| edit_file (with ranges), read_file partials | Built-in | Phase 1 | Always on |
| glob, grep (structured results) | Built-in | Phase 1 | Always on |
| web_fetch | Built-in | Phase 2 | Always on |
| Context compaction | Built-in | Phase 3 | Always on |
| Token tracking | Built-in | Phase 3b | Always on |
| Image support | Built-in | Done | Always on |
| VM orchestration | MCP (`aegis-mcp-guest`) | Done | Pre-bundled in `agent.json` |
| Browser | MCP (Chrome DevTools / Playwright) | Future | User adds/removes in `agent.json` |
| Semantic memory | MCP (`aegis-mcp-memory`) | Future | User adds/removes in `agent.json` |
| GitHub, Slack, Jira, DB, etc. | MCP | User brings | User adds in `agent.json` |

---

## 7. Verification

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
