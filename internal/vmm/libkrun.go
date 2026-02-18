package vmm

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/xfeldman/aegis/internal/config"
)

// WorkerConfig is the JSON configuration sent to the vmm-worker process.
// Must match the WorkerConfig in cmd/aegis-vmm-worker/main.go.
type WorkerConfig struct {
	RootfsPath string `json:"rootfs_path"`
	MemoryMB   int    `json:"memory_mb"`
	VCPUs      int    `json:"vcpus"`
	ExecPath   string `json:"exec_path"`
	HostAddr   string `json:"host_addr"` // host:port for harness to connect back to
}

type vmInstance struct {
	id       string
	config   VMConfig
	cmd      *exec.Cmd
	hostAddr string // host:port the harness will connect to
	done     chan struct{}
}

// LibkrunVMM implements the VMM interface using libkrun on macOS.
// It spawns a separate worker process per VM because krun_start_enter() takes
// over the calling process and never returns.
type LibkrunVMM struct {
	mu        sync.Mutex
	instances map[string]*vmInstance
	dataDir   string
	workerBin string
}

// NewLibkrunVMM creates a new libkrun VMM backend.
func NewLibkrunVMM(cfg *config.Config) (*LibkrunVMM, error) {
	workerBin := filepath.Join(cfg.BinDir, "aegis-vmm-worker")
	if _, err := os.Stat(workerBin); err != nil {
		return nil, fmt.Errorf("vmm-worker binary not found at %s: %w", workerBin, err)
	}

	return &LibkrunVMM{
		instances: make(map[string]*vmInstance),
		dataDir:   cfg.DataDir,
		workerBin: workerBin,
	}, nil
}

func (l *LibkrunVMM) CreateVM(cfg VMConfig) (Handle, error) {
	id := fmt.Sprintf("vm-%d", time.Now().UnixNano())
	h := Handle{ID: id}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.instances[id] = &vmInstance{
		id:     id,
		config: cfg,
		done:   make(chan struct{}),
	}

	return h, nil
}

// StartVM starts the VM. hostAddr is the host TCP address the harness should
// connect back to (e.g., "127.0.0.1:59123"). This must be set before calling StartVM
// via SetHostAddr.
func (l *LibkrunVMM) StartVM(h Handle) error {
	l.mu.Lock()
	inst, ok := l.instances[h.ID]
	if !ok {
		l.mu.Unlock()
		return fmt.Errorf("vm %s not found", h.ID)
	}
	hostAddr := inst.hostAddr
	l.mu.Unlock()

	if hostAddr == "" {
		return fmt.Errorf("vm %s: hostAddr not set (call SetHostAddr before StartVM)", h.ID)
	}

	wc := WorkerConfig{
		RootfsPath: inst.config.RootfsPath,
		MemoryMB:   inst.config.MemoryMB,
		VCPUs:      inst.config.VCPUs,
		ExecPath:   "/usr/bin/aegis-harness",
		HostAddr:   hostAddr,
	}

	wcJSON, err := json.Marshal(wc)
	if err != nil {
		return fmt.Errorf("marshal worker config: %w", err)
	}

	cmd := exec.Command(l.workerBin)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout // TODO: capture to log
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"AEGIS_VMM_CONFIG="+string(wcJSON),
		// libkrun dynamically loads libkrunfw at runtime via dlopen.
		"DYLD_FALLBACK_LIBRARY_PATH=/opt/homebrew/lib:/usr/local/lib:/usr/lib",
	)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start vmm-worker: %w", err)
	}

	l.mu.Lock()
	inst.cmd = cmd
	l.mu.Unlock()

	// Monitor worker process exit in background
	go func() {
		_ = cmd.Wait()
		close(inst.done)
	}()

	return nil
}

// SetHostAddr sets the host TCP address the harness should connect back to.
// Must be called before StartVM.
func (l *LibkrunVMM) SetHostAddr(h Handle, addr string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	inst, ok := l.instances[h.ID]
	if !ok {
		return fmt.Errorf("vm %s not found", h.ID)
	}
	inst.hostAddr = addr
	return nil
}

// Done returns a channel that is closed when the VM worker process exits.
func (l *LibkrunVMM) Done(h Handle) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()

	inst, ok := l.instances[h.ID]
	if !ok {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return inst.done
}

func (l *LibkrunVMM) PauseVM(h Handle) error {
	return ErrNotSupported
}

func (l *LibkrunVMM) ResumeVM(h Handle) error {
	return ErrNotSupported
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

	l.mu.Lock()
	delete(l.instances, h.ID)
	l.mu.Unlock()

	return nil
}

func (l *LibkrunVMM) Snapshot(h Handle, path string) error {
	return ErrNotSupported
}

func (l *LibkrunVMM) Restore(snapshotPath string) (Handle, error) {
	return Handle{}, ErrNotSupported
}

func (l *LibkrunVMM) Capabilities() BackendCaps {
	return BackendCaps{
		Pause:           false,
		SnapshotRestore: false,
		Name:            "libkrun",
	}
}
