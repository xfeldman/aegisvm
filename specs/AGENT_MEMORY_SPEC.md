# Agent Memory Spec

## Problem

Agents lose knowledge across sessions. A user tells the agent their preferences, project context, or decisions — next session, it's gone. The agent also can't build up understanding of the workspace over time.

## Design Principles

- **Built-in, not MCP.** Memory needs to inject into the context window before the LLM sees messages. An MCP tool can only be called *by* the LLM — it can't modify what the LLM sees before it starts thinking. CRUD tools are built-in for the same reason — no benefit to an external process for a local JSONL file.
- **File-backed, no external deps.** JSONL on the workspace filesystem. Survives restart, pause/resume, snapshot/restore. No vector DB, no embedding API.
- **Two layers.** Explicit tools for the agent to store/search. Automatic injection so relevant memories appear without the agent having to ask.
- **Small and bounded.** Memory injection is capped. The agent's context window is precious — don't fill it with stale memories.
- **Prompt-guided, not keyword-triggered.** The agent decides when to store via system prompt guidance, not by pattern-matching user phrases like "remember". This keeps behavior predictable and avoids false triggers.

## Storage

```
/workspace/.aegis/memory/
  memories.jsonl
```

Each line is a memory entry:

```json
{"id":"m-1","scope":"user","text":"User prefers Python over JavaScript","tags":["preference"],"ts":"2026-02-26T12:00:00Z"}
{"id":"m-2","scope":"workspace","text":"Project uses PostgreSQL 16 on port 5432","tags":["infra"],"ts":"2026-02-26T12:05:00Z"}
{"id":"m-3","scope":"workspace","text":"Deploy target is AWS us-east-1","tags":["infra","deploy"],"ts":"2026-02-26T12:10:00Z"}
```

Fields:
- `id` — auto-generated, `m-<monotonic counter>`
- `text` — the memory content (free-form text, 1-500 chars)
- `scope` — optional, default `"workspace"`. One of `"user"` | `"workspace"` | `"session"`. Reserved for future filtering — stored now, not filtered on yet.
- `tags` — optional classification, agent-chosen (0-5 tags)
- `ts` — creation timestamp

No update-in-place. To correct a memory, delete the old one and store a new one. Append-only simplifies concurrency and makes the file trivially recoverable.

## Built-in Tools

### `memory_store`

Store a new memory.

```json
{
  "text": "string, required — the fact or note to remember (max 500 chars)",
  "tags": "string[], optional — classification tags",
  "scope": "string, optional — 'user' | 'workspace' | 'session' (default 'workspace')"
}
```

Returns: `{"ok": true, "id": "m-4"}`

**Secret rejection:** Before storing, the text is checked against patterns that look like secrets:
- API key prefixes: `sk-`, `ghp_`, `gho_`, `glpat-`, `xoxb-`, `xoxp-`
- Generic: `Bearer `, `token:`, `password:`
- High-entropy base64 blobs (40+ alphanumeric chars with mixed case and digits)

If a match is found, the store is rejected with `{"ok": false, "error": "text appears to contain a secret — not stored"}`.

**Prune on store:** If total memory count exceeds `max_total` (default 500), the oldest entries are pruned to make room before appending.

Implementation: acquire process mutex, append a JSON line to `memories.jsonl`, fsync.

### `memory_search`

Search stored memories by keyword or tag.

```json
{
  "query": "string, optional — keyword search across memory text",
  "tag": "string, optional — filter by tag"
}
```

Returns up to 20 matches, newest first:

```json
{
  "count": 2,
  "memories": [
    {"id": "m-3", "text": "Deploy target is AWS us-east-1", "tags": ["infra","deploy"], "ts": "..."},
    {"id": "m-2", "text": "Project uses PostgreSQL 16 on port 5432", "tags": ["infra"], "ts": "..."}
  ]
}
```

Search is case-insensitive substring match on `text`. If both `query` and `tag` are set, both must match (AND). If neither is set, returns the 20 most recent memories.

### `memory_delete`

Delete a memory by ID.

```json
{
  "id": "string, required — memory ID to delete"
}
```

Returns: `{"ok": true}`

Implementation: acquire mutex, write `memories.jsonl` to a temp file excluding the deleted ID, atomic rename over the original.

## Automatic Context Injection

The key feature that requires built-in implementation. On every LLM call, `assembleContext` injects relevant memories into the prompt.

### How it works

In `session.go`, `assembleContext` gains a memory injection step:

1. Load all memories from `memories.jsonl` (cached in-process, reloaded when file mtime changes)
2. Score relevance against the **last user message** using keyword overlap
3. Select top memories up to a **2KB budget** (measured by text length)
4. If no scored matches, include the 5 most recent memories (recency fallback)
5. Inject as a block after the system prompt, with IDs for easy reference:

```
[Memories]
- (m-1, preference) User prefers Python over JavaScript
- (m-2, infra) Project uses PostgreSQL 16 on port 5432
- (m-3, infra) Deploy target is AWS us-east-1
```

Including IDs lets the agent reference and delete specific memories without searching first.

### Injection modes

