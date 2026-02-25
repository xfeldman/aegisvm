// aegis-gateway is the host-side messaging adapter for the Aegis Agent Kit.
//
// It bridges external messaging channels (Telegram) with Aegis instances
// via the tether protocol, enabling wake-on-message and streaming UX.
//
// Lifecycle: spawned by aegisd per instance (via kit manifest instance_daemons).
// Receives AEGIS_INSTANCE env var to identify which instance to route to.
// Reads per-instance config from ~/.aegis/kits/{handle}/gateway.json.
// Hot-reloads config on file change. Idles if no config found.
//
// Build: CGO_ENABLED=0 go build -o aegis-gateway ./cmd/aegis-gateway
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xfeldman/aegisvm/internal/blob"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	instanceHandle := os.Getenv("AEGIS_INSTANCE")
	socketPath := os.Getenv("AEGIS_SOCKET")
	if socketPath == "" {
		socketPath = defaultSocketPath()
	}

	if instanceHandle == "" {
		// Backward compat: no AEGIS_INSTANCE means legacy singleton mode
		log.Println("aegis-gateway starting (legacy mode)")
		runLegacy(socketPath)
		return
	}

	log.Printf("aegis-gateway starting for instance %q", instanceHandle)

	client := newAegisClient(socketPath)

	gw := &Gateway{
		instanceHandle: instanceHandle,
		aegisClient:    client,
		activeReplies:  make(map[int64]*activeReply),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handling: SIGTERM/SIGINT = shutdown, SIGHUP = reload config
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	// Start config watcher + gateway loop
	go gw.run(ctx)

	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				log.Println("SIGHUP received, reloading config")
				gw.reloadConfig()
			default:
				log.Println("aegis-gateway stopping")
				cancel()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// runLegacy handles the old singleton gateway mode (no AEGIS_INSTANCE env).
// Reads config from AEGIS_GATEWAY_CONFIG or ~/.aegis/gateway.json.
func runLegacy(socketPath string) {
	cfg := loadLegacyConfig(socketPath)
	client := newAegisClient(cfg.AegisSocket)

	// Resolve bot token
	if cfg.Telegram.BotToken == "" && cfg.Telegram.BotTokenSecret != "" {
		val, err := client.getSecret(cfg.Telegram.BotTokenSecret)
		if err != nil {
			log.Fatalf("resolve bot token secret %q: %v", cfg.Telegram.BotTokenSecret, err)
		}
		cfg.Telegram.BotToken = val
		log.Printf("bot token resolved from secret %q", cfg.Telegram.BotTokenSecret)
	}
	if cfg.Telegram.BotToken == "" {
		log.Fatal("telegram bot token not configured")
	}

	gw := &Gateway{
		instanceHandle: cfg.Telegram.Instance,
		aegisClient:    client,
		activeReplies:  make(map[int64]*activeReply),
		currentConfig:  &cfg.Telegram,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go gw.subscribeEgress(ctx)
	go gw.pollTelegram(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	log.Println("aegis-gateway stopping")
	cancel()
}

// Config types

type LegacyConfig struct {
	Telegram    TelegramConfig `json:"telegram"`
	AegisSocket string         `json:"aegis_socket"`
}

type TelegramConfig struct {
	BotToken       string   `json:"bot_token"`
	BotTokenSecret string   `json:"bot_token_secret"`
	Instance       string   `json:"instance"`
	AllowedChats   []string `json:"allowed_chats"`
}

// InstanceGatewayConfig is the per-instance config at ~/.aegis/kits/{handle}/gateway.json.
type InstanceGatewayConfig struct {
	Telegram TelegramConfig `json:"telegram"`
}

// Gateway

type Gateway struct {
	instanceHandle string
	aegisClient    *aegisClient
	mu             sync.Mutex
	activeReplies  map[int64]*activeReply // chatID → in-progress reply
	currentConfig  *TelegramConfig        // nil = no config loaded
	configMtime    time.Time              // last known mtime of config file
	pollCancel     context.CancelFunc     // cancel current polling+egress
	reloadCh       chan struct{}           // signal immediate config reload
	workspacePath  string                 // cached host workspace path (resolved once)
}

type activeReply struct {
	chatID       int64
	messageID    int
	text         string
	lastEdit     time.Time
	typingCancel context.CancelFunc
}

// run is the main loop: watch config, start/stop polling as config appears/disappears.
func (gw *Gateway) run(ctx context.Context) {
	gw.reloadCh = make(chan struct{}, 1)

	// Try initial config load
	gw.reloadConfig()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			gw.stopPolling()
			return
		case <-gw.reloadCh:
			gw.applyConfig(ctx)
		case <-ticker.C:
			gw.checkConfigChange(ctx)
		}
	}
}

// configPath returns the per-instance config file path.
func (gw *Gateway) configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "kits", gw.instanceHandle, "gateway.json")
}

