package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/xfeldman/aegisvm/internal/blob"
)

const draftThrottle = 200 * time.Millisecond

// TelegramChannel implements Channel for Telegram Bot API.
type TelegramChannel struct {
	deps          ChannelDeps
	mu            sync.Mutex
	config        *TelegramConfig
	execMode      map[int64]bool        // per-chat exec mode state
	activeReplies map[int64]*activeReply
	pollCancel    context.CancelFunc
	ctx           context.Context
}

func NewTelegramChannel() *TelegramChannel {
	return &TelegramChannel{
		execMode:      make(map[int64]bool),
		activeReplies: make(map[int64]*activeReply),
	}
}

func (ch *TelegramChannel) Name() string { return "telegram" }

func (ch *TelegramChannel) Start(ctx context.Context, deps ChannelDeps) error {
	ch.deps = deps
	ch.ctx = ctx
	return nil
}

func (ch *TelegramChannel) HandleFrame(frame TetherFrame) {
	ch.handleEgressFrame(frame)
}

func (ch *TelegramChannel) Reconfigure(cfg json.RawMessage) {
	var tc TelegramConfig
	if err := json.Unmarshal(cfg, &tc); err != nil {
		ch.deps.Logger.Printf("telegram: invalid config: %v", err)
		return
	}

	// Resolve bot token: literal → env var → TELEGRAM_BOT_TOKEN fallback
	if tc.BotToken == "" {
		envKey := tc.BotTokenEnv
		if envKey == "" {
			envKey = "TELEGRAM_BOT_TOKEN"
		}
		if v := os.Getenv(envKey); v != "" {
			tc.BotToken = v
		}
	}

	ch.mu.Lock()
	oldToken := ""
	if ch.config != nil {
		oldToken = ch.config.BotToken
	}
	ch.config = &tc
	ch.mu.Unlock()

	if tc.BotToken == "" {
		ch.deps.Logger.Println("telegram: no bot token configured")
		ch.stopPolling()
		return
	}

	// Restart polling if token changed or not yet started
	if tc.BotToken != oldToken || ch.pollCancel == nil {
		ch.startPolling()
	}
}

func (ch *TelegramChannel) Stop() {
	ch.stopPolling()
}

func (ch *TelegramChannel) startPolling() {
	ch.stopPolling()
	pollCtx, cancel := context.WithCancel(ch.ctx)
	ch.mu.Lock()
	ch.pollCancel = cancel
	ch.mu.Unlock()
	go ch.pollTelegram(pollCtx)
}

func (ch *TelegramChannel) stopPolling() {
	ch.mu.Lock()
	if ch.pollCancel != nil {
		ch.pollCancel()
		ch.pollCancel = nil
	}
	ch.mu.Unlock()
}

// Telegram polling

func (ch *TelegramChannel) pollTelegram(ctx context.Context) {
	offset := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ch.mu.Lock()
		cfg := ch.config
		ch.mu.Unlock()
		if cfg == nil || cfg.BotToken == "" {
			return
		}

		updates, err := ch.getUpdates(cfg.BotToken, offset, 30)
		if err != nil {
			ch.deps.Logger.Printf("telegram getUpdates: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message != nil && (u.Message.Text != "" || len(u.Message.Photo) > 0 || isImageDocument(u.Message.Document)) {
				go ch.handleTelegramMessage(u.Message)
			}
		}
	}
}

func (ch *TelegramChannel) getUpdates(botToken string, offset, timeout int) ([]telegramUpdate, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d",
		botToken, offset, timeout)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool             `json:"ok"`
		Result []telegramUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram API returned ok=false")
	}
	return result.Result, nil
}

