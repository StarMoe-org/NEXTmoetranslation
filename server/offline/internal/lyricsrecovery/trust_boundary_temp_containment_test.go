package lyricsrecovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const recoveryTestPrivateRunRootEnv = "MOESEKAI_RECOVERY_V2_TEST_PRIVATE_RUN_ROOT"

func privateRecoveryTempDir(t *testing.T) string {
	t.Helper()
	basePath := os.TempDir()
	explicitPrivateRoot := false
	if supplied, ok := os.LookupEnv(recoveryTestPrivateRunRootEnv); ok && supplied != "" {
		if strings.TrimSpace(supplied) != supplied {
			t.Fatalf("%s is not an explicit canonical path", recoveryTestPrivateRunRootEnv)
		}
		basePath = supplied
		explicitPrivateRoot = true
	}
	base, err := canonicalRecoveryTestBase(basePath, explicitPrivateRoot)
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.MkdirTemp(base, "lyrics-recovery-v2-test-")
	if err != nil {
		t.Fatal(err)
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(path)
		}
	}()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path || !privateDirectory(info) || !recoveryTestPathContained(base, path) {
		t.Fatalf("created recovery test root is not a canonical contained mode-0700 directory: path=%s resolved=%s err=%v", path, resolved, err)
	}
	created = false
	t.Cleanup(func() {
		if err := removeOwnedRecoveryTestRoot(base, path, info); err != nil {
			t.Errorf("clean recovery test root: %v", err)
		}
	})
	return path
}

func canonicalRecoveryTestBase(path string, explicitPrivateRoot bool) (string, error) {
	if path == "" || strings.TrimSpace(path) != path {
		return "", errorsForRecoveryTestBase(path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if explicitPrivateRoot && path != absolute {
		return "", fmt.Errorf("explicit recovery test run root must be canonical absolute: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	if explicitPrivateRoot && resolved != absolute {
		return "", fmt.Errorf("explicit recovery test run root must not traverse a symlink or alias: %s", path)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("recovery test base is not a direct directory: %s", resolved)
	}
	if explicitPrivateRoot && !privateDirectory(info) {
		return "", fmt.Errorf("explicit recovery test run root must be effective-UID-owned mode-0700: %s", resolved)
	}
	return resolved, nil
}

func errorsForRecoveryTestBase(path string) error {
	return fmt.Errorf("recovery test base is invalid: %q", path)
}

func recoveryTestPathContained(base, path string) bool {
	relative, err := filepath.Rel(base, path)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func removeOwnedRecoveryTestRoot(base, path string, created os.FileInfo) error {
	if !recoveryTestPathContained(base, path) {
		return fmt.Errorf("refuse to clean recovery test root outside its base: %s", path)
	}
	current, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(created, current) {
		return fmt.Errorf("refuse to clean replaced recovery test root: %s", path)
	}
	return os.RemoveAll(path)
}

func recoveryPackageFixturePath(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return "", fmt.Errorf("recovery fixture name is invalid: %q", name)
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "internal", "lyricssource", "testdata"))
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, name)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative != name || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("recovery fixture path escapes package fixture root: %q", name)
	}
	return path, nil
}

