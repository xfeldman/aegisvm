package config

import (
	"fmt"
	"runtime"
)

// Platform describes the detected host platform.
type Platform struct {
	OS   string // "darwin" or "linux"
	Arch string // "arm64" or "amd64"

	// VMM backend to use
	Backend string // "libkrun" or "firecracker"
}

// DetectPlatform detects the host platform and selects the VMM backend.
func DetectPlatform() (*Platform, error) {
	p := &Platform{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	switch {
	case p.OS == "darwin" && p.Arch == "arm64":
		p.Backend = "libkrun"
	case p.OS == "linux":
		p.Backend = "firecracker"
	default:
		return nil, fmt.Errorf(
			"unsupported platform: %s/%s. Aegis requires macOS ARM64 (libkrun) or Linux (Firecracker)",
			p.OS, p.Arch,
		)
	}

	return p, nil
}
