package lyricsrecovery

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsextractionplan"
)

const fixtureCatalogPathEnv = "MOESEKAI_RECOVERY_TEST_CATALOG"

func TestMain(m *testing.M) {
	os.Exit(runLyricsRecoveryTestMain(m))
}

func runLyricsRecoveryTestMain(m *testing.M) int {
	root, err := os.MkdirTemp("", "lyrics-recovery-package-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create lyrics-recovery package test root: %v\n", err)
		return 1
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || canonicalRoot != root {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "canonicalize lyrics-recovery package test root: path=%q resolved=%q err=%v\n", root, canonicalRoot, err)
		return 1
	}
	fixtureCatalogPath, fixtureCatalogBindingValue, err = prepareLyricsRecoveryFixtureCatalog(canonicalRoot)
	if err != nil {
		_ = os.RemoveAll(canonicalRoot)
		fmt.Fprintf(os.Stderr, "prepare lyrics-recovery package test catalog: %v\n", err)
		return 1
	}
	code := m.Run()
	if err := os.RemoveAll(canonicalRoot); err != nil {
		fmt.Fprintf(os.Stderr, "remove lyrics-recovery package test root: %v\n", err)
		code = 1
	}
	return code
}

func prepareLyricsRecoveryFixtureCatalog(root string) (string, lyricsextractionplan.RecoveryCatalogBinding, error) {
	sourcePath := os.Getenv(fixtureCatalogPathEnv)
	explicitSource := sourcePath != ""
	if sourcePath == "" {
		sourcePath = fixtureCatalogDefaultPath
	}
	if body, err := os.ReadFile(sourcePath); err == nil {
		path := filepath.Join(root, filepath.Base(fixtureCatalogDefaultPath))
		if err := os.WriteFile(path, body, 0o444); err != nil {
			return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
		}
		if err := os.Chmod(path, 0o444); err != nil {
			return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
		}
		return path, lyricsextractionplan.RecoveryCatalogBinding{
			Path: path, SizeBytes: 1_150_976,
			SourceSHA256:          "58626dcd03a8bc06ffa1e1c8fba3cfa6dea0560fb471abd802829b4a7d6dd7f4",
			SchemaVersion:         lyricsextractionplan.CatalogSchemaVersion,
			RuntimeSchemaVersion:  lyricsextractionplan.MaximumCatalogRuntimeSchema,
			RecordCount:           704,
			IdentityPolicyVersion: lyricsextractionplan.CompiledEffectiveVersions().Policies.CatalogIdentity,
			IdentitySHA256:        "a17efa8a7c5e6c533d2502f01fccd7c5ddf9cd68bb28a489b7f7f6552e127fe2",
			MusicIDsSHA256:        "510da78c96ff21ac6f200dbfc3054be326c081d3fd0876d12ae3557d49188fa1",
		}, nil
	} else if explicitSource {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, fmt.Errorf("read %s: %w", fixtureCatalogPathEnv, err)
	}
	return writeSyntheticLyricsRecoveryFixtureCatalog(root)
}

func writeSyntheticLyricsRecoveryFixtureCatalog(root string) (string, lyricsextractionplan.RecoveryCatalogBinding, error) {
	path := filepath.Join(root, "synthetic-catalog-v18-704.db")
	database, err := sql.Open("sqlite", "file:"+path+"?mode=rwc")
	if err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	database.SetMaxOpenConns(1)
	fail := func(cause error) (string, lyricsextractionplan.RecoveryCatalogBinding, error) {
		_ = database.Close()
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, cause
	}
	if _, err := database.Exec(`PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL;
		CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY);
		CREATE TABLE catalog_music(
			music_id INTEGER PRIMARY KEY,
			title_ja TEXT NOT NULL,
			producer_metadata TEXT NOT NULL,
			lyricist TEXT NOT NULL,
			composer TEXT NOT NULL,
			arranger TEXT NOT NULL,
			vocal_signals_json TEXT NOT NULL,
			lyrics_catalog_fingerprint TEXT NOT NULL,
			lyrics_catalog_policy_version TEXT NOT NULL
		)`); err != nil {
		return fail(err)
	}
	transaction, err := database.Begin()
	if err != nil {
		return fail(err)
	}
	for version := 1; version <= lyricsextractionplan.CatalogSchemaVersion; version++ {
		if _, err := transaction.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, version); err != nil {
			_ = transaction.Rollback()
			return fail(err)
		}
	}
	policy := lyricsextractionplan.CompiledEffectiveVersions().Policies.CatalogIdentity
	musicIDs := make([]int, 704)
	identityDigest := sha256.New()
	_, _ = identityDigest.Write([]byte("moesekai-lyrics-recovery-catalog-fingerprints-v1\x00"))
	var encoded [8]byte
	for index := range musicIDs {
		musicID := index + 1
		musicIDs[index] = musicID
		title := fmt.Sprintf("試験曲%03d", musicID)
		producer := "試験制作者"
		lyricist := "試験作詞"
		composer := "試験作曲"
		arranger := "試験編曲"
		vocalsJSON := "[]"
		switch musicID {
		case 2:
			title = "ロキ"
			producer = "みきとP | みきとP | みきとP"
			lyricist, composer, arranger = "みきとP", "みきとP", "みきとP"
			vocalsJSON = `[{"vocalType":"sekai"}]`
		case 235:
			title = "Journey"
			producer = "DECO*27 | DECO*27 | Rockwell"
			lyricist, composer, arranger = "DECO*27", "DECO*27", "Rockwell"
			vocalsJSON = `[{"vocalType":"sekai"}]`
		}
		fingerprintBytes := sha256.Sum256([]byte(fmt.Sprintf("synthetic-recovery-catalog-v1:%d", musicID)))
		fingerprint := hex.EncodeToString(fingerprintBytes[:])
		if _, err := transaction.Exec(`INSERT INTO catalog_music
			(music_id,title_ja,producer_metadata,lyricist,composer,arranger,vocal_signals_json,
			 lyrics_catalog_fingerprint,lyrics_catalog_policy_version) VALUES (?,?,?,?,?,?,?,?,?)`,
			musicID, title, producer, lyricist, composer, arranger, vocalsJSON, fingerprint, policy); err != nil {
			_ = transaction.Rollback()
			return fail(err)
		}
		binary.BigEndian.PutUint64(encoded[:], uint64(musicID))
		_, _ = identityDigest.Write(encoded[:])
		_, _ = identityDigest.Write(fingerprintBytes[:])
	}
	if err := transaction.Commit(); err != nil {
		return fail(err)
	}
	if err := database.Close(); err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	if err := os.Chmod(path, 0o444); err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			return "", lyricsextractionplan.RecoveryCatalogBinding{}, fmt.Errorf("synthetic recovery catalog retained %s sidecar", suffix)
		}
	}
	sourceDigest := sha256.Sum256(body)
	musicIDsSHA256, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
	if err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	return path, lyricsextractionplan.RecoveryCatalogBinding{
		Path: path, SizeBytes: int64(len(body)), SourceSHA256: hex.EncodeToString(sourceDigest[:]),
		SchemaVersion:        lyricsextractionplan.CatalogSchemaVersion,
		RuntimeSchemaVersion: lyricsextractionplan.MaximumCatalogRuntimeSchema,
		RecordCount:          704, IdentityPolicyVersion: policy,
		IdentitySHA256: hex.EncodeToString(identityDigest.Sum(nil)), MusicIDsSHA256: musicIDsSHA256,
	}, nil
}
