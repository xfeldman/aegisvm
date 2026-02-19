package config

import (
	"os"
	"path/filepath"
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

	// ReleasesDir is the directory for release rootfs copies.
	ReleasesDir string

	// WorkspacesDir is the directory for app workspace volumes.
	WorkspacesDir string

	// LogsDir is the directory for per-instance log files.
	LogsDir string

	// MasterKeyPath is the path to the AES-256 master key for secret encryption.
	MasterKeyPath string

	// PauseAfterIdle is the duration after which an idle instance is paused (SIGSTOP).
	PauseAfterIdle time.Duration

	// TerminateAfterIdle is the duration after which a paused instance is terminated.
	TerminateAfterIdle time.Duration
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	aegisDir := filepath.Join(homeDir, ".aegis")
	execDir := executableDir()

	return &Config{
		DataDir:            filepath.Join(aegisDir, "data"),
		BinDir:             execDir,
		SocketPath:         filepath.Join(aegisDir, "aegisd.sock"),
		BaseRootfsPath:     filepath.Join(aegisDir, "base-rootfs"),
		DefaultMemoryMB:    512,
		DefaultVCPUs:       1,
		RouterAddr:         "127.0.0.1:8099",
		DBPath:             filepath.Join(aegisDir, "data", "aegis.db"),
		ImageCacheDir:      filepath.Join(aegisDir, "data", "images"),
		ReleasesDir:        filepath.Join(aegisDir, "data", "releases"),
		WorkspacesDir:      filepath.Join(aegisDir, "data", "workspaces"),
		LogsDir:            filepath.Join(aegisDir, "data", "logs"),
		MasterKeyPath:      filepath.Join(aegisDir, "master.key"),
		PauseAfterIdle:     60 * time.Second,
		TerminateAfterIdle: 20 * time.Minute,
	}
}

// EnsureDirs creates all required directories.
func (c *Config) EnsureDirs() error {
	dirs := []string{
		c.DataDir,
		filepath.Join(c.DataDir, "sockets"),
		filepath.Dir(c.SocketPath),
		c.ImageCacheDir,
		c.ReleasesDir,
		c.WorkspacesDir,
		c.LogsDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	return nil
}

// executableDir returns the directory containing the current executable.
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
