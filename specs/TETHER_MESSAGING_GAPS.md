# Tether Messaging Feature Gaps

**Status:** Draft
**Date:** 2026-02-26
**Context:** Tether currently carries text + images + sender identity. To fully bridge messaging platforms (Telegram, WhatsApp, Slack, Discord) without losing platform features, the tether payload needs additional fields. This document maps what's missing.

---

## 1. Current Tether Payload

### `user.message` payload (today)

```json
{
  "text": "Hello",
  "images": [{"media_type": "image/png", "blob": "abc...def.png", "size": 1024}],
  "user": {"id": "123", "name": "Alice", "username": "alice"}
}
```

### `assistant.done` payload (today)

```json
{
  "text": "Here's the answer...",
  "images": [{"media_type": "image/png", "blob": "fed...cba.png", "size": 2048}]
}
```

### Session ID (today)

```json
{"channel": "telegram", "id": "12345"}
```

---

## 2. Feature Gap Map

| Feature | Telegram | WhatsApp | Slack | Discord | Tether today | Priority |
|---------|----------|----------|-------|---------|-------------|----------|
| Text messages | Yes | Yes | Yes | Yes | **Yes** | — |
| Images | Yes | Yes | Yes | Yes | **Yes** | — |
| Sender identity | Yes | Yes | Yes | Yes | **Yes** | — |
| Group vs DM | Yes | Yes | Yes | Yes | **No** | High |
| Reply / thread | Yes | Yes | Yes | Yes | **No** | High |
| Voice messages | Yes | Yes | No | Yes | **No** | Medium |
| Files / documents | Yes | Yes | Yes | Yes | **No** | Medium |
| Reactions | Yes | Yes | Yes | Yes | **No** | Low |
| Message edits | Yes | No | Yes | Yes | **No** | Low |
| Message deletes | Yes | Yes | Yes | Yes | **No** | Low |
| Stickers | Yes | Yes | No | Yes | **No** | Low |
| Location | Yes | Yes | No | No | **No** | Low |
| Contacts | Yes | Yes | No | No | **No** | Low |
| Inline buttons | Yes | No | Yes | Yes | **No** | Low |
| Polls | Yes | No | No | Yes | **No** | Low |
| Typing indicator | Yes | Yes | Yes | Yes | **Yes** (status.presence) | — |

---

## 3. High Priority: Group Chat Support

### What's needed

The agent needs to know:
1. **Is this a group or DM?** — affects behavior (e.g., only respond when mentioned in groups)
2. **Group identity** — which group the message came from
3. **Mention detection** — was the bot @mentioned? (gateway should handle this, not the agent)

### Proposed payload addition

```json
{
  "text": "...",
  "user": {"id": "123", "name": "Alice"},
  "chat": {
    "type": "group",
    "title": "Project Alpha"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `chat.type` | string | `"dm"` or `"group"` |
| `chat.title` | string | Group name (empty for DMs) |

### Gateway behavior for groups

The gateway should filter before sending to tether:
- **Mention gating:** Only forward messages where the bot is @mentioned (or replies to bot messages)
- **Strip mention:** Remove `@botname` prefix from text before forwarding
- The agent doesn't need to know about mention mechanics — it just receives messages that are addressed to it

This keeps the agent side simple. Group awareness is just `chat.type` for context in the system prompt.

---

## 4. High Priority: Reply / Thread Support

### What's needed

1. **Ingress:** User replies to a specific bot message → agent should know which message is being replied to
2. **Egress:** Agent can request its response be sent as a reply to the user's message (preserves conversation threading in groups)

### Proposed payload addition

**Ingress (`user.message`):**
```json
{
  "text": "Can you explain more?",
  "reply_to": "tg-12345-42"
}
```

`reply_to` references a `msg_id` from a previous tether frame. The gateway maps platform-specific reply IDs to tether msg_ids.

**Egress (`assistant.done`):**
```json
{
  "text": "Sure, here's more detail...",
  "reply_to": "tg-12345-99"
}
```

Gateway sends the response as a Telegram reply to the referenced message.

### Threading model

Tether sessions already provide conversation isolation. Reply support adds **intra-session** message linking. This is important for groups where multiple conversations interleave.

---

## 5. Medium Priority: Voice Messages

### What's needed

Voice messages are common on Telegram and WhatsApp. The agent should be able to receive and respond with audio.

### Approach

Same pattern as images — blob store + refs:

```json
{
  "text": "",
  "audio": [{"media_type": "audio/ogg", "blob": "abc...def.ogg", "size": 50000, "duration": 5}]
}
```

Gateway downloads voice message → writes blob → sends ref. Agent can transcribe (via LLM or Whisper) and respond.

**Egress:** Agent produces audio (TTS) → blob → gateway sends as voice message.

Blob store already supports any file type by design — just needs extension mapping for audio (`audio/ogg` → `.ogg`, `audio/mp4` → `.m4a`).

---

## 6. Medium Priority: Files / Documents

### What's needed

Users send PDFs, spreadsheets, code files. Agent should be able to receive and work with them.

### Approach

New payload field:

```json
{
  "text": "Check this report",
  "files": [{"media_type": "application/pdf", "blob": "abc...def.pdf", "size": 500000, "filename": "report.pdf"}]
}
```

Blob store handles storage. The `filename` field preserves the original name for the agent to use. Blob key includes a file-type extension derived from media type.

Blob store `extForMediaType` needs expansion beyond images: `.pdf`, `.csv`, `.json`, `.txt`, `.zip`, etc.

---

## 7. Low Priority: Reactions

### What's needed

1. **Ingress:** User reacts to a bot message with an emoji
2. **Egress:** Agent reacts to a user message

### Proposed frame type

New frame types `user.reaction` and `assistant.reaction`:

```json
{
  "type": "user.reaction",
  "payload": {
    "msg_id": "tg-12345-42",
    "emoji": "👍"
  }
}
```

Not part of `user.message` — reactions are events on existing messages, not new messages.

---

## 8. Low Priority: Message Edits and Deletes

### Edits

New frame type `user.edit`:

```json
{
  "type": "user.edit",
  "payload": {
    "msg_id": "tg-12345-42",
    "text": "corrected text"
  }
}
```

Agent can choose to acknowledge the correction or ignore it.

### Deletes

New frame type `user.delete`:

```json
{
  "type": "user.delete",
  "payload": {
    "msg_id": "tg-12345-42"
  }
}
```

---

## 9. Implementation Order

| Phase | Features | Rationale |
|-------|----------|-----------|
| **v1 (now)** | Text, images, sender identity | Done |
| **v2** | Group chat (`chat.type`, `chat.title`), reply/thread (`reply_to`) | Needed for meaningful group conversations |
| **v3** | Voice messages, files/documents | Extends blob store pattern, high user demand |
| **v4** | Reactions, edits, deletes | Nice-to-have, low agent utility |
| **Deferred** | Stickers, location, contacts, inline buttons, polls | Platform-specific UI, low cross-platform value |

### Design principle

Each feature follows the same pattern:
1. **Gateway** handles platform-specific download/upload/API calls
2. **Tether** carries normalized payloads with blob refs
3. **Agent** receives platform-agnostic data, responds with platform-agnostic data
4. **Gateway** translates back to platform-specific format

The agent never needs to know it's talking to Telegram vs WhatsApp. The gateway is the only place where platform differences exist.
