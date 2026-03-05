// aegis-gateway is the host-side messaging adapter for the Aegis Agent Kit.
//
// It bridges external messaging channels (Telegram, future Discord/Slack)
// with Aegis instances via the tether protocol, enabling wake-on-message
// and streaming UX.
//
// Architecture:
//   - Gateway core: config watch, egress subscription, channel registry, cron
//   - Channels: pluggable adapters (see channel.go interface, telegram.go impl)
//   - Cron scheduler: timer → tether injector, stays in core
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

	"github.com/xfeldman/aegisvm/internal/blob"
	"github.com/xfeldman/aegisvm/internal/cron"
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
		channels:       make(map[string]Channel),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handling: SIGTERM/SIGINT = shutdown, SIGHUP = reload config
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	// Start egress subscription (always on — serves all channels and cron)
	go gw.subscribeEgress(ctx)

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

	// Bot token: config literal → env var declared in config → TELEGRAM_BOT_TOKEN fallback
	if cfg.Telegram.BotToken == "" {
		envKey := cfg.Telegram.BotTokenEnv
		if envKey == "" {
			envKey = "TELEGRAM_BOT_TOKEN"
		}
		if v := os.Getenv(envKey); v != "" {
			cfg.Telegram.BotToken = v
		}
	}
	if cfg.Telegram.BotToken == "" {
		log.Fatal("telegram bot token not configured (use --env TELEGRAM_BOT_TOKEN on the instance)")
	}

	instanceHandle := cfg.Telegram.Instance

	gw := &Gateway{
		instanceHandle: instanceHandle,
		aegisClient:    client,
		channels:       make(map[string]Channel),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go gw.subscribeEgress(ctx)

	// Create and start Telegram channel directly in legacy mode
	tg := NewTelegramChannel()
	deps := gw.buildChannelDeps("telegram")
	tg.Start(ctx, deps)
	cfgJSON, _ := json.Marshal(cfg.Telegram)
	tg.Reconfigure(cfgJSON)
	gw.channels["telegram"] = tg

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	log.Println("aegis-gateway stopping")
	cancel()
}

// Gateway is the core gateway process.

type Gateway struct {
	instanceHandle string
	aegisClient    *aegisClient
	mu             sync.Mutex
	channels       map[string]Channel // active channels by name
	configMtime    time.Time
	reloadCh       chan struct{}
	workspacePath  string
	// Cron scheduler state
	cronEntries []gwCronEntry
	cronState   map[string]*cronState
	cronMtime   time.Time
}

// run is the main loop: watch config, start/stop channels as config appears/disappears.
func (gw *Gateway) run(ctx context.Context) {
	gw.reloadCh = make(chan struct{}, 1)
	gw.cronState = make(map[string]*cronState)

	// Try initial config load
	gw.reloadConfig()
	gw.checkCronChange()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Cron ticker: align to next minute boundary, then tick every minute
	nextMinute := time.Now().Truncate(time.Minute).Add(time.Minute)
	cronTimer := time.NewTimer(time.Until(nextMinute))
	defer cronTimer.Stop()
	var cronTicker *time.Ticker

	for {
		select {
		case <-ctx.Done():
			gw.stopAllChannels()
			if cronTicker != nil {
				cronTicker.Stop()
			}
			return
		case <-gw.reloadCh:
			gw.applyConfig(ctx)
		case <-ticker.C:
			gw.checkConfigChange(ctx)
			gw.checkCronChange()
		case <-cronTimer.C:
			gw.evaluateCron()
			cronTicker = time.NewTicker(time.Minute)
			cronTimer.Stop()
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-cronTicker.C:
						gw.evaluateCron()
					}
				}
			}()
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
	hasChannels := len(gw.channels) > 0
	oldMtime := gw.configMtime
	gw.mu.Unlock()

	if err != nil {
		if hasChannels {
			log.Printf("config file removed, stopping channels")
			gw.stopAllChannels()
			gw.mu.Lock()
			gw.configMtime = time.Time{}
			gw.mu.Unlock()
		}
		return
	}

	if info.ModTime() != oldMtime {
		gw.applyConfig(ctx)
	}
}

