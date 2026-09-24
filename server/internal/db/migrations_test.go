package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func legacyFixtureCopy(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := copyFixture(filepath.Join("testdata", "legacy-v2.db"), path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDurableLyricsDiscoveryMigrationChecksumsRemainImmutable(t *testing.T) {
	for version, want := range map[int]string{
		10: "0337eb9ab9698b464a1cba88e282f7f14771572af0ae0a99b61e67920fce25b3",
		11: "b396a5365098bb08f83557cb7e5029df9abbf139825b993a4673eb2c69b1ea78",
		12: "1bcc25c5e99cdb8eabdf339a7a9b1dc91c3f820e4abacfd1db46f53f8f8150d3",
		13: "ad19d1def9f1c0008372b382100a6f29b69f75cec7d4bd7bb1068de831c07f82",
		14: "2f1fe73e7f61a68b5ca821dd5ce485b25693732f5bd3b72b5913e78244397576",
		15: "090284b989d62edcc0dc54a211f118b04fe1178288d854c76a27a69a8e4c61b0",
		16: "02f4d7aae30d979bcfd32c721fe1a95cb8e6ce9d269f77d01fdb11b6d2a63d84",
		17: "665a877eef31d2882468c7ca8a29a0732f51b332996039281efc901bb23ea48a",
		18: "9ef12f0d266c281cfae1b76f80a61eb6c5142fd64ea9a45d7b97e327216031ff",
		19: "6c2977cc4290ec56af216d1888e21ac64bbc281aa4b669e662840a5e75f3046b",
		20: "ba96cd088d14cdc9d7e34536a16d438f34d7fa232182d5e3000aa9fc0f9328dc",
		21: "820f2be54c57bc56aeb938f498a73109f62266a562764346b557603e90ec0282",
		22: "64edad15266a55c04f7d300d043a59a2c82e1f43f1b5f56cf5d3c7552533832d",
		23: "65e375543d264f60af66984ab50c87a05bf593c512926813e4870a8a388bd40f",
		24: "fefc0fba06b0de2af2ef7d7f9802d8eeb0e6bdcd911b1f16a6fb0a4e9a7a6469",
		25: "3f6094fef8835e1846b648e877c008fd48d17ab0855519580e9d3840a014224b",
		26: "ded39b7f7ec1286d02842938a2f429a56b8b38daba8294cd115dcefe6f149953",
		27: "9de2359101ac9c9a9ff01389804030d1fbc7e85253ff4e5059d2ceceb5f1ca9b",
		28: "eb21867c6bd48192174450bb5b87041435a63e6c4a0b2080fa7c97e2fdbefb5d",
		29: "f3dff40248118068ac37d440099ff231fbbd268d899732e224ee82386b0636d0",
		30: "8a0d9ec76bdbd264be1342afa7ab02decbdb07f345a808259eac5488426e6089",
	} {
		if got := migrations[version-1].checksum(); got != want {
			t.Fatalf("migration v%d checksum=%s want=%s", version, got, want)
		}
	}
}

func TestMigrationsAfterV34MustNotMutateContentTables(t *testing.T) {
	contentTables := []string{
		"song_lyrics_translation_edition_lines",
		"song_lyrics_translation_edition_localizations",
		"song_lyrics_rendition_translation_lines",
		"song_lyrics_rendition_localizations",
		"song_lyric_lines",
		"song_lyric_segments",
		"song_lyrics",
		"song_lyrics_publications",
		"song_lyrics_source_documents",
		"catalog_music",
		// Prefix of the immutable recovery import ledger and its takeover rows.
		"lyrics_recovery_",
	}
	// The patterns below match by prefix, so they also cover tables such as
	// song_lyrics_source_artifacts that only share a content-table name. A
	// migration may rebuild one of those listed here, and only as a verbatim
	// copy: INSERT INTO <table> (<columns>) SELECT <same columns> FROM
	// <renamed predecessor>, then DROP TABLE <renamed predecessor>.
	// TestMigrationV37RebuildsSongLyricsSourceArtifactsVerbatim compares every
	// row before and after.
	verbatimRebuilds := map[int]map[string]string{
		37: {"SONG_LYRICS_SOURCE_ARTIFACTS": "SONG_LYRICS_SOURCE_ARTIFACTS_V36"},
	}
	// These migrations may only create the named table with its indexes and
	// RAISE-only triggers.
	createOnly := map[int]string{
		38: "LYRICS_RECOVERY_TAKEOVERS",
	}

	for _, m := range migrations {
		if m.version <= 34 {
			continue
		}
		// A before/after hook runs arbitrary Go against the same transaction, so
		// the SQL scan below cannot see what it writes. Require review instead.
		if m.before != nil || m.after != nil {
			t.Fatalf("migration %d (%s) carries a Go hook that this guard cannot inspect; keep post-v34 migrations pure DDL or move the logic behind an API handler",
				m.version, m.name)
		}
		upper := strings.ToUpper(m.sql)
		normalized := strings.Join(strings.Fields(upper), " ")
		if table, ok := createOnly[m.version]; ok {
			statements := migrationSQLStatements(m.sql)
			if len(statements) == 0 {
				t.Fatalf("migration %d (%s) has no statements", m.version, m.name)
			}
			for _, statement := range statements {
				if !isCreateOnlyStatement(statement, table) {
					t.Fatalf("migration %d (%s) may only create %s, its indexes and RAISE-only triggers; found %q",
						m.version, m.name, table, statement)
				}
			}
		}
		for _, table := range contentTables {
			tableUpper := strings.ToUpper(table)
			patterns := []string{
				"INSERT INTO " + tableUpper,
				"INSERT OR REPLACE INTO " + tableUpper,
				"INSERT OR IGNORE INTO " + tableUpper,
				"INSERT OR ABORT INTO " + tableUpper,
				"INSERT OR FAIL INTO " + tableUpper,
				"INSERT OR ROLLBACK INTO " + tableUpper,
				"REPLACE INTO " + tableUpper,
				"UPDATE " + tableUpper,
				"UPDATE OR REPLACE " + tableUpper,
				"UPDATE OR IGNORE " + tableUpper,
				"DELETE FROM " + tableUpper,
				"DROP TABLE " + tableUpper,
			}
			for _, pattern := range patterns {
				for offset := 0; ; {
					index := strings.Index(normalized[offset:], pattern)
					if index < 0 {
						break
					}
					statement := normalized[offset+index:]
					offset += index + len(pattern)
					if end := strings.IndexByte(statement, ';'); end >= 0 {
						statement = statement[:end]
					}
					if isVerbatimRebuildStatement(strings.TrimSpace(statement), verbatimRebuilds[m.version]) {
						continue
					}
					t.Fatalf("migration %d (%s) modifies content table %q via %q; content mutations must go through /api/editor/v1/lyrics/save or /api/editor/v1/lyrics/translation-editions, not schema migrations",
						m.version, m.name, table, pattern)
				}
			}
		}
	}
}

var verbatimRebuildCopyStatement = regexp.MustCompile(`^INSERT INTO ([A-Z0-9_]+) \(([^)]*)\) SELECT (.*) FROM ([A-Z0-9_]+)( ORDER BY [A-Z0-9_]+(, ?[A-Z0-9_]+)*)?$`)

// isVerbatimRebuildStatement reports whether a normalized statement is the
// column-for-column copy into a rebuilt table from its renamed predecessor, or
// the drop of that predecessor.
func isVerbatimRebuildStatement(statement string, rebuilds map[string]string) bool {
	if match := verbatimRebuildCopyStatement.FindStringSubmatch(statement); match != nil {
		predecessor, ok := rebuilds[match[1]]
		return ok && match[4] == predecessor &&
			strings.ReplaceAll(match[2], " ", "") == strings.ReplaceAll(match[3], " ", "")
	}
	if dropped, ok := strings.CutPrefix(statement, "DROP TABLE "); ok {
		for _, predecessor := range rebuilds {
			if dropped == predecessor {
				return true
			}
		}
	}
	return false
}

// migrationSQLStatements splits migration SQL into normalized upper-case
// statements, dropping -- comments and keeping each trigger body whole.
func migrationSQLStatements(sqlText string) []string {
	var lines []string
	for _, line := range strings.Split(sqlText, "\n") {
		if index := strings.Index(line, "--"); index >= 0 {
			line = line[:index]
		}
		lines = append(lines, line)
	}
	rest := strings.Join(strings.Fields(strings.ToUpper(strings.Join(lines, "\n"))), " ")
	var statements []string
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return statements
		}
		end := strings.IndexByte(rest, ';')
		if strings.HasPrefix(rest, "CREATE TRIGGER ") {
			if bodyEnd := strings.Index(rest, "; END;"); bodyEnd >= 0 {
				end = bodyEnd + len("; END")
			}
		}
		if end < 0 {
			return append(statements, rest)
		}
		statements = append(statements, strings.TrimSpace(rest[:end]))
		rest = rest[end+1:]
	}
}