func newRecoveryTrustBoundarySandbox(t *testing.T) string {
	t.Helper()
	base, err := canonicalRecoveryTestBase(os.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.MkdirTemp(base, "lyrics-recovery-v2-trust-boundary-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.RemoveAll(path)
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !privateDirectory(info) {
		_ = os.RemoveAll(path)
		t.Fatalf("trust-boundary sandbox is not private: %v", err)
	}
	t.Cleanup(func() {
		if err := removeOwnedRecoveryTestRoot(base, path, info); err != nil {
			t.Errorf("clean trust-boundary sandbox: %v", err)
		}
	})
	return path
}

func TestPrivateRecoveryTempDirCanonicalizesTMPDIRAndCleansOnlyItsCreatedRoot(t *testing.T) {
	sandbox := newRecoveryTrustBoundarySandbox(t)
	realTMPDIR := filepath.Join(sandbox, "tmp-real")
	if err := os.Mkdir(realTMPDIR, 0o700); err != nil {
		t.Fatal(err)
	}
	configuredTMPDIR := realTMPDIR
	aliasTMPDIR := filepath.Join(sandbox, "tmp-alias")
	if err := os.Symlink(realTMPDIR, aliasTMPDIR); err == nil {
		configuredTMPDIR = aliasTMPDIR
	}
	historicalSentinel := filepath.Join(realTMPDIR, "historical-forensic.keep")
	if err := os.WriteFile(historicalSentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	var created string
	t.Run("created root", func(t *testing.T) {
		t.Setenv("TMPDIR", configuredTMPDIR)
		t.Setenv(recoveryTestPrivateRunRootEnv, "")
		created = privateRecoveryTempDir(t)
		if !recoveryTestPathContained(realTMPDIR, created) {
			t.Fatalf("recovery test root escaped canonical TMPDIR: root=%s tmpdir=%s", created, realTMPDIR)
		}
		if err := os.WriteFile(filepath.Join(created, "owned.tmp"), []byte("owned"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Lstat(created); !os.IsNotExist(err) {
		t.Fatalf("created recovery test root survived cleanup: %v", err)
	}
	if body, err := os.ReadFile(historicalSentinel); err != nil || string(body) != "preserve" {
		t.Fatalf("cleanup changed the sibling forensic sentinel: body=%q err=%v", body, err)
	}
}

func TestPrivateRecoveryTempDirUsesExplicitCanonicalPrivateRunRoot(t *testing.T) {
	sandbox := newRecoveryTrustBoundarySandbox(t)
	explicitRoot := filepath.Join(sandbox, "explicit-private-run-root")
	otherTMPDIR := filepath.Join(sandbox, "other-tmp")
	for _, path := range []string{explicitRoot, otherTMPDIR} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	siblingSentinel := filepath.Join(explicitRoot, "prior-run.keep")
	if err := os.WriteFile(siblingSentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	var created string
	t.Run("created root", func(t *testing.T) {
		t.Setenv("TMPDIR", otherTMPDIR)
		t.Setenv(recoveryTestPrivateRunRootEnv, explicitRoot)
		created = privateRecoveryTempDir(t)
		if !recoveryTestPathContained(explicitRoot, created) {
			t.Fatalf("recovery test root escaped explicit private run root: root=%s supplied=%s", created, explicitRoot)
		}
	})
	if _, err := os.Lstat(created); !os.IsNotExist(err) {
		t.Fatalf("created recovery test root survived cleanup: %v", err)
	}
	if body, err := os.ReadFile(siblingSentinel); err != nil || string(body) != "preserve" {
		t.Fatalf("cleanup changed the pre-existing sibling: body=%q err=%v", body, err)
	}
}

func TestExplicitRecoveryTestRunRootKeepsProductionCanonicalPrivateBoundary(t *testing.T) {
	sandbox := newRecoveryTrustBoundarySandbox(t)
	privateRoot := filepath.Join(sandbox, "private-root")
	if err := os.Mkdir(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(sandbox, "private-root-alias")
	if err := os.Symlink(privateRoot, alias); err == nil {
		if _, err := canonicalRecoveryTestBase(alias, true); err == nil {
			t.Fatal("explicit symlink run root was accepted")
		}
	}
	wrongMode := filepath.Join(sandbox, "wrong-mode-root")
	if err := os.Mkdir(wrongMode, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wrongMode, 0o755); err != nil {
		t.Fatal(err)
	}
	wrongModeInfo, err := os.Lstat(wrongMode)
	if err != nil {
		t.Fatal(err)
	}
	if !wrongModeInfo.IsDir() || wrongModeInfo.Mode()&os.ModeSymlink != 0 || wrongModeInfo.Mode().Perm() != 0o755 {
		t.Fatalf("wrong-mode fixture is not a direct mode-0755 directory: mode=%v", wrongModeInfo.Mode())
	}
	if _, err := canonicalRecoveryTestBase(wrongMode, true); err == nil {
		t.Fatal("explicit non-private run root was accepted")
	}
}

func TestRecoveryFixtureResolutionIsPackageLocalAndTrimpathStable(t *testing.T) {
	path, err := recoveryPackageFixturePath("sekaipedia-list-335193.json")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatalf("recovery fixture path is not canonical absolute: %s", path)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("recovery fixture is not a direct regular file: info=%v err=%v", info, err)
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 {
		t.Fatalf("read recovery package fixture: bytes=%d err=%v", len(body), err)
	}
	for _, invalid := range []string{"", ".", "..", "../sekaipedia-list-335193.json", "nested/fixture.json"} {
		if _, err := recoveryPackageFixturePath(invalid); err == nil {
			t.Fatalf("unsafe recovery fixture name was accepted: %q", invalid)
		}
	}
}
