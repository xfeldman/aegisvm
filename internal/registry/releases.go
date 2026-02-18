package registry

import (
	"database/sql"
	"time"
)

// Release represents an immutable rootfs artifact built from an OCI image.
type Release struct {
	ID          string    `json:"id"`
	AppID       string    `json:"app_id"`
	ImageDigest string    `json:"image_digest"`
	RootfsPath  string    `json:"rootfs_path"`
	Label       string    `json:"label,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// SaveRelease inserts a release.
func (d *DB) SaveRelease(rel *Release) error {
	_, err := d.db.Exec(`
		INSERT INTO releases (id, app_id, image_digest, rootfs_path, label, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, rel.ID, rel.AppID, rel.ImageDigest, rel.RootfsPath, rel.Label,
		rel.CreatedAt.Format(time.RFC3339))
	return err
}

// GetRelease retrieves a release by ID.
func (d *DB) GetRelease(id string) (*Release, error) {
	row := d.db.QueryRow(`
		SELECT id, app_id, image_digest, rootfs_path, label, created_at
		FROM releases WHERE id = ?
	`, id)
	return scanRelease(row)
}

// GetLatestRelease returns the most recent release for an app.
func (d *DB) GetLatestRelease(appID string) (*Release, error) {
	row := d.db.QueryRow(`
		SELECT id, app_id, image_digest, rootfs_path, label, created_at
		FROM releases WHERE app_id = ? ORDER BY created_at DESC LIMIT 1
	`, appID)
	return scanRelease(row)
}

// ListReleases returns all releases for an app.
func (d *DB) ListReleases(appID string) ([]*Release, error) {
	rows, err := d.db.Query(`
		SELECT id, app_id, image_digest, rootfs_path, label, created_at
		FROM releases WHERE app_id = ? ORDER BY created_at DESC
	`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var releases []*Release
	for rows.Next() {
		rel, err := scanReleaseRow(rows)
		if err != nil {
			return nil, err
		}
		releases = append(releases, rel)
	}
	return releases, rows.Err()
}

// DeleteRelease removes a release.
func (d *DB) DeleteRelease(id string) error {
	_, err := d.db.Exec(`DELETE FROM releases WHERE id = ?`, id)
	return err
}

func scanRelease(row *sql.Row) (*Release, error) {
	var rel Release
	var label sql.NullString
	var createdStr string

	err := row.Scan(&rel.ID, &rel.AppID, &rel.ImageDigest, &rel.RootfsPath, &label, &createdStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if label.Valid {
		rel.Label = label.String
	}
	rel.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &rel, nil
}

func scanReleaseRow(rows *sql.Rows) (*Release, error) {
	var rel Release
	var label sql.NullString
	var createdStr string

	err := rows.Scan(&rel.ID, &rel.AppID, &rel.ImageDigest, &rel.RootfsPath, &label, &createdStr)
	if err != nil {
		return nil, err
	}

	if label.Valid {
		rel.Label = label.String
	}
	rel.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &rel, nil
}