// reloadConfig signals an immediate config reload.
func (gw *Gateway) reloadConfig() {
	select {
	case gw.reloadCh <- struct{}{}:
	default:
	}
}

// checkConfigChange checks if the config file has changed (mtime-based).
func (gw *Gateway) checkConfigChange(ctx context.Context) {
	path := gw.configPath()
	info, err := os.Stat(path)

	gw.mu.Lock()
	hadConfig := gw.currentConfig != nil
	oldMtime := gw.configMtime
	gw.mu.Unlock()

	if err != nil {
		// File doesn't exist
		if hadConfig {
			log.Printf("config file removed, stopping polling")
			gw.stopPolling()
			gw.mu.Lock()
			gw.currentConfig = nil
			gw.configMtime = time.Time{}
			gw.mu.Unlock()
		}
		return
	}

	if info.ModTime() != oldMtime {
		gw.applyConfig(ctx)
	}
}

// applyConfig reads the config file and starts/restarts polling if changed.
func (gw *Gateway) applyConfig(ctx context.Context) {
	path := gw.configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("read config: %v", err)
		}
		return
	}

	info, _ := os.Stat(path)

	var cfg InstanceGatewayConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("parse config (keeping last-known-good): %v", err)
		return
	}

	// Resolve bot token from secret store
	tc := cfg.Telegram
	if tc.BotToken == "" && tc.BotTokenSecret != "" {
		val, err := gw.aegisClient.getSecret(tc.BotTokenSecret)
		if err != nil {
			log.Printf("resolve bot token secret %q: %v (keeping last-known-good)", tc.BotTokenSecret, err)
			return
		}
		tc.BotToken = val
		log.Printf("bot token resolved from secret %q", tc.BotTokenSecret)
	}

	if tc.BotToken == "" {
		log.Printf("config has no bot token, waiting...")
		return
	}

	// Set instance handle from env (override any value in config)
	tc.Instance = gw.instanceHandle

	gw.mu.Lock()
	changed := gw.currentConfig == nil || gw.currentConfig.BotToken != tc.BotToken
	gw.currentConfig = &tc
	if info != nil {
		gw.configMtime = info.ModTime()
	}
	gw.mu.Unlock()

	if changed {
		// Restart polling with new config
		gw.stopPolling()
		pollCtx, pollCancel := context.WithCancel(ctx)
		gw.mu.Lock()
		gw.pollCancel = pollCancel
		gw.mu.Unlock()
		log.Printf("config loaded, starting Telegram polling")
		go gw.subscribeEgress(pollCtx)
		go gw.pollTelegram(pollCtx)
	} else {
		log.Printf("config reloaded (no bot token change)")
	}
}

// stopPolling stops the current polling and egress subscription.
func (gw *Gateway) stopPolling() {
	gw.mu.Lock()
	if gw.pollCancel != nil {
		gw.pollCancel()
		gw.pollCancel = nil
	}
	gw.mu.Unlock()
}

// TetherFrame for gateway use.
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

// Telegram polling

func (gw *Gateway) pollTelegram(ctx context.Context) {
	offset := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		gw.mu.Lock()
		cfg := gw.currentConfig
		gw.mu.Unlock()
		if cfg == nil {
			return
		}

		updates, err := gw.getUpdates(cfg.BotToken, offset, 30)
		if err != nil {
			log.Printf("telegram getUpdates: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message != nil && (u.Message.Text != "" || len(u.Message.Photo) > 0 || isImageDocument(u.Message.Document)) {
				go gw.handleTelegramMessage(u.Message)
			}
		}
	}
}

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

