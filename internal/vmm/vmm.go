// Package vmm defines the virtual machine manager interface.
// This interface is FROZEN — do not modify without explicit approval.
// Both LibkrunVMM (macOS) and FirecrackerVMM (Linux) implement this interface.
package vmm

import (
	"errors"
	"fmt"
)

// ErrNotSupported is returned when a backend does not support a capability.
var ErrNotSupported = errors.New("operation not supported by this backend")

// Handle is an opaque reference to a running VM.
type Handle struct {
	ID string
}

func (h Handle) String() string {
	return h.ID
}

// VMConfig describes how to create a VM.
type VMConfig struct {
	// RootfsPath is the path to the ext4 rootfs image.
	RootfsPath string

	// MemoryMB is the amount of RAM in megabytes.
	MemoryMB int

	// VCPUs is the number of virtual CPUs.
	VCPUs int

	// KernelPath is the path to the vmlinux kernel image.
	// If empty, the backend uses its default kernel.
	KernelPath string

	// KernelArgs are additional kernel boot arguments.
	KernelArgs string

	// WorkspacePath is the path to the workspace volume to mount.
	// Empty means no workspace mount.
	WorkspacePath string
}

// BackendCaps reports what a VMM backend can do.
type BackendCaps struct {
	// Pause indicates whether pause/resume with RAM retained is supported.
	Pause bool

	// SnapshotRestore indicates whether save/restore of full VM memory to disk is supported.
	SnapshotRestore bool

	// Name is the backend identifier ("libkrun" or "firecracker").
	Name string
}

func (c BackendCaps) String() string {
	return fmt.Sprintf("backend=%s pause=%v snapshot=%v", c.Name, c.Pause, c.SnapshotRestore)
}

// VMM is the virtual machine manager interface.
// All aegisd core logic calls this interface — it never knows which backend is active.
type VMM interface {
	// CreateVM creates a new VM with the given configuration.
	// The VM is created but not started.
	CreateVM(config VMConfig) (Handle, error)

	// StartVM starts a previously created VM.
	StartVM(h Handle) error

	// PauseVM pauses a running VM, retaining RAM.
	// Returns ErrNotSupported if the backend does not support pause.
	PauseVM(h Handle) error

	// ResumeVM resumes a paused VM.
	// Returns ErrNotSupported if the backend does not support resume.
	ResumeVM(h Handle) error

	// StopVM stops and destroys a VM, freeing all resources.
	StopVM(h Handle) error

	// Snapshot saves a running VM's full state (RAM + disk) to the given path.
	// Returns ErrNotSupported if the backend does not support snapshots.
	Snapshot(h Handle, path string) error

	// Restore restores a VM from a previously saved snapshot.
	// Returns ErrNotSupported if the backend does not support snapshots.
	Restore(snapshotPath string) (Handle, error)

	// Capabilities returns what this backend supports.
	Capabilities() BackendCaps
}
