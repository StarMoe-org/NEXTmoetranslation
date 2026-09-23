package lyricsextractionplan

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
)

const (
	testRecoveryPackageRoot      = "server/offline/internal/lyricsrecovery"
	testReleasePackageRoot       = "server/offline/cmd/lyrics-recovery"
	testProviderCoordPackageRoot = "server/offline/internal/lyricsprovidercoord"
	testProductionPackageRoot    = "server/internal/lyricssource"
	testMediaFixturePath         = testRecoveryPackageRoot + "/testdata/page.json"
	testRawFixturePath           = testRecoveryPackageRoot + "/testdata/raw.wiki"
)

func TestRecoverySourceSelectionPolicyV3IncludesBuildVariantsEmbedsAndReviewedFixtures(t *testing.T) {
	root := newSyntheticRecoverySourceTree(t)
	snapshot, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	policy := CompiledRecoverySourceSelectionPolicy()
	if policy.Version != RecoverySourceSelectionPolicyV3 || policy.SnapshotAlgorithm != RecoverySourceSnapshotAlgorithmV2 ||
		policy.FixtureManifestVersion != RecoveryFixtureManifestSchemaVersionV2 || !reflect.DeepEqual(
		policy.PackageRoots, []string{
			recoveryServerPackageRoot, recoveryCommandPackageRoot, recoveryInternalPackageRoot,
			recoveryOfflinePackageRoot, recoveryOfflineCommandPackageRoot, recoveryOfflineInternalPackageRoot,
		},
	) || !reflect.DeepEqual(policy.RequiredPaths, []string{
		"server/go.mod", "server/go.sum", "server/offline/go.mod", "server/offline/go.sum", recoveryFixtureManifestPathV2,
	}) {
		t.Fatalf("compiled source-selection policy=%+v", policy)
	}
	want := []string{
		"server/go.mod",
		"server/go.sum",
		"server/offline/go.mod",
		"server/offline/go.sum",
		recoveryFixtureManifestPathV2,
		testProductionPackageRoot + "/source.go",
		testReleasePackageRoot + "/main.go",
		testProviderCoordPackageRoot + "/coord.go",
		testRecoveryPackageRoot + "/assets/embedded.txt",
		testRecoveryPackageRoot + "/embed.go",
		testRecoveryPackageRoot + "/main.go",
		testRecoveryPackageRoot + "/native.s",
		testRecoveryPackageRoot + "/object.syso",
		testMediaFixturePath,
		testRawFixturePath,
		testRecoveryPackageRoot + "/variant_plan9.go",
	}
	sort.Strings(want)
	got := make([]string, len(snapshot.Files))
	zeroPaths := map[string]bool{}
	for index, file := range snapshot.Files {
		got[index] = file.Path
		zeroPaths[file.Path] = file.SizeBytes == 0
		if index > 0 && snapshot.Files[index-1].Path >= file.Path {
			t.Fatalf("source paths are not strictly sorted: %+v", snapshot.Files)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected paths=%v want=%v", got, want)
	}
	for _, relativePath := range []string{
		"server/go.sum", "server/offline/go.sum", testRecoveryPackageRoot + "/assets/embedded.txt",
		testRecoveryPackageRoot + "/native.s",
	} {
		if !zeroPaths[relativePath] {
			t.Fatalf("legitimate zero-byte source %q was not retained", relativePath)
		}
	}
	second, err := PrepareRecoverySourceSnapshot(root, snapshot.CapturedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, second) {
		t.Fatal("source selection or hashing is not deterministic")
	}
}

func TestRecoverySourceVerificationRejectsExtraMissingAndDriftedIdentities(t *testing.T) {
	tests := map[string]struct {
		mutateTree     func(*testing.T, string)
		mutateSnapshot func(*testing.T, string, SourceSnapshot) SourceSnapshot
		errorContains  string
	}{
		"extra Go": {
			mutateTree: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/extra.go", []byte("package lyricsrecovery\n"))
			},
			errorContains: "omits current eligible file",
		},
		"extra companion input": {
			mutateTree: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/extra.S", nil)
			},
			errorContains: "omits current eligible file",
		},
		"missing Go": {
			mutateTree: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/main.go"))); err != nil {
					t.Fatal(err)
				}
			},
			errorContains: "declares missing or ineligible file",
		},
		"missing embed identity": {
			mutateSnapshot: func(t *testing.T, _ string, snapshot SourceSnapshot) SourceSnapshot {
				return recoverySnapshotWithoutPath(t, snapshot, testRecoveryPackageRoot+"/assets/embedded.txt")
			},
			errorContains: "omits current eligible file",
		},
		"extra snapshot identity": {
			mutateSnapshot: func(t *testing.T, root string, snapshot SourceSnapshot) SourceSnapshot {
				identity := writeSourceTestFile(t, root, "server/unselected.txt", []byte("not selected"))
				return recoverySnapshotWithIdentity(t, snapshot, identity)
			},
			errorContains: "declares missing or ineligible file",
		},
		"fixture raw drift": {
			mutateTree: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testRawFixturePath, []byte("changed fixture"))
			},
			errorContains: "raw size",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := newSyntheticRecoverySourceTree(t)
			snapshot, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z")
			if err != nil {
				t.Fatal(err)
			}
			if test.mutateTree != nil {
				test.mutateTree(t, root)
			}
			if test.mutateSnapshot != nil {
				snapshot = test.mutateSnapshot(t, root, snapshot)
			}
			if err := verifyRecoverySourceSnapshotIdentity(root, snapshot, nil); err == nil ||
				!strings.Contains(err.Error(), test.errorContains) {
				t.Fatalf("verification error=%v want substring %q", err, test.errorContains)
			}
		})
	}
}

