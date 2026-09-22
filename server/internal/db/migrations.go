package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type migration struct {
	version int
	name    string
	sql     string
	before  func(*sql.Tx) error
	after   func(*sql.Tx) error
}

func (m migration) checksum() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", m.version, m.name, m.sql)))
	return hex.EncodeToString(sum[:])
}

// Checksums of migration SQL that was already applied to a live database by an
// earlier deployment. The song-682 translation migrations were reissued while
// rolling out, so their persisted checksums differ from the current SQL text.
// Accepting exactly these recorded digests keeps startup working without
// weakening verification for any other migration.
var historicalMigrationChecksums = map[int][]string{
	32: {
		"18a454e0940769b55c13a22cd07e94ebd46222963bd0aec554c43cee50b4fb99",
		"1c5ad0e5f8cbb0bbf30f4c8bca6b9598259aec88c1e0d60cec8acc650a71bbeb",
	},
	33: {
		"cf44587e9263b8b0ee8e619217b5eb286da774a2f361601487edcf605407191f",
		"123c9f2976e18825a63f0ee3b3ef1639b4e2c4d7b3f621fdb46438bfc4c748bf",
		"a1e9588a8dc59e92132aa4f56d11edd5787f6813c86406f837189934b7fad510",
	},
	34: {
		"e61ad71423c414567bd644c46d5f98b2e6ee331cf10692d241cf905e77216a9a",
		"e9efdb750e13af654fcb7c04a87f30277f1a671e6cf40bcf862b3bcbec6e9856",
		"918e4dd2e211a3e4f56350f74804d6734cfba066905d956ca70dd765b3b7d450",
		"7e37d49e275c56cb5328c8a11129aae3673960a16d98c9808886b8b0e23931eb",
		"82d56af980b152ea6237db48c7540119bd66dd56b8fee438206fe4bc54d2c086",
	},
}

func migrationChecksumMatches(m migration, actualChecksum string) bool {
	if actualChecksum == m.checksum() {
		return true
	}
	for _, known := range historicalMigrationChecksums[m.version] {
		if actualChecksum == known {
			return true
		}
	}
	return false
}

func (m migration) validateDefinition() error {
	if m.version >= 13 && (m.before != nil || m.after != nil) {
		return fmt.Errorf("migration %d cannot use an unchecksummed migration callback", m.version)
	}
	return nil
}

var migrationBeforeCommitHook func(version int) error
var migrationBackupBeforeRenameHook func(tempPath string) error

