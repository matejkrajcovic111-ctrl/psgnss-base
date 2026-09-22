// Package store owns the SQLite database: schema, migrations, accessors.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary stays static
)

//go:embed schema.sql
var schemaSQL string

// SchemaVersion is bumped whenever a migration adds or changes stored data.
const SchemaVersion = 6

type Store struct{ db *sql.DB }

// Open creates the database if absent and applies the schema idempotently.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One writer; SQLite serialises writes anyway and this avoids lock churn.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	// Connection-level setting: PRAGMA foreign_keys is a no-op inside a
	// transaction. Set it before beginning the atomic schema migration.
	if _, err := s.db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var cur int
	err = tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&cur)
	if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if cur > SchemaVersion {
		return fmt.Errorf("database schema is version %d but this binary understands %d; "+
			"refusing to downgrade", cur, SchemaVersion)
	}
	if _, err := tx.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if cur < 2 {
		if _, err := tx.Exec(`
			ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
			ALTER TABLE users ADD COLUMN expires_at INTEGER;
			CREATE TABLE user_ip_rules (
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				cidr TEXT NOT NULL,
				PRIMARY KEY (user_id, cidr)
			);`); err != nil {
			return fmt.Errorf("migrate users to v2: %w", err)
		}
	}
	if cur < 3 {
		if _, err := tx.Exec(`ALTER TABLE telemetry_epoch ADD COLUMN sig_blob BLOB NOT NULL DEFAULT X'';`); err != nil {
			return fmt.Errorf("migrate telemetry to v3: %w", err)
		}
	}
	if cur < 4 {
		if _, err := tx.Exec(`
			CREATE TABLE integrity_settings (
				id INTEGER PRIMARY KEY CHECK (id=1), enabled INTEGER NOT NULL DEFAULT 0,
				schedule TEXT NOT NULL DEFAULT '03:00', duration_minutes INTEGER NOT NULL DEFAULT 15,
				tolerance_horizontal_mm INTEGER NOT NULL DEFAULT 30,
				tolerance_vertical_mm INTEGER NOT NULL DEFAULT 50,
				host TEXT NOT NULL DEFAULT '', mountpoint TEXT NOT NULL DEFAULT '',
				username TEXT NOT NULL DEFAULT '', password_enc BLOB, password_nonce BLOB,
				updated_at INTEGER NOT NULL DEFAULT 0
			);
			INSERT OR IGNORE INTO integrity_settings (id) VALUES (1);
			CREATE TABLE integrity_runs (
				id INTEGER PRIMARY KEY, started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL,
				status TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '', solution TEXT NOT NULL DEFAULT '',
				samples INTEGER NOT NULL DEFAULT 0, latitude REAL, longitude REAL, height REAL,
				horizontal_mm REAL, vertical_mm REAL
			);
			CREATE INDEX idx_integrity_runs_started ON integrity_runs(started_at DESC);`); err != nil {
			return fmt.Errorf("migrate external integrity monitor to v4: %w", err)
		}
	}
	if cur < 5 {
		if _, err := tx.Exec(`
			CREATE TABLE push_targets (
				id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE COLLATE NOCASE,
				enabled INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL,
				host TEXT NOT NULL, mountpoint TEXT NOT NULL,
				protocol TEXT NOT NULL DEFAULT 'v2', username TEXT NOT NULL DEFAULT '',
				password_enc BLOB, password_nonce BLOB,
				updated_at INTEGER NOT NULL DEFAULT 0
			);`); err != nil {
			return fmt.Errorf("migrate NTRIP push-out to v5: %w", err)
		}
	}
	// v6 drops the original deployment's reference network from the defaults: the
	// integrity monitor works against any NTRIP network covering the area, and a
	// vendor name in an unconfigured field reads like a requirement. Only a row
	// that still holds those defaults and has no stored credential is cleared, so
	// a configured monitor is untouched.
	if cur < 6 {
		if _, err := tx.Exec(`UPDATE integrity_settings SET host='', mountpoint=''
			WHERE id=1 AND enabled=0 AND username='' AND password_enc IS NULL
			AND host='skpos.gku.sk:2101' AND mountpoint='SKPOS_CM_32_MSM7'`); err != nil {
			return fmt.Errorf("migrate integrity defaults to v6: %w", err)
		}
	}
	if cur != SchemaVersion {
		if _, err := tx.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, SchemaVersion, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Close() error { return s.db.Close() }

// Version reports the applied schema version.
func (s *Store) Version() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&v)
	return v, err
}
