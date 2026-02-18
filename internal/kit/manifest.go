// Package kit provides kit manifest parsing and hooks.
package kit

import (
	"fmt"
	"os"
	"time"

	"github.com/xfeldman/aegis/internal/registry"
	"gopkg.in/yaml.v3"
)

// Manifest is the YAML on-disk representation of a kit.
type Manifest struct {
	Name        string         `yaml:"name" json:"name"`
	Version     string         `yaml:"version" json:"version"`
	Description string         `yaml:"description,omitempty" json:"description,omitempty"`
	Image       string         `yaml:"image" json:"image"`
	Config      ManifestConfig `yaml:"config,omitempty" json:"config,omitempty"`
}

// ManifestConfig mirrors the kit config in YAML format.
type ManifestConfig struct {
	Secrets    ManifestSecrets    `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Routing    ManifestRouting    `yaml:"routing,omitempty" json:"routing,omitempty"`
	Networking ManifestNetworking `yaml:"networking,omitempty" json:"networking,omitempty"`
	Policies   ManifestPolicies   `yaml:"policies,omitempty" json:"policies,omitempty"`
	Resources  ManifestResources  `yaml:"resources,omitempty" json:"resources,omitempty"`
}

// ManifestSecrets defines secrets in the manifest.
type ManifestSecrets struct {
	Required []ManifestSecretDef `yaml:"required,omitempty" json:"required,omitempty"`
	Optional []ManifestSecretDef `yaml:"optional,omitempty" json:"optional,omitempty"`
}

// ManifestSecretDef defines a single secret in the manifest.
type ManifestSecretDef struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// ManifestRouting defines routing in the manifest.
type ManifestRouting struct {
	DefaultPort int               `yaml:"default_port,omitempty" json:"default_port,omitempty"`
	Healthcheck string            `yaml:"healthcheck,omitempty" json:"healthcheck,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// ManifestNetworking defines networking in the manifest.
type ManifestNetworking struct {
	Egress []string `yaml:"egress,omitempty" json:"egress,omitempty"`
}

// ManifestPolicies defines policies in the manifest.
type ManifestPolicies struct {
	MaxMemoryMB int `yaml:"max_memory_mb,omitempty" json:"max_memory_mb,omitempty"`
	MaxVCPUs    int `yaml:"max_vcpus,omitempty" json:"max_vcpus,omitempty"`
}

// ManifestResources defines default resource allocations.
type ManifestResources struct {
	MemoryMB int `yaml:"memory_mb,omitempty" json:"memory_mb,omitempty"`
	VCPUs    int `yaml:"vcpus,omitempty" json:"vcpus,omitempty"`
}

// ParseFile reads and parses a kit manifest from a YAML file.
func ParseFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes parses a kit manifest from YAML bytes.
func ParseBytes(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	if m.Name == "" {
		return nil, fmt.Errorf("manifest missing required field: name")
	}
	if m.Version == "" {
		return nil, fmt.Errorf("manifest missing required field: version")
	}
	if m.Image == "" {
		return nil, fmt.Errorf("manifest missing required field: image")
	}

	return &m, nil
}

// ToKit converts a manifest to a registry Kit.
func (m *Manifest) ToKit() *registry.Kit {
	k := &registry.Kit{
		Name:        m.Name,
		Version:     m.Version,
		Description: m.Description,
		ImageRef:    m.Image,
		InstalledAt: time.Now(),
	}

	// Convert secrets
	for _, s := range m.Config.Secrets.Required {
		k.Config.Secrets.Required = append(k.Config.Secrets.Required, registry.KitSecretDef{
			Name:        s.Name,
			Description: s.Description,
		})
	}
	for _, s := range m.Config.Secrets.Optional {
		k.Config.Secrets.Optional = append(k.Config.Secrets.Optional, registry.KitSecretDef{
			Name:        s.Name,
			Description: s.Description,
		})
	}

	// Convert routing
	k.Config.Routing = registry.KitRouting{
		DefaultPort: m.Config.Routing.DefaultPort,
		Healthcheck: m.Config.Routing.Healthcheck,
		Headers:     m.Config.Routing.Headers,
	}

	// Convert networking
	k.Config.Networking = registry.KitNetworking{
		Egress: m.Config.Networking.Egress,
	}

	// Convert policies
	k.Config.Policies = registry.KitPolicies{
		MaxMemoryMB: m.Config.Policies.MaxMemoryMB,
		MaxVCPUs:    m.Config.Policies.MaxVCPUs,
	}

	// Convert resources
	k.Config.Resources = registry.KitResources{
		MemoryMB: m.Config.Resources.MemoryMB,
		VCPUs:    m.Config.Resources.VCPUs,
	}

	return k
}
