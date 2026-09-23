package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/offline/internal/lyricsextractionplan"
)

func TestRunFiltersCatalogDeterministically(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, "source.db")
	writeTestCatalog(t, sourcePath)
	sourceBody, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceSHA := sha256Hex(sourceBody)
	target := testTargetMap(sourceSHA)
	targetBody, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	targetBody = append(targetBody, '\n')
	targetPath := filepath.Join(root, "target-map.json")
	if err := os.WriteFile(targetPath, targetBody, 0o600); err != nil {
		t.Fatal(err)
	}
	targetSHA := sha256Hex(targetBody)

	outputs := []string{filepath.Join(root, "filtered-one"), filepath.Join(root, "filtered-two")}
	for _, outputRoot := range outputs {
		var output strings.Builder
		err := run(context.Background(), []string{
			"-source-catalog", sourcePath,
			"-expected-source-catalog-sha256", sourceSHA,
			"-target-map", targetPath,
			"-expected-target-map-sha256", targetSHA,
			"-output", outputRoot,
		}, &output)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "records=2") {
			t.Fatalf("unexpected output %q", output.String())
		}
		info, err := os.Lstat(filepath.Join(outputRoot, catalogFileName))
		if err != nil || info.Mode().Perm() != 0o444 || linkCount(info) != 1 {
			t.Fatalf("catalog info=%v err=%v", info, err)
		}
		database, err := sql.Open("sqlite", "file:"+filepath.Join(outputRoot, catalogFileName)+"?mode=ro&immutable=1")
		if err != nil {
			t.Fatal(err)
		}
		var count, excluded int
		if err := database.QueryRow(`SELECT COUNT(*) FROM catalog_music`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if err := database.QueryRow(`SELECT COUNT(*) FROM catalog_music WHERE music_id=2`).Scan(&excluded); err != nil {
			t.Fatal(err)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		if count != 2 || excluded != 0 {
			t.Fatalf("count=%d excluded=%d", count, excluded)
		}
	}
	firstCatalog, _ := os.ReadFile(filepath.Join(outputs[0], catalogFileName))
	secondCatalog, _ := os.ReadFile(filepath.Join(outputs[1], catalogFileName))
	firstReceipt, _ := os.ReadFile(filepath.Join(outputs[0], receiptFileName))
	secondReceipt, _ := os.ReadFile(filepath.Join(outputs[1], receiptFileName))
	if string(firstCatalog) != string(secondCatalog) || string(firstReceipt) != string(secondReceipt) {
		t.Fatal("catalog filtering was not byte-deterministic")
	}
}

func TestValidateTargetMapRejectsUnauthorizedProvider(t *testing.T) {
	target := testTargetMap(strings.Repeat("a", 64))
	target.Mappings[0].Provider = "fandom"
	if err := validateTargetMap(target, target.CatalogSHA256); err == nil || !strings.Contains(err.Error(), "unauthorized provider") {
		t.Fatalf("expected provider rejection, got %v", err)
	}
}

func writeTestCatalog(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+path+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY,name TEXT NOT NULL,checksum TEXT NOT NULL,applied_at INTEGER NOT NULL)`,
		`CREATE TABLE catalog_music (music_id INTEGER PRIMARY KEY,title_ja TEXT NOT NULL,lyrics_catalog_fingerprint TEXT NOT NULL,lyrics_catalog_policy_version TEXT NOT NULL)`,
		`INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES (?, 'catalog', 'checksum', 0)`,
	}
	for index, statement := range statements {
		var execErr error
		if index == 2 {
			_, execErr = database.Exec(statement, lyricsextractionplan.CatalogSchemaVersion)
		} else {
			_, execErr = database.Exec(statement)
		}
		if execErr != nil {
			database.Close()
			t.Fatal(execErr)
		}
	}
	policy := lyricsextractionplan.CompiledEffectiveVersions().Policies.CatalogIdentity
	for musicID, title := range map[int]string{1: "One", 2: "Excluded", 3: "Three"} {
		if _, err := database.Exec(`INSERT INTO catalog_music VALUES (?,?,?,?)`,
			musicID, title, strings.Repeat(string(rune('a'+musicID)), 64), policy); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
}

func testTargetMap(catalogSHA string) targetMapReport {
	sekaipedia := json.RawMessage(`{"pageId":1}`)
	moegirl := json.RawMessage(`{"pageUrl":"https://zh.moegirl.org.cn/example"}`)
	mappings := []targetMapMapping{
		{MusicID: 1, CatalogJapaneseTitle: "One", Provider: "sekaipedia", Sekaipedia: &sekaipedia},
		{MusicID: 3, CatalogJapaneseTitle: "Three", Provider: "moegirl_public_exact", MoegirlPublicExact: &moegirl},
	}
	mappingBody, _ := json.Marshal(mappings)
	return targetMapReport{
		SchemaVersion: 1, CatalogSHA256: catalogSHA, CatalogCount: 3, Inputs: json.RawMessage(`{}`),
		MappingCount: 2, SekaipediaCount: 1, MoegirlPublicExactCount: 1,
		MusicIDSetEncoding: "decimal-newline-v1", MusicIDSetSHA256: decimalMusicIDsSHA256(mappings),
		MappingsSHA256: sha256Hex(mappingBody), ExcludedMusic: []catalogSong{{MusicID: 2, JapaneseTitle: "Excluded"}},
		Mappings: mappings,
	}
}
