# Tether Image Support

**Status:** Draft
**Scope:** Add image content to the tether protocol. Tether frames carry opaque blob refs; actual bytes live in a blob store. v1 blob store: shared workspace filesystem (`WorkspaceBlobStore`).

---

## 1. What This Enables

Tether is text-only. This spec adds images in both directions:
- **Host → agent**: "analyze this screenshot" (MCP `tether_send`)
- **Telegram → agent**: user sends a photo in chat
- **Agent → host**: agent generates a chart, returns it in `assistant.done`
- **Agent → Telegram**: agent responds with an image

**Key design decision:** workspace is always a shared live-mount between host and guest. Instead of encoding images as base64 in JSON frames, we write image bytes to a well-known blob directory in the workspace. Tether frames carry only a content-addressed ref. Both sides resolve the ref to the same file on the shared filesystem.

This eliminates: base64 bloat in frames, scanner buffer bumps, tether store memory concerns, large-payload transport through vsock/HTTP, and complex MCP image correlation schemes.

---

## 2. Blob Store

### 2.1 Abstraction

The blob store is an interface, not a path convention. The tether payload carries an opaque blob key (`{sha256}.{ext}`). Producers call `PutBlob(bytes, mediaType) → key`. Consumers call `GetBlob(key) → bytes`. How those map to storage is the blob store's business — callers never construct paths or assume filesystem layout.

```go
// BlobStore is the interface for image blob storage.
// v1 implementation: workspace filesystem.
// Future: object store, content-addressed remote, etc.
type BlobStore interface {
    Put(data []byte, mediaType string) (key string, err error)
    Get(key string) ([]byte, error)
}
```

**v1 implementation: `WorkspaceBlobStore`** — backed by the shared workspace filesystem.

### 2.2 v1 implementation: workspace filesystem

```
/workspace/.aegis/blobs/
  a1b2c3d4e5f6...0a.png
  f0e1d2c3b4a5...1b.jpg
```

- Key format: `{sha256-hex}.{ext}` — content-addressed, automatic dedup
- Extension derived from media type: `image/png` → `.png`, `image/jpeg` → `.jpg`, `image/gif` → `.gif`, `image/webp` → `.webp`
- Storage path: `{workspace_root}/.aegis/blobs/{key}`

Resolution per participant:

| Who | Workspace root | Blob path |
|-----|---------------|-----------|
| Guest agent | `/workspace/` | `/workspace/.aegis/blobs/{key}` |
| MCP server | Host path from instance info | `{host_workspace}/.aegis/blobs/{key}` |
| Gateway | Host path from instance info | `{host_workspace}/.aegis/blobs/{key}` |

### 2.3 v1 implementation

```go
// validBlobKey matches keys produced by Put: 64 hex chars + known extension.
var validBlobKey = regexp.MustCompile(`^[a-f0-9]{64}\.(png|jpg|gif|webp)$`)

type WorkspaceBlobStore struct {
    root string // workspace root (host or guest path)
}

func (s *WorkspaceBlobStore) Put(data []byte, mediaType string) (string, error) {
    hash := sha256.Sum256(data)
    key := hex.EncodeToString(hash[:]) + extForMediaType(mediaType)
    dir := filepath.Join(s.root, ".aegis", "blobs")
    final := filepath.Join(dir, key)

    // Content-addressed dedup: skip if exists
    if _, err := os.Stat(final); err == nil {
        return key, nil
    }

    os.MkdirAll(dir, 0755)

    // Atomic write: tmp then rename (prevents partial files on crash)
    tmp, err := os.CreateTemp(dir, ".tmp-*")
    if err != nil {
        return "", err
    }
    if _, err := tmp.Write(data); err != nil {
        tmp.Close()
        os.Remove(tmp.Name())
        return "", err
    }
    tmp.Close()
    if err := os.Rename(tmp.Name(), final); err != nil {
        os.Remove(tmp.Name())
        return "", err
    }
    return key, nil
}

func (s *WorkspaceBlobStore) Get(key string) ([]byte, error) {
    // Validate key format to prevent path traversal
    if !validBlobKey.MatchString(key) {
        return nil, fmt.Errorf("invalid blob key: %q", key)
    }
    path := filepath.Join(s.root, ".aegis", "blobs", key)
    return os.ReadFile(path)
}
```