func (gw *Gateway) getUpdates(botToken string, offset, timeout int) ([]telegramUpdate, error) {
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

func (gw *Gateway) handleTelegramMessage(msg *telegramMessage) {
	chatID := msg.Chat.ID

	gw.mu.Lock()
	cfg := gw.currentConfig
	gw.mu.Unlock()
	if cfg == nil {
		return
	}

	// Check allowed chats
	if !isChatAllowed(cfg, chatID) {
		log.Printf("chat %d not allowed, ignoring", chatID)
		return
	}

	instanceID := cfg.Instance
	if instanceID == "" {
		log.Printf("no instance configured for gateway")
		return
	}

	// Start typing indicator
	typingCtx, typingCancel := context.WithCancel(context.Background())
	go gw.sendTypingLoop(typingCtx, chatID, cfg.BotToken)

	// Handle photo ingress: download, write to blob store, build image ref
	var imageRef *imageRefPayload
	if fileID, isImage := gw.extractImageFileID(msg); isImage {
		workspace := gw.resolveWorkspace()
		if workspace == "" {
			typingCancel()
			gw.sendTelegramMessage(chatID, "Images require workspace mapping on the agent instance.", cfg.BotToken)
			return
		}

		photoBytes, mediaType, err := gw.downloadTelegramFile(cfg.BotToken, fileID)
		if err != nil {
			log.Printf("download telegram file: %v", err)
			typingCancel()
			gw.sendTelegramMessage(chatID, fmt.Sprintf("Failed to download image: %v", err), cfg.BotToken)
			return
		}

		blobStore := blob.NewWorkspaceBlobStore(workspace)
		key, err := blobStore.Put(photoBytes, mediaType)
		if err != nil {
			log.Printf("blob store put: %v", err)
			typingCancel()
			gw.sendTelegramMessage(chatID, "Failed to process image.", cfg.BotToken)
			return
		}
		imageRef = &imageRefPayload{MediaType: mediaType, Blob: key, Size: int64(len(photoBytes))}
		log.Printf("photo ingress: %s (%d bytes) → %s", mediaType, len(photoBytes), key)
	}

	// Build and send tether frame (wake-on-message happens inside aegisd)
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

	if err := gw.aegisClient.postTetherFrame(instanceID, frame); err != nil {
		log.Printf("tether ingress failed: %v", err)
		typingCancel()
		gw.sendTelegramMessage(chatID, "Failed to deliver message to agent.", cfg.BotToken)
		return
	}

	// Store typing cancel so the egress handler can stop it on assistant.done
	gw.mu.Lock()
	gw.activeReplies[chatID] = &activeReply{
		chatID:       chatID,
		typingCancel: typingCancel,
	}
	gw.mu.Unlock()
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

func (gw *Gateway) sendTypingLoop(ctx context.Context, chatID int64, botToken string) {
	timeout := time.After(2 * time.Minute)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	gw.sendChatAction(chatID, "typing", botToken)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			return
		case <-ticker.C:
			gw.sendChatAction(chatID, "typing", botToken)
		}
	}
}

// Egress subscriber

func (gw *Gateway) subscribeEgress(ctx context.Context) {
	gw.mu.Lock()
	cfg := gw.currentConfig
	gw.mu.Unlock()
	if cfg == nil {
		return
	}
	instanceID := cfg.Instance

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Printf("egress: connecting to %s tether stream...", instanceID)
		body, err := gw.aegisClient.streamTether(ctx, instanceID)
		if err != nil {
			log.Printf("egress subscribe: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("egress: connected to %s", instanceID)

		gw.processEgressStream(ctx, body)
		body.Close()

		log.Printf("egress stream ended, reconnecting...")
		time.Sleep(1 * time.Second)
	}
}

func (gw *Gateway) processEgressStream(ctx context.Context, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var frame TetherFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			log.Printf("egress: unmarshal error: %v", err)
			continue
		}

		if frame.Session.Channel != "telegram" {
			continue
		}

		gw.handleEgressFrame(frame)
	}
}

