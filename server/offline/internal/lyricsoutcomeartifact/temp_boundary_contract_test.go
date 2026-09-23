package lyricsoutcomeartifact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var artifactTestTempRoot string

func TestMain(m *testing.M) {
	os.Exit(runArtifactTestMain(m))
}

func runArtifactTestMain(m *testing.M) int {
	base, err := canonicalTestPath(os.TempDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "canonicalize provider outcome artifact test temp base: %v\n", err)
		return 1
	}
	root, err := os.MkdirTemp(base, "lyrics-outcome-artifact-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create provider outcome artifact test temp root: %v\n", err)
		return 1
	}
	canonicalRoot, err := canonicalTestPath(root)
	if err != nil || canonicalRoot != root {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "canonicalize provider outcome artifact test temp root: path=%q resolved=%q err=%v\n", root, canonicalRoot, err)
		return 1
	}
	if err := os.Chmod(canonicalRoot, 0o700); err != nil {
		_ = os.RemoveAll(canonicalRoot)
		fmt.Fprintf(os.Stderr, "secure provider outcome artifact test temp root: %v\n", err)
		return 1
	}
	rootInfo, statErr := os.Lstat(canonicalRoot)
	privateErr := validatePrivateDirectory(rootInfo)
	if statErr != nil || privateErr != nil {
		_ = os.RemoveAll(canonicalRoot)
		fmt.Fprintf(os.Stderr, "validate provider outcome artifact test temp root: stat=%v private=%v\n", statErr, privateErr)
		return 1
	}

	artifactTestTempRoot = canonicalRoot
	code := m.Run()

	if err := os.RemoveAll(canonicalRoot); err != nil {
		fmt.Fprintf(os.Stderr, "remove provider outcome artifact test temp root: %v\n", err)
		code = 1
	}
	return code
}

func TestArtifactTestTempBoundaryIsCanonicalPrivateTMPDIR(t *testing.T) {
	path := privateTempDir(t)
	relative, err := filepath.Rel(artifactTestTempRoot, path)
	if err != nil || !filepath.IsLocal(relative) {
		t.Fatalf("provider outcome artifact tests escaped their stable private root: path=%q root=%q err=%v", path, artifactTestTempRoot, err)
	}
}

func TestArtifactProductionBoundaryStillRejectsAliasedPrivateRoot(t *testing.T) {
	base := privateTempDir(t)
	realParent := filepath.Join(base, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias-parent")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	outcomes := filepath.Join(aliasParent, "outcomes")
	if err := CreatePrivateDirectory(outcomes); err == nil {
		t.Fatal("provider outcome directory creation accepted an aliased private root")
	}
	if _, err := os.Lstat(filepath.Join(realParent, "outcomes")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected aliased private root was mutated: %v", err)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp(artifactTestTempRoot, "case-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	canonical, err := canonicalTestPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != path {
		t.Fatalf("provider outcome artifact test temp directory is not canonical: path=%q resolved=%q", path, canonical)
	}
	relative, err := filepath.Rel(artifactTestTempRoot, canonical)
	if err != nil || !filepath.IsLocal(relative) {
		t.Fatalf("provider outcome artifact test temp directory escaped private root: path=%q root=%q err=%v", canonical, artifactTestTempRoot, err)
	}
	if err := os.Chmod(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateDirectory(info); err != nil {
		t.Fatalf("provider outcome artifact test temp directory is not private: path=%q info=%v err=%v", canonical, info, err)
	}
	return canonical
}

func canonicalTestPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
