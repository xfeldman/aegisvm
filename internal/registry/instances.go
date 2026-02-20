package registry

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Instance represents a persistent instance.
type Instance struct {
	ID          string            `json:"id"`
	State       string            `json:"state"`
	Command     []string          `json:"command"`
	ExposePorts []int             `json:"expose_ports"`
	VMID        string            `json:"vm_id,omitempty"`
	Handle      string            `json:"handle,omitempty"`
	ImageRef    string            `json:"image_ref,omitempty"`
	Workspace   string            `json:"workspace,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	SecretKeys  []string          `json:"secret_keys,omitempty"`
	PublicPorts map[int]int       `json:"public_ports,omitempty"` // guestPort → publicPort
	Enabled     bool              `json:"enabled"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// SaveInstance inserts or replaces an instance.
func (d *DB) SaveInstance(inst *Instance) error {
	cmdJSON, _ := json.Marshal(inst.Command)
	portsJSON, _ := json.Marshal(inst.ExposePorts)
	envJSON, _ := json.Marshal(inst.Env)
	secretKeysJSON, _ := json.Marshal(inst.SecretKeys)
	publicPortsJSON, _ := json.Marshal(inst.PublicPorts)

	enabledInt := 0
	if inst.Enabled {
		enabledInt = 1
	}

	_, err := d.db.Exec(`
		INSERT INTO instances (id, state, command, expose_ports, vm_id, handle, image_ref, workspace, env, secret_keys, public_ports, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			state = excluded.state,
			command = excluded.command,
			expose_ports = excluded.expose_ports,
			vm_id = excluded.vm_id,
			handle = excluded.handle,
			image_ref = excluded.image_ref,
			workspace = excluded.workspace,
			env = excluded.env,
			secret_keys = excluded.secret_keys,
			public_ports = excluded.public_ports,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at
	`, inst.ID, inst.State, string(cmdJSON), string(portsJSON), inst.VMID,
		inst.Handle, inst.ImageRef, inst.Workspace, string(envJSON), string(secretKeysJSON),
		string(publicPortsJSON), enabledInt,
		inst.CreatedAt.Format(time.RFC3339), time.Now().Format(time.RFC3339))
	return err
}

// GetInstance retrieves an instance by ID.
func (d *DB) GetInstance(id string) (*Instance, error) {
	row := d.db.QueryRow(`
		SELECT id, state, command, expose_ports, vm_id, handle, image_ref, workspace, env, secret_keys, public_ports, enabled, created_at, updated_at
		FROM instances WHERE id = ?
	`, id)
	return scanInstance(row)
}

// GetInstanceByHandle retrieves an instance by handle.
func (d *DB) GetInstanceByHandle(handle string) (*Instance, error) {
	row := d.db.QueryRow(`
		SELECT id, state, command, expose_ports, vm_id, handle, image_ref, workspace, env, secret_keys, public_ports, enabled, created_at, updated_at
		FROM instances WHERE handle = ?
	`, handle)
	return scanInstance(row)
}

// ListInstances returns all instances.
func (d *DB) ListInstances() ([]*Instance, error) {
	rows, err := d.db.Query(`
		SELECT id, state, command, expose_ports, vm_id, handle, image_ref, workspace, env, secret_keys, public_ports, enabled, created_at, updated_at
		FROM instances ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*Instance
	for rows.Next() {
		inst, err := scanInstanceRow(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// UpdateState updates an instance's state.
func (d *DB) UpdateState(id, state string) error {
	res, err := d.db.Exec(`
		UPDATE instances SET state = ?, updated_at = datetime('now') WHERE id = ?
	`, state, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("instance %s not found", id)
	}
	return nil
}

// UpdateVMID updates the VM handle ID for an instance.
func (d *DB) UpdateVMID(id, vmID string) error {
	_, err := d.db.Exec(`
		UPDATE instances SET vm_id = ?, updated_at = datetime('now') WHERE id = ?
	`, vmID, id)
	return err
}

// UpdateEnabled updates the enabled flag for an instance.
func (d *DB) UpdateEnabled(id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := d.db.Exec(`
		UPDATE instances SET enabled = ?, updated_at = datetime('now') WHERE id = ?
	`, v, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("instance %s not found", id)
	}
	return nil
}

// DeleteInstance removes an instance.
func (d *DB) DeleteInstance(id string) error {
	_, err := d.db.Exec(`DELETE FROM instances WHERE id = ?`, id)
	return err
}

func scanInstance(row *sql.Row) (*Instance, error) {
	var inst Instance
	var cmdJSON, portsJSON, envJSON, secretKeysJSON, publicPortsJSON, createdStr, updatedStr string
	var enabledInt int

	err := row.Scan(&inst.ID, &inst.State, &cmdJSON, &portsJSON, &inst.VMID,
		&inst.Handle, &inst.ImageRef, &inst.Workspace, &envJSON, &secretKeysJSON,
		&publicPortsJSON, &enabledInt, &createdStr, &updatedStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(cmdJSON), &inst.Command)
	json.Unmarshal([]byte(portsJSON), &inst.ExposePorts)
	json.Unmarshal([]byte(envJSON), &inst.Env)
	json.Unmarshal([]byte(secretKeysJSON), &inst.SecretKeys)
	json.Unmarshal([]byte(publicPortsJSON), &inst.PublicPorts)
	inst.Enabled = enabledInt != 0
	inst.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	inst.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
	return &inst, nil
}

func scanInstanceRow(rows *sql.Rows) (*Instance, error) {
	var inst Instance
	var cmdJSON, portsJSON, envJSON, secretKeysJSON, publicPortsJSON, createdStr, updatedStr string
	var enabledInt int

	err := rows.Scan(&inst.ID, &inst.State, &cmdJSON, &portsJSON, &inst.VMID,
		&inst.Handle, &inst.ImageRef, &inst.Workspace, &envJSON, &secretKeysJSON,
		&publicPortsJSON, &enabledInt, &createdStr, &updatedStr)
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(cmdJSON), &inst.Command)
	json.Unmarshal([]byte(portsJSON), &inst.ExposePorts)
	json.Unmarshal([]byte(envJSON), &inst.Env)
	json.Unmarshal([]byte(secretKeysJSON), &inst.SecretKeys)
	json.Unmarshal([]byte(publicPortsJSON), &inst.PublicPorts)
	inst.Enabled = enabledInt != 0
	inst.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	inst.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)
	return &inst, nil
}