var (
	createOnlyTableStatement   = regexp.MustCompile(`^CREATE TABLE ([A-Z0-9_]+) \(`)
	createOnlyIndexStatement   = regexp.MustCompile(`^CREATE INDEX [A-Z0-9_]+ ON ([A-Z0-9_]+) ?\([A-Z0-9_, ]+\)$`)
	createOnlyTriggerStatement = regexp.MustCompile(`^CREATE TRIGGER [A-Z0-9_]+ BEFORE (?:INSERT|UPDATE|DELETE) ON ([A-Z0-9_]+) (?:WHEN .* )?BEGIN SELECT RAISE\(ABORT, '[^';]*'\); END$`)
)

// isCreateOnlyStatement reports whether a normalized statement creates table,
// an index on it, or a trigger on it whose body only raises.
func isCreateOnlyStatement(statement, table string) bool {
	if strings.Contains(statement, "INSERT INTO") || strings.Contains(statement, "DELETE FROM") ||
		strings.Contains(statement, "UPDATE ") && !strings.Contains(statement, "BEFORE UPDATE ON ") {
		return false
	}
	for _, pattern := range []*regexp.Regexp{createOnlyTableStatement, createOnlyIndexStatement, createOnlyTriggerStatement} {
		if match := pattern.FindStringSubmatch(statement); match != nil {
			return match[1] == table
		}
	}
	return false
}

