// aegis-gateway is the host-side messaging adapter for the Aegis Agent Kit.
//
// It bridges external messaging channels (Telegram) with Aegis instances
// via the tether protocol, enabling wake-on-message and streaming UX.
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
	log.Println("aegis-gateway starting")

	cfg := loadConfig()
	client := newAegisClient(cfg.AegisSocket)

	// Resolve bot token from aegis secret store if not set directly
	if cfg.Telegram.BotToken == "" && cfg.Telegram.BotTokenSecret != "" {
		val, err := client.getSecret(cfg.Telegram.BotTokenSecret)
		if err != nil {
			log.Fatalf("resolve bot token secret %q: %v", cfg.Telegram.BotTokenSecret, err)
		}
		cfg.Telegram.BotToken = val
		log.Printf("bot token resolved from secret %q", cfg.Telegram.BotTokenSecret)
	}

	if cfg.Telegram.BotToken == "" {
		log.Fatal("telegram bot token not configured (set TELEGRAM_BOT_TOKEN or bot_token_secret in config)")
	}

	gw := &Gateway{
		cfg:           cfg,
		aegisClient:   client,
		activeReplies: make(map[int64]*activeReply),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start egress subscriber (reads tether stream from aegisd)
	go gw.subscribeEgress(ctx)

	// Start Telegram polling
	go gw.pollTelegram(ctx)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	log.Println("aegis-gateway stopping")
	cancel()
}

// Config

type Config struct {
	Telegram    TelegramConfig `json:"telegram"`
	AegisSocket string         `json:"aegis_socket"`
}

type TelegramConfig struct {
	BotToken       string   `json:"bot_token"`
	BotTokenSecret string   `json:"bot_token_secret"`
	Instance       string   `json:"instance"`
	AllowedChats   []string `json:"allowed_chats"`
}

func loadConfig() *Config {
	cfg := &Config{
		AegisSocket: defaultSocketPath(),
	}

	// Try config file
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

	// Env overrides
	if token := os.Getenv("TELEGRAM_BOT_TOKEN"); token != "" {
		cfg.Telegram.BotToken = token
	}
	if inst := os.Getenv("AEGIS_GATEWAY_INSTANCE"); inst != "" {
		cfg.Telegram.Instance = inst
	}

	// Allow secret name from env
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

// Gateway

type Gateway struct {
	cfg           *Config
	aegisClient   *aegisClient
	mu            sync.Mutex
	activeReplies map[int64]*activeReply // chatID → in-progress reply
}

type activeReply struct {
	chatID       int64
	messageID    int
	text         string
	lastEdit     time.Time
	typingCancel context.CancelFunc
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

		updates, err := gw.getUpdates(offset, 30)
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
	MessageID int          `json:"message_id"`
	Chat      telegramChat `json:"chat"`
	Text      string       `json:"text"`
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

func (gw *Gateway) getUpdates(offset, timeout int) ([]telegramUpdate, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d",
		gw.cfg.Telegram.BotToken, offset, timeout)
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

	// Check allowed chats
	if !gw.isChatAllowed(chatID) {
		log.Printf("chat %d not allowed, ignoring", chatID)
		return
	}

	instanceID := gw.cfg.Telegram.Instance
	if instanceID == "" {
		log.Printf("no instance configured for gateway")
		return
	}

	// Start typing indicator
	typingCtx, typingCancel := context.WithCancel(context.Background())
	go gw.sendTypingLoop(typingCtx, chatID)

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
		Payload: mustMarshal(map[string]string{"text": msg.Text}),
	}

	if err := gw.aegisClient.postTetherFrame(instanceID, frame); err != nil {
		log.Printf("tether ingress failed: %v", err)
		typingCancel()
		gw.sendTelegramMessage(chatID, "Failed to deliver message to agent.")
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

func (gw *Gateway) isChatAllowed(chatID int64) bool {
	if len(gw.cfg.Telegram.AllowedChats) == 0 {
		return true
	}
	chatStr := fmt.Sprintf("%d", chatID)
	for _, allowed := range gw.cfg.Telegram.AllowedChats {
		if allowed == "*" || allowed == chatStr {
			return true
		}
	}
	return false
}

func (gw *Gateway) sendTypingLoop(ctx context.Context, chatID int64) {
	// Telegram typing indicator expires after ~5s.
	// Send every 3s to avoid visible blink between refreshes.
	// Hard timeout at 2 minutes to prevent zombie typing on crashes.
	timeout := time.After(2 * time.Minute)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	gw.sendChatAction(chatID, "typing")
	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			return
		case <-ticker.C:
			gw.sendChatAction(chatID, "typing")
		}
	}
}

// Egress subscriber

func (gw *Gateway) subscribeEgress(ctx context.Context) {
	instanceID := gw.cfg.Telegram.Instance
	if instanceID == "" {
		return
	}

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

		// Reconnect after stream ends
		log.Printf("egress stream ended, reconnecting...")
		time.Sleep(1 * time.Second)
	}
}

func (gw *Gateway) processEgressStream(ctx context.Context, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024) // 256KB max line

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
			// First delta — send a new message
			var typingCancel context.CancelFunc
			if ok && reply.typingCancel != nil {
				typingCancel = reply.typingCancel
			}
			gw.mu.Unlock()
			msgID := gw.sendTelegramMessage(chatID, payload.Text)
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
		// Throttle edits to max 1/sec
		if time.Since(reply.lastEdit) >= time.Second {
			text := reply.text
			msgID := reply.messageID
			reply.lastEdit = time.Now()
			gw.mu.Unlock()
			gw.editTelegramMessage(chatID, msgID, text)
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
		// Cancel typing indicator
		if ok && reply.typingCancel != nil {
			reply.typingCancel()
		}
		if ok && reply.messageID != 0 {
			// Final edit with complete text
			msgID := reply.messageID
			delete(gw.activeReplies, chatID)
			gw.mu.Unlock()
			if payload.Text != "" {
				gw.editTelegramMessage(chatID, msgID, payload.Text)
			}
		} else {
			delete(gw.activeReplies, chatID)
			gw.mu.Unlock()
			// No prior deltas — send complete message
			if payload.Text != "" {
				gw.sendTelegramMessage(chatID, payload.Text)
			}
		}

	case "status.presence":
		// Presence signals → typing indicator
		gw.sendChatAction(chatID, "typing")
	}
}

// Telegram API helpers

func (gw *Gateway) sendTelegramMessage(chatID int64, text string) int {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", gw.cfg.Telegram.BotToken)
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

func (gw *Gateway) editTelegramMessage(chatID int64, messageID int, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", gw.cfg.Telegram.BotToken)
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

func (gw *Gateway) sendChatAction(chatID int64, action string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", gw.cfg.Telegram.BotToken)
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

func parseChatID(s string) int64 {
	var id int64
	fmt.Sscanf(s, "%d", &id)
	return id
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