func (ch *TelegramChannel) handleTelegramMessage(msg *telegramMessage) {
	chatID := msg.Chat.ID

	ch.mu.Lock()
	cfg := ch.config
	ch.mu.Unlock()
	if cfg == nil {
		return
	}

	// Check allowed chats
	if !isChatAllowed(cfg, chatID) {
		ch.deps.Logger.Printf("chat %d not allowed, ignoring", chatID)
		return
	}

	// Exec mode commands
	text := msg.Text
	if text == "/exec" {
		ch.mu.Lock()
		ch.execMode[chatID] = true
		ch.mu.Unlock()
		ch.sendTelegramMessage(chatID, "⚡ Exec mode. Each command runs in a fresh shell.\nUse && to chain: cd /workspace && ls -la\nSend /exit to return to chat.", cfg.BotToken)
		return
	}
	if text == "/exit" {
		ch.mu.Lock()
		wasExec := ch.execMode[chatID]
		delete(ch.execMode, chatID)
		ch.mu.Unlock()
		if wasExec {
			ch.sendTelegramMessage(chatID, "💬 Back to chat mode.", cfg.BotToken)
			return
		}
	}

	ch.mu.Lock()
	inExec := ch.execMode[chatID]
	ch.mu.Unlock()
	if inExec && text != "" {
		go ch.handleExecMessage(chatID, text, cfg.BotToken)
		return
	}

	// Normal tether routing
	instanceID := cfg.Instance
	if instanceID == "" {
		instanceID = ch.deps.InstanceHandle
	}
	if instanceID == "" {
		ch.deps.Logger.Printf("no instance configured for gateway")
		return
	}

	// Start typing indicator
	typingCtx, typingCancel := context.WithCancel(context.Background())
	go ch.sendTypingLoop(typingCtx, chatID, cfg.BotToken)

	// Handle photo ingress
	var imageRef *imageRefPayload
	if fileID, isImage := ch.extractImageFileID(msg); isImage {
		workspace := ch.deps.WorkspacePath()
		if workspace == "" {
			typingCancel()
			ch.sendTelegramMessage(chatID, "Images require workspace mapping on the agent instance.", cfg.BotToken)
			return
		}

		photoBytes, mediaType, err := ch.downloadTelegramFile(cfg.BotToken, fileID)
		if err != nil {
			ch.deps.Logger.Printf("download telegram file: %v", err)
			typingCancel()
			ch.sendTelegramMessage(chatID, fmt.Sprintf("Failed to download image: %v", err), cfg.BotToken)
			return
		}

		blobStore := ch.deps.BlobStore()
		if blobStore == nil {
			typingCancel()
			ch.sendTelegramMessage(chatID, "Images require workspace mapping on the agent instance.", cfg.BotToken)
			return
		}
		key, err := blobStore.Put(photoBytes, mediaType)
		if err != nil {
			ch.deps.Logger.Printf("blob store put: %v", err)
			typingCancel()
			ch.sendTelegramMessage(chatID, "Failed to process image.", cfg.BotToken)
			return
		}
		imageRef = &imageRefPayload{MediaType: mediaType, Blob: key, Size: int64(len(photoBytes))}
		ch.deps.Logger.Printf("photo ingress: %s (%d bytes) → %s", mediaType, len(photoBytes), key)
	}

	// Build and send tether frame
	frame := TetherFrame{
		V:    1,
		Type: "user.message",
		TS:   time.Now().UTC().Format(time.RFC3339Nano),
		Session: SessionID{
			Channel: "telegram",
			ID:      fmt.Sprintf("%d", chatID),
		},
		MsgID:   fmt.Sprintf("tg-%d-%d", chatID, msg.MessageID),
		Seq:     int64(msg.MessageID),
		Payload: mustMarshal(buildUserPayload(msg, imageRef)),
	}

	if err := ch.deps.SendTether(frame); err != nil {
		ch.deps.Logger.Printf("tether ingress failed: %v", err)
		typingCancel()
		ch.sendTelegramMessage(chatID, "Failed to deliver message to agent.", cfg.BotToken)
		return
	}

	// Store typing cancel so the egress handler can stop it on assistant.done
	ch.mu.Lock()
	ch.activeReplies[chatID] = &activeReply{
		chatID:       chatID,
		typingCancel: typingCancel,
	}
	ch.mu.Unlock()
}