func TestRecoverySourceSelectionRejectsUnsupportedLayouts(t *testing.T) {
	tests := map[string]struct {
		mutate        func(*testing.T, string)
		errorContains string
	}{
		"source outside reviewed root": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "server/not-reviewed/evil.go", []byte("package evil\n"))
			},
			errorContains: "outside the reviewed package roots",
		},
		"source-shaped cache child": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testProviderCoordPackageRoot+"/cache/generated.go", []byte("package cache\n"))
			},
			errorContains: "outside the reviewed package roots",
		},
		"source-shaped embed child": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/assets/evil.go", []byte("package evil\n"))
			},
			errorContains: "outside the reviewed package roots",
		},
		"nested module": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/nested/go.mod", []byte("module example.invalid/nested\n"))
			},
			errorContains: "nested module metadata",
		},
		"nested go.sum": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/nested/go.sum", nil)
			},
			errorContains: "nested module metadata",
		},
		"vendor": {
			mutate: func(t *testing.T, root string) {
				if err := os.Mkdir(filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/vendor")), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			errorContains: "vendor",
		},
		"root go.work": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "go.work", []byte("go 1.25\n"))
			},
			errorContains: "workspace file",
		},
		"server go.work.sum": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "server/go.work.sum", nil)
			},
			errorContains: "workspace file",
		},
		"replace directive": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "server/go.mod", []byte("module example.invalid/recovery-source\n\ngo 1.25\n\nreplace example.invalid/a => ../a\n"))
			},
			errorContains: "replace directive",
		},
		"replace block without whitespace": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "server/go.mod", []byte("module example.invalid/recovery-source\n\ngo 1.25\n\nreplace(\nexample.invalid/a => ../a\n)\n"))
			},
			errorContains: "replace directive",
		},
		"offline replace target": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "server/offline/go.mod",
					[]byte("module example.invalid/recovery-source/offline\n\ngo 1.25\n\nreplace moesekai/server => ../../\n"))
			},
			errorContains: "unauthorized replace directive",
		},
		"offline second replace": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "server/offline/go.mod",
					append(syntheticOfflineModuleFile(), []byte("replace example.invalid/a => ../a\n")...))
			},
			errorContains: "declares 2 replace directives",
		},
		"offline missing replace": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "server/offline/go.mod",
					[]byte("module example.invalid/recovery-source/offline\n\ngo 1.25\n"))
			},
			errorContains: "declares 0 replace directives",
		},
		"offline nested module": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testReleasePackageRoot+"/go.mod", []byte("module example.invalid/nested\n"))
			},
			errorContains: "nested module metadata",
		},
		"offline archive source": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, "server/offline/_archive/archived-command/main.go", []byte("package main\n"))
			},
			errorContains: "outside the reviewed package roots",
		},
		"offline deep internal source": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, recoveryOfflineInternalPackageRoot+"/a/b/c.go", []byte("package b\n"))
			},
			errorContains: "outside the reviewed package roots",
		},
		"unreviewed fixture extra": {
			mutate: func(t *testing.T, root string) {
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/testdata/extra.bin", []byte("extra"))
			},
			errorContains: "exact testdata closure",
		},
		"symlink": {
			mutate: func(t *testing.T, root string) {
				link := filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/linked.txt"))
				if err := os.Symlink("main.go", link); err != nil {
					t.Fatal(err)
				}
			},
			errorContains: "symlink",
		},
		"hardlink": {
			mutate: func(t *testing.T, root string) {
				if err := os.Link(
					filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/main.go")),
					filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/alias.txt")),
				); err != nil {
					t.Fatal(err)
				}
			},
			errorContains: "filesystem link",
		},
		"special FIFO": {
			mutate: func(t *testing.T, root string) {
				fifo := filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/pipe"))
				if err := syscall.Mkfifo(fifo, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			errorContains: "regular files",
		},
		"group writable": {
			mutate: func(t *testing.T, root string) {
				if err := os.Chmod(filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/main.go")), 0o664); err != nil {
					t.Fatal(err)
				}
			},
			errorContains: "mode",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := newSyntheticRecoverySourceTree(t)
			test.mutate(t, root)
			if _, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z"); err == nil ||
				!strings.Contains(err.Error(), test.errorContains) {
				t.Fatalf("unsupported layout error=%v want substring %q", err, test.errorContains)
			}
		})
	}
}

