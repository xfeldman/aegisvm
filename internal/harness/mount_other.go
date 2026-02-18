//go:build !linux

package harness

// mountWorkspace is a no-op on non-Linux platforms.
// The harness only runs inside Linux VMs.
func mountWorkspace() {}

// mountEssential is a no-op on non-Linux platforms.
func mountEssential() {}
