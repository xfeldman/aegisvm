// Package vmm defines the virtual machine manager interface.
// Both LibkrunVMM (macOS) and FirecrackerVMM (Linux) implement this interface.
package vmm

import (
	"context"
	"fmt"
)

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

// PortExpose describes a guest port to expose on the host.
type PortExpose struct {
	GuestPort int
	Protocol  string // "http", "tcp", "grpc"
}

// VMConfig describes how to create a VM.
type VMConfig struct {
	// Rootfs is the root filesystem for the VM.
	Rootfs RootFS

	// MemoryMB is the amount of RAM in megabytes.
	MemoryMB int

	// VCPUs is the number of virtual CPUs.
	VCPUs int

	// WorkspacePath is the path to the workspace volume to mount.
	// Empty means no workspace mount.
	WorkspacePath string

	// ExposePorts lists guest ports to expose on the host via port mapping.
	// When set, the backend allocates random host ports and maps them to guest ports.
	ExposePorts []PortExpose
}

// HostEndpoint describes a mapped port on the host side.
type HostEndpoint struct {
	GuestPort int
	HostPort  int
	Protocol  string

	// BackendAddr is the address to dial for this endpoint.
	// When set (e.g. "172.16.0.2" for tap networking), the router dials
	// BackendAddr:HostPort instead of 127.0.0.1:HostPort.
	// Empty means use 127.0.0.1 (libkrun/gvproxy backward compat).
	BackendAddr string
}

// BackendCaps reports what a VMM backend can do.
type BackendCaps struct {
	// Pause indicates whether pause/resume with RAM retained is supported.
	Pause bool

	// PersistentPause indicates that paused VMs retain state indefinitely
	// without needing to be stopped. When true, the lifecycle manager skips
	// the PAUSED → STOPPED transition entirely — the OS manages memory
	// pressure via swap. When false (e.g. Firecracker/KVM), paused VMs
	// should be stopped after a timeout to free hypervisor resources.
	PersistentPause bool

	// RootFSType is the rootfs format this backend expects.
	RootFSType RootFSType

	// Name is the backend identifier ("libkrun" or "firecracker").
	Name string

	// NetworkBackend is the active networking mode ("gvproxy" or "tsi").
	NetworkBackend string
}

func (c BackendCaps) String() string {
	return fmt.Sprintf("backend=%s pause=%v rootfs=%s network=%s",
		c.Name, c.Pause, c.RootFSType, c.NetworkBackend)
}

// ControlChannel is a message-oriented, bidirectional channel between aegisd
// and the guest harness. The underlying transport is backend-specific:
//   - libkrun: TCP over TSI (harness connects outbound to host listener)
//   - Firecracker: vsock (host connects to guest via AF_VSOCK)
//
// Framing contract:
//   - Each message is exactly one complete JSON-RPC 2.0 object.
//   - Send writes one message. Recv returns one message.
//   - The wire encoding is newline-delimited JSON (one JSON object per line).
//   - Implementations handle framing internally — callers never see delimiters.
//
// Core code uses ControlChannel for all harness communication. It never
// sees TCP, vsock, or unix sockets — only Send/Recv/Close.
type ControlChannel interface {
	// Send writes exactly one JSON-RPC message to the harness.
	// msg must be a complete JSON object (no trailing newline required —
	// the implementation adds framing).
	// Respects the context deadline for write timeout.
	Send(ctx context.Context, msg []byte) error

	// Recv reads and returns exactly one complete JSON-RPC message from the
	// harness. Blocks until a full message is available or the context is done.
	// The returned bytes are a complete JSON object with no trailing newline.
	Recv(ctx context.Context) ([]byte, error)

	// Close shuts down the channel and releases resources.
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
	PauseVM(h Handle) error

	// ResumeVM resumes a paused VM.
	ResumeVM(h Handle) error

	// StopVM stops and destroys a VM, freeing all resources.
	StopVM(h Handle) error

	// HostEndpoints returns the resolved host endpoints for a VM's exposed ports.
	// Only valid after StartVM returns successfully.
	HostEndpoints(h Handle) ([]HostEndpoint, error)

	// Capabilities returns what this backend supports.
	Capabilities() BackendCaps
}
