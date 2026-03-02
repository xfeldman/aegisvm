# Host LLM Proxy
## Local Model Support via Existing Delivery Channel

**Status:** Draft
**Scope:** Enable in-VM agents to use LLM inference servers running on the host (Ollama, LM Studio, vLLM, etc.) without exposing host networking to the guest.
**Depends on:** [AEGIS_AGENT_KIT_v0_1.md](AEGIS_AGENT_KIT_v0_1.md), [KIT_BOUNDARY_SPEC.md](KIT_BOUNDARY_SPEC.md)

---

## 1. Problem

The agent runtime inside the VM calls LLM APIs over the internet (OpenAI, Anthropic). Users who run local inference servers (Ollama, LM Studio, vLLM, llama.cpp) on the host cannot use them — the VM's `localhost` is the VM itself, not the host.

Exposing the host's network to the guest (e.g., via `host.internal` DNS) breaks the isolation boundary and creates an attack surface: any guest process could probe the host's ports, reach databases, dev servers, or other services never intended to be guest-accessible.

## 2. Design

Route LLM traffic through the existing vsock delivery infrastructure — the same path that tether frames use. The guest never learns the host's IP. No new streaming plumbing; we reuse the notification delivery mechanism that already carries tether frames from aegisd through the harness to the agent.

Each layer reads one token from the model string and forwards the rest.

### 2.1 Model String Format

```
host:<provider>/<model>
```

Examples:

```
host:ollama/llama3.2
host:ollama/qwen2.5:14b
host:lmstudio/mistral-7b-instruct
host:vllm/meta-llama/Llama-3.1-8B
```

Parsing — left to right, each layer consumes its prefix:

| Token | Consumed by | Meaning |
|-------|-------------|---------|
| `host:` | Agent | Route through harness to aegisd, not direct egress |
| `ollama/` | aegisd | Resolve provider → `localhost:11434` |
| `llama3.2` | Provider | Resolve model |

Non-`host:` models (`openai/gpt-4.1`, `anthropic/claude-sonnet-4-6`) continue to use direct egress. No change to existing behavior.

### 2.2 Delivery Mechanism

The host-to-guest delivery path already exists:

```
aegisd: demux.SendNotification("tether.frame", frame)
  → vsock
  → harness recv loop: case "tether.frame" → tetherBuffer.enqueue()
  → HTTP POST to agent:7778/v1/tether/recv
```

LLM response streaming uses the same path with a different notification name (`llm.frame` instead of `tether.frame`) to skip tether-specific behavior (ack, inbox persistence).

### 2.3 Request Flow

**Request body schema:**

```json
{
  "provider": "ollama",
  "model": "llama3.2",
  "body": {
    "model": "llama3.2",
    "messages": [{"role": "user", "content": "hello"}],
    "tools": [{"type": "function", "function": {"name": "bash", ...}}],
    "max_tokens": 4096,
    "stream": true
  }
}
```

The agent builds `provider` and `model` from the parsed model string. `body` is a complete OpenAI-compatible chat completion request. aegisd reads `provider` to resolve the endpoint URL, then POSTs `body` as-is to the provider. aegisd does not inspect or modify `body`.

**Request direction** — agent initiates via the guest API (existing `hrpc.Call()` pattern):

```
Agent                          Harness                      aegisd                   Ollama
  │                               │                            │                        │
  │ POST /v1/llm/chat             │                            │                        │
  │ {provider, model, body}       │                            │                        │
  │ ─────────────────────────────>│                            │                        │
  │                               │ Call("llm.chat", params)   │                        │
  │                               │ ───────────────────────── >│                        │
  │                               │                            │ (validates provider,   │
  │                               │       {req_id: "llm-1"}   │  spawns proxy goroutine)│
  │       ← 200 {req_id} ←────── │ <───────────────────────── │                        │
  │                               │                            │ POST /v1/chat/...     │
  │                               │                            │ ──────────────────────>│
  │                               │                            │                        │
```

aegisd validates the provider and returns `{req_id}` immediately — before connecting to Ollama. The actual HTTP request to the provider happens in a background goroutine. If the provider is unreachable or the model doesn't exist, the error arrives as an `llm.error` frame, not an RPC error. The only RPC error is an unknown provider name.

This avoids the harness `hrpc.Call()` 30-second timeout problem. Ollama cold-starting a model (loading weights from disk) can take 30+ seconds. By returning `{req_id}` immediately, the timeout is irrelevant.

