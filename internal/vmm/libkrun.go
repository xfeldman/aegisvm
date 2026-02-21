package vmm

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	PortMap       []string `json:"port_map,omitempty"`       // e.g. ["8080:80"] — host_port:guest_port
	MappedVolumes []string `json:"mapped_volumes,omitempty"` // e.g. ["workspace:/path"] — tag:path

	// gvproxy networking (when NetworkMode == "gvproxy")
	NetworkMode   string `json:"network_mode,omitempty"`
	GvproxySocket string `json:"gvproxy_socket,omitempty"`
	VsockPort     int    `json:"vsock_port,omitempty"`
}

// harnessVsockPort is the vsock port the harness connects to for the control channel.
const harnessVsockPort = 5000

type vmInstance struct {
	id        string
	config    VMConfig
	cmd       *exec.Cmd
	done      chan struct{}
	endpoints []HostEndpoint
	gvproxy   *gvproxyInstance // non-nil when using gvproxy networking
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
//   - gvproxy: spawns gvproxy for virtio-net, uses unix socket + vsock for control channel.
//     Port forwarding via gvproxy HTTP API. No TSI — real NIC in guest.
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

	useGvproxy := l.cfg.NetworkBackend == "gvproxy" && l.cfg.GvproxyBin != ""

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
	var gvp *gvproxyInstance

	if useGvproxy {
		// --- gvproxy networking path ---
		sockDir := filepath.Join(l.cfg.DataDir, "sockets")

		// 1. Spawn gvproxy
		var err error
		gvp, err = startGvproxy(l.cfg.GvproxyBin, h.ID, sockDir)
		if err != nil {
			return nil, fmt.Errorf("start gvproxy: %w", err)
		}

		// 2. Listen on unix socket for harness control channel (vsock → unix socket)
		ctlSocketPath := filepath.Join(sockDir, fmt.Sprintf("ctl-%s.sock", h.ID))
		os.Remove(ctlSocketPath) // clean stale
		ln, err = net.Listen("unix", ctlSocketPath)
		if err != nil {
			gvp.Stop()
			return nil, fmt.Errorf("listen for harness (unix): %w", err)
		}

		// 3. Allocate host ports for exposed guest ports
		var endpoints []HostEndpoint
		for _, ep := range cfg.ExposePorts {
			tmpLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				ln.Close()
				gvp.Stop()
				return nil, fmt.Errorf("allocate port for guest port %d: %w", ep.GuestPort, err)
			}
			hostPort := tmpLn.Addr().(*net.TCPAddr).Port
			tmpLn.Close()

			endpoints = append(endpoints, HostEndpoint{
				GuestPort: ep.GuestPort,
				HostPort:  hostPort,
				Protocol:  ep.Protocol,
			})
		}

		l.mu.Lock()
		inst.endpoints = endpoints
		l.mu.Unlock()

		wc.NetworkMode = "gvproxy"
		wc.GvproxySocket = gvp.netSocket
		wc.VsockPort = harnessVsockPort
		wc.HostAddr = ctlSocketPath // reuse field for unix socket path
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
		if gvp != nil {
			gvp.Stop()
		}
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
		if gvp != nil {
			gvp.Stop()
		}
		return nil, fmt.Errorf("start vmm-worker: %w", err)
	}

	l.mu.Lock()
	inst.cmd = cmd
	inst.gvproxy = gvp
	l.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		close(inst.done)
	}()

	// Wait for harness to connect back (with timeout)
	if tcpLn, ok := ln.(*net.TCPListener); ok {
		tcpLn.SetDeadline(time.Now().Add(30 * time.Second))
	} else if unixLn, ok := ln.(*net.UnixListener); ok {
		unixLn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	conn, err := ln.Accept()
	ln.Close() // only need one connection
	if err != nil {
		if gvp != nil {
			gvp.Stop()
		}
		return nil, fmt.Errorf("harness did not connect within 30s: %w", err)
	}

	// For gvproxy: now that harness is connected (VM is alive),
	// expose ports via gvproxy API before marking instance as RUNNING.
	if gvp != nil {
		l.mu.Lock()
		endpoints := inst.endpoints
		l.mu.Unlock()

		for _, ep := range endpoints {
			if err := gvp.ExposePort(ep.HostPort, ep.GuestPort); err != nil {
				log.Printf("gvproxy: expose port %d→%d failed: %v", ep.HostPort, ep.GuestPort, err)
				conn.Close()
				gvp.Stop()
				return nil, fmt.Errorf("gvproxy expose port %d: %w", ep.GuestPort, err)
			}
		}
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

	// Stop gvproxy process and clean up sockets
	if inst.gvproxy != nil {
		inst.gvproxy.Stop()
	}

	// Clean up control socket
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
