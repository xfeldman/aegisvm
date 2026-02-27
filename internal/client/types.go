package client

import "time"

// Instance represents an aegisd instance.
type Instance struct {
	ID                string                 `json:"id"`
	State             string                 `json:"state"`
	Enabled           bool                   `json:"enabled"`
	Command           []string               `json:"command"`
	Handle            string                 `json:"handle,omitempty"`
	ImageRef          string                 `json:"image_ref,omitempty"`
	Kit               string                 `json:"kit,omitempty"`
	Workspace         string                 `json:"workspace,omitempty"`
	IdlePolicy        string                 `json:"idle_policy,omitempty"`
	CreatedAt         string                 `json:"created_at"`
	StoppedAt         string                 `json:"stopped_at,omitempty"`
	LastActiveAt      string                 `json:"last_active_at,omitempty"`
	ActiveConnections int                    `json:"active_connections,omitempty"`
	ExposePorts       []int                  `json:"expose_ports,omitempty"`
	Endpoints         []Endpoint             `json:"endpoints,omitempty"`
	GatewayRunning    bool                   `json:"gateway_running,omitempty"`
	LeaseHeld         bool                   `json:"lease_held,omitempty"`
	LeaseReason       string                 `json:"lease_reason,omitempty"`
	LeaseExpiresAt    string                 `json:"lease_expires_at,omitempty"`
	Extra             map[string]interface{} `json:"-"` // catch-all for unknown fields
}

// Endpoint represents a port mapping.
type Endpoint struct {
	GuestPort  int    `json:"guest_port"`
	PublicPort int    `json:"public_port"`
	Protocol   string `json:"protocol"`
}

// CreateInstanceRequest is the request body for creating an instance.
type CreateInstanceRequest struct {
	Command      []string          `json:"command"`
	Handle       string            `json:"handle,omitempty"`
	ImageRef     string            `json:"image_ref,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Secrets      []string          `json:"secrets,omitempty"`
	Workspace    string            `json:"workspace,omitempty"`
	MemoryMB     int               `json:"memory_mb,omitempty"`
	VCPUs        int               `json:"vcpus,omitempty"`
	IdlePolicy   string            `json:"idle_policy,omitempty"`
	Capabilities interface{}       `json:"capabilities,omitempty"`
	Kit          string            `json:"kit,omitempty"`
}

// CreateInstanceResponse is the response from creating an instance.
type CreateInstanceResponse struct {
	ID       string   `json:"id"`
	State    string   `json:"state"`
	Handle   string   `json:"handle,omitempty"`
	Command  []string `json:"command"`
	ImageRef string   `json:"image_ref,omitempty"`
	Kit      string   `json:"kit,omitempty"`
}

// ExecResult holds the result of an exec command.
type ExecResult struct {
	ExecID    string `json:"exec_id"`
	StartedAt string `json:"started_at"`
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"` // collected stdout+stderr
}

// ExposeRequest is the request body for exposing a port.
type ExposeRequest struct {
	Port       int    `json:"port"`
	PublicPort int    `json:"public_port,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
}

// ExposeResponse is the response from exposing a port.
type ExposeResponse struct {
	GuestPort  int    `json:"guest_port"`
	PublicPort int    `json:"public_port"`
	Protocol   string `json:"protocol"`
	URL        string `json:"url"`
}

// TetherSendResult is the response from sending a tether message.
type TetherSendResult struct {
	MsgID      string `json:"msg_id"`
	SessionID  string `json:"session_id"`
	IngressSeq int64  `json:"ingress_seq"`
}

// TetherPollOpts configures a tether poll request.
type TetherPollOpts struct {
	Channel      string
	SessionID    string
	ReplyToMsgID string
	AfterSeq     int64
	Limit        int
	Types        []string
	WaitMs       int
}

// TetherFrame is a single tether frame.
type TetherFrame struct {
	Seq     int64                  `json:"seq"`
	Type    string                 `json:"type"`
	MsgID   string                 `json:"msg_id,omitempty"`
	Session map[string]interface{} `json:"session,omitempty"`
	Content map[string]interface{} `json:"content,omitempty"`
	Extra   map[string]interface{} `json:"-"`
}

// TetherPollResult is the response from polling tether frames.
type TetherPollResult struct {
	Frames   []TetherFrame `json:"frames"`
	NextSeq  int64         `json:"next_seq"`
	TimedOut bool          `json:"timed_out"`
}

// SecretInfo represents a secret (name + metadata, no value).
type SecretInfo struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// DaemonStatus is the response from the status endpoint.
type DaemonStatus struct {
	Status       string                 `json:"status"`
	Backend      string                 `json:"backend"`
	Capabilities map[string]interface{} `json:"capabilities"`
}

// Kit represents a kit manifest.
type Kit struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
}

// LogEntry represents a single log line from an instance.
type LogEntry struct {
	Timestamp time.Time `json:"ts"`
	Stream    string    `json:"stream"`
	Line      string    `json:"line"`
	ExecID    string    `json:"exec_id,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Done      bool      `json:"done,omitempty"`
}

// APIError is returned when the API returns an error response.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return e.Message
}
