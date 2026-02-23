package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
)

// WorkerConfig is the JSON configuration sent to the vmm-worker process.
// Must match the WorkerConfig in cmd/aegis-vmm-worker/main.go.
type WorkerConfig struct {
	RootfsPath    string   `json:"rootfs_path"`
	MemoryMB      int      `json:"memory_mb"`
	VCPUs         int      `json:"vcpus"`
	ExecPath      string   `json:"exec_path"`
	HostAddr      string   `json:"host_addr"`
	PortMap       []string `json:"port_map,omitempty"`       // e.g. ["8080:80"] — TSI only
	MappedVolumes []string `json:"mapped_volumes,omitempty"` // e.g. ["workspace:/path"] — tag:path

	// gvproxy networking (when NetworkMode == "gvproxy")
	// The worker embeds the gvisor-tap-vsock library in-process.
	NetworkMode string             `json:"network_mode,omitempty"`
	VsockPort   int                `json:"vsock_port,omitempty"`
	ExposePorts []WorkerPortForward `json:"expose_ports,omitempty"` // pre-expose via in-process gvproxy
	SocketDir   string             `json:"socket_dir,omitempty"`   // where worker creates net socket
}

// WorkerPortForward describes a port to forward from host to guest.
type WorkerPortForward struct {
	HostPort  int `json:"host_port"`
	GuestPort int `json:"guest_port"`
}

// harnessVsockPort is the vsock port the harness connects to for the control channel.
const harnessVsockPort = 5000

type vmInstance struct {
	id        string
	config    VMConfig
	cmd       *exec.Cmd
	done      chan struct{}
	endpoints []HostEndpoint
	sockDir   string // directory for gvproxy sockets (empty if TSI mode)
}

// LibkrunVMM implements the VMM interface using libkrun on macOS.
// It spawns a separate worker process per VM because krun_start_enter() takes
// over the calling process and never returns.
type LibkrunVMM struct {
	mu        sync.Mutex
	instances map[string]*vmInstance
	workerBin string
	cfg       *config.Config
}

func NewLibkrunVMM(cfg *config.Config) (*LibkrunVMM, error) {
	workerBin := filepath.Join(cfg.BinDir, "aegis-vmm-worker")
	if _, err := os.Stat(workerBin); err != nil {
		return nil, fmt.Errorf("vmm-worker binary not found at %s: %w", workerBin, err)
	}

	return &LibkrunVMM{
		instances: make(map[string]*vmInstance),
		workerBin: workerBin,
		cfg:       cfg,
	}, nil
}

func (l *LibkrunVMM) CreateVM(cfg VMConfig) (Handle, error) {
	if cfg.Rootfs.Type != RootFSDirectory {
		return Handle{}, fmt.Errorf("libkrun backend requires RootFSDirectory, got %s", cfg.Rootfs.Type)
	}

	id := fmt.Sprintf("vm-%d", time.Now().UnixNano())

	l.mu.Lock()
	defer l.mu.Unlock()

	l.instances[id] = &vmInstance{
		id:     id,
		config: cfg,
		done:   make(chan struct{}),
	}

	return Handle{ID: id}, nil
}