**Key validation:** `Get` rejects any key not matching `^[a-f0-9]{64}\.(png|jpg|gif|webp)$`. This prevents path traversal attacks if a malicious producer injects keys like `../../etc/passwd`. `Put` produces keys that always match this pattern by construction.

**Atomic writes:** `Put` writes to a temp file in the blob directory then `rename()`s to the final path. This prevents consumers from reading partial files if the producer crashes mid-write. `rename()` is atomic on both ext4 and APFS.

### 2.4 Instance isolation

Each instance has its own workspace, and therefore its own blob namespace. The MCP server and gateway always resolve blobs against the *target instance's* workspace root (from instance info), never a global shared directory. This prevents cross-instance blob leakage.

A future daemon-managed blob store could share blobs across instances (content-addressed dedup makes this safe), but v1 keeps it per-instance.

### 2.5 Future implementations

The `BlobStore` interface allows swapping the backend without changing any tether payload, session format, or consumer code:

- **Remote/cloud workspace**: blob store backed by object storage (S3, GCS), `Put` uploads, `Get` downloads
- **Content-addressed daemon**: aegisd-managed blob store with dedup across instances
- **Hybrid**: workspace for hot blobs, cold blobs evicted to remote

The payload shape `{media_type, blob, size}` never changes. Only the resolver behind `Put`/`Get` changes.

### 2.6 Size and count limits

| Limit | Value | Rationale |
|-------|-------|-----------|
| **Per-image max** | 10 MB | Matches Anthropic API limit, within Telegram 20 MB |
| **Max images per message** | 4 | Keeps LLM context and Telegram UX sane |

Enforced at producers (MCP server, gateway) on the raw bytes *before* writing to blob store. No per-frame byte limit needed — frames are tiny (just refs).

### 2.7 Cleanup

Not implemented in v1. Blobs accumulate. Content-addressed means automatic dedup, so growth is bounded by unique images, not unique messages.

GC strategy for later: blobs are refcounted by scanning session logs (`.jsonl` files). A blob is unreferenced if no active session log contains its key. A compactor can delete unreferenced blobs without touching session files or tether state.

### 2.8 Requirement: workspace mapping

Image support requires `--workspace`. If `tether_send` includes images and the instance has no workspace, return error: `"images require workspace mapping"`.

This is a natural constraint — any meaningful agent work already uses workspace for session persistence, tool output, etc.

---

## 3. Payload Changes

### 3.1 Image ref object

