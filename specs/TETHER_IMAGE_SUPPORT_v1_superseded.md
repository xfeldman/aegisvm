# Tether Image Support

**Status:** Draft
**Scope:** Add image content to the tether protocol — both ingress (host/gateway → agent) and egress (agent → host/gateway). Inline base64 in payloads. No new wire format, no new endpoints.

---

## 1. What This Enables

Tether is currently text-only. Every `user.message` payload is `{text, user?}`. Every `assistant.done` and `assistant.delta` payload is `{text}`. This spec adds images alongside text in both directions.

Use cases:
- **Host → agent**: "analyze this screenshot" with an attached image (MCP `tether_send`)
- **Telegram → agent**: user sends a photo in chat, agent sees it
- **Agent → host**: agent generates a chart, returns it in `assistant.done` (MCP `tether_read`)
- **Agent → Telegram**: agent responds with an image (gateway sends via `sendPhoto`)

The tether frame envelope doesn't change. `Payload` is already `json.RawMessage` everywhere — harness, demuxer, store, gateway — so all intermediate layers forward images transparently. Changes are confined to **producers** (MCP server, gateway) and **consumers** (guest agent, gateway, MCP server).

---

## 2. Payload Changes

### 2.1 `user.message` payload

Before:
```json
{
  "text": "Hello",
  "user": {"id": "123", "name": "Alice"}
}
```

After:
```json
{
  "text": "Analyze this chart",
  "images": [
    {"media_type": "image/png", "data": "<base64>"}
  ],
  "user": {"id": "123", "name": "Alice"}
}
```

- `text` remains required. An image-only message uses `text: ""`.
- `images` is optional. When absent, behavior is unchanged (backward compatible).
- Each image is `{media_type, data}` where `data` is standard base64 (RFC 4648), no data-URI prefix.
- `media_type` is a MIME type: `image/png`, `image/jpeg`, `image/gif`, `image/webp`.

### 2.2 `assistant.done` payload

Before:
```json
{
  "text": "Here's the analysis..."
}
```

After:
```json
{
  "text": "Here's the analysis...",
  "images": [
    {"media_type": "image/png", "data": "<base64>"}
  ]
}
```

Same structure. `images` is optional.

### 2.3 `assistant.delta` payload

**No change.** Deltas remain text-only. Images are only emitted in `assistant.done`. Rationale: streaming partial image data is pointless for base64 — the consumer can't render it until it's complete. The LLM APIs don't stream image output either.

### 2.4 Image object

