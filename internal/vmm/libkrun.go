package vmm

import (
	"encoding/json"
	"fmt"
	"net"
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
	HostAddr   string `json:"host_addr"`
}

type vmInstance struct {
	id     string
	config VMConfig
	cmd    *exec.Cmd
	done   chan struct{}
}

// LibkrunVMM implements the VMM interface using libkrun on macOS.
// It spawns a separate worker process per VM because krun_start_enter() takes
// over the calling process and never returns.
type LibkrunVMM struct {
	mu        sync.Mutex
	instances map[string]*vmInstance
	workerBin string
}

func NewLibkrunVMM(cfg *config.Config) (*LibkrunVMM, error) {
	workerBin := filepath.Join(cfg.BinDir, "aegis-vmm-worker")
	if _, err := os.Stat(workerBin); err != nil {
		return nil, fmt.Errorf("vmm-worker binary not found at %s: %w", workerBin, err)
	}

	return &LibkrunVMM{
		instances: make(map[string]*vmInstance),
		workerBin: workerBin,
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
// Internally: starts a TCP listener on localhost, spawns the vmm-worker
// (which boots the VM via libkrun), and waits for the harness to connect
// back via TSI. Returns a ready-to-use ControlChannel.
func (l *LibkrunVMM) StartVM(h Handle) (ControlChannel, error) {
	l.mu.Lock()
	inst, ok := l.instances[h.ID]
	if !ok {
		l.mu.Unlock()
		return nil, fmt.Errorf("vm %s not found", h.ID)
	}
	cfg := inst.config
	l.mu.Unlock()

	// 1. Start TCP listener for harness callback
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for harness: %w", err)
	}

	hostAddr := ln.Addr().String()

	// 2. Spawn vmm-worker
	wc := WorkerConfig{
		RootfsPath: cfg.Rootfs.Path,
		MemoryMB:   cfg.MemoryMB,
		VCPUs:      cfg.VCPUs,
		ExecPath:   "/usr/bin/aegis-harness",
		HostAddr:   hostAddr,
	}

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

	// 3. Wait for harness to connect back (with timeout)
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Second))
	conn, err := ln.Accept()
	ln.Close() // only need one connection
	if err != nil {
		return nil, fmt.Errorf("harness did not connect within 30s: %w", err)
	}

	return NewNetControlChannel(conn), nil
}

func (l *LibkrunVMM) PauseVM(h Handle) error  { return ErrNotSupported }
func (l *LibkrunVMM) ResumeVM(h Handle) error { return ErrNotSupported }

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
		RootFSType:      RootFSDirectory,
		Name:            "libkrun",
	}
}