```json
{
  "media_type": "image/png",
  "blob": "a1b2c3d4e5f6...0a.png",
  "size": 1048576
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `media_type` | string | yes | MIME type: `image/png`, `image/jpeg`, `image/gif`, `image/webp` |
| `blob` | string | yes | Filename in `.aegis/blobs/`. Format: `{sha256}.{ext}` |
| `size` | int | yes | Decoded byte count. Consumer uses for validation / display hints. |

No `data` field. No `url`. No `ref` URI scheme. Just a filename that both sides resolve against their own workspace root.

### 3.2 `user.message` payload

```json
{
  "text": "Analyze this chart",
  "images": [
    {"media_type": "image/png", "blob": "a1b2c3...0a.png", "size": 524288}
  ],
  "user": {"id": "123", "name": "Alice"}
}
```

- `images` is optional. When absent, behavior is unchanged (backward compatible).
- `text` remains required. Image-only message uses `text: ""`.

### 3.3 `assistant.done` payload

```json
{
  "text": "Here's the analysis...",
  "images": [
    {"media_type": "image/png", "blob": "f0e1d2...1b.png", "size": 1048576}
  ]
}
```

Same structure. `images` is optional.

### 3.4 `assistant.delta` payload

**No change.** Deltas remain text-only.

---

## 4. MCP Tool Changes

### 4.1 `tether_send`

Add `images` parameter. MCP callers (Claude Code) provide base64 because MCP is JSON over stdio — no filesystem access from the tool call itself.

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

**Behavior:**
1. Validate image count (≤ 4) and per-image size (≤ 10 MB decoded)
2. Resolve blob store for instance (v1: look up workspace path via `GET /v1/instances/{instance}`, construct `WorkspaceBlobStore`)
3. If no blob store available (no workspace) → error: `"images require workspace mapping"`
4. For each image: decode base64, `blobStore.Put(bytes, mediaType)` → key
5. Build payload with `images: [{media_type, blob, size}]` (refs, not data)
6. POST frame to `/v1/instances/{instance}/tether` as today

The frame sent over the wire contains only blob refs (~100 bytes per image). The actual image bytes were written to the blob store in step 4.

### 4.2 `tether_read`

No input schema change.

**New behavior:** When `assistant.done` frames contain `payload.images`, read the blobs via the blob store and return them as MCP `image` content blocks.

```go
// For each assistant.done frame with images:
for _, img := range payload.Images {
    data, _ := blobStore.Get(img.Blob)
    result.Content = append(result.Content, mcpContent{
        Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: img.MediaType,
    })
}
```

MCP result:
```json
{
  "content": [
    {"type": "text", "text": "{\"frames\":[...],\"next_seq\":5}"},
    {"type": "image", "data": "<base64>", "mimeType": "image/png"}
  ]
}
```

The JSON text blob includes the blob refs in the frames as-is. MCP image content blocks are appended after. Claude Code sees images natively.

For correlation: each image in the JSON blob has `blob` as a unique identifier. The MCP image content items appear in the same order as images across frames. If the caller needs to map them, the `blob` filenames in the JSON match 1:1 with the appended image content items.

**`mcpContent` struct change:**

```go
type mcpContent struct {
    Type     string `json:"type"`               // "text" or "image"
    Text     string `json:"text,omitempty"`      // for type="text"
    Data     string `json:"data,omitempty"`      // for type="image" (base64)
    MimeType string `json:"mimeType,omitempty"`  // for type="image"
}
```

### 4.3 Workspace path lookup

`tether_send` and `tether_read` need the host-side workspace path. Two options:

**Option A — Instance info endpoint.** Add `workspace` field to `GET /v1/instances/{id}` response. MCP server fetches it per call. Simple, no caching.

**Option B — Cache on first use.** MCP server caches workspace path per instance handle. Avoids repeated API calls.

**v1: Option A.** One extra GET per tether call is cheap (local unix socket). No stale cache risk.

**aegisd change:** Add `workspace` to instance info response:

```go
if inst.WorkspacePath != "" {
    resp["workspace"] = inst.WorkspacePath
}
```

---

## 5. Guest Agent Changes

### 5.1 `handleUserMessage` — parse images

```go
var payload struct {
    Text   string `json:"text"`
    Images []struct {
        MediaType string `json:"media_type"`
        Blob      string `json:"blob"`
        Size      int64  `json:"size"`
    } `json:"images"`
    User *struct { ... } `json:"user"`
}
```

If `payload.Images` is non-empty:
1. For each image, read from `/workspace/.aegis/blobs/{blob}` → raw bytes
2. Build LLM content blocks (base64 needed here — LLM APIs require it)
3. If blob file missing, skip that image (graceful degradation)

### 5.2 LLM content block construction

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

Base64 encoding happens only here — at the LLM API boundary. Not in transport, not in storage.

### 5.3 Session storage

Session JSONL stores blob refs, same as the wire format:

```jsonl
{"role":"user","content":[{"type":"text","text":"[Alice]: Analyze this"},{"type":"image","media_type":"image/png","blob":"a1b2c3...0a.png"}],"ts":"...","user":"Alice"}
```

Lines stay small. Scanner buffer stays at 256 KB. No changes to `loadFromDisk`.

On `assembleContext`: read blob from disk, construct LLM content blocks with base64. Missing blob → silently drop image block.

`turnSize()` for image ref turns: fixed cost of 2000 chars per image for context windowing. Ensures image turns are evictable without being overly aggressive.

### 5.4 Image egress — agent → host

When the agent produces an image (tool output, generated file):
1. Write to blob store: `PutBlob("/workspace", bytes, mediaType)` → filename
2. Include ref in `assistant.done` payload

```go
func (a *Agent) sendDoneWithImages(session SessionID, text string, images []ImageRef) {
    payload := map[string]interface{}{"text": text}
    if len(images) > 0 {
        payload["images"] = images
    }
    a.sendFrame(TetherFrame{
        V: 1, Type: "assistant.done", TS: now(), Session: session,
        Payload: mustMarshal(payload),
    })
}

