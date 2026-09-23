package lyricsrecovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/offline/internal/lyricsextractionplan"
)

type catalogTestRecord struct {
	musicID     int
	title       string
	lyricist    string
	lyricistSet bool
	vocalsJSON  string
}

type catalogTestFileInfo struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (info catalogTestFileInfo) Sys() any { return info.stat }

func TestCatalogPinsImmutableDescriptorAndPreservesExactBytes(t *testing.T) {
	path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "original"}})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var dataSourceName string
	catalog, verification, err := openCatalogAgainstPlanWithOpeners(t.Context(), path, binding, os.Open,
		func(driverName, sourceName string) (*sql.DB, error) {
			dataSourceName = sourceName
			return sql.Open(driverName, sourceName)
		})
	if err != nil {
		t.Fatal(err)
	}
	if verification.SizeBytes != binding.SizeBytes || verification.SourceSHA256 != binding.SourceSHA256 ||
		verification.SchemaVersion != binding.SchemaVersion || verification.RecordCount != binding.RecordCount ||
		verification.IdentitySHA256 != binding.IdentitySHA256 || verification.MusicIDsSHA256 != binding.MusicIDsSHA256 {
		t.Fatalf("catalog verification=%+v binding=%+v", verification, binding)
	}
	parsed, err := url.Parse(dataSourceName)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Scheme != "file" || !strings.HasPrefix(parsed.Path, "/dev/fd/") ||
		query.Get("immutable") != "1" || query.Get("mode") != "ro" ||
		!catalogTestContains(query["_pragma"], "query_only(1)") {
		t.Fatalf("recovery catalog descriptor URI=%q", dataSourceName)
	}
	identity, err := catalog.MusicIdentity(t.Context(), 2)
	if err != nil || identity.JapaneseTitle != "original" {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	if _, err := catalog.connection.ExecContext(t.Context(), `UPDATE catalog_music SET title_ja='mutated' WHERE music_id=2`); err == nil {
		t.Fatal("query-only immutable recovery catalog accepted a write")
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || catalogTestSHA256(after) != binding.SourceSHA256 {
		t.Fatal("recovery catalog bytes changed")
	}
	for _, suffix := range catalogSQLiteSidecarSuffixes {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("SQLite sidecar %s exists after immutable reads: %v", suffix, err)
		}
	}
}

func TestCatalogMusicIdentityRequiresAbsentLyricistForInstrumental(t *testing.T) {
	const vocals = `[{"vocalId":1264,"vocalType":"instrumental","caption":"Inst.ver."}]`
	for _, test := range []struct {
		name         string
		lyricist     string
		instrumental bool
	}{
		{name: "pure instrumental", lyricist: "", instrumental: true},
		{name: "lyrics-bearing instrumental asset", lyricist: "LindaAI-CUE(BNSI)", instrumental: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{
				musicID: 707, title: "さいたま2000", lyricist: test.lyricist, lyricistSet: true, vocalsJSON: vocals,
			}})
			catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, binding)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = catalog.Close() }()
			identity, err := catalog.MusicIdentity(t.Context(), 707)
			if err != nil {
				t.Fatal(err)
			}
			if identity.Instrumental != test.instrumental || identity.Lyricist != test.lyricist {
				t.Fatalf("identity=%+v want instrumental=%t lyricist=%q", identity, test.instrumental, test.lyricist)
			}
		})
	}
}

func TestCatalogDescriptorCleanupIsScopedOnFailureAndClose(t *testing.T) {
	for _, test := range []struct {
		name     string
		openFail bool
	}{
		{name: "open failure", openFail: true},
		{name: "successful close"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "cleanup"}})
			descriptorPath := ""
			catalog, _, err := openCatalogAgainstPlanWithOpeners(t.Context(), path, binding, os.Open,
				func(driverName, sourceName string) (*sql.DB, error) {
					parsed, parseErr := url.Parse(sourceName)
					if parseErr != nil {
						return nil, parseErr
					}
					descriptorPath = parsed.Path
					if test.openFail {
						return nil, errors.New("injected SQLite open failure")
					}
					return sql.Open(driverName, sourceName)
				})
			if test.openFail {
				if err == nil || catalog != nil {
					t.Fatalf("injected open failure catalog=%v err=%v", catalog, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := catalog.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if descriptorPath == "" {
				t.Fatal("SQLite opener did not receive a descriptor path")
			}
			if descriptor, err := os.Open(descriptorPath); err == nil {
				_ = descriptor.Close()
				t.Fatalf("scoped recovery catalog descriptor remained open at %s", descriptorPath)
			}
		})
	}
}

