// Package version holds build-time version info injected via ldflags.
//
// Build with:
//
//	go build -ldflags "-X github.com/xfeldman/aegisvm/internal/version.version=v0.2.0"
package version

// version is set at build time via -ldflags.
var version = "dev"

// Version returns the build version string.
func Version() string {
	return version
}