**Goroutine lifecycle:** aegisd tracks in-flight LLM proxy goroutines per instance (same pattern as exec goroutines). Each goroutine runs under a context derived from the instance's lifecycle context. On instance stop or vsock channel close, all in-flight goroutines are cancelled. The goroutine deregisters itself on exit (success, error, or cancel).

**Streaming direction** — aegisd sends chunks via the existing notification delivery path:

```
  │                               │                            │  SSE delta             │
  │                               │  notif: "llm.frame"        │ <────────────────────── │
  │  POST /v1/tether/recv         │  {type:llm.delta, req_id,  │                        │
  │  (existing delivery to 7778!) │   data: "..."}             │                        │
  │ <──────────────────────────── │ <────────────────────────── │                        │
  │                               │                            │                        │
  │                               │                            │  SSE delta             │
  │  POST /v1/tether/recv         │  notif: "llm.frame"        │ <────────────────────── │
  │ <──────────────────────────── │ <────────────────────────── │                        │
  │                               │                            │                        │
  │                               │                            │  SSE [DONE]            │
  │  POST /v1/tether/recv         │  notif: "llm.frame"        │ <────────────────────── │
  │  {type: llm.done, req_id,     │ <────────────────────────── │                        │
  │   tool_calls: [...]}          │                            │                        │
  │ <──────────────────────────── │                            │                        │
```

### 2.4 Layer Responsibilities

**Agent (`cmd/aegis-agent`)**

- Parses `host:` prefix from model string, creates `HostLLM` provider
- `HostLLM.StreamChat()`:
  1. Generates a `req_id`
  2. Registers a channel in a pending-request map
  3. POSTs to harness guest API at `http://127.0.0.1:7777/v1/llm/chat`
  4. Blocks reading from the channel, calling `onDelta()` for each `llm.delta` frame
  5. Returns tool calls (if any) from the `llm.done` frame
- The agent's existing tether recv handler at `:7778` routes `llm.*` frame types to the pending-request channel:

```go
func (a *Agent) handleTetherRecv(w http.ResponseWriter, r *http.Request) {
    // ... parse frame
    if strings.HasPrefix(frame.Type, "llm.") {
        a.routeLLMFrame(frame)  // sends to HostLLM's waiting channel
        return
    }
    // ... existing user.message handling
}
```

**Harness (`internal/harness`)**

Dumb pipe. Two additions:

1. **Request direction** — new guest API endpoint:

```go
// guestapi.go
mux.HandleFunc("POST /v1/llm/chat", func(w http.ResponseWriter, r *http.Request) {
    var body json.RawMessage
    json.NewDecoder(r.Body).Decode(&body)
    result, err := hrpc.Call("llm.chat", body)
    // ... write result or error to w
})
```

2. **Streaming direction** — new notification case in recv loop:

```go
// rpc.go, in handleConnection notification switch
case "llm.frame":
    tetherBuffer.enqueue(msg.Params)  // reuse existing delivery
```

That's it. The harness reuses `tetherBuffer.enqueue()` → `sendToAgent()` which POSTs to the agent at `:7778`. No SSE reconstruction, no correlation registry, no new streaming plumbing.

**aegisd**

- Handles `llm.chat` as a guest RPC request (via existing `onGuestRequest` handler)
- Parses `provider`, resolves to localhost endpoint from static map
- Validates provider exists, generates `req_id`, returns `{req_id}` immediately
- Spawns a goroutine that opens the streaming HTTP request to the provider and reads SSE
- The goroutine has access to `inst.demuxer` (via the manager/instance reference in the handler closure) and sends each chunk as `demux.SendNotification("llm.frame", frame)`:

```go
// For each SSE line from Ollama:
demux.SendNotification("llm.frame", map[string]interface{}{
    "type":   "llm.delta",
    "req_id": reqID,
    "data":   sseChunk,  // raw OpenAI SSE data line
})
```

- On stream end, sends:

```go
demux.SendNotification("llm.frame", map[string]interface{}{
    "type":   "llm.done",
    "req_id": reqID,
})
```

- On error:

```go
demux.SendNotification("llm.frame", map[string]interface{}{
    "type":   "llm.error",
    "req_id": reqID,
    "error":  err.Error(),
})
```

