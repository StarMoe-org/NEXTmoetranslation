package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/singleinstance"
)

func TestValidateOptionsRequiresExplicitOfflineBackupContract(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "")
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := options{
		rootPath: filepath.Join(directory, "root.json"), manifestPath: filepath.Join(directory, "manifest.json"),
		evidenceReceiptPath: filepath.Join(directory, "evidence.json"), evidencePath: filepath.Join(directory, "pack"),
		databasePath: filepath.Join(directory, "database.sqlite"), backupPath: filepath.Join(directory, "backup.sqlite"),
		backupSHA256: strings.Repeat("a", 64), importReceiptPath: filepath.Join(directory, "receipt.json"), actor: "operator",
	}
	if err := validateOptions(opts); err == nil || !strings.Contains(err.Error(), "confirm-local-offline") {
		t.Fatalf("missing confirmation error=%v", err)
	}
	opts.confirmLocalOffline = true
	opts.backupSHA256 = strings.Repeat("A", 64)
	if err := validateOptions(opts); err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("uppercase backup digest error=%v", err)
	}
}

func TestRecoveryLogicalStateDigestExcludesReceiptAuditButIncludesRecoveryRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logical.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	before, err := sqliteLogicalStateDigest(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO audit_log(ts,user,action,detail) VALUES (1,'operator','lyrics.import_recovery.receipt','fixture')`); err != nil {
		t.Fatal(err)
	}
	afterAudit, err := sqliteLogicalStateDigest(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if afterAudit != before {
		t.Fatalf("receipt audit created a self-referential logical digest: before=%s after=%s", before, afterAudit)
	}
	if _, err := database.Exec(`INSERT INTO lyrics_recovery_source_evidence
		(provider,evidence_id,sha256,acquisition_id,envelope_sha256,kind,origin,page_id,revision_id,revision_timestamp,
		 mediawiki_sha1,page_title,canonical_revision_url,categories_json,canonical_request_url,fetched_at,raw_bytes,
		 raw_byte_count,raw_sha256,created_at) VALUES ('sekaipedia','logical-fixture',?, ?, ?, 'mediawiki_revision',
		 'https://www.sekaipedia.org',1,1,'2026-08-12T00:00:00Z',?,'fixture','https://www.sekaipedia.org/wiki/fixture?oldid=1','[]',
		 '','2026-08-12T00:00:00Z',X'01',1,?,1)`, strings.Repeat("a", 64),
		strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 40), strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	afterRecovery, err := sqliteLogicalStateDigest(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if afterRecovery == before {
		t.Fatal("recovery mutation did not change recovery logical-state digest")
	}
}

func TestProtectedStateDigestIgnoresAllowedTablesAndDetectsBusinessState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protected.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	beforeUnscoped, err := sqliteProtectedStateDigest(t.Context(), database, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO lyrics_recovery_source_evidence
		(provider,evidence_id,sha256,acquisition_id,envelope_sha256,kind,origin,page_id,revision_id,revision_timestamp,
		 mediawiki_sha1,page_title,canonical_revision_url,categories_json,canonical_request_url,fetched_at,raw_bytes,
		 raw_byte_count,raw_sha256,created_at) VALUES ('sekaipedia','evidence-fixture',?, ?, ?, 'mediawiki_revision',
		 'https://www.sekaipedia.org',1,1,'2026-08-12T00:00:00Z',?,'fixture','https://www.sekaipedia.org/wiki/fixture?oldid=1','[]',
		 '','2026-08-12T00:00:00Z',X'01',1,?,1)`, strings.Repeat("a", 64),
		strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 40), strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	afterUnscoped, err := sqliteProtectedStateDigest(t.Context(), database, 0)
	if err != nil {
		t.Fatal(err)
	}
	if afterUnscoped == beforeUnscoped {
		t.Fatal("unscoped recovery evidence mutation was hidden from the protected digest")
	}
	if _, err := database.Exec(`INSERT INTO settings(key,value) VALUES ('protected','changed')`); err != nil {
		t.Fatal(err)
	}
	afterProtected, err := sqliteProtectedStateDigest(t.Context(), database, 0)
	if err != nil {
		t.Fatal(err)
	}
	if afterProtected == afterUnscoped {
		t.Fatal("protected business-table mutation did not change protected digest")
	}
}