func TestCatalogCloseRevalidatesOriginalPathAndStillCleansDescriptor(t *testing.T) {
	path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "close-original"}})
	replacement, _ := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "close-substitute"}})
	catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, binding)
	if err != nil {
		t.Fatal(err)
	}
	descriptorPath := catalog.descriptor
	if err := os.Rename(path, path+".pinned"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err == nil || !strings.Contains(err.Error(), "path, inode, or size changed") {
		t.Fatalf("close path replacement error=%v", err)
	}
	if descriptor, err := os.Open(descriptorPath); err == nil {
		_ = descriptor.Close()
		t.Fatalf("failed close left recovery catalog descriptor open at %s", descriptorPath)
	}
}

func TestCatalogRejectsExactPlanBindingMismatch(t *testing.T) {
	path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{
		{musicID: 2, title: "first"},
		{musicID: 235, title: "second"},
	})
	tests := []struct {
		name   string
		mutate func(*lyricsextractionplan.RecoveryCatalogBinding)
	}{
		{name: "size", mutate: func(value *lyricsextractionplan.RecoveryCatalogBinding) { value.SizeBytes++ }},
		{name: "source SHA", mutate: func(value *lyricsextractionplan.RecoveryCatalogBinding) { value.SourceSHA256 = strings.Repeat("0", 64) }},
		{name: "schema", mutate: func(value *lyricsextractionplan.RecoveryCatalogBinding) { value.SchemaVersion++ }},
		{name: "runtime schema ceiling", mutate: func(value *lyricsextractionplan.RecoveryCatalogBinding) {
			value.RuntimeSchemaVersion = value.SchemaVersion - 1
		}},
		{name: "record count", mutate: func(value *lyricsextractionplan.RecoveryCatalogBinding) { value.RecordCount++ }},
		{name: "identity policy", mutate: func(value *lyricsextractionplan.RecoveryCatalogBinding) { value.IdentityPolicyVersion += "-other" }},
		{name: "identity SHA", mutate: func(value *lyricsextractionplan.RecoveryCatalogBinding) {
			value.IdentitySHA256 = strings.Repeat("0", 64)
		}},
		{name: "music IDs SHA", mutate: func(value *lyricsextractionplan.RecoveryCatalogBinding) {
			value.MusicIDsSHA256 = strings.Repeat("0", 64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mismatch := binding
			test.mutate(&mismatch)
			catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, mismatch)
			if catalog != nil {
				_ = catalog.Close()
			}
			if err == nil {
				t.Fatal("mismatched immutable plan binding was accepted")
			}
		})
	}
}

func TestCatalogRejectsOwnerModeLinkAndAliasPolicyViolations(t *testing.T) {
	t.Run("mode", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o400, 0o440, 0o644, 0o446} {
			t.Run(mode.String(), func(t *testing.T) {
				path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "mode"}})
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				if catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, binding); err == nil {
					_ = catalog.Close()
					t.Fatalf("catalog mode %#o was accepted", mode)
				}
			})
		}
	})

	t.Run("owner metadata", func(t *testing.T) {
		path, _ := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "owner"}})
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("platform does not expose syscall.Stat_t")
		}
		wrong := *stat
		wrong.Uid++
		if err := validateCatalogFileInfo(catalogTestFileInfo{FileInfo: info, stat: &wrong}); err == nil {
			t.Fatal("non-effective-UID catalog metadata was accepted")
		}
	})

	t.Run("existing hardlink", func(t *testing.T) {
		path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "hardlink"}})
		if err := os.Link(path, path+".alias"); err != nil {
			t.Fatal(err)
		}
		if catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, binding); err == nil {
			_ = catalog.Close()
			t.Fatal("hard-linked recovery catalog was accepted")
		}
	})

	t.Run("hardlink created while open", func(t *testing.T) {
		path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "late-hardlink"}})
		catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, binding)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = catalog.Close() }()
		if err := os.Link(path, path+".late-alias"); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.MusicIdentity(t.Context(), 2); err == nil || !strings.Contains(err.Error(), "one link") {
			t.Fatalf("late hardlink query error=%v", err)
		}
	})

	t.Run("direct symlink", func(t *testing.T) {
		path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "symlink"}})
		link := filepath.Join(catalogTestResolvedTempDir(t), "catalog-link.db")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		binding.Path = link
		if catalog, _, err := OpenCatalogAgainstPlan(t.Context(), link, binding); err == nil {
			_ = catalog.Close()
			t.Fatal("symlink recovery catalog was accepted")
		}
	})

	t.Run("ancestor symlink alias", func(t *testing.T) {
		path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "ancestor-alias"}})
		aliasRoot := filepath.Join(catalogTestResolvedTempDir(t), "alias-root")
		if err := os.Symlink(filepath.Dir(path), aliasRoot); err != nil {
			t.Fatal(err)
		}
		aliasPath := filepath.Join(aliasRoot, filepath.Base(path))
		binding.Path = aliasPath
		if catalog, _, err := OpenCatalogAgainstPlan(t.Context(), aliasPath, binding); err == nil {
			_ = catalog.Close()
			t.Fatal("ancestor symlink alias was accepted")
		}
	})
}

