package registry

import (
	"database/sql"
	"fmt"
	"time"
)

// Secret represents an encrypted secret value.
type Secret struct {
	ID             string    `json:"id"`
	AppID          string    `json:"app_id"`
	Name           string    `json:"name"`
	EncryptedValue []byte    `json:"-"`
	Scope          string    `json:"scope"` // "per_app" | "per_workspace"
	CreatedAt      time.Time `json:"created_at"`
}

// SaveSecret inserts or replaces a secret.
func (d *DB) SaveSecret(s *Secret) error {
	_, err := d.db.Exec(`
		INSERT INTO secrets (id, app_id, name, value, scope, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id, name) DO UPDATE SET
			id = excluded.id,
			value = excluded.value,
			scope = excluded.scope,
			created_at = excluded.created_at
	`, s.ID, s.AppID, s.Name, s.EncryptedValue, s.Scope, s.CreatedAt.Format(time.RFC3339))
	return err
}

// GetSecretByName retrieves a secret by app ID and name.
func (d *DB) GetSecretByName(appID, name string) (*Secret, error) {
	row := d.db.QueryRow(`
		SELECT id, app_id, name, value, scope, created_at
		FROM secrets WHERE app_id = ? AND name = ?
	`, appID, name)
	return scanSecret(row)
}

// ListSecrets returns all secrets for an app.
func (d *DB) ListSecrets(appID string) ([]*Secret, error) {
	rows, err := d.db.Query(`
		SELECT id, app_id, name, value, scope, created_at
		FROM secrets WHERE app_id = ? ORDER BY name
	`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []*Secret
	for rows.Next() {
		s, err := scanSecretRow(rows)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, s)
	}
	return secrets, rows.Err()
}

// ListWorkspaceSecrets returns all workspace-scoped secrets.
func (d *DB) ListWorkspaceSecrets() ([]*Secret, error) {
	rows, err := d.db.Query(`
		SELECT id, app_id, name, value, scope, created_at
		FROM secrets WHERE scope = 'per_workspace' ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []*Secret
	for rows.Next() {
		s, err := scanSecretRow(rows)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, s)
	}
	return secrets, rows.Err()
}

// DeleteSecret removes a secret by ID.
func (d *DB) DeleteSecret(id string) error {
	_, err := d.db.Exec(`DELETE FROM secrets WHERE id = ?`, id)
	return err
}

// DeleteSecretByName removes a secret by app ID and name.
func (d *DB) DeleteSecretByName(appID, name string) error {
	res, err := d.db.Exec(`DELETE FROM secrets WHERE app_id = ? AND name = ?`, appID, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("secret %q not found", name)
	}
	return nil
}

// DeleteAppSecrets removes all secrets for an app.
func (d *DB) DeleteAppSecrets(appID string) error {
	_, err := d.db.Exec(`DELETE FROM secrets WHERE app_id = ?`, appID)
	return err
}

func scanSecret(row *sql.Row) (*Secret, error) {
	var s Secret
	var createdStr string
	err := row.Scan(&s.ID, &s.AppID, &s.Name, &s.EncryptedValue, &s.Scope, &createdStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &s, nil
}

func scanSecretRow(rows *sql.Rows) (*Secret, error) {
	var s Secret
	var createdStr string
	err := rows.Scan(&s.ID, &s.AppID, &s.Name, &s.EncryptedValue, &s.Scope, &createdStr)
	if err != nil {
		return nil, err
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &s, nil
}