func TestRecoveryFixtureManifestV2PinsRawAndRecomputedContentIdentity(t *testing.T) {
	root := newSyntheticRecoverySourceTree(t)
	manifest := readSyntheticFixtureManifestV2(t, root)
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := DecodeRecoveryFixtureManifestV2(body); err != nil || !reflect.DeepEqual(decoded, manifest) {
		t.Fatalf("decoded manifest=%+v err=%v", decoded, err)
	}
	for name, mutate := range map[string]func(*RecoveryFixtureManifestV2){
		"raw size":        func(value *RecoveryFixtureManifestV2) { value.Fixtures[0].RawSizeBytes++ },
		"raw SHA-256":     func(value *RecoveryFixtureManifestV2) { value.Fixtures[0].RawSHA256 = strings.Repeat("0", 64) },
		"content SHA-1":   func(value *RecoveryFixtureManifestV2) { value.Fixtures[0].ContentSHA1 = strings.Repeat("0", 40) },
		"content SHA-256": func(value *RecoveryFixtureManifestV2) { value.Fixtures[0].ContentSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			root := newSyntheticRecoverySourceTree(t)
			manifest := readSyntheticFixtureManifestV2(t, root)
			mutate(&manifest)
			writeSyntheticFixtureManifestV2(t, root, manifest)
			if _, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z"); err == nil {
				t.Fatal("tampered fixture manifest pin was accepted")
			}
		})
	}

	t.Run("envelope SHA-1 is recomputed", func(t *testing.T) {
		root := newSyntheticRecoverySourceTree(t)
		fixturePath := filepath.Join(root, filepath.FromSlash(testMediaFixturePath))
		body, err := os.ReadFile(fixturePath)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.Replace(body, []byte(testMediaFixtureSHA1()), []byte(strings.Repeat("b", 40)), 1)
		writeSourceTestFile(t, root, testMediaFixturePath, body)
		manifest := readSyntheticFixtureManifestV2(t, root)
		for index := range manifest.Fixtures {
			if manifest.Fixtures[index].Path == testMediaFixturePath {
				manifest.Fixtures[index].RawSizeBytes = int64(len(body))
				manifest.Fixtures[index].RawSHA256 = sha256Hex(body)
			}
		}
		writeSyntheticFixtureManifestV2(t, root, manifest)
		if _, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z"); err == nil ||
			!strings.Contains(err.Error(), "recomputed") {
			t.Fatalf("forged envelope content SHA-1 error=%v", err)
		}
	})

	t.Run("mandatory fixture is nonempty", func(t *testing.T) {
		root := newSyntheticRecoverySourceTree(t)
		writeSourceTestFile(t, root, testRawFixturePath, nil)
		manifest := readSyntheticFixtureManifestV2(t, root)
		for index := range manifest.Fixtures {
			if manifest.Fixtures[index].Path == testRawFixturePath {
				manifest.Fixtures[index].RawSizeBytes = 0
				manifest.Fixtures[index].RawSHA256 = sha256Hex(nil)
				manifest.Fixtures[index].ContentSHA1 = sha1Hex(nil)
				manifest.Fixtures[index].ContentSHA256 = sha256Hex(nil)
			}
		}
		writeSyntheticFixtureManifestV2(t, root, manifest)
		if _, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z"); err == nil {
			t.Fatal("zero-byte mandatory fixture was accepted")
		}
	})

	for name, mutated := range map[string][]byte{
		"unknown field":  bytes.Replace(body, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"provider":"literal"`), 1),
		"wrong policy":   bytes.Replace(body, []byte(RecoverySourceSelectionPolicyV3), []byte("unregistered-source-selection-v4"), 1),
		"duplicate key":  bytes.Replace(body, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"schemaVersion":2`), 1),
		"trailing space": append(append([]byte(nil), body...), ' '),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRecoveryFixtureManifestV2(mutated); err == nil {
				t.Fatal("invalid fixture manifest v2 was accepted")
			}
		})
	}
}

