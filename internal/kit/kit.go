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
	Daemons     []string `json:"daemons"`
	Image       struct {
		Base   string   `json:"base"`
		Inject []string `json:"inject"`
	} `json:"image"`
	Defaults struct {
		Command      []string         `json:"command"`
		Capabilities *json.RawMessage `json:"capabilities"`
	} `json:"defaults"`
}

// KitsDir returns the directory where kit manifests are stored.
func KitsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aegis", "kits")
}

// LoadManifest reads a kit manifest by name from ~/.aegis/kits/{name}.json.
func LoadManifest(name string) (*Manifest, error) {
	path := filepath.Join(KitsDir(), name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kit manifest %q: %w", name, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse kit manifest %q: %w", name, err)
	}
	return &m, nil
}

// ListManifests scans ~/.aegis/kits/*.json and returns all valid manifests.
func ListManifests() ([]*Manifest, error) {
	dir := KitsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read kits dir: %w", err)
	}

	var manifests []*Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		m, err := LoadManifest(name)
		if err != nil {
			continue // skip broken manifests
		}
		manifests = append(manifests, m)
	}
	return manifests, nil
}

// ValidateManifest checks that all required kit binaries exist in binDir.
// Returns a list of missing binary names (empty if all present).
func ValidateManifest(m *Manifest, binDir string) []string {
	var missing []string
	// Check daemon binaries
	for _, d := range m.Daemons {
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
