package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	_ "moesekai/server/internal/sqlitedriver"
)

func TestRenditionV3MigrationPreservesV26LayoutBytesForeignKeysAndTriggers(t *testing.T) {
	path := t.TempDir() + "/v26-rendition-v3.db"
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applySchema(); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[:26]); err != nil {
		t.Fatal(err)
	}

	if got := tableColumns(t, raw, "lyrics_recovery_import_items"); got != "batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,association_music_ids_json,state,result_sha256,draft_sha256,document_sha256,availability_document_sha256,created_at" {
		t.Fatalf("v26 recovery item columns=%q", got)
	}
	if got := tableColumns(t, raw, "song_lyrics_component_contributions"); got != "document_id,component,rendition_key,contribution_sha256" {
		t.Fatalf("v26 component contribution columns=%q", got)
	}
	if got := tableColumns(t, raw, "lyrics_recovery_import_component_contributions"); got != "batch_sha256,music_id,component,rendition_key,contribution_sha256" {
		t.Fatalf("v26 recovery contribution columns=%q", got)
	}
	if got := tableColumns(t, raw, "song_lyrics_source_documents"); got != "document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at" {
		t.Fatalf("v26 source document columns=%q", got)
	}

	if _, err := raw.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (1,'曲一'),(2,'曲二'),(3,'曲三')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO song_lyrics(music_id,translation_credit,proofreading_credit) VALUES
		(1,'译者一','校对者一'),(2,'译者二','校对者二'),(3,'','')`); err != nil {
		t.Fatal(err)
	}

	documentV1 := v26SourceDocumentJSON(1, 1, "full-vocaloid-1")
	documentV2 := v26SourceDocumentJSON(2, 2, "full-vocaloid-2")
	insertV26SourceDocument(t, raw, 101, 1, 1, documentV1, "untagged_full_only", "full-vocaloid-1")
	insertV26SourceDocument(t, raw, 102, 2, 2, documentV2, "untagged_full_only", "full-vocaloid-2")

	batchSHA := hex64("a")
	if _, err := raw.Exec(`INSERT INTO lyrics_recovery_import_batches
		(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
		 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,
		 raw_byte_count,encoded_byte_count,actor,created_at)
		VALUES (?,?,?, ?,?,?,?,?,?,?,?,?,?,?,?, ?,?)`,
		batchSHA, 1, 2, "root-v26", hex64("b"), 1, hex64("c"),
		`{"total":1}`, hex64("d"), hex64("e"), hex64("f"), 0, 0, 0, 0, "migration-test", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_recovery_import_items
		(batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,association_music_ids_json,
		 state,result_sha256,draft_sha256,document_sha256,availability_document_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		batchSHA, 3, "曲三", hex64("1"), 3, "[]", "complete", hex64("2"), hex64("3"), hex64("4"), "", 8); err != nil {
		t.Fatal(err)
	}
	insertV26RecoveryArtifact(t, raw, batchSHA, 3, "full-vocaloid")
	if _, err := raw.Exec(`INSERT INTO lyrics_recovery_import_component_contributions
		(batch_sha256,music_id,component,rendition_key,contribution_sha256)
		VALUES (?,?,?,?,?)`, batchSHA, 3, "full_text", "full-vocaloid", hex64("5")); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE sqlite_sequence SET seq=900 WHERE name='song_lyrics_source_documents'`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO sqlite_sequence(name,seq)
		SELECT 'song_lyrics_availability_documents',700
		WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='song_lyrics_availability_documents')`); err != nil {
		t.Fatal(err)
	}

	if err := database.applyMigrations(migrations[26:27]); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{
		"song_lyrics_source_documents":       900,
		"song_lyrics_availability_documents": 700,
	} {
		var sequence int64
		if err := raw.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name=?`, table).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		if sequence != want {
			t.Fatalf("v27 %s sequence=%d want=%d", table, sequence, want)
		}
	}

	for _, expected := range []struct {
		musicID int
		version int
		body    string
	}{
		{musicID: 1, version: 1, body: documentV1},
		{musicID: 2, version: 2, body: documentV2},
	} {
		var version int
		var body string
		if err := raw.QueryRow(`SELECT schema_version,document_json FROM song_lyrics_source_documents WHERE music_id=?`, expected.musicID).
			Scan(&version, &body); err != nil {
			t.Fatal(err)
		}
		if version != expected.version || body != expected.body {
			t.Fatalf("music %d source document version=%d body=%q", expected.musicID, version, body)
		}
	}
	var translationCredit, proofreadingCredit string
	if err := raw.QueryRow(`SELECT translation_credit,proofreading_credit FROM song_lyrics WHERE music_id=1`).
		Scan(&translationCredit, &proofreadingCredit); err != nil {
		t.Fatal(err)
	}
	if translationCredit != "译者一" || proofreadingCredit != "校对者一" {
		t.Fatalf("v26 independent credits changed: translation=%q proofreading=%q", translationCredit, proofreadingCredit)
	}
	var storedDocumentID int64
	if err := raw.QueryRow(`SELECT document_id FROM song_lyrics_source_documents WHERE music_id=1`).Scan(&storedDocumentID); err != nil {
		t.Fatal(err)
	}
	if storedDocumentID != 101 {
		t.Fatalf("v26 document identity changed: got %d", storedDocumentID)
	}

	var state, storedRendition, contribution string
	if err := raw.QueryRow(`SELECT state FROM lyrics_recovery_import_items WHERE batch_sha256=? AND music_id=3`, batchSHA).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "complete" {
		t.Fatalf("recovery item state changed: %q", state)
	}
	if err := raw.QueryRow(`SELECT rendition_key,contribution_sha256 FROM lyrics_recovery_import_component_contributions WHERE batch_sha256=? AND music_id=3`, batchSHA).
		Scan(&storedRendition, &contribution); err != nil {
		t.Fatal(err)
	}
	if storedRendition != "full-vocaloid" || contribution != hex64("5") {
		t.Fatalf("recovery contribution changed: rendition=%q contribution=%q", storedRendition, contribution)
	}

	assertNoForeignKeyViolations(t, raw)
	assertRenditionV3ForeignKeys(t, raw)
	for _, trigger := range []string{
		"song_lyrics_source_documents_immutable_update",
		"song_lyrics_source_documents_immutable_delete",
		"song_lyrics_component_contributions_immutable_update",
		"song_lyrics_component_contributions_immutable_delete",
		"lyrics_recovery_import_items_immutable_update",
		"lyrics_recovery_import_items_immutable_delete",
		"lyrics_recovery_import_component_contributions_immutable_update",
		"lyrics_recovery_import_component_contributions_immutable_delete",
	} {
		var count int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`, trigger).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing migration trigger %q", trigger)
		}
	}

	if _, err := raw.Exec(`UPDATE song_lyrics_source_documents SET document_json='{"changed":true}' WHERE music_id=1`); err == nil {
		t.Fatal("source document update was not immutable")
	}
	if _, err := raw.Exec(`DELETE FROM song_lyrics_source_documents WHERE music_id=1`); err == nil {
		t.Fatal("source document delete was not immutable")
	}
	if _, err := raw.Exec(`UPDATE song_lyrics_component_contributions SET contribution_sha256=? WHERE document_id=101 AND component='full_text'`, hex64("6")); err == nil {
		t.Fatal("source contribution update was not immutable")
	}
	if _, err := raw.Exec(`DELETE FROM song_lyrics_component_contributions WHERE document_id=101 AND component='full_text'`); err == nil {
		t.Fatal("source contribution delete was not immutable")
	}
	if _, err := raw.Exec(`UPDATE lyrics_recovery_import_items SET japanese_title='改变' WHERE batch_sha256=? AND music_id=3`, batchSHA); err == nil {
		t.Fatal("recovery item update was not immutable")
	}
	if _, err := raw.Exec(`DELETE FROM lyrics_recovery_import_items WHERE batch_sha256=? AND music_id=3`, batchSHA); err == nil {
		t.Fatal("recovery item delete was not immutable")
	}
	if _, err := raw.Exec(`UPDATE lyrics_recovery_import_component_contributions SET contribution_sha256=? WHERE batch_sha256=? AND music_id=3 AND component='full_text'`, hex64("7"), batchSHA); err == nil {
		t.Fatal("recovery contribution update was not immutable")
	}
	if _, err := raw.Exec(`DELETE FROM lyrics_recovery_import_component_contributions WHERE batch_sha256=? AND music_id=3 AND component='full_text'`, batchSHA); err == nil {
		t.Fatal("recovery contribution delete was not immutable")
	}
}

func tableColumns(t *testing.T, database *sql.DB, table string) string {
	t.Helper()
	rows, err := database.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(names, ",")
}

func insertV26SourceDocument(t *testing.T, database *sql.DB, documentID, musicID, version int, body, reason, renditionKey string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO song_lyrics_source_documents
		(document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?)`, documentID, musicID, version, reason, body, hex64(string(rune('a'+version))), hex64(string(rune('c'+version))), 9+int64(version)); err != nil {
		t.Fatal(err)
	}
	identityJSON := v26FixedIdentityJSON(musicID, renditionKey)
	identityDigest := sha256.Sum256([]byte(identityJSON))
	refsJSON := `[{"evidenceId":"fixed","sha256":"` + hex64("d") + `"}]`
	canonicalURL := fmt.Sprintf("https://vocaloid.fandom.com/wiki/Test?oldid=%d", 200+musicID)
	if _, err := database.Exec(`INSERT INTO song_lyrics_source_artifacts
		(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
		 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
		 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		documentID, "vocaloid_fandom", renditionKey, "https://vocaloid.fandom.com", 100+musicID, 200+musicID, "",
		strings.Repeat("a", 40), "test source", canonicalURL, "2026-07-30T12:34:57.123Z",
		`["Lyrics"]`, "Lyrics", renditionKey, "untagged_full_only", refsJSON, identityJSON,
		hex.EncodeToString(identityDigest[:]), 1, hex64("f"), hex64("0")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyrics_component_contributions
		(document_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?)`, documentID, "full_text", renditionKey, hex64("1")); err != nil {
		t.Fatal(err)
	}
}