type ImageRef struct {
    MediaType string `json:"media_type"`
    Blob      string `json:"blob"`
    Size      int64  `json:"size"`
}
```

---

## 6. Gateway Changes (Telegram)

### 6.1 Ingress — Telegram photos → blobs → tether

1. Handle `u.Message.Photo` (take largest size) and `u.Message.Document` with image MIME
2. Download via `getFile` API. Enforce 10 MB limit on download size — reject with error reply if exceeded
3. Write to blob store: `PutBlob(hostWorkspace, bytes, mediaType)` → filename
4. Include ref in `user.message` payload, use `msg.Caption` as `text`

For albums (multiple photos in one update): v1 takes first photo only. Album support is future work.

```go
// In pollTelegram:
if u.Message != nil && (u.Message.Text != "" || len(u.Message.Photo) > 0) {
    go gw.handleTelegramMessage(u.Message)
}
```

Gateway needs the host workspace path. Fetched from `GET /v1/instances/{id}` (same as MCP server).

### 6.2 Egress — tether image refs → Telegram

When `assistant.done` contains `payload.images`:

1. **Images first** — for each image, read blob from host workspace, send via `sendPhoto` (multipart form upload, raw bytes). First image uses `payload.text` as caption if ≤ 1024 chars.
2. **Text after** — if text not used as caption (or too long), send via `sendMessage`.

Deterministic ordering: images always precede text. No interleaving jank.

### 6.3 Telegram struct additions

```go
type telegramMessage struct {
    // ... existing fields ...
    Caption string          `json:"caption"`
    Photo   []telegramPhoto `json:"photo"`
}