func TestCreateOnlyStatementAllowsOnlyTheNamedTableIndexAndRaiseTriggers(t *testing.T) {
	statements := migrationSQLStatements(migrationV38LyricsRecoveryTakeoversSQL)
	if len(statements) != 5 {
		t.Fatalf("v38 statements=%d: %q", len(statements), statements)
	}
	for statement, want := range map[string]bool{
		"CREATE TABLE LYRICS_RECOVERY_TAKEOVERS (MUSIC_ID INTEGER PRIMARY KEY)":                                                                   true,
		"CREATE INDEX IDX_T ON LYRICS_RECOVERY_TAKEOVERS(BATCH_SHA256,MUSIC_ID)":                                                                  true,
		"CREATE TRIGGER T_U BEFORE UPDATE ON LYRICS_RECOVERY_TAKEOVERS BEGIN SELECT RAISE(ABORT, 'IMMUTABLE'); END":                               true,
		"CREATE TRIGGER T_D BEFORE DELETE ON LYRICS_RECOVERY_TAKEOVERS WHEN EXISTS (SELECT 1 FROM X) BEGIN SELECT RAISE(ABORT, 'IMMUTABLE'); END": true,
		"CREATE TABLE LYRICS_RECOVERY_IMPORT_ITEMS (MUSIC_ID INTEGER)":                                                                            false,
		"CREATE INDEX IDX_T ON LYRICS_RECOVERY_IMPORT_ITEMS(MUSIC_ID)":                                                                            false,
		"CREATE TRIGGER T_I AFTER INSERT ON LYRICS_RECOVERY_TAKEOVERS BEGIN DELETE FROM LYRICS_RECOVERY_IMPORT_ITEMS; END":                        false,
		"CREATE TRIGGER T_I BEFORE INSERT ON LYRICS_RECOVERY_TAKEOVERS BEGIN UPDATE SONG_LYRICS SET REVISION=1; END":                              false,
		"CREATE TRIGGER T_I BEFORE INSERT ON LYRICS_RECOVERY_TAKEOVERS BEGIN SELECT RAISE(ABORT, 'X'); DELETE FROM X; END":                        false,
		"INSERT INTO LYRICS_RECOVERY_TAKEOVERS SELECT * FROM LYRICS_RECOVERY_IMPORT_ITEMS":                                                        false,
		"DROP TRIGGER LYRICS_RECOVERY_IMPORT_ITEMS_IMMUTABLE_UPDATE":                                                                              false,
		"ALTER TABLE LYRICS_RECOVERY_TAKEOVERS ADD COLUMN X TEXT":                                                                                 false,
	} {
		if got := isCreateOnlyStatement(statement, "LYRICS_RECOVERY_TAKEOVERS"); got != want {
			t.Errorf("isCreateOnlyStatement(%q)=%v want %v", statement, got, want)
		}
	}
}

func TestVerbatimRebuildStatementAllowsOnlyTheColumnForColumnCopy(t *testing.T) {
	rebuilds := map[string]string{"SONG_LYRICS_SOURCE_ARTIFACTS": "SONG_LYRICS_SOURCE_ARTIFACTS_V36"}
	for statement, want := range map[string]bool{
		"INSERT INTO SONG_LYRICS_SOURCE_ARTIFACTS (DOCUMENT_ID,PROVIDER, ORIGIN) SELECT DOCUMENT_ID,PROVIDER,ORIGIN FROM SONG_LYRICS_SOURCE_ARTIFACTS_V36 ORDER BY DOCUMENT_ID,RENDITION_KEY": true,
		"INSERT INTO SONG_LYRICS_SOURCE_ARTIFACTS (DOCUMENT_ID,PROVIDER) SELECT DOCUMENT_ID,PROVIDER FROM SONG_LYRICS_SOURCE_ARTIFACTS_V36":                                                   true,
		"DROP TABLE SONG_LYRICS_SOURCE_ARTIFACTS_V36": true,
		"INSERT INTO SONG_LYRICS_SOURCE_ARTIFACTS (DOCUMENT_ID,ORIGIN) SELECT DOCUMENT_ID,'HTTPS://EVIL.FANDOM.COM' FROM SONG_LYRICS_SOURCE_ARTIFACTS_V36": false,
		"INSERT INTO SONG_LYRICS_SOURCE_ARTIFACTS (DOCUMENT_ID,PROVIDER) SELECT PROVIDER,DOCUMENT_ID FROM SONG_LYRICS_SOURCE_ARTIFACTS_V36":                false,
		"INSERT INTO SONG_LYRICS_SOURCE_ARTIFACTS (DOCUMENT_ID) SELECT DOCUMENT_ID FROM SONG_LYRICS_SOURCE_DOCUMENTS":                                      false,
		"INSERT INTO SONG_LYRICS_SOURCE_ARTIFACTS (DOCUMENT_ID) SELECT DOCUMENT_ID FROM SONG_LYRICS_SOURCE_ARTIFACTS_V36 ORDER BY DOCUMENT_ID LIMIT 1":     false,
		"INSERT INTO SONG_LYRICS_SOURCE_ARTIFACTS (DOCUMENT_ID) VALUES (1)":                                                                                false,
		"INSERT INTO SONG_LYRICS (MUSIC_ID) SELECT MUSIC_ID FROM SONG_LYRICS_SOURCE_ARTIFACTS_V36":                                                         false,
		"INSERT INTO SONG_LYRICS_RENDITION_SIDE_TRANSLATION_LINES (TEXT) SELECT TEXT FROM SONG_LYRICS_SOURCE_ARTIFACTS_V36":                                false,
		"DROP TABLE SONG_LYRICS_SOURCE_ARTIFACTS":            false,
		"DROP TABLE SONG_LYRICS_SOURCE_DOCUMENTS":            false,
		"DELETE FROM SONG_LYRICS_SOURCE_ARTIFACTS":           false,
		"UPDATE SONG_LYRICS_SOURCE_ARTIFACTS SET ORIGIN='X'": false,
	} {
		if got := isVerbatimRebuildStatement(statement, rebuilds); got != want {
			t.Errorf("isVerbatimRebuildStatement(%q)=%t want %t", statement, got, want)
		}
	}
	if isVerbatimRebuildStatement("DROP TABLE SONG_LYRICS_SOURCE_ARTIFACTS_V36", nil) {
		t.Fatal("a migration without a declared rebuild may not drop a content-prefixed table")
	}
}