// StartVM boots the VM and returns a ControlChannel for harness communication.
//
// Two networking modes:
//   - gvproxy: worker embeds gvisor-tap-vsock in-process for virtio-net.
//     Port forwarding pre-exposed before VM boot. SIGSTOP freezes everything.
//   - tsi (legacy): TCP listener on localhost, harness connects via TSI.
//     Port mapping via krun_set_port_map. Known ~32KB outbound body limit.
func (l *LibkrunVMM) StartVM(h Handle) (ControlChannel, error) {
	l.mu.Lock()
	inst, ok := l.instances[h.ID]
	if !ok {
		l.mu.Unlock()
		return nil, fmt.Errorf("vm %s not found", h.ID)
	}
	cfg := inst.config
	l.mu.Unlock()

	useGvproxy := l.cfg.NetworkBackend == "gvproxy"

	// Build mapped volumes list
	var mappedVolumes []string
	if cfg.WorkspacePath != "" {
		mappedVolumes = append(mappedVolumes, "workspace:"+cfg.WorkspacePath)
	}

	// Common worker config fields
	wc := WorkerConfig{
		RootfsPath:    cfg.Rootfs.Path,
		MemoryMB:      cfg.MemoryMB,
		VCPUs:         cfg.VCPUs,
		ExecPath:      "/usr/bin/aegis-harness",
		MappedVolumes: mappedVolumes,
	}

	var ln net.Listener

	if useGvproxy {
		// --- gvproxy networking path ---
		// Worker embeds gvproxy in-process. We just allocate host ports
		// and pass the config. No separate gvproxy process to manage.
		sockDir := filepath.Join(l.cfg.DataDir, "sockets")

		// Listen on unix socket for harness control channel (vsock → unix socket)
		ctlSocketPath := filepath.Join(sockDir, fmt.Sprintf("ctl-%s.sock", h.ID))
		os.Remove(ctlSocketPath) // clean stale
		var err error
		ln, err = net.Listen("unix", ctlSocketPath)
		if err != nil {
			return nil, fmt.Errorf("listen for harness (unix): %w", err)
		}

		// Allocate host ports for exposed guest ports
		var endpoints []HostEndpoint
		var exposePorts []WorkerPortForward
		for _, ep := range cfg.ExposePorts {
			tmpLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				ln.Close()
				return nil, fmt.Errorf("allocate port for guest port %d: %w", ep.GuestPort, err)
			}
			hostPort := tmpLn.Addr().(*net.TCPAddr).Port
			tmpLn.Close()

			endpoints = append(endpoints, HostEndpoint{
				GuestPort: ep.GuestPort,
				HostPort:  hostPort,
				Protocol:  ep.Protocol,
			})
			exposePorts = append(exposePorts, WorkerPortForward{
				HostPort:  hostPort,
				GuestPort: ep.GuestPort,
			})
		}

		l.mu.Lock()
		inst.endpoints = endpoints
		inst.sockDir = sockDir
		l.mu.Unlock()

		wc.NetworkMode = "gvproxy"
		wc.VsockPort = harnessVsockPort
		wc.HostAddr = ctlSocketPath
		wc.ExposePorts = exposePorts
		wc.SocketDir = sockDir
	} else {
		// --- TSI networking path (legacy) ---
		var err error
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen for harness: %w", err)
		}
		wc.HostAddr = ln.Addr().String()

		// Allocate host ports + build TSI port map
		var portMap []string
		var endpoints []HostEndpoint
		for _, ep := range cfg.ExposePorts {
			tmpLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				ln.Close()
				return nil, fmt.Errorf("allocate port for guest port %d: %w", ep.GuestPort, err)
			}
			hostPort := tmpLn.Addr().(*net.TCPAddr).Port
			tmpLn.Close()

			portMap = append(portMap, fmt.Sprintf("%d:%d", hostPort, ep.GuestPort))
			endpoints = append(endpoints, HostEndpoint{
				GuestPort: ep.GuestPort,
				HostPort:  hostPort,
				Protocol:  ep.Protocol,
			})
		}

		l.mu.Lock()
		inst.endpoints = endpoints
		l.mu.Unlock()

		wc.PortMap = portMap
	}

	// Spawn vmm-worker
	wcJSON, err := json.Marshal(wc)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("marshal worker config: %w", err)
	}

	cmd := exec.Command(l.workerBin)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"AEGIS_VMM_CONFIG="+string(wcJSON),
		// libkrun dynamically loads libkrunfw at runtime via dlopen.
		"DYLD_FALLBACK_LIBRARY_PATH=/opt/homebrew/lib:/usr/local/lib:/usr/lib",
	)

	if err := cmd.Start(); err != nil {
		ln.Close()
		return nil, fmt.Errorf("start vmm-worker: %w", err)
	}

	l.mu.Lock()
	inst.cmd = cmd
	l.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		close(inst.done)
	}()

	// Wait for harness to connect back (with timeout)
	if tcpLn, ok := ln.(*net.TCPListener); ok {
		tcpLn.SetDeadline(time.Now().Add(90 * time.Second))
	} else if unixLn, ok := ln.(*net.UnixListener); ok {
		unixLn.SetDeadline(time.Now().Add(90 * time.Second))
	}
	conn, err := ln.Accept()
	ln.Close() // only need one connection
	if err != nil {
		return nil, fmt.Errorf("harness did not connect within 90s: %w", err)
	}

	return NewNetControlChannel(conn), nil
}

func (l *LibkrunVMM) HostEndpoints(h Handle) ([]HostEndpoint, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	inst, ok := l.instances[h.ID]
	if !ok {
		return nil, fmt.Errorf("vm %s not found", h.ID)
	}
	eps := make([]HostEndpoint, len(inst.endpoints))
	copy(eps, inst.endpoints)
	return eps, nil
}