func TestLoadVerifiedRecoveryFixtureSetReturnsDefensiveDescriptorBytes(t *testing.T) {
	root := newSyntheticRecoverySourceTree(t)
	snapshot, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	plan := syntheticRecoveryPlan(t)
	plan.SourceSnapshot = snapshot
	set, err := LoadVerifiedRecoveryFixtureSet(root, plan)
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 2 || len(set.Identities()) != 2 || len(set.All()) != 2 {
		t.Fatalf("fixture set boundary len=%d identities=%d all=%d", set.Len(), len(set.Identities()), len(set.All()))
	}
	first, found := set.Bytes(testRawFixturePath)
	if !found || len(first) == 0 {
		t.Fatal("verified raw fixture is missing")
	}
	first[0] ^= 0xff
	second, found := set.Bytes(testRawFixturePath)
	if !found || bytes.Equal(first, second) || string(second) != "raw fixture\n" {
		t.Fatal("fixture Bytes did not return a defensive copy")
	}
	all := set.All()
	all[testRawFixturePath][0] ^= 0xff
	third, _ := set.Bytes(testRawFixturePath)
	if !bytes.Equal(second, third) {
		t.Fatal("fixture All exposed internal bytes")
	}
	identities := set.Identities()
	identities[0].Path = "mutated"
	if set.Identities()[0].Path == "mutated" {
		t.Fatal("fixture identities were not defensive")
	}
}

