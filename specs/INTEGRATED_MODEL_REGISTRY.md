# Integrated Local Inference
## Bundled llama-server — Zero-Config Local Models

**Status:** Draft
**Scope:** Bundle llama-server with Aegis to provide zero-config local inference with automatic context window sizing. Replace the Ollama hard dependency with an opinionated built-in default while keeping external providers as escape hatches.

---

## 1. Motivation

The current local model path requires users to install and configure Ollama separately. This creates friction that directly contradicts Aegis's accessibility goal:

- **Silent context truncation**: Ollama defaults to 4096-token context regardless of model capability. With ~29 agent tools consuming ~3500 tokens, conversation history is silently lost. Users must manually configure `num_ctx` — a non-obvious setting.
- **Configuration rejected by API**: Ollama's OpenAI-compatible endpoint refuses `num_ctx` as a request parameter (explicitly rejected upstream). Context window must be configured at the Ollama level.
- **Multi-product setup**: Install Ollama → pull model → configure context → configure Aegis → hope the settings align. Multiple products, multiple config surfaces, multiple failure modes.
- **Non-developer hostile**: The target audience shouldn't need to know what `num_ctx` means.

### What this is NOT about

This is not about performance. It's not about replacing vLLM clusters or competing with GPU inference servers. It's about:

- Eliminating config friction
- Eliminating silent misconfiguration
- Eliminating "why is my agent broken"
- Eliminating external dependency confusion

We're optimizing UX, not FLOPS.

## 2. Licensing

