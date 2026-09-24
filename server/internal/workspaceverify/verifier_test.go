package workspaceverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCheckedInProducerContractFixtureVerifies(t *testing.T) {
	root := copyFixture(t)
	manifest, err := verifyFixture(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Producer.SourceDirty || !manifest.Producer.SourceProduction || manifest.Artifact.AppVersion != "5.9.6" {
		t.Fatalf("fixture producer = %+v artifact = %+v", manifest.Producer, manifest.Artifact)
	}
	if got := manifest.RequiredRoutes; !equalRoutes(got, RequiredRoutes()) {
		t.Fatalf("fixture routes do not match server contract")
	}
	expected := []Route{
		{Method: "GET", Path: "/api/admin/lyrics-source-reviews", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: []string{"admin"}},
		{Method: "GET", Path: "/api/admin/lyrics-source-reviews/detail", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: []string{"admin"}},
		{Method: "POST", Path: "/api/admin/lyrics-source-reviews/import", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: []string{"admin"}},
		{Method: "PUT", Path: "/api/admin/lyrics-source-reviews/candidate-selection", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: []string{"admin"}},
		{Method: "PUT", Path: "/api/admin/lyrics-source-reviews/decision", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: []string{"admin"}},
	}
	var reviewRoutes []Route
	for _, route := range manifest.RequiredRoutes {
		if strings.HasPrefix(route.Path, "/api/admin/lyrics-source-reviews") {
			reviewRoutes = append(reviewRoutes, route)
		}
	}
	if !equalRoutes(reviewRoutes, expected) {
		t.Fatalf("fixture review routes=%+v, want %+v", reviewRoutes, expected)
	}
}

func TestContractStatesLenientContentAdmissionAndRequiredBackupProof(t *testing.T) {
	proofs := make(map[string]string)
	for _, route := range RequiredRoutes() {
		if route.ProducerProof != ProducerProofNone {
			proofs[route.Method+" "+route.Path] = route.ProducerProof
		}
	}
	want := map[string]string{
		"POST /api/editor/v1/backup/push":       ProducerProofRequired,
		"POST /api/editor/v1/lyrics/publish":    ProducerProofOptional,
		"POST /api/editor/v1/lyrics/unpublish":  ProducerProofOptional,
		"PUT /api/editor/v1/category/batch":     ProducerProofOptional,
		"PUT /api/editor/v1/entry":              ProducerProofOptional,
		"PUT /api/editor/v1/event-story/update": ProducerProofOptional,
		"PUT /api/editor/v1/lyrics/save":        ProducerProofOptional,
	}
	if !reflect.DeepEqual(proofs, want) {
		t.Fatalf("producer proof policy = %v, want %v", proofs, want)
	}
	rejections := expectedEditorGateContract.MutationRejections
	if expectedEditorGateContract.Version != 3 || rejections.Missing != 428 || rejections.Malformed != 400 || rejections.StaleOrRunning != 409 {
		t.Fatalf("editor gate contract = %+v", expectedEditorGateContract)
	}
}

func TestProductionWorkspaceModes(t *testing.T) {
	root := copyFixture(t)
	digest := digestPath(filepath.Join(root, ManifestFilename))

	if _, err := VerifyRuntime(Config{Production: true}); err == nil || !strings.Contains(err.Error(), "WORKSPACE_MODE is required") {
		t.Fatalf("production missing mode error = %v", err)
	}
	if manifest, err := VerifyRuntime(Config{Mode: ModeDisabled, ModeConfigured: true, Production: true}); err != nil || manifest != nil {
		t.Fatalf("production disabled manifest=%v err=%v", manifest, err)
	}
	for _, residue := range []Config{
		{RootConfigured: true},
		{ManifestSHA256Configured: true},
		{Root: root},
		{ManifestSHA256: digest},
	} {
		residue.Mode = ModeDisabled
		residue.ModeConfigured = true
		residue.Production = true
		if _, err := VerifyRuntime(residue); err == nil || !strings.Contains(err.Error(), "must both be unset") {
			t.Fatalf("production disabled residue %+v error = %v", residue, err)
		}
	}
	external := Config{
		Mode: ModeExternal, Root: root, ManifestSHA256: digest, Production: true,
		ModeConfigured: true, RootConfigured: true, ManifestSHA256Configured: true,
	}
	if _, err := VerifyRuntime(external); err == nil || !strings.Contains(err.Error(), `must be exactly "disabled" in production`) {
		t.Fatalf("production external workspace error = %v", err)
	}
	for _, invalid := range []string{"", "disabled ", " paired", "paired", "EXTERNAL"} {
		if _, err := VerifyRuntime(Config{Mode: invalid, ModeConfigured: true, Production: true}); err == nil || !strings.Contains(err.Error(), "must be exactly") {
			t.Fatalf("production accepted non-exact mode %q: %v", invalid, err)
		}
	}
}

func TestModeLessPairedArtifactVerificationRemainsCompatible(t *testing.T) {
	root := copyFixture(t)
	manifest, err := Verify(Config{Root: root, ManifestSHA256: digestPath(filepath.Join(root, ManifestFilename)), Production: true})
	if err != nil || manifest == nil || manifest.SchemaVersion != SchemaVersion {
		t.Fatalf("paired compatibility manifest=%v err=%v", manifest, err)
	}
}

func TestWorkspaceConfigurationAndProductionDirtiness(t *testing.T) {
	if manifest, err := VerifyRuntime(Config{}); err != nil || manifest != nil {
		t.Fatalf("optional development workspace manifest=%v err=%v", manifest, err)
	}
	compatibleRoot := copyFixture(t)
	compatibleConfig := Config{Root: compatibleRoot, ManifestSHA256: digestPath(filepath.Join(compatibleRoot, ManifestFilename))}
	compatible, err := Verify(compatibleConfig)
	if err != nil || compatible == nil || compatible.SchemaVersion != SchemaVersion {
		t.Fatalf("verifier-only external workspace manifest=%v err=%v", compatible, err)
	}
	for _, runtimeConfig := range []Config{
		compatibleConfig,
		{Mode: ModeExternal, ModeConfigured: true, Root: compatibleRoot, RootConfigured: true, ManifestSHA256: compatibleConfig.ManifestSHA256, ManifestSHA256Configured: true},
		{Root: t.TempDir()},
		{Root: t.TempDir(), ManifestSHA256: "ABC"},
	} {
		if _, err := VerifyRuntime(runtimeConfig); err == nil || !strings.Contains(err.Error(), "only to verifier tooling") {
			t.Fatalf("runtime accepted external workspace config %+v: %v", runtimeConfig, err)
		}
	}
	if _, err := Verify(Config{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("partial verifier workspace config error = %v", err)
	}
	if _, err := Verify(Config{Root: t.TempDir(), ManifestSHA256: "ABC"}); err == nil || !strings.Contains(err.Error(), "lowercase hexadecimal") {
		t.Fatalf("noncanonical verifier lock error = %v", err)
	}

	root := copyFixture(t)
	manifest := readTypedManifest(t, root)
	manifest.Producer.SourceDirty = true
	manifest.Producer.SourceProduction = false
	writeTypedManifest(t, root, manifest)
	if _, err := verifyFixture(root, true); err == nil || !strings.Contains(err.Error(), "sourceDirty=false and sourceProduction=true") {
		t.Fatalf("dirty production error = %v", err)
	}
	if _, err := verifyFixture(root, false); err != nil {
		t.Fatalf("dirty development workspace rejected: %v", err)
	}

	manifest.Producer.SourceDirty = false
	writeTypedManifest(t, root, manifest)
	if _, err := verifyFixture(root, true); err == nil || !strings.Contains(err.Error(), "sourceProduction=true") {
		t.Fatalf("nonproduction producer error = %v", err)
	}
	if _, err := verifyFixture(root, false); err != nil {
		t.Fatalf("nonproduction producer rejected in development: %v", err)
	}
}

func TestManifestRejectsUnknownDuplicateAndUnsupportedContracts(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, string)
		message string
	}{
		{"unknown field", func(t *testing.T, root string) {
			mutateRawManifest(t, root, func(contents string) string {
				return strings.Replace(contents, "  \"schemaVersion\": 4,", "  \"schemaVersion\": 4,\n  \"unknown\": true,", 1)
			})
		}, "must contain exactly"},
		{"duplicate key", func(t *testing.T, root string) {
			mutateRawManifest(t, root, func(contents string) string {
				return strings.Replace(contents, "  \"schemaVersion\": 4,", "  \"schemaVersion\": 4,\n  \"schemaVersion\": 4,", 1)
			})
		}, "duplicate object key"},
		{"unsupported schema", func(t *testing.T, root string) {
			manifest := readTypedManifest(t, root)
			manifest.SchemaVersion++
			writeTypedManifest(t, root, manifest)
		}, "unsupported workspace schemaVersion"},
		{"old source contract", func(t *testing.T, root string) {
			manifest := readTypedManifest(t, root)
			manifest.SourceContract.Version = 1
			writeTypedManifest(t, root, manifest)
		}, "source contract is unsupported"},
		{"old editor gate contract", func(t *testing.T, root string) {
			manifest := readTypedManifest(t, root)
			manifest.EditorGateContract.Version = 1
			writeTypedManifest(t, root, manifest)
		}, "editor gate contract is unsupported"},
		{"old two-component mutation proof", func(t *testing.T, root string) {
			manifest := readTypedManifest(t, root)
			manifest.EditorGateContract.MutationFormat = "<base64url-instanceId>:<completedGeneration>"
			writeTypedManifest(t, root, manifest)
		}, "editor gate contract is unsupported"},
		{"editor gate contract requiring the header on content routes", func(t *testing.T, root string) {
			manifest := readTypedManifest(t, root)
			manifest.EditorGateContract.Version = 2
			writeTypedManifest(t, root, manifest)
		}, "editor gate contract is unsupported"},
		{"schema 3 boolean producer proof", func(t *testing.T, root string) {
			mutateRawManifest(t, root, func(contents string) string {
				contents = strings.Replace(contents, "  \"schemaVersion\": 4,", "  \"schemaVersion\": 3,", 1)
				contents = strings.ReplaceAll(contents, "\"producerProof\": \"none\"", "\"producerProof\": false")
				contents = strings.ReplaceAll(contents, "\"producerProof\": \"optional\"", "\"producerProof\": true")
				return strings.ReplaceAll(contents, "\"producerProof\": \"required\"", "\"producerProof\": true")
			})
		}, "decode workspace manifest"},
		{"unsupported editor gate rejection", func(t *testing.T, root string) {
			manifest := readTypedManifest(t, root)
			manifest.EditorGateContract.MutationRejections.Missing = 400
			writeTypedManifest(t, root, manifest)
		}, "editor gate contract is unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := copyFixture(t)
			test.mutate(t, root)
			if _, err := verifyFixture(root, false); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestManifestRejectsNoncanonicalAndMismatchedRoutes(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]Route) []Route
		message string
	}{
		{"ordering", func(routes []Route) []Route {
			routes[0], routes[1] = routes[1], routes[0]
			return routes
		}, "canonical authorization order"},
		{"duplicate", func(routes []Route) []Route {
			return append(routes[:1], append([]Route{routes[0]}, routes[1:]...)...)
		}, "duplicate required route"},
		{"missing capability", func(routes []Route) []Route { return routes[:len(routes)-1] }, "does not match the server contract"},
		{"extra capability", func(routes []Route) []Route {
			return append(routes, Route{Method: "PUT", Path: "/api/editor/v1/other", Authentication: "bearer", ProducerProof: ProducerProofOptional, AllowedRoles: []string{"editor", "admin"}})
		}, "does not match the server contract"},
		{"content route claims the header is required", func(routes []Route) []Route {
			for index := range routes {
				if routes[index].Path == "/api/editor/v1/entry" {
					routes[index].ProducerProof = ProducerProofRequired
				}
			}
			return routes
		}, "does not match the server contract"},
		{"backup push claims the header is optional", func(routes []Route) []Route {
			for index := range routes {
				if routes[index].Path == "/api/editor/v1/backup/push" {
					routes[index].ProducerProof = ProducerProofOptional
				}
			}
			return routes
		}, "does not match the server contract"},
		{"unknown producer proof policy", func(routes []Route) []Route {
			routes[0].ProducerProof = "sometimes"
			return routes
		}, "required route 0 is invalid"},
		{"public route with producer proof", func(routes []Route) []Route {
			for index := range routes {
				if routes[index].Authentication == "none" {
					routes[index].ProducerProof = ProducerProofOptional
				}
			}
			return routes
		}, "is invalid"},
		{"query token authentication", func(routes []Route) []Route {
			for index := range routes {
				if routes[index].Path == "/sse" {
					routes[index].Authentication = "query-token"
				}
			}
			return routes
		}, "required route"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := copyFixture(t)
			manifest := readTypedManifest(t, root)
			manifest.RequiredRoutes = test.mutate(manifest.RequiredRoutes)
			writeTypedManifest(t, root, manifest)
			if _, err := verifyFixture(root, false); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestWorkspaceRejectsMissingExtraTamperedSymlinkAndNonregularFiles(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, string)
		message string
	}{
		{"missing", func(t *testing.T, root string) { mustRemove(t, filepath.Join(root, "assets", "app.js")) }, "is missing"},
		{"extra", func(t *testing.T, root string) { mustWrite(t, filepath.Join(root, "extra.txt"), []byte("extra")) }, "extra file"},
		{"extra directory", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "extra directory"},
		{"tampered size", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "assets", "app.js"), []byte("short"))
		}, "size mismatch"},
		{"tampered digest", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "assets", "app.js"), []byte("console.log('workspace altered')\n"))
			manifest := readTypedManifest(t, root)
			manifest.Files[0].Size = int64(len("console.log('workspace altered')\n"))
			writeTypedManifest(t, root, manifest)
		}, "SHA-256 mismatch"},
		{"symlink", func(t *testing.T, root string) {
			if err := os.Symlink("app.js", filepath.Join(root, "assets", "linked.js")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, "contains symlink"},
		{"nonregular", func(t *testing.T, root string) {
			if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
				t.Skipf("FIFO unavailable: %v", err)
			}
		}, "nonregular file"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := copyFixture(t)
			test.mutate(t, root)
			if _, err := verifyFixture(root, false); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestManifestRejectsUnsafeInventoryAndResourceLimitViolations(t *testing.T) {
	t.Run("path traversal", func(t *testing.T) {
		root := copyFixture(t)
		manifest := readTypedManifest(t, root)
		manifest.Files[0].Path = "../outside"
		writeTypedManifest(t, root, manifest)
		if _, err := verifyFixture(root, false); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("manifest bytes", func(t *testing.T) {
		root := copyFixture(t)
		path := filepath.Join(root, ManifestFilename)
		mustWrite(t, path, []byte(strings.Repeat(" ", MaxManifestBytes+1)))
		if _, err := Verify(Config{Root: root, ManifestSHA256: digestFile(t, path)}); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("file count", func(t *testing.T) {
		root := copyFixture(t)
		manifest := readTypedManifest(t, root)
		manifest.Files = make([]File, MaxFiles+1)
		for index := range manifest.Files {
			manifest.Files[index] = File{Path: fmt.Sprintf("assets/%04d.js", index), SHA256: strings.Repeat("0", 64)}
		}
		manifest.Files[len(manifest.Files)-1].Path = "index.html"
		writeTypedManifest(t, root, manifest)
		if _, err := verifyFixture(root, false); err == nil || !strings.Contains(err.Error(), "file count") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("single file bytes", func(t *testing.T) {
		root := copyFixture(t)
		manifest := readTypedManifest(t, root)
		manifest.Files[0].Size = MaxFileBytes + 1
		writeTypedManifest(t, root, manifest)
		if _, err := verifyFixture(root, false); err == nil || !strings.Contains(err.Error(), "file bounds") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("total bytes", func(t *testing.T) {
		root := copyFixture(t)
		manifest := readTypedManifest(t, root)
		manifest.Files = []File{
			{Path: "assets/a", Size: MaxFileBytes, SHA256: strings.Repeat("0", 64)},
			{Path: "assets/b", Size: MaxFileBytes, SHA256: strings.Repeat("0", 64)},
			{Path: "assets/c", Size: MaxFileBytes, SHA256: strings.Repeat("0", 64)},
			{Path: "assets/d", Size: MaxFileBytes, SHA256: strings.Repeat("0", 64)},
			{Path: "index.html", Size: 1, SHA256: strings.Repeat("0", 64)},
		}
		writeTypedManifest(t, root, manifest)
		if _, err := verifyFixture(root, false); err == nil || !strings.Contains(err.Error(), "total bytes") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestManifestSHA256IsAnExternalByteLock(t *testing.T) {
	root := copyFixture(t)
	if _, err := Verify(Config{Root: root, ManifestSHA256: strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched manifest lock error = %v", err)
	}
}

func copyFixture(t *testing.T) string {
	t.Helper()
	destination := t.TempDir()
	err := filepath.WalkDir(filepath.Join("testdata", "valid"), func(source string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(filepath.Join("testdata", "valid"), source)
		if err != nil || relative == "." {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		contents, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return destination
}

func readTypedManifest(t *testing.T, root string) Manifest {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(root, ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeTypedManifest(t *testing.T, root string, manifest Manifest) {
	t.Helper()
	contents, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, ManifestFilename), append(contents, '\n'))
}

func mutateRawManifest(t *testing.T, root string, mutate func(string) string) {
	t.Helper()
	path := filepath.Join(root, ManifestFilename)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, []byte(mutate(string(contents))))
}

func verifyFixture(root string, production bool) (*Manifest, error) {
	return Verify(Config{
		Root: root, ManifestSHA256: digestPath(filepath.Join(root, ManifestFilename)), Production: production,
	})
}

func digestFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func digestPath(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return strings.Repeat("f", 64)
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func mustWrite(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func equalRoutes(left, right []Route) bool {
	return reflect.DeepEqual(left, right)
}