func TestLoadVerifiedRecoveryFixtureSetRejectsDescriptorRace(t *testing.T) {
	root := newSyntheticRecoverySourceTree(t)
	snapshot, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	plan := syntheticRecoveryPlan(t)
	plan.SourceSnapshot = snapshot
	triggered := false
	hook := func(stage sourceReadStage, relativePath string) error {
		if triggered || stage != sourceReadBeforeRevalidation || relativePath != testRawFixturePath {
			return nil
		}
		triggered = true
		return os.WriteFile(filepath.Join(root, filepath.FromSlash(testRawFixturePath)), []byte("raw fixture!\n"), 0o644)
	}
	if _, err := loadVerifiedRecoveryFixtureSet(root, plan, hook); !triggered || err == nil {
		t.Fatalf("fixture race triggered=%t error=%v", triggered, err)
	}
}

func TestRecoverySourceHashingRejectsAncestorLeafContentAndLayoutRaces(t *testing.T) {
	tests := map[string]struct {
		stage  sourceReadStage
		mutate func(*testing.T, string) func()
	}{
		"ancestor swap": {
			stage: sourceReadAfterPathInspection,
			mutate: func(t *testing.T, root string) func() {
				packagePath := filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot))
				backupPath := packagePath + "-original"
				if err := os.Rename(packagePath, backupPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(packagePath, 0o755); err != nil {
					t.Fatal(err)
				}
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/main.go", []byte("package lyricsrecovery\n"))
				return func() {
					_ = os.RemoveAll(packagePath)
					_ = os.Rename(backupPath, packagePath)
				}
			},
		},
		"same-bytes inode swap": {
			stage: sourceReadAfterPathInspection,
			mutate: func(t *testing.T, root string) func() {
				filePath := filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/main.go"))
				body, err := os.ReadFile(filePath)
				if err != nil {
					t.Fatal(err)
				}
				backup := filePath + ".original"
				if err := os.Rename(filePath, backup); err != nil {
					t.Fatal(err)
				}
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/main.go", body)
				return func() {
					_ = os.Remove(filePath)
					_ = os.Rename(backup, filePath)
				}
			},
		},
		"content mutation": {
			stage: sourceReadAfterFirstChunk,
			mutate: func(t *testing.T, root string) func() {
				filePath := filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/main.go"))
				body, err := os.ReadFile(filePath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filePath, bytes.Replace(body, []byte("lyricsrecovery"), []byte("lyricsrecoverx"), 1), 0o644); err != nil {
					t.Fatal(err)
				}
				return func() { _ = os.WriteFile(filePath, body, 0o644) }
			},
		},
		"unreviewed layout extra": {
			stage: sourceReadAfterPathInspection,
			mutate: func(t *testing.T, root string) func() {
				extra := filepath.Join(root, filepath.FromSlash(testRecoveryPackageRoot+"/testdata/extra.bin"))
				writeSourceTestFile(t, root, testRecoveryPackageRoot+"/testdata/extra.bin", []byte("extra"))
				return func() { _ = os.Remove(extra) }
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := newSyntheticRecoverySourceTree(t)
			triggered := false
			var restore func()
			hook := func(stage sourceReadStage, relativePath string) error {
				if triggered || stage != test.stage || relativePath != testRecoveryPackageRoot+"/main.go" {
					return nil
				}
				triggered = true
				restore = test.mutate(t, root)
				return nil
			}
			_, err := deriveRecoverySourceIdentities(root, hook)
			if restore != nil {
				restore()
			}
			if !triggered || err == nil {
				t.Fatalf("race triggered=%t error=%v", triggered, err)
			}
		})
	}
}