```json
{
  "media_type": "image/png",
  "data": "iVBORw0KGgo..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `media_type` | string | yes | MIME type. Must be `image/png`, `image/jpeg`, `image/gif`, or `image/webp`. |
| `data` | string | yes | Base64-encoded image bytes (RFC 4648, no padding required). |

No `url` field. No file references. Everything is inline. This keeps the protocol self-contained and avoids cross-boundary file access problems (host can't read VM filesystem, VM can't read host filesystem).

### 2.5 Size and count limits

Three hard limits, enforced at every producer:

| Limit | Value | Rationale |
|-------|-------|-----------|
| **Per-image max** | 10 MB decoded (~13.3 MB base64) | Matches Anthropic API limit, within Telegram 20 MB |
| **Per-frame total** | 20 MB decoded across all images | Prevents "4 × 10 MB" nuking the store and vsock path |
| **Max images per message** | 4 | Keeps LLM context and Telegram UX sane |

Producers MUST reject images/frames exceeding these limits with a clear error. Consumers SHOULD reject them as a defense-in-depth measure.

These limits are defined as a single `MaxFrameBytes` constant (20 MB decoded, ~26.6 MB on the wire) and enforced at:
- MCP `tether_send` (reject early, before POST)
- aegisd tether ingress endpoint (reject early, before forwarding to harness)
- Gateway ingress (reject early, after Telegram download and before base64 encode)
- Harness ingress (reject early, before persisting to inbox)

### 2.6 Reserved field: `ref`

The `ref` field is reserved on image objects for future use. When present, it identifies the image by content-addressed hash instead of inline data:

```json
{"media_type": "image/png", "ref": "sha256:abc123...", "size": 1048576}
```

This enables a future migration to:
- Host-side content-addressed blob store (avoid duplicating images in store/session/logs)
- Workspace-backed blob store inside VMs
- Deduplication of repeated images across sessions

**v1 does not implement `ref`.** Producers MUST NOT emit it. Consumers MUST ignore it if present. The field name is reserved so we don't paint ourselves into a corner.

---

## 3. MCP Tool Changes

### 3.1 `tether_send`

Add `images` parameter:

```json
{
  "name": "tether_send",
  "inputSchema": {
    "type": "object",
    "properties": {
      "instance": { "type": "string", "description": "Instance handle or ID" },
      "text": { "type": "string", "description": "Message text to send to the agent" },
      "images": {
        "type": "array",
        "description": "Images to include with the message. Each image is base64-encoded.",
        "items": {
          "type": "object",
          "properties": {
            "media_type": { "type": "string", "description": "MIME type (image/png, image/jpeg, image/gif, image/webp)" },
            "data": { "type": "string", "description": "Base64-encoded image data" }
          },
          "required": ["media_type", "data"]
        }
      },
      "session_id": { "type": "string", "description": "Session ID. Defaults to 'default'." }
    },
    "required": ["instance", "text"]
  }
}
```

**Behavior change:** Build payload with both `text` and `images` (if provided). `text` is still required — MCP callers always provide at least a text description with images.

### 3.2 `tether_read`

No input schema change. The change is in the **output**.

Today `tether_read` returns frames as a JSON text blob:
```go
return textResult(string(data))
```

This works for text payloads but loses images — MCP's `text` content type can't carry binary data. When an `assistant.done` frame contains images, `tether_read` must return them as MCP `image` content blocks.

**New behavior:** Parse egress frames. For each `assistant.done` frame with `payload.images`, emit MCP `image` content items alongside the text content.

MCP result with images:
```json
{
  "content": [
    {"type": "text", "text": "{\"frames\":[{\"type\":\"assistant.done\",\"seq\":5,\"msg_id\":\"resp-1\",\"payload\":{\"text\":\"Here is the chart.\",\"images\":[{\"_mcp_index\":0},{\"_mcp_index\":1}]}}],\"next_seq\":5}"},
    {"type": "image", "data": "<base64>", "mimeType": "image/png"},
    {"type": "image", "data": "<base64>", "mimeType": "image/jpeg"}
  ]
}
```

The first content item is the full JSON response. In the JSON, each image in `payload.images` is replaced with a stub `{"_mcp_index": N}` pointing to the Nth MCP image content item (0-indexed, counting only `type:"image"` items). This lets the caller correlate images back to specific frames and `msg_id`s when multiple `assistant.done` frames are returned in one poll.

Additional `image` content items are appended after the text item, in the order they appear across frames. Claude Code sees the images natively as MCP image content.

**`mcpContent` struct change:**

```go
type mcpContent struct {
    Type     string `json:"type"`               // "text" or "image"
    Text     string `json:"text,omitempty"`      // for type="text"
    Data     string `json:"data,omitempty"`      // for type="image" (base64)
    MimeType string `json:"mimeType,omitempty"`  // for type="image"
}
```

---

## 4. Guest Agent Changes

### 4.1 `handleUserMessage` — parse images

Today:
```go
var payload struct {
    Text string `json:"text"`
    User *struct { ... } `json:"user"`
}
```

After:
```go
var payload struct {
    Text   string `json:"text"`
    Images []struct {
        MediaType string `json:"media_type"`
        Data      string `json:"data"`
    } `json:"images"`
    User *struct { ... } `json:"user"`
}
```

If `payload.Images` is non-empty, build a multi-part content block for the LLM instead of a plain string.

### 4.2 LLM content block construction

**Claude API:**
```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "[Alice]: Analyze this chart"},
    {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}
  ]
}
```

**OpenAI API:**
```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "[Alice]: Analyze this chart"},
    {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
  ]
}
```

The agent builds LLM-native content blocks from the tether image objects. The `Content interface{}` in `Turn` and `Message` already supports this — both are `interface{}` that can hold strings or content block arrays.

### 4.3 Session storage — file-backed image refs

Storing raw base64 in session JSONL is brutal: a 5 MB image becomes a 6.7 MB line, session files balloon, and `loadFromDisk` gets slow. Instead, images are written to the workspace and referenced by path in the session log.

**On ingest (user message with images):**

1. For each image, write decoded bytes to `/workspace/sessions/blobs/{sha256-hex}.{ext}`
   - Extension derived from `media_type` (`image/png` → `.png`, etc.)
   - Content-addressed: same image sent twice = same file, no duplication
2. In the session JSONL, store a ref instead of inline data:

```jsonl
{"role":"user","content":[{"type":"text","text":"[Alice]: Analyze this"},{"type":"image","media_type":"image/png","path":"/workspace/sessions/blobs/a1b2c3...f0.png"}],"ts":"...","user":"Alice"}
```

**On context assembly (`assembleContext`):**

When building LLM messages, the agent reads the blob file and constructs inline content blocks for the LLM API. If the blob file is missing (deleted, corrupted), the image block is silently dropped — the LLM gets the text portion only. This is graceful degradation, not an error.

**`turnSize()` for image refs:**

Image ref turns count as a fixed cost (e.g. 2000 chars per image) for context windowing purposes. This ensures image turns are evicted before they consume the entire context budget, but not so aggressively that a recent image is always dropped.

**Why not inline:**
- A 10-message session with 2 images each = ~130 MB JSONL file (inline) vs. ~20 KB JSONL + 130 MB in blobs (ref). The JSONL stays fast to scan.
- `loadFromDisk` scanner buffer stays at 256 KB — no bump needed for session files.
- Blobs are naturally deduped (content-addressed).
- A future compactor can delete blobs for old turns without rewriting the session file.

**Blob cleanup (future):**
Not implemented in v1. Blobs accumulate in `/workspace/sessions/blobs/`. A future session compactor can scan active sessions and delete unreferenced blobs. For v1, workspace disk is the bound.

### 4.4 Image egress — agent → host

The agent needs a way to include images in responses. Two sources:

1. **Tool results that produce images.** A bash tool that runs `python3 plot.py` and produces `/workspace/output.png`. The agent reads the file and includes it in the response.

2. **Explicit image attachment.** The agent decides to send an image (e.g., screenshot of a running app).

For v1, source (1) is the primary case. The agent's built-in `read_file` tool returns file contents. When the tool result is a known image file (by extension or content sniffing), the agent can include it in the final `assistant.done` payload.

**New helper:**
```go
func (a *Agent) sendDoneWithImages(session SessionID, text string, images []ImageObject) {
    payload := map[string]interface{}{"text": text}
    if len(images) > 0 {
        payload["images"] = images
    }
    a.sendFrame(TetherFrame{
        V: 1, Type: "assistant.done", TS: now(), Session: session,
        Payload: mustMarshal(payload),
    })
}
```

The existing `sendDone` continues to work for text-only responses.

---

## 5. Gateway Changes (Telegram)

### 5.1 Ingress — Telegram photos → tether

Today the gateway only processes `u.Message.Text != ""`. Photos are silently ignored.

**Change:** Handle `u.Message.Photo` (Telegram sends an array of photo sizes). Download the largest photo via `getFile` + `https://api.telegram.org/file/bot{token}/{file_path}`, base64-encode it, and include it in the `user.message` payload's `images` array.