// DynamicExposePort creates a gvproxy port forward at runtime.
// Returns the allocated host port. Only works in gvproxy mode.
func (l *LibkrunVMM) DynamicExposePort(h Handle, guestPort int) (int, error) {
	l.mu.Lock()
	inst, ok := l.instances[h.ID]
	l.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("vm %s not found", h.ID)
	}
	if inst.sockDir == "" {
		return 0, fmt.Errorf("dynamic expose not supported in TSI mode")
	}
	if inst.cmd == nil || inst.cmd.Process == nil {
		return 0, fmt.Errorf("vm %s not running", h.ID)
	}

	// Allocate a random host port
	tmpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate host port: %w", err)
	}
	hostPort := tmpLn.Addr().(*net.TCPAddr).Port
	tmpLn.Close()

	// Call gvproxy API on the vmm-worker's unix socket
	apiSock := filepath.Join(inst.sockDir, fmt.Sprintf("gvproxy-%d.sock", inst.cmd.Process.Pid))
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", apiSock, 5*time.Second)
			},
		},
		Timeout: 10 * time.Second,
	}

	reqBody := fmt.Sprintf(`{"local":"127.0.0.1:%d","remote":"192.168.127.2:%d","protocol":"tcp"}`,
		hostPort, guestPort)
	resp, err := client.Post("http://gvproxy/services/forwarder/expose",
		"application/json", strings.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("gvproxy expose: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("gvproxy expose returned %d: %s", resp.StatusCode, body)
	}

	// Add to endpoints
	l.mu.Lock()
	inst.endpoints = append(inst.endpoints, HostEndpoint{
		GuestPort: guestPort,
		HostPort:  hostPort,
		Protocol:  "tcp",
	})
	l.mu.Unlock()

	log.Printf("vmm: dynamic expose guest:%d → host:%d (vm %s)", guestPort, hostPort, h.ID)
	return hostPort, nil
}

// DynamicUnexposePort removes a gvproxy port forward at runtime.
func (l *LibkrunVMM) DynamicUnexposePort(h Handle, guestPort int) error {
	l.mu.Lock()
	inst, ok := l.instances[h.ID]
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("vm %s not found", h.ID)
	}

	// Find and remove the endpoint
	l.mu.Lock()
	var hostPort int
	for i, ep := range inst.endpoints {
		if ep.GuestPort == guestPort {
			hostPort = ep.HostPort
			inst.endpoints = append(inst.endpoints[:i], inst.endpoints[i+1:]...)
			break
		}
	}
	l.mu.Unlock()

	if hostPort == 0 {
		return nil // not found, nothing to do
	}

	// Call gvproxy API to unexpose
	if inst.sockDir != "" && inst.cmd != nil && inst.cmd.Process != nil {
		apiSock := filepath.Join(inst.sockDir, fmt.Sprintf("gvproxy-%d.sock", inst.cmd.Process.Pid))
		client := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.DialTimeout("unix", apiSock, 5*time.Second)
				},
			},
			Timeout: 10 * time.Second,
		}

		reqBody := fmt.Sprintf(`{"local":"127.0.0.1:%d","remote":"192.168.127.2:%d","protocol":"tcp"}`,
			hostPort, guestPort)
		resp, err := client.Post("http://gvproxy/services/forwarder/unexpose",
			"application/json", strings.NewReader(reqBody))
		if err != nil {
			log.Printf("vmm: dynamic unexpose guest:%d: %v", guestPort, err)
		} else {
			resp.Body.Close()
		}
	}

	log.Printf("vmm: dynamic unexpose guest:%d (vm %s)", guestPort, h.ID)
	return nil
}

func (l *LibkrunVMM) PauseVM(h Handle) error {
	l.mu.Lock()
	inst, ok := l.instances[h.ID]
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("vm %s not found", h.ID)
	}
	if inst.cmd == nil || inst.cmd.Process == nil {
		return fmt.Errorf("vm %s has no running process", h.ID)
	}
	return inst.cmd.Process.Signal(syscall.SIGSTOP)
}

func (l *LibkrunVMM) ResumeVM(h Handle) error {
	l.mu.Lock()
	inst, ok := l.instances[h.ID]
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("vm %s not found", h.ID)
	}
	if inst.cmd == nil || inst.cmd.Process == nil {
		return fmt.Errorf("vm %s has no running process", h.ID)
	}
	return inst.cmd.Process.Signal(syscall.SIGCONT)
}

func (l *LibkrunVMM) StopVM(h Handle) error {
	l.mu.Lock()
	inst, ok := l.instances[h.ID]
	if !ok {
		l.mu.Unlock()
		return fmt.Errorf("vm %s not found", h.ID)
	}
	l.mu.Unlock()

	if inst.cmd != nil && inst.cmd.Process != nil {
		_ = inst.cmd.Process.Kill()
		_ = inst.cmd.Wait()
	}

	// Clean up control socket (worker cleans up its own net socket on exit)
	ctlSocket := filepath.Join(l.cfg.DataDir, "sockets", fmt.Sprintf("ctl-%s.sock", h.ID))
	os.Remove(ctlSocket)

	l.mu.Lock()
	delete(l.instances, h.ID)
	l.mu.Unlock()

	return nil
}

func (l *LibkrunVMM) Capabilities() BackendCaps {
	return BackendCaps{
		Pause:          true,
		RootFSType:     RootFSDirectory,
		Name:           "libkrun",
		NetworkBackend: l.cfg.NetworkBackend,
	}
}