func TestMigrationIsTransactionalIdempotentAndBackedUp(t *testing.T) {
	path := legacyFixtureCopy(t, "production.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version, migrationCount int
	var checksum string
	if err := database.QueryRow(`SELECT version, checksum FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &checksum); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		database.Close()
		t.Fatal(err)
	}
	latest := migrations[len(migrations)-1]
	if version != latest.version || migrationCount != len(migrations) || checksum != latest.checksum() {
		database.Close()
		t.Fatalf("migration record version=%d count=%d checksum=%q; want version=%d count=%d", version, migrationCount, checksum, latest.version, len(migrations))
	}
	var segments, localized int
	if err := database.QueryRow(`SELECT COUNT(*) FROM event_story_segments`).Scan(&segments); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM event_story_segment_localizations WHERE locale='zh-CN'`).Scan(&localized); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if segments != 2 || localized != 2 {
		database.Close()
		t.Fatalf("backfill segments=%d localized=%d", segments, localized)
	}
	var talkHash, titleSource string
	if err := database.QueryRow(`SELECT source_hash FROM event_story_segments WHERE kind='talk'`).Scan(&talkHash); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT source_text FROM event_story_segments WHERE kind='title'`).Scan(&titleSource); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if talkHash == "" || titleSource != "" {
		database.Close()
		t.Fatalf("source backfill hash=%q titleSource=%q", talkHash, titleSource)
	}
	var titleSegmentID string
	if err := database.QueryRow(`SELECT segment_id FROM event_story_segments WHERE kind='title'`).Scan(&titleSegmentID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if !strings.HasSuffix(titleSegmentID, ":title:-1") {
		database.Close()
		t.Fatalf("migrated title segment ID = %q", titleSegmentID)
	}
	var scenarioTable int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='event_story_scenarios'`).Scan(&scenarioTable); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if scenarioTable != 1 {
		database.Close()
		t.Fatal("v8 scenario side table was not created")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	backupPath := path + ".pre-migration-v1.bak"
	before, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("pre-migration backup missing: %v", err)
	}
	if err := verifySQLiteBackup(backupPath); err != nil {
		t.Fatal(err)
	}
	if got := before.Mode().Perm(); got != 0o600 {
		t.Fatalf("pre-migration backup mode=%#o want %#o", got, os.FileMode(0o600))
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("idempotent reopen rewrote the pre-migration backup")
	}
}

func TestMigrationFailureRollsBackAndLeavesRecoverableBackup(t *testing.T) {
	path := legacyFixtureCopy(t, "failure.db")
	migrationBeforeCommitHook = func(version int) error {
		return errors.New("injected migration failure")
	}
	t.Cleanup(func() { migrationBeforeCommitHook = nil })
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "injected migration failure") {
		t.Fatalf("Open error = %v", err)
	}
	migrationBeforeCommitHook = nil

	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var tableCount int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatal("failed migration left schema_migrations behind")
	}
	var text string
	if err := raw.QueryRow(`SELECT cn_text FROM entries WHERE jp_key='旧キー'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "旧翻译" {
		t.Fatalf("legacy data changed to %q", text)
	}
	if err := verifySQLiteBackup(path + ".pre-migration-v1.bak"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRetryRefreshesBackupAfterLegacyWrite(t *testing.T) {
	path := legacyFixtureCopy(t, "retry-refresh.db")
	migrationBeforeCommitHook = func(version int) error {
		return errors.New("injected first migration failure")
	}
	t.Cleanup(func() { migrationBeforeCommitHook = nil })
	if _, err := Open(path); err == nil {
		t.Fatal("first migration unexpectedly succeeded")
	}

	legacy, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`UPDATE entries SET cn_text='written-after-failure' WHERE jp_key='旧キー'`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	migrationBeforeCommitHook = nil
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := sql.Open("sqlite", "file:"+path+".pre-migration-v1.bak?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var text string
	if err := backup.QueryRow(`SELECT cn_text FROM entries WHERE jp_key='旧キー'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "written-after-failure" {
		t.Fatalf("retry backup retained stale value %q", text)
	}
}