### 2.5 Provider Resolution (aegisd)

Static map, no configuration required:

| Provider | Endpoint |
|----------|----------|
| `ollama` | `http://localhost:11434/v1/chat/completions` |
| `lmstudio` | `http://localhost:1234/v1/chat/completions` |
| `vllm` | `http://localhost:8000/v1/chat/completions` |

All three expose OpenAI-compatible APIs. aegisd does not need provider-specific logic — it forwards the OpenAI-format body as-is and streams back the SSE response as-is.

Unknown providers return an RPC error: `"unknown host LLM provider: <name>"`.

### 2.6 LLM Frame Types

All frames carry `req_id` for correlation. The agent routes frames to the correct waiting `HostLLM.StreamChat()` call by `req_id`.

| Frame type | Direction | Payload | Meaning |
|------------|-----------|---------|---------|
| `llm.delta` | aegisd → agent | `{req_id, data}` | One SSE data line from the provider |
| `llm.done` | aegisd → agent | `{req_id}` | Stream complete, provider returned |
| `llm.error` | aegisd → agent | `{req_id, error}` | Provider error or connection failure |

### 2.7 Request Lifecycle & Cleanup

The agent maintains a `pendingLLM` map of `req_id` → buffered channel. Entries are created when `StreamChat()` starts and removed with `defer delete(pendingLLM, reqID)`.

