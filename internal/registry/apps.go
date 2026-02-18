package registry

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// App represents a registered application.
type App struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	ExposePorts []int             `json:"expose_ports"`
	Config      map[string]string `json:"config,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// SaveApp inserts or replaces an app.
func (d *DB) SaveApp(app *App) error {
	cmdJSON, _ := json.Marshal(app.Command)
	portsJSON, _ := json.Marshal(app.ExposePorts)
	cfgJSON, _ := json.Marshal(app.Config)

	_, err := d.db.Exec(`
		INSERT INTO apps (id, name, image, command, expose_ports, config, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			image = excluded.image,
			command = excluded.command,
			expose_ports = excluded.expose_ports,
			config = excluded.config
	`, app.ID, app.Name, app.Image, string(cmdJSON), string(portsJSON),
		string(cfgJSON), app.CreatedAt.Format(time.RFC3339))
	return err
}

// GetApp retrieves an app by ID.
func (d *DB) GetApp(id string) (*App, error) {
	row := d.db.QueryRow(`
		SELECT id, name, image, command, expose_ports, config, created_at
		FROM apps WHERE id = ?
	`, id)
	return scanApp(row)
}

// GetAppByName retrieves an app by name.
func (d *DB) GetAppByName(name string) (*App, error) {
	row := d.db.QueryRow(`
		SELECT id, name, image, command, expose_ports, config, created_at
		FROM apps WHERE name = ?
	`, name)
	return scanApp(row)
}

// ListApps returns all apps.
func (d *DB) ListApps() ([]*App, error) {
	rows, err := d.db.Query(`
		SELECT id, name, image, command, expose_ports, config, created_at
		FROM apps ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*App
	for rows.Next() {
		app, err := scanAppRow(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

// DeleteApp removes an app and its releases.
func (d *DB) DeleteApp(id string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM releases WHERE app_id = ?`, id); err != nil {
		return fmt.Errorf("delete releases: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM apps WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete app: %w", err)
	}
	return tx.Commit()
}

func scanApp(row *sql.Row) (*App, error) {
	var app App
	var cmdJSON, portsJSON, cfgJSON, createdStr string

	err := row.Scan(&app.ID, &app.Name, &app.Image, &cmdJSON, &portsJSON, &cfgJSON, &createdStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(cmdJSON), &app.Command)
	json.Unmarshal([]byte(portsJSON), &app.ExposePorts)
	json.Unmarshal([]byte(cfgJSON), &app.Config)
	app.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &app, nil
}

func scanAppRow(rows *sql.Rows) (*App, error) {
	var app App
	var cmdJSON, portsJSON, cfgJSON, createdStr string

	err := rows.Scan(&app.ID, &app.Name, &app.Image, &cmdJSON, &portsJSON, &cfgJSON, &createdStr)
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(cmdJSON), &app.Command)
	json.Unmarshal([]byte(portsJSON), &app.ExposePorts)
	json.Unmarshal([]byte(cfgJSON), &app.Config)
	app.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &app, nil
}
