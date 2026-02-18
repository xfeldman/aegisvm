// Package registry provides persistent storage for aegis instance state.
// Uses pure-Go SQLite (modernc.org/sqlite) — no cgo required.
package registry

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps an SQLite database for aegis registry storage.
type DB struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at the given path.
func Open(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Enable WAL mode for better concurrent read performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	rdb := &DB{db: db}
	if err := rdb.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return rdb, nil
}

// Close closes the database.
func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS instances (
			id          TEXT PRIMARY KEY,
			state       TEXT NOT NULL DEFAULT 'stopped',
			command     TEXT NOT NULL,
			expose_ports TEXT NOT NULL DEFAULT '[]',
			vm_id       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS apps (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL UNIQUE,
			image       TEXT NOT NULL,
			command     TEXT NOT NULL DEFAULT '[]',
			expose_ports TEXT NOT NULL DEFAULT '[]',
			config      TEXT NOT NULL DEFAULT '{}',
			created_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS releases (
			id           TEXT PRIMARY KEY,
			app_id       TEXT NOT NULL REFERENCES apps(id),
			image_digest TEXT NOT NULL,
			rootfs_path  TEXT NOT NULL,
			label        TEXT,
			created_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	}
	for _, stmt := range stmts {
		if _, err := d.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
