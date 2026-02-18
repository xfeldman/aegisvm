package config

import (
	"os"
	"path/filepath"
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
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	aegisDir := filepath.Join(homeDir, ".aegis")
	execDir := executableDir()

	return &Config{
		DataDir:         filepath.Join(aegisDir, "data"),
		BinDir:          execDir,
		SocketPath:      filepath.Join(aegisDir, "aegisd.sock"),
		BaseRootfsPath:  filepath.Join(aegisDir, "base-rootfs"),
		DefaultMemoryMB: 512,
		DefaultVCPUs:    1,
	}
}

// EnsureDirs creates all required directories.
func (c *Config) EnsureDirs() error {
	dirs := []string{
		c.DataDir,
		filepath.Join(c.DataDir, "sockets"),
		filepath.Dir(c.SocketPath),
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