```go
// In pollTelegram:
if u.Message != nil && (u.Message.Text != "" || len(u.Message.Photo) > 0) {
    go gw.handleTelegramMessage(u.Message)
}
```

```go
// In buildUserPayload:
if len(msg.Photo) > 0 {
    // Download largest photo size
    photo := msg.Photo[len(msg.Photo)-1]
    data, mediaType := gw.downloadFile(photo.FileID, botToken)
    p["images"] = []map[string]interface{}{
        {"media_type": mediaType, "data": base64Encode(data)},
    }
}
if msg.Caption != "" {
    p["text"] = msg.Caption // Telegram puts photo text in caption, not text
}
```

Telegram message types to handle:
- `photo` — array of PhotoSize, use largest. For albums (multiple photos in one update), v1 takes the largest size of the first photo only. Album support is future work.
- `document` with image MIME — treated same as photo
- Gateway MUST enforce `MaxFrameBytes` on the downloaded file size *before* base64 encoding. If the Telegram file exceeds the limit, reject with an error reply to the chat.

Not handled (future): stickers, video, audio, voice, albums (multi-photo messages).

### 5.2 Egress — tether images → Telegram

When `assistant.done` contains `payload.images`, the gateway sends images and text in a deterministic order:

1. **Images first** — each image sent via `sendPhoto` (multipart form upload: decode base64 → send raw bytes as `photo` field). The first image MAY use `payload.text` as its `caption` if text is short enough (Telegram caption limit: 1024 chars).
2. **Text after** — if text was not used as caption (or is too long), send via `editMessageText` or `sendMessage` as today.

This ordering is deterministic: images always precede the final text message. Avoids UX jank where text and photos interleave unpredictably.

