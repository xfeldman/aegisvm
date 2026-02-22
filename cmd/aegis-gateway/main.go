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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
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
			if u.Message != nil && u.Message.Text != "" {
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
	MessageID int           `json:"message_id"`
	Chat      telegramChat  `json:"chat"`
	Text      string        `json:"text"`
	From      *telegramUser `json:"from"`
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
		Payload: mustMarshal(buildUserPayload(msg)),
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

		body, err := gw.aegisClient.streamTether(ctx, instanceID)
		if err != nil {
			log.Printf("egress subscribe: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

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
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
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
		if !ok || reply.messageID == 0 {
			var typingCancel context.CancelFunc
			if ok && reply.typingCancel != nil {
				typingCancel = reply.typingCancel
			}
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
			Text string `json:"text"`
		}
		json.Unmarshal(frame.Payload, &payload)

		gw.mu.Lock()
		reply, ok := gw.activeReplies[chatID]
		if ok && reply.typingCancel != nil {
			reply.typingCancel()
		}
		if ok && reply.messageID != 0 {
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

// Helpers

func buildUserPayload(msg *telegramMessage) map[string]interface{} {
	p := map[string]interface{}{
		"text": msg.Text,
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