type telegramPhoto struct {
    FileID   string `json:"file_id"`
    Width    int    `json:"width"`
    Height   int    `json:"height"`
    FileSize int    `json:"file_size"`
}
```

---

## 7. What Doesn't Change

- **Tether frame envelope** — `Frame` struct, `Payload json.RawMessage`, types, seq, sessions: unchanged
- **aegisd API** — all endpoints unchanged (frames carry tiny refs, not image bytes)
- **Tether store** — unchanged. Ring buffer of 1000 frames stays well under 1 MB. No byte budget needed.
- **Harness passthrough** — unchanged. Forwards small JSON frames. No scanner buffer bumps.
- **Lifecycle manager / demuxer** — unchanged
- **Wire protocol** — unchanged
- **Control channel** — unchanged
- **Scanner buffers** — all remain at 256 KB. No bumps needed anywhere.

The entire "large payload" problem disappears. Tether frames with image refs are ~200 bytes larger than text-only frames.

---

## 8. aegisd Changes

One change: expose workspace path in instance info.

```go
// In handleGetInstance:
if inst.WorkspacePath != "" {
    resp["workspace"] = inst.WorkspacePath
}
```

This is needed by the MCP server and gateway to resolve blob paths on the host side.

---

## 9. Implementation Plan

### Phase 1: Plumbing

1. **`BlobStore` interface + `WorkspaceBlobStore`** in `internal/blob/` — `Put([]byte, mediaType) → key`, `Get(key) → []byte`
2. **aegisd instance info**: Add `workspace` field to `GET /v1/instances/{id}` response
3. **MCP `mcpContent`**: Add `Data` and `MimeType` fields for image content type

### Phase 2: Ingress (host → agent)

4. **MCP `tether_send`**: Add `images` param, validate limits, decode base64, `blobStore.Put()`, send ref in frame
5. **Guest agent `handleUserMessage`**: Parse `payload.images`, `blobStore.Get()`, build LLM content blocks
6. **`ClaudeLLM`**: Construct image content blocks when content is array
7. **`OpenAILLM`**: Construct image_url content blocks when content is array
8. **Session storage**: Store blob refs in JSONL, read blobs on `assembleContext`
9. **Test**: `tether_send` with image → blob appears in workspace → agent sees image → LLM responds

### Phase 3: Egress (agent → host)

10. **Guest agent**: `sendDoneWithImages` helper, `blobStore.Put()` for tool-produced images
11. **MCP `tether_read`**: Parse `assistant.done` image refs, `blobStore.Get()`, return as MCP image content
12. **Test**: Agent produces image → blob in workspace → `tether_read` returns image to Claude Code

### Phase 4: Telegram

13. **Gateway ingress**: Handle photos, download, enforce size limit, `blobStore.Put()`, send ref
14. **Gateway egress**: Parse image refs, `blobStore.Get()`, `sendPhoto`
15. **Test**: Photo in Telegram → agent analyzes → responds with text

---

## 10. Testing

### Integration tests

1. **MCP image round-trip**: `tether_send` with image → blob written to workspace → agent reads blob → LLM responds about image
2. **MCP image egress**: Agent tool produces image → `PutBlob` → ref in `assistant.done` → `tether_read` reads blob → returns MCP image content
3. **Blob dedup**: Send same image twice → single blob file, two refs pointing to it
4. **Oversized image**: `tether_send` with 15 MB image → error before blob write
5. **Too many images**: `tether_send` with 5 images → error
6. **No workspace**: `tether_send` with image to instance without workspace → error: "images require workspace mapping"
7. **Missing blob graceful degradation**: Delete blob file → agent assembles context → image block silently dropped, text preserved
8. **Backward compatibility**: `tether_send` without images → unchanged behavior
9. **Session persistence**: Send image → restart VM → session loads with blob refs → `assembleContext` reads blobs → LLM sees images
10. **Key validation**: `blobStore.Get("../../etc/passwd")` → error (invalid key format)
11. **Blob dedup across messages**: Send same image in two different messages → single file on disk

### Gateway tests (manual for v1)

10. **Telegram photo in**: Send photo → blob in workspace → agent describes image
11. **Telegram image out**: Agent generates chart → blob → photo in Telegram, then text
12. **Telegram photo too large**: Send huge photo → gateway rejects before blob write

---

## 11. Comparison with v1 (inline base64)

| Concern | v1 (inline base64) | v2 (workspace blobs) |
|---------|-------------------|---------------------|
| Frame size | Up to ~26 MB per frame | ~200 bytes extra per ref |
| Scanner buffers | Bump to 32 MB in 3 places | No changes |
| Tether store memory | Byte budget eviction needed | Unchanged (frames are tiny) |
| Session JSONL | Blob refs (both versions) | Same |
| Transport overhead | Base64 through vsock + HTTP | File write to shared FS |
| aegisd / harness changes | Scanner bumps, body limits | One field in instance info |
| Complexity | MaxFrameBytes constant, 4-layer enforcement | BlobStore interface + one v1 impl |
| Constraint | None | Workspace required (v1 blob store) |
| Swappable storage | No (bytes baked into protocol) | Yes (swap BlobStore impl, payload unchanged) |
