package registry

import (
	"database/sql"
	"time"
)

// Secret represents an encrypted secret value.
type Secret struct {
	ID             string    `json:"id"`
	AppID          string    `json:"app_id"`
	Name           string    `json:"name"`
	EncryptedValue []byte    `json:"-"`
	Scope          string    `json:"scope"` // "per_workspace"
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