func (gw *Gateway) handleEgressFrame(frame TetherFrame) {
	chatID := parseChatID(frame.Session.ID)
	if chatID == 0 {
		return
	}

	gw.mu.Lock()
	cfg := gw.currentConfig
	gw.mu.Unlock()
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

		gw.mu.Lock()
		reply, ok := gw.activeReplies[chatID]
		if !ok {
			// No active reply — this is a replayed frame from a previous session. Ignore.
			gw.mu.Unlock()
			return
		}
		if reply.messageID == 0 {
			// First delta — send a new Telegram message
			typingCancel := reply.typingCancel
			gw.mu.Unlock()
			msgID := gw.sendTelegramMessage(chatID, payload.Text, botToken)
			gw.mu.Lock()
			gw.activeReplies[chatID] = &activeReply{
				chatID:       chatID,
				messageID:    msgID,
				text:         payload.Text,
				lastEdit:     time.Now(),
				typingCancel: typingCancel,
			}
			gw.mu.Unlock()
			return
		}

		reply.text += payload.Text
		if time.Since(reply.lastEdit) >= time.Second {
			text := reply.text
			msgID := reply.messageID
			reply.lastEdit = time.Now()
			gw.mu.Unlock()
			gw.editTelegramMessage(chatID, msgID, text, botToken)
		} else {
			gw.mu.Unlock()
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

		gw.mu.Lock()
		reply, ok := gw.activeReplies[chatID]
		if !ok {
			// No active reply — replayed frame from previous session. Ignore.
			gw.mu.Unlock()
			return
		}
		if reply.typingCancel != nil {
			reply.typingCancel()
		}

		// Handle images in response
		if len(payload.Images) > 0 {
			msgID := reply.messageID
			delete(gw.activeReplies, chatID)
			gw.mu.Unlock()

			// If there was a streaming message, finalize it with text (no images inline)
			if msgID != 0 && payload.Text != "" {
				gw.editTelegramMessage(chatID, msgID, payload.Text, botToken)
			}

			// Send images from blob store
			textUsedAsCaption := false
			workspace := gw.resolveWorkspace()
			if workspace != "" {
				blobStore := blob.NewWorkspaceBlobStore(workspace)
				for i, img := range payload.Images {
					imgData, err := blobStore.Get(img.Blob)
					if err != nil {
						log.Printf("egress blob read %s: %v", img.Blob, err)
						continue
					}
					caption := ""
					if i == 0 && msgID == 0 && payload.Text != "" && len(payload.Text) <= 1024 {
						caption = payload.Text
						textUsedAsCaption = true
					}
					gw.sendTelegramPhoto(chatID, imgData, caption, botToken)
				}
			}

			// Send text separately if it wasn't used as caption and wasn't already streamed
			if !textUsedAsCaption && msgID == 0 && payload.Text != "" {
				gw.sendTelegramMessage(chatID, payload.Text, botToken)
			}
			return
		}

		// Text-only response (existing logic)
		if reply.messageID != 0 {
			msgID := reply.messageID
			delete(gw.activeReplies, chatID)
			gw.mu.Unlock()
			if payload.Text != "" {
				gw.editTelegramMessage(chatID, msgID, payload.Text, botToken)
			}
		} else {
			delete(gw.activeReplies, chatID)
			gw.mu.Unlock()
			if payload.Text != "" {
				gw.sendTelegramMessage(chatID, payload.Text, botToken)
			}
		}

	case "status.presence":
		gw.sendChatAction(chatID, "typing", botToken)
	}
}

// Telegram API helpers

func (gw *Gateway) sendTelegramMessage(chatID int64, text, botToken string) int {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("sendMessage: %v", err)
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

func (gw *Gateway) editTelegramMessage(chatID int64, messageID int, text, botToken string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("editMessageText: %v", err)
		return
	}
	resp.Body.Close()
}

func (gw *Gateway) sendChatAction(chatID int64, action, botToken string) {
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

// Aegis client (talks to aegisd via unix socket)

type aegisClient struct {
	socketPath string
	httpClient *http.Client
}

func newAegisClient(socketPath string) *aegisClient {
	return &aegisClient{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
		},
	}
}