```go
case "assistant.done":
    var payload struct {
        Text   string        `json:"text"`
        Images []ImageObject `json:"images"`
    }
    // ...
    if len(payload.Images) > 0 {
        for i, img := range payload.Images {
            caption := ""
            if i == 0 && len(payload.Text) <= 1024 {
                caption = payload.Text
            }
            gw.sendTelegramPhoto(chatID, img, caption, botToken)
        }
        if len(payload.Text) > 1024 {
            gw.sendTelegramMessage(chatID, payload.Text, botToken)
        }
    } else if payload.Text != "" {
        // existing text-only handling
    }
```

### 5.3 Telegram struct changes

```go
type telegramMessage struct {
    MessageID int              `json:"message_id"`
    Chat      telegramChat     `json:"chat"`
    Text      string           `json:"text"`
    Caption   string           `json:"caption"`
    Photo     []telegramPhoto  `json:"photo"`
    From      *telegramUser    `json:"from"`
}

type telegramPhoto struct {
    FileID   string `json:"file_id"`
    Width    int    `json:"width"`
    Height   int    `json:"height"`
    FileSize int    `json:"file_size"`
}
```

---

## 6. Buffer and Size Changes

### 6.1 Scanner buffers

Several scanner buffers are set to 256 KB. A single 5 MB image is ~6.7 MB base64. Buffers on paths that carry full tether frames must grow.

| Location | Current | New | Why |
|----------|---------|-----|-----|
| `cmd/aegis-gateway/main.go` processEgressStream | 256 KB | 32 MB | Egress frames may contain images |
| `cmd/aegis-agent/session.go` loadFromDisk | 256 KB | 256 KB | **No change** — images stored as file refs, not inline |
| `cmd/aegis-agent/llm_claude.go` parseClaudeStream | 256 KB | 256 KB | No change — LLM SSE events are small |
| `cmd/aegis-agent/llm_openai.go` parseOpenAIStream | 256 KB | 256 KB | No change — same |
| `internal/harness/rpc.go` inbox persistence | 256 KB | 32 MB | Tether frames in inbox may contain images |

The control channel (JSON-RPC over vsock) doesn't use a line scanner — it uses length-prefixed framing or reads full JSON objects. No change needed there.

### 6.2 HTTP body limits

Check for implicit limits at:
- Harness `127.0.0.1:7778` endpoint (agent HTTP server) — if `http.MaxBytesReader` or similar is used, raise to `MaxFrameBytes`
- aegisd tether ingress endpoint — same
- Any JSON-RPC framing limits in the control channel

Define `MaxFrameBytes = 28 * 1024 * 1024` (28 MB, covering 20 MB decoded + base64 overhead + JSON envelope) in one place and reference it everywhere.

### 6.3 Tether store memory budget

The ring buffer currently stores 1000 frames by count. With images, count-based eviction is dangerous: 50 image frames could consume 1.3 GB.

**Change: add a byte budget alongside the count cap.**

```go
const (
    defaultRingSize    = 1000       // max frames
    defaultRingBytes   = 128 << 20  // 128 MB max payload bytes
)
```

`Append()` tracks cumulative payload bytes. When either limit is exceeded, evict oldest frames until both are satisfied. Implementation:

```go
type ringBuffer struct {
    // ... existing fields ...
    totalBytes int64  // sum of len(frame.Payload) for all stored frames
}

func (rb *ringBuffer) append(frame Frame) {
    payloadSize := int64(len(frame.Payload))
    // ... assign seq, write to ring ...

    // Evict oldest while over byte budget
    for rb.count > 1 && rb.totalBytes > defaultRingBytes {
        evicted := rb.frames[rb.head]
        rb.totalBytes -= int64(len(evicted.Payload))
        rb.head = (rb.head + 1) % rb.cap
        rb.count--
    }
}
```

This means a burst of image frames evicts older frames faster, preserving recent history while bounding memory. Small frames (deltas, presence) are cheap; a 1000-frame ring of text-only frames uses ~1 MB.

---

## 7. What Doesn't Change

- **Tether frame envelope** — `Frame` struct, `Payload json.RawMessage`, types, seq, sessions: all unchanged
- **aegisd API** — `POST /v1/instances/{id}/tether`, `GET .../tether/poll`, `GET .../tether/stream`: all unchanged. They forward `json.RawMessage` payloads opaquely.
- **Tether store** — stores `Frame` with `json.RawMessage` payload. No schema awareness. Eviction policy changes (byte budget added, see §6.3), but the API is unchanged.
- **Harness passthrough** — forwards `json.RawMessage` to agent at 7778, persists to inbox. Unchanged (except scanner buffer).
- **Lifecycle manager / demuxer** — routes `tether.frame` notifications. Payload-agnostic. Unchanged.
- **Wire protocol** — JSON-RPC 2.0 over vsock. Unchanged.
- **Control channel** — unchanged.

