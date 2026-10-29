package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "moesekai/server/internal/sqlitedriver"
)

var (
	freshOpenCount     atomic.Int32
	emptyTemplateOnce  sync.Once
	emptyTemplateBytes []byte
)

func seedFreshDatabaseFromTemplate(path string) bool {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") || strings.ContainsAny(path, "?#") {
		return false
	}
	if freshOpenCount.Add(1) == 1 {
		// Production opens a single database per process; avoid building or
		// retaining the template unless multiple fresh databases are opened.
		return false
	}
	emptyTemplateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "moesekai-empty-db-*")
		if err != nil {
			return
		}
		defer os.RemoveAll(dir)
		tplPath := filepath.Join(dir, "template.db")
		d, err := openDatabase(tplPath, false)
		if err != nil {
			return
		}
		_, _ = d.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		if err := d.Close(); err != nil {
			return
		}
		if b, err := os.ReadFile(tplPath); err == nil && len(b) > 0 {
			emptyTemplateBytes = b
		}
	})
	if len(emptyTemplateBytes) == 0 {
		return false
	}
	if err := os.WriteFile(path, emptyTemplateBytes, 0o600); err != nil {
		return false
	}
	return true
}

// DB wraps a SQLite connection with the application schema applied.
type DB struct {
	*sql.DB
	path string
}

// Open opens (or creates) the SQLite database at path and applies the schema
// using the CGO SQLite driver registered by internal/sqlitedriver.
func Open(path string) (*DB, error) {
	return openDatabase(path, true)
}

func openDatabase(path string, allowFreshTemplate bool) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("secure sqlite directory: %w", err)
	}
	_, statErr := os.Stat(path)
	preexisting := statErr == nil
	seededFromTemplate := false
	if preexisting {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secure sqlite database: %w", err)
		}
	} else if allowFreshTemplate && os.IsNotExist(statErr) {
		seededFromTemplate = seedFreshDatabaseFromTemplate(path)
	}
	// WAL lets many readers run concurrently with a single writer, which is the
	// whole point of using it here: background jobs (cn-sync, AI translate,
	// backup) hold long write transactions while editors keep reading. For that
	// concurrency to actually happen the pool must allow more than one
	// connection — capping it at 1 serializes every query behind the current
	// writer and turns sync windows into multi-minute stalls.
	//
	// _txlock=immediate makes write transactions begin with BEGIN IMMEDIATE so
	// they take the write lock up front and queue on busy_timeout instead of
	// deadlocking on lock upgrade (the classic multi-writer SQLITE_BUSY trap).
	// busy_timeout is raised to 10s to ride out the longest cn-sync commits.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_txlock=immediate", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Allow concurrent readers alongside the single writer (WAL's design). One
	// writer at a time is enforced by SQLite itself + BEGIN IMMEDIATE, not by
	// starving the pool.
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxIdleTime(2 * time.Minute)
	sqlDB.SetConnMaxLifetime(0)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("secure sqlite database: %w", err)
	}
	d := &DB{DB: sqlDB, path: path}
	if seededFromTemplate {
		return d, nil
	}
	pending, err := d.pendingMigrations()
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("inspect migrations: %w", err)
	}
	if preexisting && len(pending) > 0 {
		if _, err := d.createPreMigrationBackup(pending[0].version); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("pre-migration backup: %w", err)
		}
	}
	if err := d.applySchema(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := d.applyMigrations(pending); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	if err := d.ensureRuntimeInvariants(context.Background()); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply runtime invariants: %w", err)
	}
	if err := d.IntegrityCheck(context.Background()); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("verify sqlite integrity: %w", err)
	}
	return d, nil
}

// Path returns the SQLite database path used by Open.
func (d *DB) Path() string { return d.path }

// Checkpoint flushes committed WAL pages into the main database and truncates
// the sidecar. It is used before publishing an offline staging database.
func (d *DB) Checkpoint(ctx context.Context) error {
	rows, err := d.QueryContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var busy, logFrames, checkpointed int
		if err := rows.Scan(&busy, &logFrames, &checkpointed); err != nil {
			return err
		}
		if busy != 0 {
			return fmt.Errorf("sqlite checkpoint remained busy")
		}
	}
	return rows.Err()
}

// IntegrityCheck requires SQLite's complete integrity check to return exactly
// one "ok" row.
func (d *DB) IntegrityCheck(ctx context.Context) error {
	rows, err := d.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		count++
		if result != "ok" {
			return fmt.Errorf("sqlite integrity check: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("sqlite integrity check returned %d rows", count)
	}
	return nil
}

func (d *DB) applySchema() error {
	_, err := d.Exec(schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	category    TEXT NOT NULL,
	field       TEXT NOT NULL,
	jp_key      TEXT NOT NULL,
	cn_text     TEXT NOT NULL DEFAULT '',
	source      TEXT NOT NULL DEFAULT 'unknown',
	ids_json    TEXT NOT NULL DEFAULT '',
	updated_at  INTEGER NOT NULL DEFAULT 0,
	updated_by  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (category, field, jp_key)
);
CREATE INDEX IF NOT EXISTS idx_entries_cat_field ON entries(category, field);
CREATE INDEX IF NOT EXISTS idx_entries_source ON entries(category, field, source);

CREATE TABLE IF NOT EXISTS event_stories (
	event_id     INTEGER PRIMARY KEY,
	source       TEXT NOT NULL DEFAULT 'unknown',
	version      TEXT NOT NULL DEFAULT '1.0',
	last_updated INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS event_story_episodes (
	event_id        INTEGER NOT NULL,
	episode_no      TEXT NOT NULL,
	scenario_id     TEXT NOT NULL DEFAULT '',
	title           TEXT NOT NULL DEFAULT '',
	title_source    TEXT NOT NULL DEFAULT '',
	talk_order_json TEXT NOT NULL DEFAULT '',
	position        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (event_id, episode_no),
	FOREIGN KEY (event_id) REFERENCES event_stories(event_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS event_story_lines (
	event_id     INTEGER NOT NULL,
	episode_no   TEXT NOT NULL,
	jp_key       TEXT NOT NULL,
	cn_text      TEXT NOT NULL DEFAULT '',
	source       TEXT NOT NULL DEFAULT 'unknown',
	speaker_name TEXT NOT NULL DEFAULT '',
	position     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (event_id, episode_no, jp_key),
	FOREIGN KEY (event_id) REFERENCES event_stories(event_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	role          TEXT NOT NULL DEFAULT 'editor',
	created_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS settings (
	key       TEXT PRIMARY KEY,
	value     TEXT NOT NULL DEFAULT '',
	encrypted INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS audit_log (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	ts     INTEGER NOT NULL,
	user   TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);
`
