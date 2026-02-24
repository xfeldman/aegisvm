package config

import (
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Config holds aegisd runtime configuration.
type Config struct {
	// DataDir is the base directory for aegis runtime data.
	DataDir string

	// BinDir is the directory containing aegis binaries.
	BinDir string

	// SocketPath is the unix socket path for the aegisd API.
	SocketPath string

	// BaseRootfsPath is the path to the base rootfs directory.
	BaseRootfsPath string

	// DefaultMemoryMB is the default VM memory in megabytes.
	DefaultMemoryMB int

	// DefaultVCPUs is the default number of virtual CPUs.
	DefaultVCPUs int

	// RouterAddr is the address for the HTTP router (serve mode).
	RouterAddr string

	// DBPath is the path to the SQLite database.
	DBPath string

	// ImageCacheDir is the directory for cached OCI image rootfs directories.
	ImageCacheDir string

	// OverlaysDir is the directory for instance rootfs overlays.
	OverlaysDir string

	// WorkspacesDir is the directory for workspace volumes.
	WorkspacesDir string

	// LogsDir is the directory for per-instance log files.
	LogsDir string

	// MasterKeyPath is the path to the AES-256 master key for secret encryption.
	MasterKeyPath string

	// PauseAfterIdle is the duration after which an idle instance is paused (SIGSTOP).
	PauseAfterIdle time.Duration

	// StopAfterIdle is the duration after which a paused instance is stopped.
	StopAfterIdle time.Duration

	// NetworkBackend selects the data-plane networking mode for libkrun VMs.
	// "auto" (default): gvproxy on darwin, tap on linux.
	// "gvproxy": in-process gvisor-tap-vsock (compiled into vmm-worker).
	// "tsi": TSI unconditionally (known ~32KB outbound body limit).
	// "tap": tap + iptables NAT (Cloud Hypervisor on Linux).
	NetworkBackend string

	// KernelPath is the path to the vmlinux kernel image (Linux/CH only).
	KernelPath string

	// CloudHypervisorBin is the path to the cloud-hypervisor binary.
	// Empty means search PATH.
	CloudHypervisorBin string

	// VirtiofsdBin is the path to the virtiofsd binary.
	// Empty means search PATH.
	VirtiofsdBin string

	// SnapshotsDir is the directory for VM memory snapshots (Linux/CH only).
	SnapshotsDir string
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	aegisDir := filepath.Join(homeDir, ".aegis")
	execDir := executableDir()

	// Platform-specific base rootfs path
	baseRootfs := filepath.Join(aegisDir, "base-rootfs")
	if runtime.GOOS == "linux" {
		baseRootfs = filepath.Join(aegisDir, "base-rootfs.ext4")
	}

	return &Config{
		DataDir:            filepath.Join(aegisDir, "data"),
		BinDir:             execDir,
		SocketPath:         filepath.Join(aegisDir, "aegisd.sock"),
		BaseRootfsPath:     baseRootfs,
		DefaultMemoryMB:    512,
		DefaultVCPUs:       1,
		RouterAddr:         "127.0.0.1:8099",
		DBPath:             filepath.Join(aegisDir, "data", "aegis.db"),
		ImageCacheDir:      filepath.Join(aegisDir, "data", "images"),
		OverlaysDir:        filepath.Join(aegisDir, "data", "overlays"),
		WorkspacesDir:      filepath.Join(aegisDir, "data", "workspaces"),
		LogsDir:            filepath.Join(aegisDir, "data", "logs"),
		MasterKeyPath:      filepath.Join(aegisDir, "master.key"),
		PauseAfterIdle:     60 * time.Second,
		StopAfterIdle:      5 * time.Minute,
		NetworkBackend:     "auto",
		KernelPath:         filepath.Join(aegisDir, "kernel", "vmlinux"),
		SnapshotsDir:       filepath.Join(aegisDir, "data", "snapshots"),
	}
}

// EnsureDirs creates all required directories.
func (c *Config) EnsureDirs() error {
	dirs := []string{
		c.DataDir,
		filepath.Join(c.DataDir, "sockets"),
		filepath.Dir(c.SocketPath),
		c.ImageCacheDir,
		c.OverlaysDir,
		c.WorkspacesDir,
		c.LogsDir,
	}
	if runtime.GOOS == "linux" {
		dirs = append(dirs, filepath.Dir(c.KernelPath), c.SnapshotsDir)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	return nil
}

// ResolveNetworkBackend resolves "auto" to a concrete backend.
// On darwin (macOS), gvproxy is always available (compiled into vmm-worker).
// On linux, tap + iptables NAT (Cloud Hypervisor).
func (c *Config) ResolveNetworkBackend() {
	switch c.NetworkBackend {
	case "gvproxy", "tsi", "tap":
		// Explicit choice — keep as-is
	default:
		// "auto" or unset
		switch runtime.GOOS {
		case "darwin":
			c.NetworkBackend = "gvproxy"
		case "linux":
			c.NetworkBackend = "tap"
		default:
			c.NetworkBackend = "tsi"
		}
	}
}

// executableDir returns the directory containing the current executable.
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
