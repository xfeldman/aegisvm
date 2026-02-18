package registry

import (
	"database/sql"
	"encoding/json"
	"time"
)

// Kit represents an installed kit.
type Kit struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	Config      KitConfig `json:"config"`
	ImageRef    string    `json:"image_ref"`
	InstalledAt time.Time `json:"installed_at"`
}

// KitConfig holds the kit's configuration.
type KitConfig struct {
	Secrets    KitSecrets    `json:"secrets,omitempty"`
	Routing    KitRouting    `json:"routing,omitempty"`
	Networking KitNetworking `json:"networking,omitempty"`
	Policies   KitPolicies   `json:"policies,omitempty"`
	Resources  KitResources  `json:"resources,omitempty"`
}

// KitSecrets defines required and optional secrets for a kit.
type KitSecrets struct {
	Required []KitSecretDef `json:"required,omitempty"`
	Optional []KitSecretDef `json:"optional,omitempty"`
}

// KitSecretDef defines a single secret requirement.
type KitSecretDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// KitRouting defines routing configuration for a kit.
type KitRouting struct {
	DefaultPort int               `json:"default_port,omitempty"`
	Healthcheck string            `json:"healthcheck,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// KitNetworking defines networking requirements.
type KitNetworking struct {
	Egress []string `json:"egress,omitempty"`
}

// KitPolicies defines kit policies.
type KitPolicies struct {
	MaxMemoryMB int `json:"max_memory_mb,omitempty"`
	MaxVCPUs    int `json:"max_vcpus,omitempty"`
}

// KitResources defines default resource allocations.
type KitResources struct {
	MemoryMB int `json:"memory_mb,omitempty"`
	VCPUs    int `json:"vcpus,omitempty"`
}

// SaveKit inserts or replaces a kit.
func (d *DB) SaveKit(k *Kit) error {
	cfgJSON, _ := json.Marshal(k.Config)
	_, err := d.db.Exec(`
		INSERT INTO kits (name, version, description, config, image_ref, installed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			version = excluded.version,
			description = excluded.description,
			config = excluded.config,
			image_ref = excluded.image_ref,
			installed_at = excluded.installed_at
	`, k.Name, k.Version, k.Description, string(cfgJSON), k.ImageRef,
		k.InstalledAt.Format(time.RFC3339))
	return err
}

// GetKit retrieves a kit by name.
func (d *DB) GetKit(name string) (*Kit, error) {
	row := d.db.QueryRow(`
		SELECT name, version, description, config, image_ref, installed_at
		FROM kits WHERE name = ?
	`, name)
	return scanKit(row)
}

// ListKits returns all installed kits.
func (d *DB) ListKits() ([]*Kit, error) {
	rows, err := d.db.Query(`
		SELECT name, version, description, config, image_ref, installed_at
		FROM kits ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var kits []*Kit
	for rows.Next() {
		k, err := scanKitRow(rows)
		if err != nil {
			return nil, err
		}
		kits = append(kits, k)
	}
	return kits, rows.Err()
}

// DeleteKit removes a kit by name.
func (d *DB) DeleteKit(name string) error {
	res, err := d.db.Exec(`DELETE FROM kits WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanKit(row *sql.Row) (*Kit, error) {
	var k Kit
	var cfgJSON, installedStr string
	err := row.Scan(&k.Name, &k.Version, &k.Description, &cfgJSON, &k.ImageRef, &installedStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(cfgJSON), &k.Config)
	k.InstalledAt, _ = time.Parse(time.RFC3339, installedStr)
	return &k, nil
}

func scanKitRow(rows *sql.Rows) (*Kit, error) {
	var k Kit
	var cfgJSON, installedStr string
	err := rows.Scan(&k.Name, &k.Version, &k.Description, &cfgJSON, &k.ImageRef, &installedStr)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(cfgJSON), &k.Config)
	k.InstalledAt, _ = time.Parse(time.RFC3339, installedStr)
	return &k, nil
}
