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

// RootFSType describes the format of a root filesystem.
type RootFSType int

const (
	// RootFSDirectory is a host directory (used by libkrun's krun_set_root).
	RootFSDirectory RootFSType = iota
	// RootFSBlockImage is a raw block device image, e.g. ext4 (used by Firecracker).
	RootFSBlockImage
)

func (t RootFSType) String() string {
	switch t {
	case RootFSDirectory:
		return "directory"
	case RootFSBlockImage:
		return "block-image"
	default:
		return "unknown"
	}
}

// RootFS describes a root filesystem for a VM.
// libkrun expects a directory. Firecracker expects a raw block image.
// Core never assumes the format — the backend declares what it needs
// via Capabilities().RootFSType, and the image pipeline produces the right artifact.
type RootFS struct {
	Type RootFSType
	Path string
}

// VMConfig describes how to create a VM.
type VMConfig struct {
	// Rootfs is the root filesystem for the VM.
	Rootfs RootFS

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

	// RootFSType is the rootfs format this backend expects.
	RootFSType RootFSType

	// Name is the backend identifier ("libkrun" or "firecracker").
	Name string
}

func (c BackendCaps) String() string {
	return fmt.Sprintf("backend=%s pause=%v snapshot=%v rootfs=%s",
		c.Name, c.Pause, c.SnapshotRestore, c.RootFSType)
}

// ControlChannel is a bidirectional, newline-delimited byte stream between
// aegisd and the guest harness. The underlying transport is backend-specific:
//   - libkrun: TCP over TSI (harness connects outbound to host listener)
//   - Firecracker: vsock (host connects to guest via AF_VSOCK)
//
// Core code uses ControlChannel for all harness communication. It never
// sees TCP, vsock, or unix sockets — only Send/Recv/Close.
type ControlChannel interface {
	// Send writes a message (typically a JSON-RPC line) to the harness.
	Send(msg []byte) error

	// Recv reads the next message from the harness. Blocks until data is available.
	Recv() ([]byte, error)

	// Close shuts down the channel.
	Close() error
}

// VMM is the virtual machine manager interface.
// All aegisd core logic calls this interface — it never knows which backend is active.
type VMM interface {
	// CreateVM creates a new VM with the given configuration.
	// The VM is created but not started.
	CreateVM(config VMConfig) (Handle, error)

	// StartVM starts a previously created VM and returns a ControlChannel
	// for communicating with the guest harness.
	// The channel is ready to use when StartVM returns — the backend handles
	// all transport setup (TCP listener + accept for libkrun, vsock dial for Firecracker).
	StartVM(h Handle) (ControlChannel, error)

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
