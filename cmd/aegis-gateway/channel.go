package main

import (
	"context"
	"encoding/json"
	"io"
	"log"

	"github.com/xfeldman/aegisvm/internal/blob"
)

// Channel bridges an external messaging service to tether.
// Each implementation owns its own protocol (polling, webhooks, websockets)
// and translates between that protocol and tether frames.
type Channel interface {
	// Name returns the channel identifier used in tether session routing
	// (e.g. "telegram", "discord"). Must match frame.Session.Channel.
	Name() string

	// Start begins the channel's ingress loop (polling, webhook listener, etc).
	// The channel must respect ctx cancellation for clean shutdown.
	Start(ctx context.Context, deps ChannelDeps) error

	// HandleFrame is called for each egress frame whose Session.Channel
	// matches this channel's Name(). The channel decides which frame types
	// to handle (delta, done, reasoning, presence, etc).
	HandleFrame(frame TetherFrame)

	// Reconfigure hot-reloads channel config. Called when the gateway
	// config file changes. The channel should update its state atomically.
	// If the new config disables this channel, it should stop gracefully.
	Reconfigure(cfg json.RawMessage)

	// Stop shuts down the channel. Called on gateway shutdown or when
	// the channel is removed from config.
	Stop()
}

// ChannelDeps provides the gateway services a channel needs.
type ChannelDeps struct {
	// SendTether delivers a user.message frame to the instance via tether.
	// Handles wake-on-message internally.
	SendTether func(frame TetherFrame) error

	// Exec runs a command on the instance and streams output as NDJSON.
	// Returns an NDJSON reader. Caller must close.
	// Handles wake-on-exec internally.
	Exec func(command []string) (io.ReadCloser, error)

	// InstanceHandle is the instance this gateway serves.
	InstanceHandle string

	// WorkspacePath returns the host workspace path (lazy-resolved, cached).
	WorkspacePath func() string

	// BlobStore returns the workspace blob store for image ingress.
	BlobStore func() *blob.WorkspaceBlobStore

	// Logger for channel-scoped logging.
	Logger *log.Logger
}

// channelFactories maps config key → channel constructor.
// Adding a new channel means adding a Go file and registering here.
var channelFactories = map[string]func() Channel{
	"telegram": func() Channel { return NewTelegramChannel() },
}
