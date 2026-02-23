// Package kit provides kit manifest loading, listing, and validation.
// Kits are optional add-on bundles that extend AegisVM with specific capabilities.
// Kit manifests live at ~/.aegis/kits/<name>.json.
package kit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manifest describes a kit's configuration, daemons, image recipe, and defaults.
type Manifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`

	// InstanceDaemons lists binaries to spawn per enabled instance using this kit.
	// aegisd manages their lifecycle: start on instance create/enable,
	// stop on instance disable/delete, restart on crash with backoff.
	InstanceDaemons []string `json:"instance_daemons,omitempty"`

	Image struct {
		Base   string   `json:"base"`
		Inject []string `json:"inject"`
	} `json:"image"`
	Defaults struct {
		Command      []string         `json:"command"`
		Capabilities *json.RawMessage `json:"capabilities"`
	} `json:"defaults"`
}

// KitsDir returns the primary user kit manifest directory.
func KitsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "kits")
}

// kitSearchDirs returns directories to scan for kit manifests, in priority order:
// 1. ~/.aegis/kits/ — user config (make install-kit, manual)
// 2. {prefix}/share/aegisvm/kits/ — Homebrew install (prefix-relative, works on any HOMEBREW_PREFIX)
//
// User dir takes priority: if the same kit name exists in both, user's wins.
// The Homebrew path is needed because macOS sandbox prevents Homebrew's
// post_install from writing to ~/.aegis/.
func kitSearchDirs() []string {
	dirs := []string{KitsDir()}

	// Homebrew prefix: binary is at {prefix}/bin/aegis (symlink).
	// Resolve os.Args[0] to absolute WITHOUT following symlinks,
	// so {prefix}/bin/aegis → {prefix}/share/aegisvm/kits/.
	// os.Executable() resolves symlinks into the Cellar which is wrong.
	if len(os.Args) > 0 {
		binPath := os.Args[0]
		if !filepath.IsAbs(binPath) {
			if abs, err := filepath.Abs(binPath); err == nil {
				binPath = abs
			}
		}
		shareDir := filepath.Join(filepath.Dir(binPath), "..", "share", "aegisvm", "kits")
		if clean := filepath.Clean(shareDir); clean != dirs[0] {
			dirs = append(dirs, clean)
		}
	}

	return dirs
}

// LoadManifest reads a kit manifest by name, searching user dir then Homebrew.
func LoadManifest(name string) (*Manifest, error) {
	for _, dir := range kitSearchDirs() {
		path := filepath.Join(dir, name+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		return &m, nil
	}
	return nil, fmt.Errorf("kit manifest %q not found", name)
}

// ListManifests scans all kit directories and returns valid manifests.
// User dir takes priority over Homebrew for same-named kits.
func ListManifests() ([]*Manifest, error) {
	seen := make(map[string]bool)
	var manifests []*Manifest

	for _, dir := range kitSearchDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".json")
			if seen[name] {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var m Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			seen[name] = true
			manifests = append(manifests, &m)
		}
	}
	return manifests, nil
}

// ValidateManifest checks that all required kit binaries exist in binDir.
// Returns a list of missing binary names (empty if all present).
func ValidateManifest(m *Manifest, binDir string) []string {
	var missing []string
	// Check instance daemon binaries
	for _, d := range m.InstanceDaemons {
		if _, err := os.Stat(filepath.Join(binDir, d)); err != nil {
			missing = append(missing, d)
		}
	}
	// Check inject binaries
	for _, b := range m.Image.Inject {
		if _, err := os.Stat(filepath.Join(binDir, b)); err != nil {
			missing = append(missing, b)
		}
	}
	return missing
}
