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
			handle      TEXT NOT NULL DEFAULT '',
			image_ref   TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS secrets (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL UNIQUE,
			value      BLOB NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	}
	for _, stmt := range stmts {
		if _, err := d.db.Exec(stmt); err != nil {
			return err
		}
	}

	// Migrate from older schema versions (ignore errors — columns may already exist)
	migrations := []string{
		`ALTER TABLE instances ADD COLUMN handle TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE instances ADD COLUMN image_ref TEXT NOT NULL DEFAULT ''`,
	}
	for _, stmt := range migrations {
		d.db.Exec(stmt)
	}

	// Migrate old secrets schema (had app_id, scope columns) to new (name-only)
	var hasAppID int
	row := d.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('secrets') WHERE name='app_id'`)
	if row.Scan(&hasAppID) == nil && hasAppID > 0 {
		d.db.Exec(`DROP TABLE secrets`)
		d.db.Exec(`CREATE TABLE IF NOT EXISTS secrets (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL UNIQUE,
			value      BLOB NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`)
	}

	return nil
}