func TestProtectedStateDigestScopesV29PeerSideTranslationsByRecoveryBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protected-v29-peer.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	currentBatch := strings.Repeat("a", 64)
	unrelatedBatch := strings.Repeat("b", 64)
	if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (10,'current'),(11,'unrelated')`); err != nil {
		t.Fatal(err)
	}
	for _, document := range []struct {
		id      int
		musicID int
		batch   string
		sha     string
		key     string
	}{
		{id: 1, musicID: 10, batch: currentBatch, sha: strings.Repeat("c", 64), key: "current"},
		{id: 2, musicID: 11, batch: unrelatedBatch, sha: strings.Repeat("d", 64), key: "unrelated"},
	} {
		if _, err := database.Exec(`INSERT INTO song_lyrics_source_documents
			(document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
			VALUES (?,?,3,'','{}',?,?,1)`, document.id, document.musicID, document.sha, document.batch); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO song_lyrics_rendition_localizations
			(document_id,rendition_key,locale,updated_at,updated_by,revision) VALUES (?,?, 'zh-CN',1,'test',1)`,
			document.id, document.key); err != nil {
			t.Fatal(err)
		}
	}
	scope := recoveryProtectedScope{batchSHA256: currentBatch, evidenceJSON: "[]"}
	before, err := sqliteProtectedStateDigest(t.Context(), database, 0, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyrics_rendition_side_translation_lines
		(document_id,rendition_key,side,locale,position,text) VALUES (1,'current','game','zh-CN',0,'allowed')`); err != nil {
		t.Fatal(err)
	}
	afterCurrent, err := sqliteProtectedStateDigest(t.Context(), database, 0, scope)
	if err != nil {
		t.Fatal(err)
	}
	if afterCurrent != before {
		t.Fatalf("current recovery batch peer-side row changed protected digest: before=%s after=%s", before, afterCurrent)
	}
	if _, err := database.Exec(`INSERT INTO song_lyrics_rendition_side_translation_lines
		(document_id,rendition_key,side,locale,position,text) VALUES (2,'unrelated','game','zh-CN',0,'protected')`); err != nil {
		t.Fatal(err)
	}
	afterUnrelated, err := sqliteProtectedStateDigest(t.Context(), database, 0, scope)
	if err != nil {
		t.Fatal(err)
	}
	if afterUnrelated == afterCurrent {
		t.Fatal("unrelated recovery batch peer-side mutation was hidden from the protected digest")
	}
}

func TestReceiptReservationIsNoOverwriteAndDetectsPathSwap(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "receipt.json")
	reservation, err := reserveReceipt(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := reserveReceipt(path); err == nil {
		_ = second.finish()
		t.Fatal("second no-overwrite receipt reservation succeeded")
	}
	displaced := filepath.Join(directory, "displaced.json")
	if err := os.Rename(path, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reservation.verify("after swap"); err == nil || !strings.Contains(err.Error(), "path or inode changed") {
		t.Fatalf("receipt path swap error=%v", err)
	}
	reservation.commitAttempted = true
	if err := reservation.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteSidecarAndSingleInstanceLockFailClosed(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "database.sqlite")
	if err := os.WriteFile(path, []byte("not used"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectSQLiteSidecars(path, "database"); err == nil || !strings.Contains(err.Error(), "-wal") {
		t.Fatalf("WAL sidecar error=%v", err)
	}
	if err := os.Remove(path + "-wal"); err != nil {
		t.Fatal(err)
	}
	owner, err := singleinstance.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if second, err := singleinstance.Acquire(path); err == nil || !errors.Is(err, singleinstance.ErrAlreadyOwned) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second ownership acquisition error=%v", err)
	}
}

func TestPinnedSQLiteAnchorBindsInspectedInodeAndDetectsDatabasePathSwap(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "database.sqlite")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Checkpoint(context.Background()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := inspectExistingPrivateRegular(path, "database")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := openPinnedWritableFile(path, info, "database")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.close()
	anchor, err := createPinnedSQLiteAnchor(pinned)
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.close()
	displaced := filepath.Join(directory, "database-original.sqlite")
	if err := os.Rename(path, displaced); err != nil {
		t.Fatal(err)
	}
	replacement, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pinned.verifySamePath("after database swap", false); err == nil {
		t.Fatal("database path swap was not detected")
	}
	anchored, err := db.OpenOfflinePinned(anchor.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anchored.Exec(`INSERT INTO settings(key,value) VALUES ('anchor','pinned')`); err != nil {
		anchored.Close()
		t.Fatal(err)
	}
	if err := anchored.Checkpoint(t.Context()); err != nil {
		anchored.Close()
		t.Fatal(err)
	}
	if err := anchored.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := sql.Open("sqlite", "file:"+displaced+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	var value string
	if err := readOnly.QueryRow(`SELECT value FROM settings WHERE key='anchor'`).Scan(&value); err != nil || value != "pinned" {
		t.Fatalf("anchored write did not target inspected inode value=%q err=%v", value, err)
	}
}