func v26SourceDocumentJSON(version, musicID int, renditionKey string) string {
	return fmt.Sprintf(`{"schemaVersion":%d,"preserved":"v%d-document-bytes","fixedIdentities":[%s]}`,
		version, version, v26FixedIdentityJSON(musicID, renditionKey))
}

func v26FixedIdentityJSON(musicID int, renditionKey string) string {
	return fmt.Sprintf(`{"provider":"vocaloid_fandom","origin":"https://vocaloid.fandom.com","pageId":%d,"revisionId":%d,"sha1":"%s","title":"test source","canonicalUrl":"https://vocaloid.fandom.com/wiki/Test?oldid=%d","fetchedAt":"2026-07-30T12:34:57.123Z","categories":["Lyrics"],"section":"Lyrics","renditionKey":%q,"compositionRenditionKey":%q,"versionReason":"untagged_full_only","indexEvidenceRefs":[{"evidenceId":"fixed","sha256":"%s"}]}`,
		100+musicID, 200+musicID, strings.Repeat("a", 40), 200+musicID, renditionKey, renditionKey, hex64("d"))
}

func insertV26RecoveryArtifact(t *testing.T, database *sql.DB, batchSHA string, musicID int, renditionKey string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO lyrics_recovery_import_artifacts
		(batch_sha256,music_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,
		 page_title,canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
		 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,
		 artifact_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		batchSHA, musicID, "vocaloid_fandom", renditionKey, "https://vocaloid.fandom.com", 301, 401, "", strings.Repeat("b", 40),
		"recovery source", "https://vocaloid.fandom.com/wiki/Recovery?oldid=401", "2026-07-30T12:34:57.123Z", `[]`, "Lyrics", "", "untagged_full_only",
		`[{"evidenceId":"fixed","sha256":"`+hex64("d")+`"}]`, `{}`, hex64("6"), 1, hex64("7"), hex64("8"), 10); err != nil {
		t.Fatal(err)
	}
}

func assertNoForeignKeyViolations(t *testing.T, database *sql.DB) {
	t.Helper()
	rows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, rowid, parent, fk any
		if err := rows.Scan(&table, &rowid, &parent, &fk); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("foreign key violation table=%v rowid=%v parent=%v fk=%v", table, rowid, parent, fk)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func assertRenditionV3ForeignKeys(t *testing.T, database *sql.DB) {
	t.Helper()
	rows, err := database.Query(`PRAGMA foreign_key_list(song_lyrics_component_contributions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		found[table+"/"+from+"/"+to+"/"+onDelete] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"song_lyrics_source_documents/document_id/document_id/CASCADE",
		"song_lyrics_source_artifacts/document_id/document_id/CASCADE",
		"song_lyrics_source_artifacts/rendition_key/rendition_key/CASCADE",
	} {
		if !found[expected] {
			t.Fatalf("missing v27 component foreign key %q; found=%v", expected, found)
		}
	}
}

func hex64(value string) string {
	if len(value) == 1 {
		return strings.Repeat(value, 64)
	}
	return fmt.Sprintf("%064s", value)
}