func TestInterruptedOrCorruptMigrationBackupRecovers(t *testing.T) {
	path := legacyFixtureCopy(t, "backup-recovery.db")
	backupPath := path + ".pre-migration-v1.bak"
	if err := os.WriteFile(backupPath, []byte("interrupted backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath+".tmp", []byte("stale temporary backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	if err := verifySQLiteBackup(backupPath); err != nil {
		t.Fatalf("replacement backup is not recoverable: %v", err)
	}
	if _, err := os.Stat(backupPath + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary backup remains after recovery: %v", err)
	}
}

func TestMigrationBackupRenameFailureKeepsExistingFinal(t *testing.T) {
	path := legacyFixtureCopy(t, "backup-rename-failure.db")
	backupPath := path + ".pre-migration-v1.bak"
	corrupt := []byte("existing interrupted final")
	if err := os.WriteFile(backupPath, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	migrationBackupBeforeRenameHook = func(string) error { return errors.New("injected rename interruption") }
	t.Cleanup(func() { migrationBackupBeforeRenameHook = nil })
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "injected rename interruption") {
		t.Fatalf("Open error = %v", err)
	}
	migrationBackupBeforeRenameHook = nil
	got, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("failed atomic replacement changed final backup to %q", got)
	}
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	if err := verifySQLiteBackup(backupPath); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationChecksumMismatchRefusesStartup(t *testing.T) {
	path := legacyFixtureCopy(t, "checksum.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE schema_migrations SET checksum='tampered' WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	database.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Open error = %v", err)
	}
}

func TestMigrationHistoryGapRefusesStartup(t *testing.T) {
	path := legacyFixtureCopy(t, "migration-gap.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM schema_migrations WHERE version=3`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "not a contiguous prefix") {
		t.Fatalf("Open error = %v", err)
	}
}

func TestPreviousBinaryQueriesRemainCompatibleAfterMigration(t *testing.T) {
	path := legacyFixtureCopy(t, "rolling.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()

	legacy, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(`UPDATE entries SET cn_text='rolling-old-binary', source='human' WHERE jp_key='旧キー'`); err != nil {
		t.Fatalf("legacy writer failed after additive migration: %v", err)
	}
	var got string
	if err := legacy.QueryRow(`SELECT cn_text FROM entries WHERE category='cards' AND field='prefix' AND jp_key='旧キー'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "rolling-old-binary" {
		t.Fatalf("legacy query got %q", got)
	}
	if _, err := legacy.Exec(`INSERT INTO event_story_segment_localizations
		(segment_id, locale, text, source, updated_at, updated_by, revision)
		SELECT segment_id, 'en-US', 'manual English', 'human', 1, 'editor', 1
		FROM event_story_segments WHERE event_id=7 AND kind='talk'`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO event_story_scenarios(event_id, episode_no, scenario_id, canonical_json, sha256)
		VALUES (7, '1', 'legacy-scenario', '{"ScenarioId":"legacy-scenario","Snippets":[],"TalkData":[],"SpecialEffectData":[],"AppearCharacters":[]}',
		'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DELETE FROM event_stories WHERE event_id=7`); err != nil {
		t.Fatalf("legacy event replace delete failed: %v", err)
	}
	var localized int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM event_story_segment_localizations WHERE locale='en-US' AND text='manual English'`).Scan(&localized); err != nil {
		t.Fatal(err)
	}
	if localized != 1 {
		t.Fatal("legacy event replace cascaded into additive locale content")
	}
	var scenarios int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM event_story_scenarios WHERE event_id=7`).Scan(&scenarios); err != nil {
		t.Fatal(err)
	}
	if scenarios != 1 {
		t.Fatal("legacy event replace cascaded into scenario snapshots")
	}
}

func TestV9MigrationRetainsExistingSegmentsAndAllowsRecoveryIdentityAtSamePosition(t *testing.T) {
	path := legacyFixtureCopy(t, "v9-segment-recovery.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:8]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	var originalID, episodeNo, scenarioID, kind, jpKey, sourceText, sourceHash string
	var eventID, position int
	if err := raw.QueryRow(`SELECT segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash
		FROM event_story_segments WHERE kind='talk'`).Scan(&originalID, &eventID, &episodeNo, &scenarioID,
		&kind, &position, &jpKey, &sourceText, &sourceHash); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	recoveryID := originalID + ":recovery"
	if _, err := migrated.Exec(`INSERT INTO event_story_segments
		(segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, recoveryID, eventID, episodeNo, "replacement-scenario", kind,
		position, jpKey, sourceText, sourceHash); err != nil {
		t.Fatalf("same-position recovery segment after v9: %v", err)
	}
	var count int
	if err := migrated.QueryRow(`SELECT COUNT(*) FROM event_story_segments WHERE segment_id IN (?, ?)`,
		originalID, recoveryID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("v9 segment preservation count=%d err=%v", count, err)
	}
}

func TestV10MigrationCreatesDurableLyricsDiscoveryQueue(t *testing.T) {
	path := legacyFixtureCopy(t, "v10-lyrics-discovery-queue.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:9]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var version int
	if err := migrated.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
	if _, err := migrated.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
		VALUES (?, 'discover', 'queued', 1, ?, 'shadow-v1', 0, 3, 1, 1, 1, 1)`, strings.Repeat("a", 64), strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := migrated.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
		VALUES (?, 'unsupported', 'queued', 2, 0, 3, 1, 1, 1, 1)`, strings.Repeat("b", 64)); err == nil {
		t.Fatal("v10 queue accepted unsupported job kind")
	}
	if _, err := migrated.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
		VALUES (?, 'fetch_revision', 'queued', 2, 0, 3, 1, 1, 1, 1)`, strings.Repeat("c", 64)); err == nil {
		t.Fatal("v10 queue accepted incomplete fetch_revision target")
	}
	if _, err := migrated.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, attempts, max_attempts, next_attempt_at, lease_owner, created_at, updated_at, version)
		VALUES (?, 'discover', 'queued', 3, 0, 3, 1, 'orphan_owner', 1, 1, 1)`, strings.Repeat("d", 64)); err == nil {
		t.Fatal("v10 queue accepted lease owner outside leased state")
	}
}

func TestV11MigrationUpgradesExistingQueueAndConstrainsShadowResults(t *testing.T) {
	path := legacyFixtureCopy(t, "v11-lyrics-discovery-shadow.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:10]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
		VALUES (?, 'discover', 'queued', 7, 0, 3, 1, 1, 1, 1)`, strings.Repeat("e", 64)); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[10:11]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	defer raw.Close()
	migrated := raw
	var fingerprint, policy string
	if err := migrated.QueryRow(`SELECT catalog_fingerprint, policy_version FROM lyrics_discovery_jobs WHERE music_id=7`).Scan(&fingerprint, &policy); err != nil {
		t.Fatal(err)
	}
	if fingerprint != "" || policy != "" {
		t.Fatalf("v11 changed legacy queue generation fingerprint=%q policy=%q", fingerprint, policy)
	}
	fingerprint = strings.Repeat("f", 64)
	if _, err := migrated.Exec(`UPDATE lyrics_discovery_jobs SET catalog_fingerprint=?, policy_version='shadow-v1' WHERE music_id=7`, fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := migrated.Exec(`INSERT INTO lyrics_discovery_shadow_results
		(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
		SELECT job_id, music_id, catalog_fingerprint, policy_version, 'candidates_found', 1, '{"candidates":[{"pageId":12}]}', 2
		FROM lyrics_discovery_jobs WHERE music_id=7`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrated.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
		VALUES (?, 'discover', 'queued', 8, ?, 'shadow-v1', 0, 3, 1, 1, 1, 1)`, strings.Repeat("d", 64), strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"duplicate job": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, music_id, catalog_fingerprint, policy_version, 'no_candidates', 0, '{}', 3
			FROM lyrics_discovery_jobs WHERE music_id=7`,
		"invalid fingerprint": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, music_id, 'bad', policy_version, 'no_candidates', 0, '{}', 3
			FROM lyrics_discovery_jobs WHERE music_id=8`,
		"invalid policy": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, music_id, catalog_fingerprint, ' shadow-v1 ', 'no_candidates', 0, '{}', 3
			FROM lyrics_discovery_jobs WHERE music_id=8`,
		"invalid found count": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, music_id, catalog_fingerprint, policy_version, 'candidates_found', 2, '{}', 3
			FROM lyrics_discovery_jobs WHERE music_id=8`,
		"invalid ambiguous count": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, music_id, catalog_fingerprint, policy_version, 'ambiguous', 0, '{}', 3
			FROM lyrics_discovery_jobs WHERE music_id=8`,
		"invalid JSON": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, music_id, catalog_fingerprint, policy_version, 'no_candidates', 0, '{', 3
			FROM lyrics_discovery_jobs WHERE music_id=8`,
		"non-object JSON": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, music_id, catalog_fingerprint, policy_version, 'no_candidates', 0, '[]', 3
			FROM lyrics_discovery_jobs WHERE music_id=8`,
		"mismatched music": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, 999, catalog_fingerprint, policy_version, 'no_candidates', 0, '{}', 3
			FROM lyrics_discovery_jobs WHERE music_id=8`,
		"missing job": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			VALUES (999, 8, '` + strings.Repeat("a", 64) + `', 'shadow-v1', 'no_candidates', 0, '{}', 3)`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := migrated.Exec(statement); err == nil {
				t.Fatalf("v11 accepted %s", name)
			}
		})
	}
}

