package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// JSON-RPC 2.0 message types

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      interface{}     `json:"id,omitempty"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
	ID      interface{} `json:"id"`
}

type rpcNotification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// RPC params/results

type runParams struct {
	Command         []string          `json:"command"`
	Env             map[string]string `json:"env,omitempty"`
	Workdir         string            `json:"workdir,omitempty"`
	ExposePorts     []int             `json:"expose_ports,omitempty"`     // guest ports to proxy
	CapabilityToken string            `json:"capability_token,omitempty"` // guest orchestration token
}

type runResult struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

type healthResult struct {
	Status string `json:"status"`
}

type logParams struct {
	Stream string `json:"stream"` // "stdout" or "stderr"
	Line   string `json:"line"`
}

type execParams struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	ExecID  string            `json:"exec_id"`
}

type execLogParams struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
	ExecID string `json:"exec_id,omitempty"`
}

type execResult struct {
	ExecID    string `json:"exec_id"`
	StartedAt string `json:"started_at"`
}

// processTracker tracks running processes for cleanup on shutdown.
type processTracker struct {
	mu               sync.Mutex
	processes        []*exec.Cmd
	primary          *exec.Cmd // primary process started by `run` RPC
	restartRequested bool      // set by self_restart, checked on process exit
}

func (pt *processTracker) add(cmd *exec.Cmd) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.processes = append(pt.processes, cmd)
}

func (pt *processTracker) setPrimary(cmd *exec.Cmd) bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if pt.primary != nil {
		return false // already running
	}
	pt.primary = cmd
	pt.processes = append(pt.processes, cmd)
	return true
}

func (pt *processTracker) killAll() {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	for _, cmd := range pt.processes {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}

// harnessRPC provides bidirectional JSON-RPC over the control channel.
// It handles incoming requests from aegisd (run, exec, etc.) AND supports
// sending requests TO aegisd for the guest API (guest.spawn, etc.).
type harnessRPC struct {
	conn    net.Conn
	mu      sync.Mutex // serializes writes to conn
	pending sync.Map   // id → chan json.RawMessage (responses to our calls)
	nextID  int64

	// capabilityToken is stored from the run RPC params.
	// Attached to all guest API → aegisd requests automatically.
	capabilityToken string

	// portProxy is the active port proxy (set during handleRun).
	// Used by ports_changed notifications to start/stop proxies.
	portProxy *portProxy

	// tracker manages child processes across reconnects. Lives on harnessRPC
	// (not handleConnection) so the primary process survives quiesce/reconnect.
	tracker *processTracker

	// quiesced is set by quiesce.stop RPC — suppresses optional traffic
	// (activity probes, log flushes) until the next reconnect.
	quiesced bool
}

func newHarnessRPC(conn net.Conn) *harnessRPC {
	return &harnessRPC{conn: conn}
}

// Call sends an RPC request to aegisd and waits for the response.
// Used by the guest API server to forward spawn/list/stop requests.
func (h *harnessRPC) Call(method string, params interface{}) (json.RawMessage, error) {
	h.nextID++
	// Use string IDs to avoid collision with aegisd's numeric IDs
	id := fmt.Sprintf("g-%d", h.nextID)

	respCh := make(chan json.RawMessage, 1)
	h.pending.Store(id, respCh) // id is a string like "g-1"
	defer h.pending.Delete(id)

	// Attach capability token if this is a guest.* request
	if h.capabilityToken != "" {
		// Wrap params to include token
		paramsJSON, _ := json.Marshal(params)
		var m map[string]interface{}
		json.Unmarshal(paramsJSON, &m)
		if m == nil {
			m = make(map[string]interface{})
		}
		m["_token"] = h.capabilityToken
		params = m
	}

	msg, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      id,
	})
	msg = append(msg, '\n')

	h.mu.Lock()
	_, err := h.conn.Write(msg)
	h.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("send %s: %w", method, err)
	}

	// Wait for response (with timeout)
	select {
	case resp := <-respCh:
		// Parse response to check for errors
		var parsed struct {
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		json.Unmarshal(resp, &parsed)
		if parsed.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, parsed.Error.Message)
		}
		return parsed.Result, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("%s: timeout waiting for response", method)
	}
}

// dispatchResponse routes an incoming response to the pending Call.
func (h *harnessRPC) dispatchResponse(id interface{}, msg []byte) {
	// Our IDs are strings like "g-1", "g-2"
	idStr := fmt.Sprintf("%v", id)
	if ch, ok := h.pending.Load(idStr); ok {
		ch.(chan json.RawMessage) <- msg
	}
}

// handleConnection processes JSON-RPC messages from a single host connection.
// Messages are classified as:
//   - Request from aegisd (has method + id): dispatched to request handler
//   - Response to our call (has id, no method): dispatched to pending Call()
//   - Notification (has method, no id): currently unused from aegisd→harness direction
func handleConnection(ctx context.Context, conn net.Conn, hrpc *harnessRPC) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max message

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse to classify the message type
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method,omitempty"`
			ID      interface{}     `json:"id,omitempty"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Printf("invalid JSON-RPC message: %v", err)
			continue
		}

		if msg.ID != nil && msg.Method == "" {
			// Response to one of our outgoing calls (guest API → aegisd)
			hrpc.dispatchResponse(msg.ID, line)
			continue
		}

		if msg.Method == "" {
			continue
		}

		// Notification from aegisd (no ID)
		if msg.ID == nil {
			switch msg.Method {
			case "tether.frame":
				go handleTetherFrame(conn, msg.Params)
			case "ports_changed":
				go handlePortsChanged(msg.Params, hrpc)
			}
			continue
		}

		// Request from aegisd — dispatch to handler
		var req rpcRequest
		json.Unmarshal(line, &req)

		resp := dispatch(ctx, &req, conn, hrpc.tracker, hrpc)
		if resp != nil {
			respJSON, _ := json.Marshal(resp)
			respJSON = append(respJSON, '\n')
			hrpc.mu.Lock()
			_, err := conn.Write(respJSON)
			hrpc.mu.Unlock()
			if err != nil {
				log.Printf("write response: %v", err)
				return
			}
		}

		if req.Method == "shutdown" {
			return
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("read error: %v", err)
	}
}

// dispatch routes a JSON-RPC request to the appropriate handler.
func dispatch(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *processTracker, hrpc *harnessRPC) *rpcResponse {
	switch req.Method {
	case "run":
		return handleRun(ctx, req, conn, tracker, hrpc)

	case "exec":
		return handleExec(ctx, req, conn, tracker)

	case "health":
		return &rpcResponse{
			JSONRPC: "2.0",
			Result:  healthResult{Status: "ok"},
			ID:      req.ID,
		}

	case "quiesce.stop":
		// Daemon is about to close the channel, snapshot, and stop the VM.
		// Suppress optional traffic. The primary process keeps running —
		// it will be frozen by vm.pause and captured in the snapshot.
		// After ACK, any frames we send may be dropped.
		log.Println("quiesce.stop: entering quiesced mode")
		hrpc.quiesced = true
		return &rpcResponse{
			JSONRPC: "2.0",
			Result:  map[string]string{"status": "ready"},
			ID:      req.ID,
		}

	case "shutdown":
		log.Println("shutdown requested")
		return &rpcResponse{
			JSONRPC: "2.0",
			Result:  map[string]string{"status": "shutting_down"},
			ID:      req.ID,
		}

	default:
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
			ID: req.ID,
		}
	}
}

// handleRun starts the primary process asynchronously, streaming output as log
// notifications. When the process exits, it sends a processExited notification.
// Only one primary process is allowed per instance.
func handleRun(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *processTracker, hrpc *harnessRPC) *rpcResponse {
	var params runParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: fmt.Sprintf("invalid params: %v", err),
			},
			ID: req.ID,
		}
	}

	if len(params.Command) == 0 {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: "command is required",
			},
			ID: req.ID,
		}
	}

	log.Printf("run: %v", params.Command)

	// Store capability token for guest API requests
	if params.CapabilityToken != "" && hrpc != nil {
		hrpc.capabilityToken = params.CapabilityToken
		log.Printf("capability token received (guest API enabled)")
	}

	// Start port proxies for exposed ports before the app.
	// These forward 0.0.0.0:port → 127.0.0.1:port so that apps binding
	// to localhost are reachable via gvproxy's virtio-net ingress.
	var pp *portProxy
	if len(params.ExposePorts) > 0 {
		pp = startPortProxies(params.ExposePorts)
	} else {
		// Create empty portProxy for runtime expose support
		pp = &portProxy{}
	}
	// Store on hrpc so ports_changed notifications and guest API can use it
	if hrpc != nil {
		hrpc.portProxy = pp
	}

	cmd, err := startPrimaryProcess(ctx, params, conn, tracker, hrpc)
	if err != nil {
		if pp != nil {
			pp.Stop()
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32000,
				Message: fmt.Sprintf("start process: %v", err),
			},
			ID: req.ID,
		}
	}

	if !tracker.setPrimary(cmd) {
		cmd.Process.Kill()
		if pp != nil {
			pp.Stop()
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32000,
				Message: "primary process already running",
			},
			ID: req.ID,
		}
	}

	// Start activity monitor — sends periodic heartbeats to aegisd when the
	// guest has outbound connections, CPU usage, or network traffic.
	go monitorActivity(ctx, cmd.Process.Pid, conn, hrpc)

	return &rpcResponse{
		JSONRPC: "2.0",
		Result: runResult{
			PID:       cmd.Process.Pid,
			StartedAt: time.Now().Format(time.RFC3339),
		},
		ID: req.ID,
	}
}

// monitorActivity periodically checks guest activity and sends "activity"
// notifications to aegisd. Only sends when activity is detected — silence
// means idle, which lets the idle timer run naturally.
func monitorActivity(ctx context.Context, pid int, conn net.Conn, hrpc *harnessRPC) {
	// Baseline samples for deltas
	prevCPU := processUsedCPUTicks(pid)
	prevTx, prevRx := ethByteCounters()

	for {
		// 5s ± 500ms jitter to avoid synchronizing multiple VMs
		jitter := time.Duration(time.Now().UnixNano()%1000-500) * time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(5*time.Second + jitter):
		}

		// Sample current values
		curCPU := processUsedCPUTicks(pid)
		curTx, curRx := ethByteCounters()
		tcp := countEstablishedTCP()

		// Compute deltas
		cpuDelta := curCPU - prevCPU
		netDelta := (curTx - prevTx) + (curRx - prevRx)
		prevCPU = curCPU
		prevTx = curTx
		prevRx = curRx

		// Convert CPU ticks to approximate milliseconds (assuming 100 HZ = 10ms/tick)
		cpuMs := cpuDelta * 10

		// Only send if meaningful activity detected.
		// net_bytes threshold filters out background ARP/keepalive noise (~70 bytes/5s).
		const netBytesThreshold = 512
		log.Printf("activity probe: tcp=%d cpu_ms=%d net_bytes=%d (threshold=%d)", tcp, cpuMs, netDelta, netBytesThreshold)
		if hrpc.quiesced {
			continue // suppressed — daemon is about to snapshot
		}
		if tcp > 0 || cpuMs > 0 || netDelta > netBytesThreshold {
			err := sendNotification(conn, "activity", map[string]interface{}{
				"tcp":       tcp,
				"cpu_ms":    cpuMs,
				"net_bytes": netDelta,
			})
			if err != nil {
				return // connection dead
			}
		}
	}
}

// handleExec starts a command asynchronously in the guest, streaming output as log
// notifications with exec_id, and sending execDone when the process exits.
func handleExec(ctx context.Context, req *rpcRequest, conn net.Conn, tracker *processTracker) *rpcResponse {
	var params execParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: fmt.Sprintf("invalid params: %v", err),
			},
			ID: req.ID,
		}
	}

	if len(params.Command) == 0 {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: "command is required",
			},
			ID: req.ID,
		}
	}

	if params.ExecID == "" {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32602,
				Message: "exec_id is required",
			},
			ID: req.ID,
		}
	}

	log.Printf("exec: %v (exec_id=%s)", params.Command, params.ExecID)

	cmd, err := startExecProcess(ctx, params, conn)
	if err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			Error: &rpcError{
				Code:    -32000,
				Message: fmt.Sprintf("exec: %v", err),
			},
			ID: req.ID,
		}
	}

	tracker.add(cmd)

	return &rpcResponse{
		JSONRPC: "2.0",
		Result: execResult{
			ExecID:    params.ExecID,
			StartedAt: time.Now().Format(time.RFC3339),
		},
		ID: req.ID,
	}
}

// agentRuntimeAddr is where the guest agent runtime listens for tether frames.
const agentRuntimeAddr = "http://127.0.0.1:7778"

// tetherBuffer buffers tether frames until the agent runtime is ready.
// Frames are delivered in order. Once the agent is reachable, subsequent
// frames are forwarded directly (no buffering).
var tetherBuffer = &tetherFrameBuffer{
	ready: make(chan struct{}),
}

type tetherFrameBuffer struct {
	mu      sync.Mutex
	queue   []json.RawMessage
	started bool
	ready   chan struct{}
}

// enqueue adds a frame. If the agent is already reachable, sends directly.
// Otherwise buffers and kicks off a drain goroutine on first frame.
func (b *tetherFrameBuffer) enqueue(params json.RawMessage) {
	// Fast path: agent already confirmed reachable
	select {
	case <-b.ready:
		sendToAgent(params)
		return
	default:
	}

	b.mu.Lock()
	// Copy params since the underlying buffer may be reused
	cp := make(json.RawMessage, len(params))
	copy(cp, params)
	if len(b.queue) < 100 {
		b.queue = append(b.queue, cp)
	} else {
		log.Printf("tether: buffer full, dropping frame")
	}
	if !b.started {
		b.started = true
		go b.drain()
	}
	b.mu.Unlock()
}

// drain waits for the agent to become reachable, then flushes all buffered frames.
func (b *tetherFrameBuffer) drain() {
	// Wait for agent to be ready (poll every 200ms, up to 30s)
	for i := 0; i < 150; i++ {
		resp, err := http.Get(agentRuntimeAddr + "/v1/tether/recv")
		if err == nil {
			resp.Body.Close()
			// Agent is listening (405 = wrong method, but server is up)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	close(b.ready)

	// Flush buffered frames
	b.mu.Lock()
	queued := b.queue
	b.queue = nil
	b.mu.Unlock()

	for _, params := range queued {
		sendToAgent(params)
	}
}

func sendToAgent(params json.RawMessage) {
	resp, err := http.Post(agentRuntimeAddr+"/v1/tether/recv", "application/json", bytes.NewReader(params))
	if err != nil {
		log.Printf("tether: send to agent failed: %v", err)
		return
	}
	resp.Body.Close()
}

// handleTetherFrame processes an incoming tether.frame:
// 1. Emit event.ack immediately (delivery receipt)
// 2. Persist to /workspace/tether/inbox.ndjson
// 3. Forward to agent runtime (if available)
func handleTetherFrame(conn net.Conn, params json.RawMessage) {
	// Parse frame to extract session info for ack
	var frame struct {
		Session struct {
			Channel string `json:"channel"`
			ID      string `json:"id"`
		} `json:"session"`
		MsgID string `json:"msg_id"`
	}
	json.Unmarshal(params, &frame)

	// 1. Emit ack
	sendNotification(conn, "tether.frame", map[string]interface{}{
		"v":       1,
		"type":    "event.ack",
		"session": frame.Session,
		"msg_id":  frame.MsgID,
		"payload": map[string]string{"status": "received"},
	})

	// 2. Persist to inbox
	tetherPersistFrame(params)

	// 3. Forward to agent runtime
	tetherBuffer.enqueue(params)
}

// tetherInboxPath is the append-only inbox file for persisting tether frames.
const tetherInboxPath = "/workspace/tether/inbox.ndjson"

// tetherPersistFrame appends a tether frame to the inbox file.
// Creates the directory and file if they don't exist.
func tetherPersistFrame(params json.RawMessage) {
	dir := filepath.Dir(tetherInboxPath)
	os.MkdirAll(dir, 0755)

	f, err := os.OpenFile(tetherInboxPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("tether: persist inbox: %v", err)
		return
	}
	defer f.Close()
	f.Write(params)
	f.Write([]byte("\n"))
}

// handlePortsChanged handles a ports_changed notification from aegisd.
// This is sent when the HOST exposes or unexposes a port on a running instance.
// The harness starts/stops the local port proxy accordingly.
func handlePortsChanged(params json.RawMessage, hrpc *harnessRPC) {
	var msg struct {
		Action    string `json:"action"`
		GuestPort int    `json:"guest_port"`
	}
	if err := json.Unmarshal(params, &msg); err != nil {
		log.Printf("ports_changed: invalid params: %v", err)
		return
	}

	pp := hrpc.portProxy
	if pp == nil {
		return
	}

	switch msg.Action {
	case "expose":
		log.Printf("ports_changed: expose guest port %d", msg.GuestPort)
		pp.AddPort(msg.GuestPort)
	case "unexpose":
		log.Printf("ports_changed: unexpose guest port %d", msg.GuestPort)
		pp.RemovePort(msg.GuestPort)
	}
}

// sendNotification sends a JSON-RPC notification (no ID, no response expected).
func sendNotification(conn net.Conn, method string, params interface{}) error {
	notif := rpcNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(notif)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}
