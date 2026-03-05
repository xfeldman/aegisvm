package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xfeldman/aegisvm/internal/cron"
)

// Tether frame types (shared between gateway core and channels)

type TetherFrame struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	TS      string          `json:"ts,omitempty"`
	Session SessionID       `json:"session"`
	MsgID   string          `json:"msg_id,omitempty"`
	Seq     int64           `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type SessionID struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
}

// Config types

type LegacyConfig struct {
	Telegram    TelegramConfig `json:"telegram"`
	AegisSocket string         `json:"aegis_socket"`
}

type TelegramConfig struct {
	BotToken     string   `json:"bot_token"`
	BotTokenEnv  string   `json:"bot_token_env"`
	Instance     string   `json:"instance"`
	AllowedChats []string `json:"allowed_chats"`
}

type InstanceGatewayConfig struct {
	Telegram TelegramConfig `json:"telegram"`
}

// Telegram API types

type telegramUpdate struct {
	UpdateID int              `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID int               `json:"message_id"`
	Chat      telegramChat      `json:"chat"`
	Text      string            `json:"text"`
	Caption   string            `json:"caption"`
	Photo     []telegramPhoto   `json:"photo"`
	Document  *telegramDocument `json:"document"`
	From      *telegramUser     `json:"from"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type telegramPhoto struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int    `json:"file_size"`
}

type telegramDocument struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type"`
	FileSize int    `json:"file_size"`
}

// Reply tracking

type activeReply struct {
	chatID       int64
	messageID    int
	text         string
	lastDraft    time.Time
	typingCancel func()
}

// Image helpers

type imageRefPayload struct {
	MediaType string `json:"media_type"`
	Blob      string `json:"blob"`
	Size      int64  `json:"size"`
}

// Cron types

type gwCronEntry struct {
	ID       string
	Schedule *cron.Schedule
	Message  string
	Session  string
	Enabled  bool
}

type cronState struct {
	lastFiredMinute string // "2006-01-02T15:04" — dedup within same minute
}

// Utility functions

func parseChatID(s string) int64 {
	var id int64
	fmt.Sscanf(s, "%d", &id)
	return id
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func isImageDocument(doc *telegramDocument) bool {
	if doc == nil {
		return false
	}
	switch doc.MimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func mediaTypeFromPath(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	default:
		return "image/jpeg" // Telegram photos are JPEG by default
	}
}

func buildUserPayload(msg *telegramMessage, imageRef *imageRefPayload) map[string]interface{} {
	text := msg.Text
	if len(msg.Photo) > 0 || isImageDocument(msg.Document) {
		text = msg.Caption
	}

	p := map[string]interface{}{
		"text": text,
	}
	if msg.From != nil {
		user := map[string]interface{}{
			"id": fmt.Sprintf("%d", msg.From.ID),
		}
		if msg.From.Username != "" {
			user["username"] = msg.From.Username
		}
		if msg.From.FirstName != "" {
			user["name"] = msg.From.FirstName
		}
		p["user"] = user
	}
	if imageRef != nil {
		p["images"] = []imageRefPayload{*imageRef}
	}
	return p
}

func isChatAllowed(cfg *TelegramConfig, chatID int64) bool {
	if len(cfg.AllowedChats) == 0 {
		return true
	}
	chatStr := fmt.Sprintf("%d", chatID)
	for _, allowed := range cfg.AllowedChats {
		if allowed == "*" || allowed == chatStr {
			return true
		}
	}
	return false
}
