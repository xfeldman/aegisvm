// Package client provides a shared Go client for the aegisd HTTP API.
// Used by the CLI, MCP server, UI server, and gateway — replaces per-binary
// unix socket boilerplate.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client talks to aegisd over a unix socket.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// New creates a client connected to the aegisd unix socket at socketPath.
func New(socketPath string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					d.Timeout = 5 * time.Second
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 0, // no timeout for streaming
		},
		baseURL: "http://aegis",
	}
}

// DefaultSocketPath returns the default aegisd socket path (~/.aegis/aegisd.sock).
func DefaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "aegisd.sock")
}

// NewDefault creates a client using the default socket path.
func NewDefault() *Client {
	return New(DefaultSocketPath())
}

// --- Instance management ---

// ListInstances returns all instances, optionally filtered by state.
func (c *Client) ListInstances(ctx context.Context, state string) ([]Instance, error) {
	path := "/v1/instances"
	if state != "" {
		path += "?state=" + url.QueryEscape(state)
	}
	var out []Instance
	if err := c.doJSON(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetInstance returns a single instance by ID or handle.
func (c *Client) GetInstance(ctx context.Context, idOrHandle string) (*Instance, error) {
	var out Instance
	if err := c.doJSON(ctx, "GET", "/v1/instances/"+url.PathEscape(idOrHandle), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateInstance creates and starts a new instance.
func (c *Client) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*CreateInstanceResponse, error) {
	var out CreateInstanceResponse
	if err := c.doJSON(ctx, "POST", "/v1/instances", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartInstance starts (or re-enables) a stopped instance.
func (c *Client) StartInstance(ctx context.Context, idOrHandle string) error {
	return c.doJSON(ctx, "POST", "/v1/instances/"+url.PathEscape(idOrHandle)+"/start", nil, nil)
}

// DisableInstance stops a running instance and prevents auto-wake.
func (c *Client) DisableInstance(ctx context.Context, idOrHandle string) error {
	return c.doJSON(ctx, "POST", "/v1/instances/"+url.PathEscape(idOrHandle)+"/disable", nil, nil)
}

// DeleteInstance removes an instance entirely.
func (c *Client) DeleteInstance(ctx context.Context, idOrHandle string) error {
	return c.doJSON(ctx, "DELETE", "/v1/instances/"+url.PathEscape(idOrHandle), nil, nil)
}

// PauseInstance pauses a running instance.
func (c *Client) PauseInstance(ctx context.Context, idOrHandle string) error {
	return c.doJSON(ctx, "POST", "/v1/instances/"+url.PathEscape(idOrHandle)+"/pause", nil, nil)
}

// ResumeInstance resumes a paused instance.
func (c *Client) ResumeInstance(ctx context.Context, idOrHandle string) error {
	return c.doJSON(ctx, "POST", "/v1/instances/"+url.PathEscape(idOrHandle)+"/resume", nil, nil)
}

// ExposePort exposes a guest port on the host.
func (c *Client) ExposePort(ctx context.Context, idOrHandle string, req ExposeRequest) (*ExposeResponse, error) {
	var out ExposeResponse
	if err := c.doJSON(ctx, "POST", "/v1/instances/"+url.PathEscape(idOrHandle)+"/expose", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UnexposePort removes a port mapping.
func (c *Client) UnexposePort(ctx context.Context, idOrHandle string, guestPort int) error {
	return c.doJSON(ctx, "DELETE", "/v1/instances/"+url.PathEscape(idOrHandle)+"/expose/"+strconv.Itoa(guestPort), nil, nil)
}

// --- Exec + Logs ---

// Exec runs a command inside a running instance and collects the output.
func (c *Client) Exec(ctx context.Context, idOrHandle string, command []string) (*ExecResult, error) {
	body := map[string]interface{}{"command": command}
	resp, err := c.doRaw(ctx, "POST", "/v1/instances/"+url.PathEscape(idOrHandle)+"/exec", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result ExecResult
	var output strings.Builder
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var entry map[string]interface{}
		if err := dec.Decode(&entry); err != nil {
			break
		}
		if execID, ok := entry["exec_id"].(string); ok && result.ExecID == "" {
			result.ExecID = execID
			if t, ok := entry["started_at"].(string); ok {
				result.StartedAt = t
			}
		}
		if line, ok := entry["line"].(string); ok {
			output.WriteString(line)
			output.WriteByte('\n')
		}
		if done, ok := entry["done"].(bool); ok && done {
			if code, ok := entry["exit_code"].(float64); ok {
				result.ExitCode = int(code)
			}
		}
	}
	result.Output = strings.TrimRight(output.String(), "\n")
	return &result, nil
}

// StreamLogs returns a reader for instance logs (NDJSON stream).
// Caller must close the returned ReadCloser.
func (c *Client) StreamLogs(ctx context.Context, idOrHandle string, follow bool, tail int) (io.ReadCloser, error) {
	params := url.Values{}
	if follow {
		params.Set("follow", "true")
	}
	if tail > 0 {
		params.Set("tail", strconv.Itoa(tail))
	}
	path := "/v1/instances/" + url.PathEscape(idOrHandle) + "/logs"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	resp, err := c.doRaw(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// --- Tether ---

// TetherSend sends a message to the in-VM agent.
func (c *Client) TetherSend(ctx context.Context, idOrHandle string, sessionID, text string) (*TetherSendResult, error) {
	body := map[string]interface{}{
		"type":  "user.message",
		"msg_id": fmt.Sprintf("msg-%d", time.Now().UnixNano()),
		"session": map[string]string{
			"id": sessionID,
		},
		"content": map[string]string{
			"text": text,
		},
	}
	var out TetherSendResult
	if err := c.doJSON(ctx, "POST", "/v1/instances/"+url.PathEscape(idOrHandle)+"/tether", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TetherPoll polls for tether frames.
func (c *Client) TetherPoll(ctx context.Context, idOrHandle string, opts TetherPollOpts) (*TetherPollResult, error) {
	params := url.Values{}
	if opts.SessionID != "" {
		params.Set("session_id", opts.SessionID)
	}
	if opts.Channel != "" {
		params.Set("channel", opts.Channel)
	}
	if opts.ReplyToMsgID != "" {
		params.Set("reply_to_msg_id", opts.ReplyToMsgID)
	}
	if opts.AfterSeq > 0 {
		params.Set("after_seq", strconv.FormatInt(opts.AfterSeq, 10))
	}
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if len(opts.Types) > 0 {
		params.Set("types", strings.Join(opts.Types, ","))
	}
	if opts.WaitMs > 0 {
		params.Set("wait_ms", strconv.Itoa(opts.WaitMs))
	}
	path := "/v1/instances/" + url.PathEscape(idOrHandle) + "/tether/poll"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var out TetherPollResult
	if err := c.doJSON(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Secrets ---

// ListSecrets returns all secret names (no values).
func (c *Client) ListSecrets(ctx context.Context) ([]SecretInfo, error) {
	var out []SecretInfo
	if err := c.doJSON(ctx, "GET", "/v1/secrets", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetSecret creates or updates a secret.
func (c *Client) SetSecret(ctx context.Context, name, value string) error {
	body := map[string]string{"value": value}
	return c.doJSON(ctx, "PUT", "/v1/secrets/"+url.PathEscape(name), body, nil)
}

// DeleteSecret removes a secret.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	return c.doJSON(ctx, "DELETE", "/v1/secrets/"+url.PathEscape(name), nil, nil)
}

// --- Workspace file access ---

// ReadWorkspaceFile reads a file from an instance's workspace.
// Returns the file content, content-type, and any error.
func (c *Client) ReadWorkspaceFile(ctx context.Context, idOrHandle, path string) ([]byte, string, error) {
	reqPath := "/v1/instances/" + url.PathEscape(idOrHandle) + "/workspace?path=" + url.QueryEscape(path)
	resp, err := c.doRaw(ctx, "GET", reqPath, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// WriteWorkspaceFile writes a file to an instance's workspace.
func (c *Client) WriteWorkspaceFile(ctx context.Context, idOrHandle, path string, data []byte) error {
	reqPath := "/v1/instances/" + url.PathEscape(idOrHandle) + "/workspace?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+reqPath, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseError(resp)
	}
	return nil
}

// --- Status ---

// Status returns the daemon status.
func (c *Client) Status(ctx context.Context) (*DaemonStatus, error) {
	var out DaemonStatus
	if err := c.doJSON(ctx, "GET", "/v1/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Kits ---

// ListKits returns installed kits.
func (c *Client) ListKits(ctx context.Context) ([]Kit, error) {
	var out []Kit
	if err := c.doJSON(ctx, "GET", "/v1/kits", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- Internal helpers ---

// doJSON makes a JSON request and decodes the JSON response into result.
// If body is non-nil, it's encoded as JSON. If result is nil, the response body is discarded.
func (c *Client) doJSON(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	resp, err := c.doRaw(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if result == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

// doRaw makes an HTTP request and returns the raw response.
// Caller is responsible for closing resp.Body.
func (c *Client) doRaw(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s %s: %w", method, path, err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, parseError(resp)
	}
	return resp, nil
}

// parseError reads an error response body and returns an APIError.
func parseError(resp *http.Response) error {
	var errResp struct {
		Error string `json:"error"`
	}
	data, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(data, &errResp) == nil && errResp.Error != "" {
		return &APIError{StatusCode: resp.StatusCode, Message: errResp.Error}
	}
	return &APIError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
}

// HTTPClient returns the underlying http.Client for advanced use cases
// like direct streaming requests.
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

// BaseURL returns the base URL used for requests.
func (c *Client) BaseURL() string {
	return c.baseURL
}