The key insight: because `Payload` is `json.RawMessage` at every intermediate layer, image data flows through the entire pipeline without any of these layers needing to understand it.

---

## 8. Implementation Plan

### Phase 0: Limits and plumbing (do first)

1. **Define `MaxFrameBytes`, `MaxImageBytes`, `MaxImagesPerMessage` constants** in a shared location (`internal/tether/limits.go` or similar)
2. **Tether store byte budget**: Add `totalBytes` tracking + byte-budget eviction to `ringBuffer`
3. **Scanner buffer bumps**: Gateway egress, harness inbox (32 MB)
4. **Validate no implicit HTTP body limits** on harness 7778, aegisd tether endpoint, control channel framing

### Phase 1: Ingress (host → agent)

5. **MCP `tether_send`**: Add `images` parameter, validate limits, include in payload
6. **aegisd tether ingress**: Validate `MaxFrameBytes` on incoming POST body, reject early
7. **Guest agent `handleUserMessage`**: Parse `payload.images`, write blobs to `/workspace/sessions/blobs/`, build LLM content blocks
8. **`ClaudeLLM`**: Construct `{type:"image", source:{type:"base64",...}}` content blocks when content is array
9. **`OpenAILLM`**: Construct `{type:"image_url", image_url:{url:"data:..."}}` content blocks when content is array
10. **Session storage**: Store image turns as file-backed refs (§4.3), implement blob write + read-on-assemble
11. **Test**: MCP `tether_send` with image → agent sees image → LLM responds about image content

### Phase 2: Egress (agent → host)

12. **Guest agent**: Add `sendDoneWithImages` helper
13. **MCP `mcpContent`**: Add `Data` and `MimeType` fields
14. **MCP `tether_read`**: Parse `assistant.done` images, replace with `_mcp_index` stubs, emit MCP `image` content blocks with correlation
15. **Test**: Agent responds with image → MCP `tether_read` returns correlated image content → Claude Code sees it

### Phase 3: Telegram

16. **Gateway ingress**: Handle `Message.Photo`, download via `getFile` (enforce `MaxImageBytes` on download), base64-encode, include in payload
17. **Gateway egress**: Parse `assistant.done` images, send via `sendPhoto` (images first, then text; caption for short text)
18. **Gateway**: Add `telegramPhoto` struct, `Caption` field, bump egress scanner buffer
19. **Test**: Send photo in Telegram → agent analyzes it → responds with text

### Phase 4: Hardening

20. **Size validation**: Enforce all three limits at every producer (MCP, gateway, aegisd, harness)
21. **Media type validation**: Reject unsupported MIME types at producers
22. **Oversize rejection test**: `tether_send` with 15 MB image → error; frame with 5 × 10 MB images → error
23. **Context windowing**: Verify `turnSize()` fixed cost for image refs causes proper eviction behavior

---

## 9. Testing

### Integration tests

1. **MCP image round-trip**: `tether_send` with image → agent receives image → LLM responds → `tether_read` returns response mentioning image content
2. **MCP image egress**: Agent tool produces image file → agent includes in response → `tether_read` returns MCP image content with `_mcp_index` correlation
3. **Oversized single image**: `tether_send` with 15 MB image → error
4. **Oversized frame total**: `tether_send` with 4 × 8 MB images (32 MB total) → error (exceeds `MaxFrameBytes`)
5. **Too many images**: `tether_send` with 5 images → error (exceeds `MaxImagesPerMessage`)
6. **Backward compatibility**: `tether_send` without images → unchanged behavior
7. **Session persistence with blob refs**: Send image message → restart VM → session loads correctly, blob file exists, LLM context assembly reads blob and constructs inline content blocks
8. **Missing blob graceful degradation**: Delete a blob file → session loads → image turn is assembled without image (text only), no crash
9. **Tether store byte budget**: Append many image frames → verify oldest evicted when byte budget exceeded → verify small text frames survive longer
10. **Multi-frame image correlation**: Two `assistant.done` frames with images in one `tether_read` poll → verify `_mcp_index` stubs correctly map to returned MCP image content items

### Gateway tests (manual for v1)

11. **Telegram photo in**: Send photo in Telegram chat → agent describes image
12. **Telegram image out**: Agent generates chart → photo appears in Telegram, then text
13. **Telegram photo size limit**: Send very large photo → gateway rejects before tether send