func TestCatalogRejectsEverySQLiteSidecarBeforeOpenAndDuringUse(t *testing.T) {
	for _, suffix := range catalogSQLiteSidecarSuffixes {
		t.Run("before open "+strings.TrimPrefix(suffix, "-"), func(t *testing.T) {
			path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "sidecar"}})
			if err := os.WriteFile(path+suffix, []byte("sidecar"), 0o600); err != nil {
				t.Fatal(err)
			}
			if catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, binding); err == nil || !strings.Contains(err.Error(), suffix) {
				if catalog != nil {
					_ = catalog.Close()
				}
				t.Fatalf("sidecar %s error=%v", suffix, err)
			}
		})

		t.Run("during use "+strings.TrimPrefix(suffix, "-"), func(t *testing.T) {
			path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "late-sidecar"}})
			catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, binding)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = catalog.Close() }()
			catalog.testHookAfterQueryValidation = func() error {
				return os.WriteFile(path+suffix, []byte("late sidecar"), 0o600)
			}
			if _, err := catalog.MusicIdentity(t.Context(), 2); err == nil || !strings.Contains(err.Error(), suffix) {
				t.Fatalf("late sidecar %s error=%v", suffix, err)
			}
		})
	}
}

func TestCatalogPinnedDescriptorCannotBeReopenedThroughSubstitutedPath(t *testing.T) {
	path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "pinned-descriptor"}})
	pinned, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	replacement, _ := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "substituted-path"}})
	moved := path + ".original"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	catalog, _, err := OpenCatalogAgainstPinnedFile(t.Context(), path, binding, pinned)
	if catalog != nil {
		_ = catalog.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "inode changed") {
		t.Fatalf("substituted path with pinned descriptor error=%v", err)
	}
	info, statErr := pinned.Stat()
	if statErr != nil || info.Size() != binding.SizeBytes {
		t.Fatalf("caller-owned pinned descriptor was not retained: info=%v err=%v", info, statErr)
	}
}

func TestCatalogRejectsOpenedInodeMismatchAndPathSwaps(t *testing.T) {
	t.Run("opener returns another inode", func(t *testing.T) {
		path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "inspected"}})
		replacement, _ := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "other-inode"}})
		catalog, _, err := openCatalogAgainstPlanWithOpeners(t.Context(), path, binding,
			func(string) (*os.File, error) { return os.Open(replacement) }, sql.Open)
		if catalog != nil {
			_ = catalog.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "inode changed") {
			t.Fatalf("inode mismatch error=%v", err)
		}
	})

	t.Run("swap before descriptor open", func(t *testing.T) {
		path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "inspected"}})
		replacement, _ := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "substituted"}})
		moved := path + ".inspected"
		catalog, _, err := openCatalogAgainstPlanWithOpeners(t.Context(), path, binding, func(openPath string) (*os.File, error) {
			if err := os.Rename(openPath, moved); err != nil {
				return nil, err
			}
			if err := os.Rename(replacement, openPath); err != nil {
				return nil, err
			}
			return os.Open(openPath)
		}, sql.Open)
		if catalog != nil {
			_ = catalog.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "changed while being opened") {
			t.Fatalf("pre-descriptor swap error=%v", err)
		}
	})

	t.Run("swap before SQLite opens descriptor", func(t *testing.T) {
		path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "pinned"}})
		replacement, _ := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "substituted"}})
		moved := path + ".pinned"
		var dataSourceName string
		catalog, _, err := openCatalogAgainstPlanWithOpeners(t.Context(), path, binding, os.Open,
			func(driverName, sourceName string) (*sql.DB, error) {
				dataSourceName = sourceName
				if err := os.Rename(path, moved); err != nil {
					return nil, err
				}
				if err := os.Rename(replacement, path); err != nil {
					return nil, err
				}
				return sql.Open(driverName, sourceName)
			})
		if catalog != nil {
			_ = catalog.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "changed") || !strings.Contains(dataSourceName, "/dev/fd/") {
			t.Fatalf("pre-SQLite swap DSN=%q error=%v", dataSourceName, err)
		}
	})
}