func TestRecoveryFixtureManifestV1DecoderRemainsStable(t *testing.T) {
	manifest := RecoveryFixtureManifestV1{
		SchemaVersion: RecoveryFixtureManifestSchemaVersionV1, SelectionPolicy: RecoverySourceSelectionPolicyV1,
		Fixtures: []RecoveryFixtureIdentityV1{{
			Path: "server/internal/lyricssource/testdata/legacy.json", Format: RecoveryFixtureFormatMediaWikiPageV1,
			PageID: 1, RevisionID: 2, SHA1: strings.Repeat("a", 40),
		}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := DecodeRecoveryFixtureManifestV1(body); err != nil || !reflect.DeepEqual(decoded, manifest) {
		t.Fatalf("legacy manifest decoded=%+v err=%v", decoded, err)
	}
}

func TestCurrentRepositoryRecoverySourceSelectionV3IsDeterministic(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(filepath.Join(workingDirectory, "..", "..", "..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	first, err := PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PrepareRecoverySourceSnapshot(root, first.CapturedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("current repository recovery source snapshot is not deterministic")
	}
	paths := make(map[string]struct{}, len(first.Files))
	for _, file := range first.Files {
		paths[file.Path] = struct{}{}
	}
	for _, required := range []string{
		"server/go.mod",
		"server/go.sum",
		"server/offline/go.mod",
		"server/offline/go.sum",
		recoveryFixtureManifestPathV2,
		"server/internal/lyricssource/recovery_config.go",
		"server/offline/cmd/lyrics-recovery/main.go",
		"server/offline/internal/lyricsprovidercoord/contract.go",
	} {
		if _, ok := paths[required]; !ok {
			t.Fatalf("current repository source set omitted %q", required)
		}
	}
	manifestBody, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(recoveryFixtureManifestPathV2)))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodeRecoveryFixtureManifestV2(manifestBody)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Fixtures) != 88 {
		t.Fatalf("reviewed current fixture closure count=%d want=88", len(manifest.Fixtures))
	}
}

func newSyntheticRecoverySourceTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSourceTestFile(t, root, "server/go.mod", []byte("module example.invalid/recovery-source\n\ngo 1.25\n"))
	writeSourceTestFile(t, root, "server/go.sum", nil)
	writeSourceTestFile(t, root, "server/offline/go.mod", syntheticOfflineModuleFile())
	writeSourceTestFile(t, root, "server/offline/go.sum", nil)
	writeSourceTestFile(t, root, testProductionPackageRoot+"/source.go", []byte("package lyricssource\n"))
	writeSourceTestFile(t, root, testReleasePackageRoot+"/main.go", []byte("package main\n"))
	writeSourceTestFile(t, root, testProviderCoordPackageRoot+"/coord.go", []byte("package lyricsprovidercoord\n"))
	writeSourceTestFile(t, root, testProviderCoordPackageRoot+"/cache/provider-response.json", []byte("unreviewed cache data\n"))
	writeSourceTestFile(t, root, testRecoveryPackageRoot+"/main.go", []byte("package lyricsrecovery\n"))
	writeSourceTestFile(t, root, testRecoveryPackageRoot+"/variant_plan9.go", []byte("//go:build plan9\n\npackage lyricsrecovery\n"))
	writeSourceTestFile(t, root, testRecoveryPackageRoot+"/native.s", nil)
	writeSourceTestFile(t, root, testRecoveryPackageRoot+"/object.syso", []byte("synthetic system object input\n"))
	writeSourceTestFile(t, root, testRecoveryPackageRoot+"/embed.go", []byte("package lyricsrecovery\n\nimport _ \"embed\"\n\n//go:embed assets/*.txt\nvar asset string\n"))
	writeSourceTestFile(t, root, testRecoveryPackageRoot+"/assets/embedded.txt", nil)
	writeSourceTestFile(t, root, testMediaFixturePath, syntheticMediaWikiFixtureBody())
	writeSourceTestFile(t, root, testRawFixturePath, []byte("raw fixture\n"))
	manifest := RecoveryFixtureManifestV2{
		SchemaVersion: RecoveryFixtureManifestSchemaVersionV2, SelectionPolicy: RecoverySourceSelectionPolicyV3,
		SnapshotAlgorithm: RecoverySourceSnapshotAlgorithmV2,
		Fixtures: []RecoveryFixtureIdentityV2{
			syntheticFixtureIdentityV2(t, testMediaFixturePath, RecoveryFixtureFormatMediaWikiPageV1, syntheticMediaWikiFixtureBody()),
			syntheticFixtureIdentityV2(t, testRawFixturePath, RecoveryFixtureFormatRawFileV1, []byte("raw fixture\n")),
		},
	}
	writeSyntheticFixtureManifestV2(t, root, manifest)
	return root
}