func (c *aegisClient) getSecret(name string) (string, error) {
	url := fmt.Sprintf("http://aegis/v1/secrets/%s", name)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("get secret: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get secret %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Value string `json:"value"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Value, nil
}

func (c *aegisClient) getInstanceWorkspace(instanceID string) (string, error) {
	url := fmt.Sprintf("http://aegis/v1/instances/%s", instanceID)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("get instance: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get instance %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Workspace string `json:"workspace"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Workspace, nil
}

func (c *aegisClient) postTetherFrame(instanceID string, frame interface{}) error {
	data, _ := json.Marshal(frame)
	url := fmt.Sprintf("http://aegis/v1/instances/%s/tether", instanceID)
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("tether POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tether POST %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *aegisClient) streamTether(ctx context.Context, instanceID string) (io.ReadCloser, error) {
	url := fmt.Sprintf("http://aegis/v1/instances/%s/tether/stream", instanceID)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tether stream: %w", err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("tether stream %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// Legacy config loading (backward compat)

func loadLegacyConfig(socketPath string) *LegacyConfig {
	cfg := &LegacyConfig{
		AegisSocket: socketPath,
	}

	paths := []string{
		os.Getenv("AEGIS_GATEWAY_CONFIG"),
		filepath.Join(os.Getenv("HOME"), ".aegis", "gateway.json"),
	}

	for _, p := range paths {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if json.Unmarshal(data, cfg) == nil {
			log.Printf("loaded config from %s", p)
			break
		}
	}

	if token := os.Getenv("TELEGRAM_BOT_TOKEN"); token != "" {
		cfg.Telegram.BotToken = token
	}
	if inst := os.Getenv("AEGIS_GATEWAY_INSTANCE"); inst != "" {
		cfg.Telegram.Instance = inst
	}
	if cfg.Telegram.BotTokenSecret == "" {
		if secretName := os.Getenv("TELEGRAM_BOT_TOKEN_SECRET"); secretName != "" {
			cfg.Telegram.BotTokenSecret = secretName
		}
	}

	return cfg
}

func defaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "aegisd.sock")
}

// Image helpers

// isImageDocument returns true if the document has a supported image MIME type.
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

// resolveWorkspace returns the cached host workspace path, fetching it once from aegisd.
func (gw *Gateway) resolveWorkspace() string {
	gw.mu.Lock()
	if gw.workspacePath != "" {
		ws := gw.workspacePath
		gw.mu.Unlock()
		return ws
	}
	gw.mu.Unlock()

	ws, err := gw.aegisClient.getInstanceWorkspace(gw.instanceHandle)
	if err != nil {
		log.Printf("resolve workspace: %v", err)
		return ""
	}

	gw.mu.Lock()
	gw.workspacePath = ws
	gw.mu.Unlock()
	return ws
}

// getFilePath calls Telegram getFile API and returns the file_path for download.
func (gw *Gateway) getFilePath(botToken, fileID string) (string, error) {
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

// downloadTelegramFile downloads a file from Telegram and returns the bytes and guessed media type.
func (gw *Gateway) downloadTelegramFile(botToken, fileID string) ([]byte, string, error) {
	filePath, err := gw.getFilePath(botToken, fileID)
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

	// Guess media type from file extension
	mediaType := mediaTypeFromPath(filePath)
	return data, mediaType, nil
}

// mediaTypeFromPath guesses the MIME type from a file path extension.
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

// sendTelegramPhoto sends a photo to a Telegram chat via multipart upload.
// Returns the message_id of the sent message.
func (gw *Gateway) sendTelegramPhoto(chatID int64, photoBytes []byte, caption, botToken string) int {
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
		log.Printf("sendPhoto: create form: %v", err)
		return 0
	}
	part.Write(photoBytes)
	w.Close()

	resp, err := http.Post(url, w.FormDataContentType(), &buf)
	if err != nil {
		log.Printf("sendPhoto: %v", err)
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
		body, _ := io.ReadAll(resp.Body)
		log.Printf("sendPhoto: API error: %s", string(body))
	}
	return result.Result.MessageID
}

// Helpers

type imageRefPayload struct {
	MediaType string `json:"media_type"`
	Blob      string `json:"blob"`
	Size      int64  `json:"size"`
}

// extractImageFileID returns the file ID and true if the message contains a photo or image document.
func (gw *Gateway) extractImageFileID(msg *telegramMessage) (string, bool) {
	if len(msg.Photo) > 0 {
		// Take the largest photo (last in array)
		return msg.Photo[len(msg.Photo)-1].FileID, true
	}
	if isImageDocument(msg.Document) {
		return msg.Document.FileID, true
	}
	return "", false
}

func buildUserPayload(msg *telegramMessage, imageRef *imageRefPayload) map[string]interface{} {
	// For photo messages, use Caption as text
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

func parseChatID(s string) int64 {
	var id int64
	fmt.Sscanf(s, "%d", &id)
	return id
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