Terminal events:
- `llm.done` received → return response
- `llm.error` received → return error
- Context cancelled (caller's context) → return error
- Orphaned frame (no matching `req_id`) → log warning, discard

On instance shutdown (SIGTERM), the vsock channel closes. aegisd detects EOF and aborts any in-flight proxy goroutines. No explicit cancel RPC needed.

### 2.8 Backpressure

`demux.SendNotification()` writes synchronously under a mutex. If the vsock buffer fills, the write blocks, which slows aegisd's read from Ollama, which slows generation. This is correct backpressure. The existing log streaming path handles similar throughput (~100 notifications/sec) without issues.

### 2.9 Why `llm.frame` Instead of `tether.frame`

Both use the same vsock notification → harness → agent:7778 delivery path. But they are separate notification names because `tether.frame` handling in the harness does extra work we don't want for LLM:

| Behavior | `tether.frame` | `llm.frame` |
|----------|----------------|-------------|
| Emit `event.ack` | Yes | No |
| Persist to `inbox.ndjson` | Yes | No |
| Forward to agent:7778 | Yes | Yes |

The harness `llm.frame` handler is one line: `tetherBuffer.enqueue(msg.Params)`.

---

## 3. Agent Configuration

### 3.1 agent.json

```json
{
  "model": "host:ollama/llama3.2",
  "max_tokens": 4096
}
```

No `api_key_env`, no `base_url`. The `host:` prefix is the entire configuration.

### 3.2 Environment Override

```
AEGIS_MODEL=host:ollama/llama3.2
```

Same format. Overrides `agent.json`.

### 3.3 Switching Models

Switching between local and cloud is a one-line config change:

```json
{"model": "host:ollama/llama3.2"}
```
```json
{"model": "openai/gpt-4.1", "api_key_env": "OPENAI_API_KEY"}
```

---

## 4. Tool Calling

Local models have varying levels of tool calling support.

### 4.1 Models with tool calling

Ollama supports the OpenAI tool calling format for compatible models (Llama 3.1+, Qwen 2.5, Mistral, Command R+). The agent receives raw SSE data lines in `llm.delta` frames. Each frame's `data` field is one SSE `data:` line from the provider — same JSON format as OpenAI streaming chunks:

```json
{"choices":[{"delta":{"content":"Hello"}}]}
{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"..."}}]}}]}
```

`HostLLM` parses each chunk inline: extract `delta.content` → call `onDelta()`, accumulate `delta.tool_calls` → return from `StreamChat()`. This is the same logic as `parseOpenAIStream()` but driven by channel reads instead of `io.Reader` line scanning — simpler, not a direct reuse.

### 4.2 Models without tool calling

If a model does not support tools, Ollama will either ignore the `tools` parameter or return an error. The agent should:

1. Send tools in the first request
2. If the response contains no tool calls, the agent works in **chat-only mode** — responds with text, no tool use
3. If the provider returns an error mentioning tools, retry without the `tools` parameter and log a warning once: `"model <name> does not support tool calling, falling back to chat-only mode"`
4. Cache the fallback decision for the instance lifetime — don't retry tools on every call

### 4.3 Error semantics

- Provider returns HTTP error before streaming → RPC error, `StreamChat()` returns error immediately
- Provider stream fails mid-way (TCP close, 500, malformed SSE) → aegisd sends `llm.error`. The agent has already been calling `onDelta()` for received chunks, so the user may have seen partial output. This is the same behavior as a cloud API dropping mid-stream — the response just stops. Not unique to this feature.

---

## 5. Security

### 5.1 What this does NOT expose

- The host's network. The guest still cannot reach `localhost`, `192.168.*`, or any host service.
- Arbitrary host ports. Only the providers in aegisd's static map are reachable.
- Ollama's full API surface. Only `/v1/chat/completions` is proxied. Model management endpoints (`/api/pull`, `/api/delete`) are not reachable.

### 5.2 Attack surface

The only new attack surface is: a guest process can send OpenAI-compatible chat requests through the vsock channel, which aegisd proxies to a local inference server. This is:

- **Controlled**: Only known providers at known ports
- **Auditable**: aegisd can log all proxied requests
- **Bounded**: Only the chat completions endpoint is proxied
- **Opt-in**: Only instances with `host:` model strings use this path

### 5.3 Potential mitigations (future, not v1)

- Rate limiting on `llm.chat` RPC
- Per-instance allowlist of permitted `host:` providers
- Request size limits
- Capability token validation (reuse existing guest API auth)

---

## 6. Changes Required

| Component | File(s) | Change | ~Lines |
|-----------|---------|--------|--------|
| **Agent** | `cmd/aegis-agent/main.go` | Parse `host:` prefix, create `HostLLM` provider | ~10 |
| **Agent** | `cmd/aegis-agent/llm_host.go` (new) | `HostLLM` struct: send request via harness, wait for `llm.*` frames on channel, call `onDelta`, parse tool calls | ~40 |
| **Agent** | `cmd/aegis-agent/tether.go` | Route `llm.*` frame types to `HostLLM` pending channel | ~10 |
| **Harness** | `internal/harness/guestapi.go` | `POST /v1/llm/chat` → `hrpc.Call("llm.chat", body)` | ~10 |
| **Harness** | `internal/harness/rpc.go` | `case "llm.frame": tetherBuffer.enqueue(msg.Params)` | ~3 |
| **aegisd** | `internal/lifecycle/manager.go` | Handle `llm.chat` guest request: proxy to provider, stream `llm.frame` notifications | ~50 |
| **Kit manifest** | `kits/agent.json` | Update usage text to document `host:` model format | ~5 |
| | | **Total** | **~130** |

### Why the harness barely changes

The harness is a dumb pipe. For the streaming direction, `llm.frame` notifications reuse the existing `tetherBuffer.enqueue()` → `sendToAgent()` path that already delivers `tether.frame` notifications to the agent at `:7778`. No new streaming infrastructure, no SSE reconstruction, no notification correlation. One `case` statement.

---

## 7. Non-goals

- **Running models inside the VM.** Possible but impractical — RAM constraints, no GPU passthrough. Out of scope.
- **Model management.** Pulling, deleting, or listing models on the host. Use `ollama pull` directly.
- **Provider auto-discovery.** No probing host ports. If Ollama isn't running, the error says so.
- **Custom provider URLs.** The static map covers the three major local servers. If someone runs Ollama on port 9999, they can configure it themselves (future: `aegis config set llm.ollama.port 9999`).
- **Non-OpenAI-compatible protocols.** All supported providers speak the OpenAI chat completions API. No protocol translation.
- **New streaming infrastructure.** The entire point of this design is to avoid building a new streaming channel. We reuse the existing notification delivery path.

---

## 8. Future Extensions

- **Cancellation** — `llm.cancel` RPC to abort in-flight generation. Requires adding user-interrupt support to the agent's agentic loop first. On SIGTERM, vsock close already handles cleanup.
- **`host:` provider config in aegisd** — custom ports, auth tokens for providers that need them
- **Model listing** — `aegis llm list` that queries the host provider for available models
- **Multiple simultaneous providers** — agent config supports fallback chains
- **Capability-gated access** — instance capabilities control which `host:` providers are allowed
- **Usage metrics** — aegisd logs token counts, latency, model per request for observability