func concatMigrations(groups ...[]migration) []migration {
	var total int
	for _, g := range groups {
		total += len(g)
	}
	out := make([]migration, 0, total)
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

var migrations = concatMigrations(
	migrationsV1ToV10,
	migrationsV11ToV20,
	migrationsV21ToV30,
	migrationsV31ToV34,
)

// ValidateKnownMigrationPrefix verifies an existing schema_migrations ledger
// without applying migrations. It is intended for immutable/read-only command
// paths that must accept a reviewed historical prefix while still rejecting
// gaps, unknown versions, and altered migration identities.
func (d *DB) ValidateKnownMigrationPrefix(ctx context.Context, minimumVersion, maximumVersion int) (int, error) {
	if ctx == nil {
		return 0, errors.New("migration prefix validation requires context")
	}
	if minimumVersion < 1 || maximumVersion < minimumVersion || maximumVersion > len(migrations) {
		return 0, errors.New("migration prefix validation range is invalid")
	}
	var exists int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists != 1 {
		return 0, errors.New("schema_migrations ledger is missing")
	}
	rows, err := d.QueryContext(ctx, `SELECT version,name,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	expectedVersion := 1
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return 0, err
		}
		if version < 1 || version > len(migrations) {
			return 0, fmt.Errorf("database migration version %d is newer than this binary", version)
		}
		if version != expectedVersion {
			return 0, fmt.Errorf("database migration history is not a contiguous prefix: expected version %d, found %d", expectedVersion, version)
		}
		want := migrations[version-1]
		if name != want.name || !migrationChecksumMatches(want, checksum) {
			return 0, fmt.Errorf("migration %d checksum mismatch", version)
		}
		expectedVersion++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	actualVersion := expectedVersion - 1
	if actualVersion < minimumVersion || actualVersion > maximumVersion {
		return actualVersion, fmt.Errorf("database migration prefix version %d is outside supported range %d through %d",
			actualVersion, minimumVersion, maximumVersion)
	}
	return actualVersion, nil
}

func (d *DB) pendingMigrations() ([]migration, error) {
	var exists int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return append([]migration(nil), migrations...), nil
	}
	rows, err := d.Query(`SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := map[int]string{}
	expectedVersion := 1
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return nil, err
		}
		if version < 1 || version > len(migrations) {
			return nil, fmt.Errorf("database migration version %d is newer than this binary", version)
		}
		if version != expectedVersion {
			return nil, fmt.Errorf("database migration history is not a contiguous prefix: expected version %d, found %d", expectedVersion, version)
		}
		want := migrations[version-1]
		if name != want.name || !migrationChecksumMatches(want, checksum) {
			return nil, fmt.Errorf("migration %d checksum mismatch", version)
		}
		applied[version] = checksum
		expectedVersion++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var pending []migration
	for _, m := range migrations {
		if err := m.validateDefinition(); err != nil {
			return nil, err
		}
		if _, ok := applied[m.version]; !ok {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

func (d *DB) applyMigrations(pending []migration) error {
	for _, m := range pending {
		if err := m.validateDefinition(); err != nil {
			return err
		}
		if m.version == 13 {
			if err := d.validateV13LyricsSourceJobs(); err != nil {
				return fmt.Errorf("migration 13 preflight: %w", err)
			}
		}
		if err := d.applyMigration(m); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) applyMigration(m migration) error {
	foreignKeysOff := strings.Contains(m.sql, "-- migration:foreign_keys_off")
	var (
		conn *sql.Conn
		tx   *sql.Tx
		err  error
	)
	if foreignKeysOff {
		conn, err = d.Conn(context.Background())
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
			return fmt.Errorf("migration %d disable foreign keys: %w", m.version, err)
		}
		defer conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`)
		defer conn.ExecContext(context.Background(), `PRAGMA legacy_alter_table=OFF`)
		tx, err = conn.BeginTx(context.Background(), nil)
	} else {
		tx, err = d.Begin()
	}
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	if m.before != nil {
		if err := m.before(tx); err != nil {
			return rollback(fmt.Errorf("migration %d preflight: %w", m.version, err))
		}
	}
	if _, err := tx.Exec(m.sql); err != nil {
		return rollback(fmt.Errorf("migration %d %s: %w", m.version, m.name, err))
	}
	if foreignKeysOff {
		rows, err := tx.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			return rollback(fmt.Errorf("migration %d foreign key check: %w", m.version, err))
		}
		hasViolation := rows.Next()
		closeErr := rows.Close()
		if hasViolation || closeErr != nil {
			return rollback(fmt.Errorf("migration %d produced invalid foreign keys", m.version))
		}
	}
	if m.after != nil {
		if err := m.after(tx); err != nil {
			return rollback(fmt.Errorf("migration %d post-step: %w", m.version, err))
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		m.version, m.name, m.checksum(), time.Now().Unix()); err != nil {
		return rollback(err)
	}
	if migrationBeforeCommitHook != nil {
		if err := migrationBeforeCommitHook(m.version); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if foreignKeysOff {
		if _, err := conn.ExecContext(context.Background(), `PRAGMA legacy_alter_table=OFF`); err != nil {
			return fmt.Errorf("migration %d restore alter-table semantics: %w", m.version, err)
		}
		if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); err != nil {
			return fmt.Errorf("migration %d restore foreign keys: %w", m.version, err)
		}
		var enabled int
		if err := conn.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
			return fmt.Errorf("migration %d did not restore foreign key enforcement", m.version)
		}
	}
	return nil
}

func (d *DB) validateV13LyricsSourceJobs() error {
	rows, err := d.Query(`SELECT j.job_id, COUNT(DISTINCT json_extract(candidate.value, '$.sha1'))
		FROM lyrics_discovery_jobs j
		LEFT JOIN lyrics_discovery_shadow_results r ON r.job_id=j.job_id
		LEFT JOIN json_each(json_extract(r.result_json, '$.candidates')) candidate ON
			typeof(json_extract(candidate.value, '$.pageId'))='integer'
			AND json_extract(candidate.value, '$.pageId')=j.page_id
			AND typeof(json_extract(candidate.value, '$.revisionId'))='integer'
			AND json_extract(candidate.value, '$.revisionId')=j.revision_id
			AND typeof(json_extract(candidate.value, '$.sha1'))='text'
			AND length(json_extract(candidate.value, '$.sha1'))=40
			AND json_extract(candidate.value, '$.sha1')=lower(json_extract(candidate.value, '$.sha1'))
			AND json_extract(candidate.value, '$.sha1') NOT GLOB '*[^0-9a-f]*'
		WHERE j.kind='fetch_revision'
		GROUP BY j.job_id ORDER BY j.job_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var jobID int64
		var sha1Count int
		if err := rows.Scan(&jobID, &sha1Count); err != nil {
			return err
		}
		switch {
		case sha1Count == 0:
			return fmt.Errorf("lyrics discovery job %d has unreconcilable v12 fetch_revision expected_sha1", jobID)
		case sha1Count > 1:
			return fmt.Errorf("lyrics discovery job %d has conflicting v12 fetch_revision expected_sha1 values", jobID)
		}
	}
	return rows.Err()
}

func (d *DB) createPreMigrationBackup(version int) (string, error) {
	if strings.TrimSpace(d.path) == "" || strings.Contains(d.path, ":memory:") {
		return "", nil
	}
	backupPath := fmt.Sprintf("%s.pre-migration-v%d.bak", d.path, version)
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
		return "", err
	}
	tempPath := backupPath + ".tmp"
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := d.Exec(`VACUUM INTO ?`, tempPath); err != nil {
		return "", err
	}
	defer os.Remove(tempPath)
	if err := verifySQLiteBackup(tempPath); err != nil {
		return "", err
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return "", err
	}
	backupFile, err := os.OpenFile(tempPath, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	if err := backupFile.Sync(); err != nil {
		backupFile.Close()
		return "", err
	}
	if err := backupFile.Close(); err != nil {
		return "", err
	}
	if migrationBackupBeforeRenameHook != nil {
		if err := migrationBackupBeforeRenameHook(tempPath); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tempPath, backupPath); err != nil {
		return "", err
	}
	dir, err := os.Open(filepath.Dir(backupPath))
	if err != nil {
		return "", err
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return "", err
	}
	if err := dir.Close(); err != nil {
		return "", err
	}
	return backupPath, nil
}

func verifySQLiteBackup(path string) error {
	backup, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer backup.Close()
	var result string
	if err := backup.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("backup integrity_check: %s", result)
	}
	return nil
}