func TestCatalogQueryRaceUsesPinnedInodeThenRejectsOriginalPathSwap(t *testing.T) {
	path, binding := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "pinned-original"}})
	replacement, _ := writeCatalogTestDatabase(t, []catalogTestRecord{{musicID: 2, title: "substituted-path"}})
	catalog, _, err := OpenCatalogAgainstPlan(t.Context(), path, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	moved := path + ".pinned"
	pinnedTitle := ""
	catalog.testHookAfterQueryValidation = func() error {
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		if err := os.Rename(replacement, path); err != nil {
			return err
		}
		return catalog.connection.QueryRowContext(context.Background(),
			`SELECT title_ja FROM catalog_music WHERE music_id=2`).Scan(&pinnedTitle)
	}
	identity, err := catalog.MusicIdentity(t.Context(), 2)
	if err == nil || !strings.Contains(err.Error(), "path, inode, or size changed") {
		t.Fatalf("race identity=%+v error=%v", identity, err)
	}
	if pinnedTitle != "pinned-original" {
		t.Fatalf("SQLite query reopened substituted path title=%q", pinnedTitle)
	}
}

func writeCatalogTestDatabase(t *testing.T, records []catalogTestRecord) (string, lyricsextractionplan.RecoveryCatalogBinding) {
	t.Helper()
	if len(records) == 0 {
		t.Fatal("catalog test records are required")
	}
	records = append([]catalogTestRecord(nil), records...)
	sort.Slice(records, func(left, right int) bool { return records[left].musicID < records[right].musicID })
	root := catalogTestResolvedTempDir(t)
	path := filepath.Join(root, "catalog.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE schema_migrations(version INTEGER NOT NULL);
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
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, lyricsextractionplan.CatalogSchemaVersion); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	policy := lyricsextractionplan.CompiledEffectiveVersions().Policies.CatalogIdentity
	fingerprints := make([]string, len(records))
	for index, record := range records {
		fingerprint := sha256.Sum256([]byte(record.title + "\x00" + strconv.Itoa(record.musicID)))
		fingerprints[index] = hex.EncodeToString(fingerprint[:])
		lyricist := "lyricist"
		if record.lyricistSet {
			lyricist = record.lyricist
		}
		vocalsJSON := record.vocalsJSON
		if vocalsJSON == "" {
			vocalsJSON = "[]"
		}
		if _, err := database.Exec(`INSERT INTO catalog_music(
			music_id,title_ja,producer_metadata,lyricist,composer,arranger,vocal_signals_json,
			lyrics_catalog_fingerprint,lyrics_catalog_policy_version) VALUES (?,?,?,?,?,?,?,?,?)`,
			record.musicID, record.title, "producer", lyricist, "composer", "arranger", vocalsJSON, fingerprints[index], policy); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	musicIDs := make([]int, len(records))
	identityDigest := sha256.New()
	_, _ = identityDigest.Write([]byte("moesekai-lyrics-recovery-catalog-fingerprints-v1\x00"))
	var encoded [8]byte
	for index, record := range records {
		musicIDs[index] = record.musicID
		binary.BigEndian.PutUint64(encoded[:], uint64(record.musicID))
		_, _ = identityDigest.Write(encoded[:])
		fingerprint, err := hex.DecodeString(fingerprints[index])
		if err != nil {
			t.Fatal(err)
		}
		_, _ = identityDigest.Write(fingerprint)
	}
	musicIDsSHA, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
	if err != nil {
		t.Fatal(err)
	}
	return path, lyricsextractionplan.RecoveryCatalogBinding{
		Path:                  path,
		SizeBytes:             int64(len(body)),
		SourceSHA256:          catalogTestSHA256(body),
		SchemaVersion:         lyricsextractionplan.CatalogSchemaVersion,
		RuntimeSchemaVersion:  lyricsextractionplan.MaximumCatalogRuntimeSchema,
		RecordCount:           len(records),
		IdentityPolicyVersion: policy,
		IdentitySHA256:        hex.EncodeToString(identityDigest.Sum(nil)),
		MusicIDsSHA256:        musicIDsSHA,
	}
}

func catalogTestResolvedTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func catalogTestSHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func catalogTestContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