// applyConfig reads the config file and starts/reconfigures channels.
func (gw *Gateway) applyConfig(ctx context.Context) {
	path := gw.configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("read config: %v", err)
		}
		return
	}

	// Update mtime immediately to prevent duplicate applyConfig from ticker
	info, _ := os.Stat(path)
	if info != nil {
		gw.mu.Lock()
		gw.configMtime = info.ModTime()
		gw.mu.Unlock()
	}

	// Parse as generic map to route each key to its channel
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		log.Printf("parse config (keeping last-known-good): %v", err)
		return
	}

	// Start or reconfigure channels
	seen := make(map[string]bool)
	for name, cfgJSON := range rawConfig {
		factory, ok := channelFactories[name]
		if !ok {
			continue // unknown channel key, ignore
		}
		seen[name] = true

		gw.mu.Lock()
		ch, exists := gw.channels[name]
		gw.mu.Unlock()

		if !exists {
			// New channel — create and start
			ch = factory()
			deps := gw.buildChannelDeps(name)
			ch.Start(ctx, deps)
			gw.mu.Lock()
			gw.channels[name] = ch
			gw.mu.Unlock()
			log.Printf("channel %q started", name)
		}

		ch.Reconfigure(cfgJSON)
	}

	// Stop channels that are no longer in config
	gw.mu.Lock()
	for name, ch := range gw.channels {
		if !seen[name] {
			ch.Stop()
			delete(gw.channels, name)
			log.Printf("channel %q stopped (removed from config)", name)
		}
	}
	gw.mu.Unlock()
}

func (gw *Gateway) stopAllChannels() {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	for name, ch := range gw.channels {
		ch.Stop()
		delete(gw.channels, name)
	}
}

// buildChannelDeps creates the ChannelDeps for a channel.
func (gw *Gateway) buildChannelDeps(name string) ChannelDeps {
	return ChannelDeps{
		SendTether: func(frame TetherFrame) error {
			return gw.aegisClient.postTetherFrame(gw.instanceHandle, frame)
		},
		Exec: func(command []string) (io.ReadCloser, error) {
			return gw.aegisClient.execInstance(gw.instanceHandle, command)
		},
		InstanceHandle: gw.instanceHandle,
		WorkspacePath:  gw.resolveWorkspace,
		BlobStore: func() *blob.WorkspaceBlobStore {
			ws := gw.resolveWorkspace()
			if ws == "" {
				return nil
			}
			return blob.NewWorkspaceBlobStore(ws)
		},
		Logger: log.New(log.Writer(), fmt.Sprintf("[%s] ", name), log.LstdFlags|log.Lshortfile),
	}
}

// Egress subscriber

func (gw *Gateway) subscribeEgress(ctx context.Context) {
	instanceID := gw.instanceHandle

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

		gw.handleEgressFrameByChannel(frame)
	}
}

// handleEgressFrameByChannel routes a tether frame to the appropriate channel.
func (gw *Gateway) handleEgressFrameByChannel(frame TetherFrame) {
	gw.mu.Lock()
	ch, ok := gw.channels[frame.Session.Channel]
	gw.mu.Unlock()
	if ok {
		ch.HandleFrame(frame)
	}
}

// --- Cron scheduler ---

// cronPath returns the workspace cron file path (host-side).
func (gw *Gateway) cronPath() string {
	ws := gw.resolveWorkspace()
	if ws == "" {
		return ""
	}
	return filepath.Join(ws, ".aegis", "cron.json")
}