Configurable via `agent.json`:

- `"relevant"` (default) — score against last user message, inject top matches
- `"recent_only"` — always inject N most recent, no scoring
- `"off"` — no automatic injection; agent can still use memory_search explicitly

### Relevance scoring

Simple keyword overlap — no embeddings:

```
score(memory, userMessage) =
  count of unique words in memory.text that appear in userMessage (case-insensitive)
  + recency bonus: min(1.0, 0.1 / max(age_in_hours, 0.1))
```

Tokenization: split on non-alphanumeric characters, lowercase, drop words shorter than 3 characters.

Stopword filter: a small hardcoded list of high-frequency low-signal words is excluded from matching:

```
"the", "and", "for", "are", "but", "not", "you", "all", "can", "has", "her",
"was", "one", "our", "out", "its", "use", "how", "may", "who", "did", "get",
"had", "him", "his", "let", "say", "she", "too", "own", "way", "about",
"could", "from", "have", "into", "just", "like", "make", "many", "some",
"than", "that", "them", "then", "this", "very", "when", "what", "with",
"will", "would", "been", "each", "more", "most", "much", "must", "only",
"also", "back", "being", "come", "every", "first", "here", "know", "made",
"need", "over", "such", "take", "where", "which", "while", "work",
"project", "please", "help", "want", "using", "thing", "file", "should"
```

This list is deliberately conservative — it only removes words that are almost never meaningful in a memory relevance context. Better to surface a marginal match than miss a real one.

### Budgeting

- **Max injection size:** 2KB of memory text (roughly 500 tokens)
- **Max memories injected:** 10
- **Max total memories stored:** 500 (oldest auto-pruned on store if exceeded)

These are defaults, overridable in `agent.json`:

```json
{
  "memory": {
    "inject_mode": "relevant",
    "max_inject_chars": 2000,
    "max_inject_count": 10,
    "max_total": 500
  }
}
```

## Concurrency

The agent processes one message at a time per session (synchronous agentic loop), so contention is rare. Still, for correctness:

- **Process-level `sync.Mutex`** on the memory store — all reads and writes acquire it
- **Append:** open with `O_APPEND`, write single line, fsync
- **Delete/prune:** write to temp file, `os.Rename` (atomic on POSIX) over the original

## System Prompt Guidance

The default system prompt gains a memory section:

```
You have persistent memory tools. Use memory_store when:
- The user explicitly asks you to remember something
- You learn a stable fact about the user or project that will be useful across sessions
Do NOT store: transient task context, secrets/tokens, or information already in files.
Use memory_delete to remove outdated memories. Memories are automatically surfaced in your context when relevant.
```

This is the primary mechanism for controlling when the agent stores memories. Prompt guidance, not keyword detection.

## What this does NOT include

- **Embedding-based semantic search.** Keyword matching is the 80/20 solution. If embeddings are needed later, add them as an optional enhancement (call an embedding API, store vectors alongside text).
- **Workspace file indexing.** Memory is for agent-curated facts, not automatic file indexing. The agent has `grep` and `glob` for file search.
- **Cross-instance memory.** Each instance has its own memory in its workspace. Sharing is a future concern.
- **Memory compaction/summarization.** If 500 memories aren't enough, that's a future problem. The agent can delete stale ones.
- **Scope-based filtering.** The `scope` field is stored but not used for filtering or injection yet. Future versions may filter injection by scope (e.g., inject only "user" scope memories in new sessions).

## Implementation

### Files to modify

| File | Changes |
|------|---------|
| `cmd/aegis-agent/memory.go` | New file — MemoryStore struct, load, append, delete, search, score, inject, secret detection |
| `cmd/aegis-agent/tools.go` | Add `memory_store`, `memory_search`, `memory_delete` tools + dispatch |
| `cmd/aegis-agent/session.go` | Call memory injection in `assembleContext` |
| `cmd/aegis-agent/main.go` | Initialize MemoryStore on Agent, update system prompt |
| `cmd/aegis-agent/mcp.go` | Add `MemoryConfig` to `AgentConfig` |
| `cmd/aegis-agent/memory_test.go` | Tests for store, search, scoring, secret rejection, injection |

### Execution order

1. `memory.go` — MemoryStore (load, append, atomic delete, search, score, secret check, inject formatting)
2. Tools in `tools.go` — memory_store (with secret rejection + prune), memory_search, memory_delete
3. Config in `mcp.go` — MemoryConfig in AgentConfig
4. Context injection in `session.go` — call memory inject in assembleContext
5. System prompt update in `main.go`
6. Tests in `memory_test.go`

### Verification

```bash
# Via tether:
# "Remember that I prefer tabs over spaces"
# → agent calls memory_store, returns ok + id

# "Remember that the database is PostgreSQL on port 5432"
# → stored

# New session — ask "What indentation style should I use?"
# → memory about tabs vs spaces is auto-injected, agent answers correctly

# "Search your memory for database"
# → returns the PostgreSQL memory

# "Delete memory m-1"
# → deleted

# "Remember my API key is sk-abc123..."
# → rejected: "text appears to contain a secret"
```
