package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestV32Song682TranslationEditionsMigration(t *testing.T) {
	path := t.TempDir() + "/song682-migration-v32.db"
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applySchema(); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[:31]); err != nil {
		t.Fatal(err)
	}
	// Seed catalog_music for song 682
	if _, err := raw.Exec(`INSERT INTO catalog_music(music_id,title_ja,title_zh,title_en) VALUES (682,'あなたしか見えないの','眼中仅有你一人','Anata Shika Mienai no')`); err != nil {
		t.Fatal(err)
	}
	// Seed legacy song_lyrics and publication for song 682
	if _, err := raw.Exec(`INSERT INTO song_lyrics(music_id,revision,updated_at,translation_credit)
		VALUES (682,8,1724544000,'@雪莹ちゃん')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO song_lyrics_publications(music_id,revision,updated_at,payload_json)
		VALUES (682,8,1724544000,'{}')`); err != nil {
		t.Fatal(err)
	}

	// Apply migration 32
	if err := database.applyMigrations(migrations[31:32]); err != nil {
		t.Fatalf("apply migration 32 failed: %v", err)
	}

	// Check that legacy records are removed
	var legacyLyricsCount, publicationCount int
	if err := raw.QueryRow(`SELECT (SELECT COUNT(*) FROM song_lyrics WHERE music_id=682),
		(SELECT COUNT(*) FROM song_lyrics_publications WHERE music_id=682)`).Scan(&legacyLyricsCount, &publicationCount); err != nil {
		t.Fatal(err)
	}
	if legacyLyricsCount != 0 || publicationCount != 0 {
		t.Fatalf("legacy records not cleaned up: lyrics=%d pubs=%d", legacyLyricsCount, publicationCount)
	}

	// Check source document and editions
	var docCount, edCount, lineCount int
	if err := raw.QueryRow(`SELECT
		(SELECT COUNT(*) FROM song_lyrics_source_documents WHERE music_id=682),
		(SELECT COUNT(*) FROM song_lyrics_translation_editions WHERE document_id=(SELECT document_id FROM song_lyrics_source_documents WHERE music_id=682)),
		(SELECT COUNT(*) FROM song_lyrics_translation_edition_lines WHERE document_id=(SELECT document_id FROM song_lyrics_source_documents WHERE music_id=682))`,
	).Scan(&docCount, &edCount, &lineCount); err != nil {
		t.Fatal(err)
	}
	if docCount != 1 {
		t.Fatalf("source document count=%d want 1", docCount)
	}
	if edCount != 2 {
		t.Fatalf("translation edition count=%d want 2", edCount)
	}
	if lineCount != 66 {
		t.Fatalf("translation edition lines count=%d want 66 (33 x 2)", lineCount)
	}

	// Check runtime invariants
	if err := database.ensureRuntimeInvariants(context.Background()); err != nil {
		t.Fatalf("runtime invariants failed: %v", err)
	}
}

// song682SeededDocument returns the exact document the v32 seed inserts, so a
// test can stage the same import the migration has to tolerate.
func song682SeededDocument(t *testing.T) (string, string) {
	t.Helper()
	const documentSHA = "a70872e510d9c168d183a83eacdf7ac634e6b0702785925e940698fd8cdb1886"
	const prefix = "682,3,'','"
	start := strings.Index(migrationV32Song682TranslationEditionsSQL, prefix)
	end := strings.Index(migrationV32Song682TranslationEditionsSQL, "','"+documentSHA)
	if start < 0 || end <= start+len(prefix) {
		t.Fatal("v32 seed document literal not found")
	}
	return migrationV32Song682TranslationEditionsSQL[start+len(prefix) : end], documentSHA
}

func TestV32Song682MigrationKeepsAlreadyImportedDocument(t *testing.T) {
	path := t.TempDir() + "/song682-migration-v32-imported.db"
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applySchema(); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[:31]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO catalog_music(music_id,title_ja,title_zh,title_en) VALUES (682,'あなたしか見えないの','眼中仅有你一人','Anata Shika Mienai no')`); err != nil {
		t.Fatal(err)
	}
	documentJSON, documentSHA := song682SeededDocument(t)
	batchSHA := strings.Repeat("b", 64)
	if _, err := raw.Exec(`INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (682,3,'',?,?,?,1756080000)`, documentJSON, documentSHA, batchSHA); err != nil {
		t.Fatal(err)
	}

	if err := database.applyMigrations(migrations[31:34]); err != nil {
		t.Fatalf("apply migrations 32 through 34 over an imported song 682: %v", err)
	}

	var documents int
	var storedBatchSHA string
	var createdAt int64
	if err := raw.QueryRow(`SELECT COUNT(*) FROM song_lyrics_source_documents WHERE music_id=682 AND document_sha256=?`,
		documentSHA).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT manifest_batch_sha256,created_at FROM song_lyrics_source_documents WHERE music_id=682`).
		Scan(&storedBatchSHA, &createdAt); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || storedBatchSHA != batchSHA || createdAt != 1756080000 {
		t.Fatalf("imported document documents=%d batch=%q createdAt=%d", documents, storedBatchSHA, createdAt)
	}
	var artifacts, contributions, editions int
	if err := raw.QueryRow(`SELECT
		(SELECT COUNT(*) FROM song_lyrics_source_artifacts),
		(SELECT COUNT(*) FROM song_lyrics_component_contributions),
		(SELECT COUNT(*) FROM song_lyrics_translation_editions)`).Scan(&artifacts, &contributions, &editions); err != nil {
		t.Fatal(err)
	}
	if artifacts != 0 || contributions != 0 || editions != 0 {
		t.Fatalf("seed attached rows to the imported document artifacts=%d contributions=%d editions=%d",
			artifacts, contributions, editions)
	}
	var temporaryObjects int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_temp_master WHERE name LIKE 'song_682%'`).Scan(&temporaryObjects); err != nil {
		t.Fatal(err)
	}
	if temporaryObjects != 0 {
		t.Fatalf("migration left %d temporary objects behind", temporaryObjects)
	}
	if err := database.ensureRuntimeInvariants(context.Background()); err != nil {
		t.Fatalf("runtime invariants failed: %v", err)
	}
}