// checkCronChange reloads cron.json if its mtime changed.
func (gw *Gateway) checkCronChange() {
	path := gw.cronPath()
	if path == "" {
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		if len(gw.cronEntries) > 0 {
			log.Printf("cron: config removed, clearing entries")
			gw.cronEntries = nil
		}
		return
	}

	if info.ModTime().Equal(gw.cronMtime) {
		return
	}
	gw.cronMtime = info.ModTime()

	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("cron: read %s: %v", path, err)
		return
	}

	var cf struct {
		Entries []struct {
			ID         string `json:"id"`
			Schedule   string `json:"schedule"`
			Message    string `json:"message"`
			Session    string `json:"session"`
			OnConflict string `json:"on_conflict"`
			Enabled    bool   `json:"enabled"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &cf); err != nil {
		log.Printf("cron: parse %s: %v", path, err)
		return
	}

	var entries []gwCronEntry
	for _, e := range cf.Entries {
		if len(entries) >= 20 {
			log.Printf("cron: max 20 entries, ignoring remainder")
			break
		}
		sched, err := cron.Parse(e.Schedule)
		if err != nil {
			log.Printf("cron [%s]: invalid schedule %q: %v (skipped)", e.ID, e.Schedule, err)
			continue
		}
		entries = append(entries, gwCronEntry{
			ID:       e.ID,
			Schedule: sched,
			Message:  e.Message,
			Session:  e.Session,
			Enabled:  e.Enabled,
		})
	}

	gw.cronEntries = entries
	log.Printf("cron: loaded %d entries from %s", len(entries), path)
}

// evaluateCron checks all cron entries and fires matching ones.
func (gw *Gateway) evaluateCron() {
	if len(gw.cronEntries) == 0 {
		return
	}

	now := time.Now().Truncate(time.Minute)
	nowKey := now.Format("2006-01-02T15:04")

	for _, entry := range gw.cronEntries {
		if !entry.Enabled {
			continue
		}

		state := gw.cronState[entry.ID]
		if state == nil {
			state = &cronState{}
			gw.cronState[entry.ID] = state
		}

		if state.lastFiredMinute == nowKey {
			continue
		}

		if !entry.Schedule.Matches(now) {
			continue
		}

		state.lastFiredMinute = nowKey
		log.Printf("cron [%s]: firing → session %s", entry.ID, entry.Session)
		go gw.fireCron(entry)
	}
}

// fireCron sends a synthetic user message via tether for a cron entry.
func (gw *Gateway) fireCron(entry gwCronEntry) {
	payload, _ := json.Marshal(map[string]interface{}{
		"text": entry.Message,
		"user": map[string]string{"id": "cron", "name": "Scheduled Task"},
	})

	frame := TetherFrame{
		V:       1,
		Type:    "user.message",
		TS:      time.Now().UTC().Format(time.RFC3339Nano),
		Session: SessionID{Channel: "cron", ID: entry.Session},
		MsgID:   fmt.Sprintf("cron-%s-%d", entry.ID, time.Now().Unix()),
		Payload: payload,
	}

	if err := gw.aegisClient.postTetherFrame(gw.instanceHandle, frame); err != nil {
		log.Printf("cron [%s]: fire failed: %v", entry.ID, err)
	}
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

// --- Aegis client (talks to aegisd via unix socket) ---

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

func (c *aegisClient) getInstanceWorkspace(instanceID string) (string, error) {
	url := fmt.Sprintf("http://aegis/v1/instances/%s", instanceID)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var inst struct {
		Workspace string `json:"workspace"`
	}
	json.NewDecoder(resp.Body).Decode(&inst)
	return inst.Workspace, nil
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

func (c *aegisClient) execInstance(instanceID string, command []string) (io.ReadCloser, error) {
	data, _ := json.Marshal(map[string]interface{}{
		"command": command,
	})
	url := fmt.Sprintf("http://aegis/v1/instances/%s/exec", instanceID)
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("exec POST: %w", err)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("exec POST %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// --- Legacy config loading ---

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
	return cfg
}

func defaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "aegisd.sock")
}
