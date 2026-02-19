package registry

import (
	"database/sql"
	"fmt"
	"time"
)

// Secret represents an encrypted secret value.
type Secret struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	EncryptedValue []byte    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

// SaveSecret inserts or replaces a secret by name.
func (d *DB) SaveSecret(s *Secret) error {
	_, err := d.db.Exec(`
		INSERT INTO secrets (id, name, value, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			id = excluded.id,
			value = excluded.value,
			created_at = excluded.created_at
	`, s.ID, s.Name, s.EncryptedValue, s.CreatedAt.Format(time.RFC3339))
	return err
}

// ListSecrets returns all secrets (names + metadata, not values).
func (d *DB) ListSecrets() ([]*Secret, error) {
	rows, err := d.db.Query(`
		SELECT id, name, value, created_at
		FROM secrets ORDER BY name
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

// DeleteSecretByName removes a secret by name.
func (d *DB) DeleteSecretByName(name string) error {
	res, err := d.db.Exec(`DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("secret %q not found", name)
	}
	return nil
}

func scanSecretRow(rows *sql.Rows) (*Secret, error) {
	var s Secret
	var createdStr string
	err := rows.Scan(&s.ID, &s.Name, &s.EncryptedValue, &createdStr)
	if err != nil {
		return nil, err
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &s, nil
}