[llama.cpp](https://github.com/ggml-org/llama.cpp) (including llama-server) is MIT licensed. Permits commercial use, bundling, and distribution without restrictions.

**Model licenses vary** (Apache 2, Llama Community License, etc.). Aegis does not redistribute models — users download them. License responsibility stays with the user, same as Ollama.

## 3. Design Principles

### Opinionated default, open system

**Built-in (zero-config default):**
- macOS ARM64 → Metal build bundled
- Linux x86_64/ARM64 → CPU-only build bundled
- Single model loaded at a time
- Auto context sizing from GGUF metadata
- No driver drama, no GPU flags, no VRAM detection, no tuning UI

**Escape hatch (already exists):**
- `host:ollama/...` → External Ollama (CUDA, multi-model, etc.)
- `host:vllm/...` → External vLLM (GPU clusters, production)
- `host:lmstudio/...` → External LM Studio
- `openai/...` → Cloud OpenAI
- `anthropic/...` → Cloud Anthropic

Users requiring CUDA, ROCm, multi-GPU, or 70B+ models use external providers via the existing `host:` prefix. The built-in server covers the 80% case — consumer hardware running small-to-medium models.

### Aegis remains inference-agnostic at the protocol level

The built-in server is just another provider. No lock-in, no special casing, no forced path. The LLM proxy abstraction is unchanged.

## 4. Phased Implementation

### Phase 0 — Bundle llama-server, direct GGUF path (v1)

Minimal integration. Solves the core problems without building a model registry.

**Model string:**
```
local:/path/to/model.gguf
local:~/.aegis/models/qwen3.5-9b-q4_k_m.gguf
```

**What it does:**
1. User drops a GGUF file in `~/.aegis/models/` (or anywhere)
2. Sets `"model": "local:~/.aegis/models/qwen3.5-9b-q4_k_m.gguf"` in agent.json
3. aegisd reads GGUF metadata → extracts native context length
4. Launches llama-server with correct `--ctx-size` (capped to safe RAM limit)
5. Proxies requests via existing LLM proxy path
6. llama-server stopped on idle, restarted on demand

**What it solves:**
- `num_ctx` gotcha → eliminated (auto-sized from metadata)
- Ollama dependency → eliminated
- Installation complexity → eliminated (bundled binary)
- Context truncation → eliminated

**What it does NOT solve:**
- Model discovery (user must find and download GGUF files manually)
- Quantization selection (user chooses)
- Model catalog / browser

**This is enough.** If adoption validates demand, proceed to Phase 1.

### Phase 1 — Model registry + `aegis model pull` (future)

Convenience layer on top of Phase 0. Not core architecture.

```bash
aegis model pull qwen3.5:9b        # Download from curated index
aegis model list                    # List downloaded models
aegis model info qwen3.5:9b        # Show size, ctx length, quantization
aegis model delete qwen3.5:9b      # Remove
```

**Model string simplifies:**
```
local:qwen3.5:9b                   # Resolved from registry
```

**Registry:** SQLite table alongside existing instance/secret registries:

```sql
CREATE TABLE models (
    id TEXT PRIMARY KEY,            -- "qwen3.5:9b"
    path TEXT NOT NULL,             -- ~/.aegis/models/qwen3.5-9b-q4_k_m.gguf
    size_bytes INTEGER,
    native_ctx_length INTEGER,      -- from GGUF metadata
    parameters TEXT,                -- "9B"
    quantization TEXT,              -- "Q4_K_M"
    pulled_at TEXT,
    source_url TEXT
);
```

**Model sources:**
- Phase 1a: Curated list of tested models (known-good tool calling, verified quantizations)
- Phase 1b: Direct HuggingFace URL support

**Desktop app integration:**
- Model browser tab (available / downloaded)
- One-click pull with progress bar
- Kit Config shows model picker dropdown

### Phase 2 — Smart defaults (future)

- Hardware detection → auto-recommend quantization based on available RAM
- RAM headroom checks before loading
- Disk space warnings before download
- "Recommended for your hardware" labels in model browser

## 5. Architecture

### 5.1 llama-server Lifecycle

aegisd manages llama-server as a child process (same pattern as aegis-gateway):

- **Started on demand**: First `local:` LLM request triggers launch
- **Idle timeout**: Model unloaded after configurable idle period (default: 10 min)
- **Single model**: One model loaded at a time (consumer hardware constraint)
- **Model swap**: Stop current → start new (future: warm cache for recent models)
- **Crash recovery**: If llama-server crashes, next LLM request restarts it

### 5.2 Context Window Auto-Sizing

The strongest technical argument for integration.

```
1. Read GGUF metadata → native_ctx_length (e.g., 262144 for Qwen 3.5)
2. Detect available system RAM
3. Estimate KV cache memory: ctx_length × model_dims × layers × 2 bytes
4. Cap at safe limit: min(native_ctx, RAM-safe estimate)
5. Launch llama-server with --ctx-size <computed>
```

Users never see `num_ctx`. It just works.

### 5.3 Request Flow

Identical to existing host LLM proxy — the only change is who manages the inference server:

```
Agent: {"model": "local:qwen3.5-9b.gguf"}
  → HostLLM.StreamChat() → POST /v1/llm/chat to harness
  → harness forwards via hrpc.Call("llm.chat", body)
  → aegisd: provider = "local" → proxy to managed llama-server
  → llm.delta / llm.done frames flow back via existing path
```

The `hostLLMProviders` map gets one new entry:

```go
"local": "http://localhost:<managed-port>/v1/chat/completions"
```

### 5.4 Build Matrix

| Platform | GPU Backend | Build | Notes |
|----------|-------------|-------|-------|
| macOS ARM64 | Metal | Bundled | Primary target. Apple Silicon unified memory. |
| Linux x86_64 | CPU-only | Bundled | Sufficient for 7B-14B models. |
| Linux ARM64 | CPU-only | Bundled | Raspberry Pi / ARM servers. |

**Explicitly not bundled:** CUDA, ROCm, Vulkan. Users needing GPU acceleration on Linux use `host:ollama/` or `host:vllm/` escape hatch.

> Built-in inference supports CPU (Linux) and Metal (macOS) only. Advanced GPU backends are intentionally not bundled. Users requiring CUDA/ROCm should use external providers via `host:` prefix.

## 6. Open Questions

- **GGUF metadata parsing** — need a Go library or minimal parser for GGUF header (context length, parameter count). Existing Go libraries?
- **llama-server port management** — fixed port or dynamic? Conflict with user's own llama-server?
- **Memory safety** — how aggressively to cap context vs. available RAM? Conservative (50% of free RAM) or aggressive (80%)?
- **Model naming in Phase 1** — follow Ollama's `name:variant` convention? Or `name-params-quant` (e.g., `qwen3.5-9b-q4_k_m`)?
- **Ollama coexistence** — `host:ollama/` continues working forever. Zero cost to maintain.

## 7. Non-goals

- Training or fine-tuning
- Running inference inside the VM (host-only — no GPU passthrough to guest)
- Non-GGUF formats
- Multi-GPU inference
- Serving models to external clients (inference is only for Aegis agents)
- Universal GPU platform (that's vLLM's job)
- Replacing Ollama for users who prefer it
