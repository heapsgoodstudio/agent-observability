// Package db owns state/claude-obs.db. WAL is load-bearing: the indexer,
// poller, alerter, and server are separate launchd processes, so readers
// must not block the writer.
package db

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_txlock=immediate"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	return d, nil
}

func Migrate(d *sql.DB) (int, error) {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return 0, fmt.Errorf("db: create schema_migrations: %w", err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return 0, fmt.Errorf("db: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	applied := 0
	for _, name := range names {
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return applied, fmt.Errorf("db: read migration %s: %w", name, err)
		}
		// _txlock=immediate makes Begin take the write lock up front, so two
		// processes migrating at once serialize here instead of racing the
		// version check.
		tx, err := d.Begin()
		if err != nil {
			return applied, fmt.Errorf("db: begin %s: %w", name, err)
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
			tx.Rollback()
			return applied, fmt.Errorf("db: check %s: %w", name, err)
		}
		if n > 0 {
			tx.Rollback()
			continue
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return applied, fmt.Errorf("db: migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, name); err != nil {
			tx.Rollback()
			return applied, fmt.Errorf("db: record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return applied, fmt.Errorf("db: commit %s: %w", name, err)
		}
		applied++
	}
	return applied, nil
}