func TestV11MigrationFailureRollsBackBothQueueColumnsAndShadowTable(t *testing.T) {
	path := legacyFixtureCopy(t, "v11-rollback.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:10]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	migrationBeforeCommitHook = func(version int) error {
		if version == 11 {
			return errors.New("injected v11 failure")
		}
		return nil
	}
	t.Cleanup(func() { migrationBeforeCommitHook = nil })
	if _, err := database.pendingMigrations(); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[10:]); err == nil || !strings.Contains(err.Error(), "injected v11 failure") {
		raw.Close()
		t.Fatalf("v11 migration error=%v", err)
	}
	migrationBeforeCommitHook = nil
	var tableCount, columnCount int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lyrics_discovery_shadow_results'`).Scan(&tableCount); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('lyrics_discovery_jobs') WHERE name IN ('catalog_fingerprint','policy_version')`).Scan(&columnCount); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if tableCount != 0 || columnCount != 0 {
		raw.Close()
		t.Fatalf("failed v11 left table=%d columns=%d", tableCount, columnCount)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version int
	if err := reopened.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("v11 retry plus pending v12 version=%d err=%v", version, err)
	}
}

func TestV12MigrationRejectsNonIntegerDiscoveryValuesOnInsertAndUpdate(t *testing.T) {
	path := legacyFixtureCopy(t, "v12-lyrics-discovery-integer-types.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	fingerprint := strings.Repeat("a", 64)
	key := strings.Repeat("b", 64)
	if _, err := database.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
		VALUES (?, 'discover', 'queued', 7, ?, 'shadow-v1', 0, 3, 1, 1, 1, 1)`, key, fingerprint); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := database.QueryRow(`SELECT job_id FROM lyrics_discovery_jobs WHERE idempotency_key=?`, key).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE lyrics_discovery_jobs
		SET state='leased', attempts=1, lease_owner='worker', lease_expires_at=100, version=2
		WHERE job_id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO lyrics_discovery_shadow_results
		(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
		VALUES (?, 7, ?, 'shadow-v1', 'ambiguous', 2, '{}', 2)`, jobID, fingerprint); err != nil {
		t.Fatal(err)
	}

	for name, statement := range map[string]string{
		"job insert music": `INSERT INTO lyrics_discovery_jobs
			(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
			VALUES ('` + strings.Repeat("c", 64) + `', 'discover', 'queued', 'not-a-music-id', '` + fingerprint + `', 'shadow-v1', 0, 3, 1, 1, 1, 1)`,
		"job insert next attempt": `INSERT INTO lyrics_discovery_jobs
			(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
			VALUES ('` + strings.Repeat("d", 64) + `', 'discover', 'queued', 8, '` + fingerprint + `', 'shadow-v1', 0, 3, 'not-a-time', 1, 1, 1)`,
		"job insert version": `INSERT INTO lyrics_discovery_jobs
			(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
			VALUES ('` + strings.Repeat("e", 64) + `', 'discover', 'queued', 9, '` + fingerprint + `', 'shadow-v1', 0, 3, 1, 1, 1, 'not-a-version')`,
		"job update": `UPDATE lyrics_discovery_jobs SET next_attempt_at='not-a-time' WHERE job_id=` + fmt.Sprint(jobID),
		"result insert count": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			VALUES (` + fmt.Sprint(jobID) + `, 7, '` + fingerprint + `', 'shadow-v1', 'ambiguous', 'not-a-count', '{}', 2)`,
		"result update": `UPDATE lyrics_discovery_shadow_results SET created_at='not-a-time' WHERE job_id=` + fmt.Sprint(jobID),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := database.Exec(statement); err == nil {
				t.Fatalf("v12 accepted %s", name)
			}
		})
	}
}

func TestV12MigrationRejectsExistingNonIntegerDiscoveryValuesAndRollsBack(t *testing.T) {
	for name, statement := range map[string]string{
		"job": `INSERT INTO lyrics_discovery_jobs
			(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
			VALUES ('` + strings.Repeat("c", 64) + `', 'discover', 'queued', 7, '` + strings.Repeat("a", 64) + `', 'shadow-v1', 0, 3, 1.5, 1, 1, 1)`,
		"result": `INSERT INTO lyrics_discovery_shadow_results
			(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at)
			SELECT job_id, music_id, catalog_fingerprint, policy_version, 'ambiguous', 2.5, '{}', 2
			FROM lyrics_discovery_jobs LIMIT 1`,
	} {
		t.Run(name, func(t *testing.T) {
			path := legacyFixtureCopy(t, "v12-existing-non-integer.db")
			raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
			if err != nil {
				t.Fatal(err)
			}
			database := &DB{DB: raw, path: path}
			if err := database.applyMigrations(migrations[:11]); err != nil {
				raw.Close()
				t.Fatal(err)
			}
			if name == "result" {
				if _, err := raw.Exec(`INSERT INTO lyrics_discovery_jobs
					(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
					VALUES (?, 'discover', 'queued', 7, ?, 'shadow-v1', 0, 3, 1, 1, 1, 1)`, strings.Repeat("b", 64), strings.Repeat("a", 64)); err != nil {
					raw.Close()
					t.Fatal(err)
				}
			}
			if _, err := raw.Exec(statement); err != nil {
				raw.Close()
				t.Fatalf("seed non-integer %s: %v", name, err)
			}
			if err := database.applyMigrations(migrations[11:]); err == nil || !strings.Contains(err.Error(), "non-integer numeric fields") {
				raw.Close()
				t.Fatalf("v12 existing %s error=%v", name, err)
			}
			var version, triggers int
			if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 11 {
				raw.Close()
				t.Fatalf("failed v12 version=%d err=%v", version, err)
			}
			if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'lyrics_discovery_%_integer_types_%'`).Scan(&triggers); err != nil || triggers != 0 {
				raw.Close()
				t.Fatalf("failed v12 triggers=%d err=%v", triggers, err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestV12MigrationRejectsExistingNonIntegerValuesThroughOpenAndPreservesBackup(t *testing.T) {
	path := legacyFixtureCopy(t, "v12-open-existing-non-integer.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:11]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_discovery_jobs
		(idempotency_key, kind, state, music_id, catalog_fingerprint, policy_version, attempts, max_attempts, next_attempt_at, created_at, updated_at, version)
		VALUES (?, 'discover', 'queued', 7, ?, 'shadow-v1', 0, 3, CAST('not-an-integer' AS TEXT), 1, 1, 1)`, strings.Repeat("d", 64), strings.Repeat("a", 64)); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	var storageClass string
	if err := raw.QueryRow(`SELECT typeof(next_attempt_at) FROM lyrics_discovery_jobs`).Scan(&storageClass); err != nil || storageClass != "text" {
		raw.Close()
		t.Fatalf("seed storage class=%q err=%v", storageClass, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "non-integer numeric fields") {
		t.Fatalf("Open accepted existing non-integer v11 data: %v", err)
	}
	backupPath := path + ".pre-migration-v12.bak"
	if err := verifySQLiteBackup(backupPath); err != nil {
		t.Fatalf("v12 pre-migration backup: %v", err)
	}
	for label, databasePath := range map[string]string{"database": path, "backup": backupPath} {
		check, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var version, triggers int
		if err := check.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 11 {
			check.Close()
			t.Fatalf("%s version=%d err=%v", label, version, err)
		}
		if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'lyrics_discovery_%_integer_types_%'`).Scan(&triggers); err != nil || triggers != 0 {
			check.Close()
			t.Fatalf("%s triggers=%d err=%v", label, triggers, err)
		}
		if err := check.QueryRow(`SELECT typeof(next_attempt_at) FROM lyrics_discovery_jobs`).Scan(&storageClass); err != nil || storageClass != "text" {
			check.Close()
			t.Fatalf("%s storage class=%q err=%v", label, storageClass, err)
		}
		if err := check.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestV12MigrationFailureRollsBackIntegerTypeTriggers(t *testing.T) {
	path := legacyFixtureCopy(t, "v12-rollback.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:11]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	migrationBeforeCommitHook = func(version int) error {
		if version == 12 {
			return errors.New("injected v12 failure")
		}
		return nil
	}
	t.Cleanup(func() { migrationBeforeCommitHook = nil })
	if err := database.applyMigrations(migrations[11:]); err == nil || !strings.Contains(err.Error(), "injected v12 failure") {
		raw.Close()
		t.Fatalf("v12 migration error=%v", err)
	}
	migrationBeforeCommitHook = nil
	var version, triggers int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 11 {
		raw.Close()
		t.Fatalf("failed v12 version=%d err=%v", version, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'lyrics_discovery_%_integer_types_%'`).Scan(&triggers); err != nil || triggers != 0 {
		raw.Close()
		t.Fatalf("failed v12 triggers=%d err=%v", triggers, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("retried v12 version=%d err=%v", version, err)
	}
}

func TestV12MigrationUpgradesV11AndCreatesIntegerTypeTriggers(t *testing.T) {
	path := legacyFixtureCopy(t, "v12-upgrade.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:11]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var version, triggers int
	if err := migrated.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
	if err := migrated.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'lyrics_discovery_%_integer_types_%'`).Scan(&triggers); err != nil || triggers != 4 {
		t.Fatalf("integer type triggers=%d err=%v", triggers, err)
	}
}

func TestV8MigrationCreatesDedicatedPreMigrationBackup(t *testing.T) {
	path := legacyFixtureCopy(t, "v8-backup.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:7]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	backupPath := path + ".pre-migration-v8.bak"
	if err := verifySQLiteBackup(backupPath); err != nil {
		t.Fatalf("v8 pre-migration backup: %v", err)
	}
	backup, err := sql.Open("sqlite", "file:"+backupPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var version int
	if err := backup.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 7 {
		t.Fatalf("v8 backup version=%d err=%v", version, err)
	}
	var scenarioTable int
	if err := backup.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='event_story_scenarios'`).Scan(&scenarioTable); err != nil || scenarioTable != 0 {
		t.Fatalf("v8 backup scenario table=%d err=%v", scenarioTable, err)
	}
}

func TestTitleIdentityMigrationMovesExistingLocalizations(t *testing.T) {
	path := legacyFixtureCopy(t, "title-identity.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:4]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	var oldID string
	if err := raw.QueryRow(`SELECT segment_id FROM event_story_segments WHERE kind='title'`).Scan(&oldID); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO event_story_segment_localizations
		(segment_id, locale, text, source, updated_at, updated_by, revision)
		VALUES (?, 'en-US', 'Existing English title', 'human', 1, 'editor', 2)`, oldID); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var newID, text string
	if err := migrated.QueryRow(`SELECT seg.segment_id, loc.text
		FROM event_story_segments seg JOIN event_story_segment_localizations loc ON loc.segment_id=seg.segment_id
		WHERE seg.kind='title' AND loc.locale='en-US'`).Scan(&newID, &text); err != nil {
		t.Fatal(err)
	}
	if newID != oldID+":-1" || text != "Existing English title" {
		t.Fatalf("migrated title id=%q text=%q old=%q", newID, text, oldID)
	}
}

func TestTalkIdentityMigrationKeepsFilteredFieldOpaqueAndPreservesLocalization(t *testing.T) {
	path := legacyFixtureCopy(t, "talk-identity.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:5]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	var oldID, oldHash string
	if err := raw.QueryRow(`SELECT segment_id, source_hash FROM event_story_segments WHERE kind='talk'`).Scan(&oldID, &oldHash); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO event_story_segment_localizations
		(segment_id, locale, text, source, updated_at, updated_by, revision)
		VALUES (?, 'en-US', 'Existing English talk', 'human', 1, 'editor', 2)`, oldID); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var newID, newHash, text string
	if err := migrated.QueryRow(`SELECT seg.segment_id, seg.source_hash, loc.text
		FROM event_story_segments seg JOIN event_story_segment_localizations loc ON loc.segment_id=seg.segment_id
		WHERE seg.kind='talk' AND loc.locale='en-US'`).Scan(&newID, &newHash, &text); err != nil {
		t.Fatal(err)
	}
	if newID != oldID+":legacy" || newHash != oldHash || text != "Existing English talk" {
		t.Fatalf("migrated talk id=%q hash=%q text=%q oldID=%q oldHash=%q", newID, newHash, text, oldID, oldHash)
	}
}

func TestAttributionAndTokenGenerationMigrationRequiresRepublish(t *testing.T) {
	path := legacyFixtureCopy(t, "attribution-token-version.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:6]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO catalog_music(music_id, title_ja, producer_metadata) VALUES (10, 'song', 'producer')`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO song_lyrics(music_id, revision, updated_at, source_hash) VALUES (10, 1, 1, 'hash')`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO song_lyrics_publications(music_id, revision, updated_at, payload_json)
		VALUES (10, 1, 1, '{"version":1,"musicId":10,"revision":1,"updatedAt":"1970-01-01T00:00:01Z","lines":[]}')`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var attribution string
	if err := migrated.QueryRow(`SELECT attribution FROM song_lyrics WHERE music_id=10`).Scan(&attribution); err != nil {
		t.Fatal(err)
	}
	var tokenVersion, publications int
	if err := migrated.QueryRow(`SELECT token_version FROM users LIMIT 1`).Scan(&tokenVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrated.QueryRow(`SELECT COUNT(*) FROM song_lyrics_publications`).Scan(&publications); err != nil {
		t.Fatal(err)
	}
	if attribution != "" || tokenVersion != 1 || publications != 0 {
		t.Fatalf("migration attribution=%q tokenVersion=%d publications=%d", attribution, tokenVersion, publications)
	}
}

func TestV26MigrationAddsIndependentNonNullLyricsCreditsWithoutRewritingLegacyAttribution(t *testing.T) {
	path := legacyFixtureCopy(t, "v26-lyrics-credits.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:25]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO catalog_music(music_id,title_ja,producer_metadata) VALUES (10,'legacy song','producer')`); err != nil {
		t.Fatal(err)
	}
	const legacyAttribution = "Legacy translator attribution"
	if _, err := raw.Exec(`INSERT INTO song_lyrics(music_id,revision,updated_at,updated_by,attribution,source_hash)
		VALUES (10,1,1,'legacy',?,?)`, legacyAttribution, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[25:26]); err != nil {
		t.Fatal(err)
	}

	var attribution, translation, proofreading string
	if err := raw.QueryRow(`SELECT attribution,translation_credit,proofreading_credit FROM song_lyrics WHERE music_id=10`).
		Scan(&attribution, &translation, &proofreading); err != nil {
		t.Fatal(err)
	}
	if attribution != legacyAttribution || translation != "" || proofreading != "" {
		t.Fatalf("migrated credits attribution=%q translation=%q proofreading=%q", attribution, translation, proofreading)
	}
	for _, column := range []string{"translation_credit", "proofreading_credit"} {
		var notNull int
		var defaultValue sql.NullString
		if err := raw.QueryRow(`SELECT "notnull",dflt_value FROM pragma_table_info('song_lyrics') WHERE name=?`, column).
			Scan(&notNull, &defaultValue); err != nil {
			t.Fatal(err)
		}
		if notNull != 1 || !defaultValue.Valid || defaultValue.String != "''" {
			t.Fatalf("column %s notnull=%d default=%q", column, notNull, defaultValue.String)
		}
	}
	if _, err := raw.Exec(`UPDATE song_lyrics SET translation_credit='Same Person',proofreading_credit='Same Person' WHERE music_id=10`); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT translation_credit,proofreading_credit FROM song_lyrics WHERE music_id=10`).
		Scan(&translation, &proofreading); err != nil {
		t.Fatal(err)
	}
	if translation != "Same Person" || proofreading != "Same Person" {
		t.Fatalf("independent credits translation=%q proofreading=%q", translation, proofreading)
	}
	if _, err := raw.Exec(`UPDATE song_lyrics SET translation_credit=NULL WHERE music_id=10`); err == nil {
		t.Fatal("translation_credit accepted NULL")
	}
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 26 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
}