// Exec mode handler

func (ch *TelegramChannel) handleExecMessage(chatID int64, text, botToken string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// Wrap in sh -c so builtins (cd, export), pipes, and redirects work.
	// This is a raw terminal mode — the user is intentionally typing shell commands.
	command := []string{"sh", "-c", text}

	// Start typing indicator
	typingCtx, typingCancel := context.WithCancel(context.Background())
	defer typingCancel()
	go ch.sendTypingLoop(typingCtx, chatID, botToken)

	body, err := ch.deps.Exec(command)
	if err != nil {
		ch.sendTelegramMessage(chatID, "exec failed: "+err.Error(), botToken)
		return
	}
	defer body.Close()

	// Stream output via sendMessageDraft, finalize with sendMessage
	var output strings.Builder
	var lastDraft time.Time

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		var entry struct {
			Line     string `json:"line"`
			Stream   string `json:"stream"`
			ExecID   string `json:"exec_id"`
			ExitCode int    `json:"exit_code"`
			Done     bool   `json:"done"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		if entry.Done {
			typingCancel()
			msg := formatExecOutput(output.String(), entry.ExitCode)
			ch.sendTelegramMessage(chatID, msg, botToken)
			return
		}

		if entry.Line != "" {
			output.WriteString(entry.Line)
			output.WriteRune('\n')
			// Stream draft if throttle elapsed and we have content
			if time.Since(lastDraft) >= draftThrottle && output.Len() > 0 {
				draft := formatExecDraft(output.String())
				ch.sendMessageDraft(chatID, draft, botToken)
				lastDraft = time.Now()
			}
		}
	}

	// If we get here without a done entry, send whatever we have
	if output.Len() > 0 {
		ch.sendTelegramMessage(chatID, formatExecOutput(output.String(), -1), botToken)
	}
}

func formatExecOutput(output string, exitCode int) string {
	const maxLen = 4000

	if output == "" {
		if exitCode != 0 {
			return fmt.Sprintf("⚠️ exit: %d", exitCode)
		}
		return "exit: 0"
	}

	if len(output) > maxLen {
		output = output[:maxLen] + "\n... (truncated)"
	}

	msg := "```\n" + output + "\n```\nexit: " + fmt.Sprintf("%d", exitCode)
	if exitCode != 0 {
		msg = "```\n" + output + "\n```\n⚠️ exit: " + fmt.Sprintf("%d", exitCode)
	}
	return msg
}

func formatExecDraft(output string) string {
	const maxLen = 4000
	if len(output) > maxLen {
		output = output[len(output)-maxLen:]
	}
	return "```\n" + output + "\n```"
}

// Egress frame handler

func (ch *TelegramChannel) handleEgressFrame(frame TetherFrame) {
	chatID := parseChatID(frame.Session.ID)
	if chatID == 0 {
		return
	}

	ch.mu.Lock()
	cfg := ch.config
	ch.mu.Unlock()
	if cfg == nil {
		return
	}
	botToken := cfg.BotToken

	switch frame.Type {
	case "assistant.delta":
		var payload struct {
			Text string `json:"text"`
		}
		json.Unmarshal(frame.Payload, &payload)
		if payload.Text == "" {
			return
		}

		ch.mu.Lock()
		reply, ok := ch.activeReplies[chatID]
		if !ok {
			ch.mu.Unlock()
			return
		}
		if reply.messageID == 0 {
			// First delta — send a new message
			typingCancel := reply.typingCancel
			ch.mu.Unlock()
			msgID := ch.sendTelegramMessagePlain(chatID, payload.Text, botToken)
			ch.mu.Lock()
			ch.activeReplies[chatID] = &activeReply{
				chatID:       chatID,
				messageID:    msgID,
				text:         payload.Text,
				lastDraft:    time.Now(),
				typingCancel: typingCancel,
			}
			ch.mu.Unlock()
			return
		}

		reply.text += payload.Text
		if time.Since(reply.lastDraft) >= draftThrottle {
			text := reply.text
			msgID := reply.messageID
			reply.lastDraft = time.Now()
			ch.mu.Unlock()
			ch.editTelegramMessagePlain(chatID, msgID, text, botToken)
		} else {
			ch.mu.Unlock()
		}

	case "assistant.done":
		var payload struct {
			Text   string `json:"text"`
			Images []struct {
				MediaType string `json:"media_type"`
				Blob      string `json:"blob"`
				Size      int64  `json:"size"`
			} `json:"images"`
		}
		json.Unmarshal(frame.Payload, &payload)

		ch.mu.Lock()
		reply, ok := ch.activeReplies[chatID]
		if !ok {
			// Unsolicited message (e.g., cron result). Send as new message.
			ch.mu.Unlock()
			if payload.Text != "" {
				ch.sendTelegramMessage(chatID, payload.Text, botToken)
			}
			return
		}
		if reply.typingCancel != nil {
			reply.typingCancel()
		}

		// Handle images in response
		if len(payload.Images) > 0 {
			msgID := reply.messageID
			delete(ch.activeReplies, chatID)
			ch.mu.Unlock()

			// Finalize streaming message with text
			if msgID != 0 && payload.Text != "" {
				ch.editTelegramMessage(chatID, msgID, payload.Text, botToken)
			}

			// Send images from blob store
			textUsedAsCaption := false
			workspace := ch.deps.WorkspacePath()
			if workspace != "" {
				blobStore := blob.NewWorkspaceBlobStore(workspace)
				for i, img := range payload.Images {
					imgData, err := blobStore.Get(img.Blob)
					if err != nil {
						ch.deps.Logger.Printf("egress blob read %s: %v", img.Blob, err)
						continue
					}
					caption := ""
					if i == 0 && msgID == 0 && payload.Text != "" && len(payload.Text) <= 1024 {
						caption = payload.Text
						textUsedAsCaption = true
					}
					ch.sendTelegramPhoto(chatID, imgData, caption, botToken)
				}
			}

			if !textUsedAsCaption && msgID == 0 && payload.Text != "" {
				ch.sendTelegramMessage(chatID, payload.Text, botToken)
			}
			return
		}

		// Text-only response — edit the streamed message with final formatted text
		if reply.messageID != 0 {
			msgID := reply.messageID
			delete(ch.activeReplies, chatID)
			ch.mu.Unlock()
			if payload.Text != "" {
				ch.editTelegramMessage(chatID, msgID, payload.Text, botToken)
			}
		} else {
			delete(ch.activeReplies, chatID)
			ch.mu.Unlock()
			if payload.Text != "" {
				ch.sendTelegramMessage(chatID, payload.Text, botToken)
			}
		}

	case "status.presence":
		ch.sendChatAction(chatID, "typing", botToken)
	}
}

// Telegram API methods

func (ch *TelegramChannel) sendTelegramMessage(chatID int64, text, botToken string) int {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"text":       commonmarkToTelegramV2(text),
		"parse_mode": "MarkdownV2",
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		ch.deps.Logger.Printf("sendMessage: %v", err)
		return 0
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Result.MessageID
}

func (ch *TelegramChannel) sendMessageDraft(chatID int64, text, botToken string) int {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessageDraft", botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"text":       commonmarkToTelegramV2(text),
		"parse_mode": "MarkdownV2",
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		ch.deps.Logger.Printf("sendMessageDraft: %v", err)
		return 0
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Result.MessageID
}

func (ch *TelegramChannel) editTelegramMessage(chatID int64, messageID int, text, botToken string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       commonmarkToTelegramV2(text),
		"parse_mode": "MarkdownV2",
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		ch.deps.Logger.Printf("editMessageText: %v", err)
		return
	}
	resp.Body.Close()
}

// Plain-text variants for streaming deltas. Partial markdown (e.g., unclosed **)
// breaks MarkdownV2 parsing, so deltas are sent as plain text. Only the final
// assistant.done message gets MarkdownV2 formatting.

func (ch *TelegramChannel) sendTelegramMessagePlain(chatID int64, text, botToken string) int {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		ch.deps.Logger.Printf("sendMessage: %v", err)
		return 0
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Result.MessageID
}

func (ch *TelegramChannel) editTelegramMessagePlain(chatID int64, messageID int, text, botToken string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		ch.deps.Logger.Printf("editMessageText: %v", err)
		return
	}
	resp.Body.Close()
}

func (ch *TelegramChannel) sendChatAction(chatID int64, action, botToken string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id": chatID,
		"action":  action,
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}

func (ch *TelegramChannel) sendTypingLoop(ctx context.Context, chatID int64, botToken string) {
	timeout := time.After(2 * time.Minute)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	ch.sendChatAction(chatID, "typing", botToken)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			return
		case <-ticker.C:
			ch.sendChatAction(chatID, "typing", botToken)
		}
	}
}

func (ch *TelegramChannel) sendTelegramPhoto(chatID int64, photoBytes []byte, caption, botToken string) int {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", botToken)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("chat_id", fmt.Sprintf("%d", chatID))
	if caption != "" {
		if len(caption) > 1024 {
			caption = caption[:1024]
		}
		w.WriteField("caption", caption)
	}
	part, err := w.CreateFormFile("photo", "image.jpg")
	if err != nil {
		ch.deps.Logger.Printf("sendPhoto: create form: %v", err)
		return 0
	}
	part.Write(photoBytes)
	w.Close()

	resp, err := http.Post(url, w.FormDataContentType(), &buf)
	if err != nil {
		ch.deps.Logger.Printf("sendPhoto: %v", err)
		return 0
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.OK {
		respBody, _ := io.ReadAll(resp.Body)
		ch.deps.Logger.Printf("sendPhoto: API error: %s", string(respBody))
	}
	return result.Result.MessageID
}

// File download helpers

func (ch *TelegramChannel) getFilePath(botToken, fileID string) (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", botToken, fileID)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
			FileSize int    `json:"file_size"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK || result.Result.FilePath == "" {
		return "", fmt.Errorf("getFile failed for %s", fileID)
	}
	if result.Result.FileSize > blob.MaxImageBytes {
		return "", fmt.Errorf("file too large: %d bytes (max %d)", result.Result.FileSize, blob.MaxImageBytes)
	}
	return result.Result.FilePath, nil
}

func (ch *TelegramChannel) downloadTelegramFile(botToken, fileID string) ([]byte, string, error) {
	filePath, err := ch.getFilePath(botToken, fileID)
	if err != nil {
		return nil, "", err
	}

	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", botToken, filePath)
	resp, err := http.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(blob.MaxImageBytes)+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > blob.MaxImageBytes {
		return nil, "", fmt.Errorf("downloaded file too large: %d bytes", len(data))
	}

	mediaType := mediaTypeFromPath(filePath)
	return data, mediaType, nil
}

func (ch *TelegramChannel) extractImageFileID(msg *telegramMessage) (string, bool) {
	if len(msg.Photo) > 0 {
		return msg.Photo[len(msg.Photo)-1].FileID, true
	}
	if isImageDocument(msg.Document) {
		return msg.Document.FileID, true
	}
	return "", false
}

// shlexSplit splits a string into tokens, respecting single and double quotes.
// "grep 'hello world' file.txt" → ["grep", "hello world", "file.txt"]
func shlexSplit(s string) []string {
	var tokens []string
	var current strings.Builder
	inSingle := false
	inDouble := false

	for _, r := range s {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case unicode.IsSpace(r) && !inSingle && !inDouble:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}