func syntheticFixtureIdentityV2(t *testing.T, relativePath, format string, body []byte) RecoveryFixtureIdentityV2 {
	t.Helper()
	identity := RecoveryFixtureIdentityV2{
		Path: relativePath, Format: format, RawSizeBytes: int64(len(body)),
		RawSHA256: sha256Hex(body), ContentSHA1: sha1Hex(body), ContentSHA256: sha256Hex(body),
	}
	if format == RecoveryFixtureFormatMediaWikiPageV1 {
		page, err := decodeSingleMediaWikiFixture(body)
		if err != nil {
			t.Fatal(err)
		}
		identity.PageID = page.PageID
		identity.RevisionID = page.RevisionID
		identity.ContentSHA1 = sha1Hex([]byte(page.Content))
		identity.ContentSHA256 = sha256Hex([]byte(page.Content))
	}
	return identity
}

func syntheticOfflineModuleFile() []byte {
	return []byte("module example.invalid/recovery-source/offline\n\ngo 1.25\n\n" + recoveryOfflineModuleBind + "\n")
}

func syntheticMediaWikiFixtureBody() []byte {
	sha := testMediaFixtureSHA1()
	return []byte(`{"batchcomplete":true,"query":{"pages":[{"pageid":101,"lastrevid":202,"revisions":[{"revid":202,"sha1":"` + sha + `","slots":{"main":{"content":"fixture body"}}}]}]}}`)
}

func testMediaFixtureSHA1() string {
	return sha1Hex([]byte("fixture body"))
}

func readSyntheticFixtureManifestV2(t *testing.T, root string) RecoveryFixtureManifestV2 {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(recoveryFixtureManifestPathV2)))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodeRecoveryFixtureManifestV2(body)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeSyntheticFixtureManifestV2(t *testing.T, root string, manifest RecoveryFixtureManifestV2) {
	t.Helper()
	sort.Slice(manifest.Fixtures, func(left, right int) bool { return manifest.Fixtures[left].Path < manifest.Fixtures[right].Path })
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeSourceTestFile(t, root, recoveryFixtureManifestPathV2, body)
}

func writeSourceTestFile(t *testing.T, root, relativePath string, body []byte) SourceFileIdentity {
	t.Helper()
	absolutePath := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(absolutePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolutePath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(absolutePath, 0o644); err != nil {
		t.Fatal(err)
	}
	return SourceFileIdentity{Path: relativePath, SizeBytes: int64(len(body)), SHA256: sha256Hex(body)}
}

func recoverySnapshotWithoutPath(t *testing.T, snapshot SourceSnapshot, relativePath string) SourceSnapshot {
	t.Helper()
	files := make([]SourceFileIdentity, 0, len(snapshot.Files)-1)
	for _, file := range snapshot.Files {
		if file.Path != relativePath {
			files = append(files, file)
		}
	}
	if len(files) != len(snapshot.Files)-1 {
		t.Fatalf("snapshot path %q was not present", relativePath)
	}
	return recoverySnapshotWithFiles(t, snapshot, files)
}

func recoverySnapshotWithIdentity(t *testing.T, snapshot SourceSnapshot, identity SourceFileIdentity) SourceSnapshot {
	t.Helper()
	files := append(append([]SourceFileIdentity(nil), snapshot.Files...), identity)
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	return recoverySnapshotWithFiles(t, snapshot, files)
}

func recoverySnapshotWithFiles(t *testing.T, snapshot SourceSnapshot, files []SourceFileIdentity) SourceSnapshot {
	t.Helper()
	digest, err := RecoverySourceSnapshotSHA256(files)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Files = files
	snapshot.SHA256 = digest
	return snapshot
}

func sha1Hex(body []byte) string {
	digest := sha1.Sum(body)
	return hex.EncodeToString(digest[:])
}
