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

### 2.5 Size limit

**Max 10 MB per image after base64 decoding** (~13.3 MB encoded). This matches the Anthropic API limit and is well within Telegram's 20 MB photo limit. Producers MUST reject images larger than this. Consumers SHOULD reject them.

No per-frame aggregate limit — a message with 3 images is fine as long as each is under 10 MB. Practical limit is the LLM's context budget; the agent runtime is responsible for managing that.

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
    {"type": "text", "text": "{\"frames\":[...],\"next_seq\":5}"},
    {"type": "image", "data": "<base64>", "mimeType": "image/png"},
    {"type": "image", "data": "<base64>", "mimeType": "image/jpeg"}
  ]
}
```

The first content item is the full JSON response (unchanged). Additional `image` content items are appended for each image in any `assistant.done` frame in the result. This means Claude Code sees the images natively.

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

### 4.3 Session storage

Turns with images are stored as content block arrays in the JSONL session file:

```jsonl
{"role":"user","content":[{"type":"text","text":"[Alice]: Analyze this"},{"type":"image","media_type":"image/png","data":"iVBO..."}],"ts":"...","user":"Alice"}
```

This makes sessions self-contained. The `turnSize()` function already handles non-string content via `json.Marshal`. Context windowing will naturally drop old image turns when the char budget is exceeded — images are expensive, so they'll be evicted early. This is correct behavior.

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
- `photo` — array of PhotoSize, use largest
- `document` with image MIME — treated same as photo

Not handled (future): stickers, video, audio, voice.

### 5.2 Egress — tether images → Telegram

When `assistant.done` contains `payload.images`, the gateway sends each image via `sendPhoto` instead of (or in addition to) `sendMessage`/`editMessageText`.

```go
case "assistant.done":
    var payload struct {
        Text   string        `json:"text"`
        Images []ImageObject `json:"images"`
    }
    // ...
    if len(payload.Images) > 0 {
        for _, img := range payload.Images {
            gw.sendTelegramPhoto(chatID, img, botToken)
        }
    }
    if payload.Text != "" {
        // existing text handling (editMessageText or sendMessage)
    }
```

`sendPhoto` accepts base64 via multipart form upload (decode base64 → send raw bytes as `photo` field).

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

## 6. Buffer Size Changes

Several scanner buffers are set to 256 KB. A single 5 MB image is ~6.7 MB base64. These buffers must grow.

| Location | Current | New | Why |
|----------|---------|-----|-----|
| `cmd/aegis-gateway/main.go` processEgressStream | 256 KB | 16 MB | Egress frames may contain images |
| `cmd/aegis-agent/session.go` loadFromDisk | 256 KB | 16 MB | Session turns may contain image content blocks |
| `cmd/aegis-agent/llm_claude.go` parseClaudeStream | 256 KB | 256 KB | No change — LLM SSE events are small |
| `cmd/aegis-agent/llm_openai.go` parseOpenAIStream | 256 KB | 256 KB | No change — same |
| `internal/harness/rpc.go` (if scanner-based) | 256 KB | 16 MB | Tether frames in inbox may contain images |

The control channel (JSON-RPC over vsock) doesn't use a line scanner — it uses length-prefixed framing or reads full JSON objects. No change needed there.

The tether store ring buffer (1000 frames in memory) can hold large frames. Worst case: 1000 frames × 13 MB = 13 GB. This won't happen in practice (most frames are small deltas/presence), but we should be aware. No change for v1 — revisit if memory becomes an issue with heavy image workloads.

---

## 7. What Doesn't Change

- **Tether frame envelope** — `Frame` struct, `Payload json.RawMessage`, types, seq, sessions: all unchanged
- **aegisd API** — `POST /v1/instances/{id}/tether`, `GET .../tether/poll`, `GET .../tether/stream`: all unchanged. They forward `json.RawMessage` payloads opaquely.
- **Tether store** — stores `Frame` with `json.RawMessage` payload. No schema awareness. Unchanged.
- **Harness passthrough** — forwards `json.RawMessage` to agent at 7778, persists to inbox. Unchanged (except scanner buffer).
- **Lifecycle manager / demuxer** — routes `tether.frame` notifications. Payload-agnostic. Unchanged.
- **Wire protocol** — JSON-RPC 2.0 over vsock. Unchanged.
- **Control channel** — unchanged.

The key insight: because `Payload` is `json.RawMessage` at every intermediate layer, image data flows through the entire pipeline without any of these layers needing to understand it.

---

## 8. Implementation Plan

### Phase 1: Ingress (host → agent)

1. **MCP `tether_send`**: Add `images` parameter, include in payload
2. **Guest agent `handleUserMessage`**: Parse `payload.images`, build LLM content blocks
3. **`ClaudeLLM`**: Construct `{type:"image", source:{type:"base64",...}}` content blocks when content is array
4. **`OpenAILLM`**: Construct `{type:"image_url", image_url:{url:"data:..."}}` content blocks when content is array
5. **Session storage**: Bump scanner buffer to 16 MB
6. **Test**: MCP `tether_send` with image → agent sees image → LLM responds about image content

### Phase 2: Egress (agent → host)

7. **Guest agent**: Add `sendDoneWithImages` helper
8. **MCP `tether_read`**: Parse `assistant.done` images, emit MCP `image` content blocks
9. **MCP `mcpContent`**: Add `Data` and `MimeType` fields
10. **Test**: Agent responds with image → MCP `tether_read` returns image content → Claude Code sees it

### Phase 3: Telegram

11. **Gateway ingress**: Handle `Message.Photo`, download via `getFile`, base64-encode, include in payload
12. **Gateway egress**: Parse `assistant.done` images, send via `sendPhoto`
13. **Gateway**: Add `telegramPhoto` struct, `Caption` field, bump egress scanner buffer
14. **Test**: Send photo in Telegram → agent analyzes it → responds with text

### Phase 4: Hardening

15. **Size validation**: Enforce 10 MB per-image limit at producers (MCP server, gateway)
16. **Media type validation**: Reject unsupported MIME types
17. **Context windowing**: Ensure large image turns are evicted first when context budget is tight (already works via `turnSize`, but verify)

---

## 9. Testing

### Integration tests

1. **MCP image round-trip**: `tether_send` with image → agent receives image → LLM responds → `tether_read` returns response mentioning image content
2. **MCP image egress**: Agent tool produces image file → agent includes in response → `tether_read` returns MCP image content
3. **Oversized image rejection**: `tether_send` with 15 MB image → error
4. **Backward compatibility**: `tether_send` without images → unchanged behavior
5. **Session persistence**: Send image message → restart VM → session loads correctly with image turns

### Gateway tests (manual for v1)

6. **Telegram photo in**: Send photo in Telegram chat → agent describes image
7. **Telegram image out**: Agent generates chart → photo appears in Telegram
