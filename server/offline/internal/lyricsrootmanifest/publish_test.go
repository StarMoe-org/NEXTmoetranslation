package lyricsrootmanifest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func rootBodyForPublication(t *testing.T) []byte {
	t.Helper()
	manifest, err := Assemble(baseRequest(ScopeFinal, currentCatalogCount), rootResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func canonicalRootTestBase(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil || absolute != resolved {
		t.Fatalf("canonical test directory path=%q err=%v", resolved, err)
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		t.Fatal(err)
	}
	return resolved
}

func privateRootTestDirectory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(canonicalRootTestBase(t), "private")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPublishCreateExclusiveModeNoOverwriteAndLeafSymlinkSafety(t *testing.T) {
	body := rootBodyForPublication(t)
	parent := privateRootTestDirectory(t)
	path := filepath.Join(parent, "root.json")
	if err := PublishCreateExclusive(path, body); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || rootFileLinkCount(info) != 1 {
		t.Fatalf("published root info=%v links=%d err=%v", info, rootFileLinkCount(info), err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := PublishCreateExclusive(path, body); !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("second root publication error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("no-overwrite changed root err=%v", err)
	}

	symlinkParent := privateRootTestDirectory(t)
	target := filepath.Join(canonicalRootTestBase(t), "leaf-symlink-target.json")
	original := []byte("do-not-overwrite")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(symlinkParent, "root.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := PublishCreateExclusive(link, body); err == nil {
		t.Fatal("symlink root publication was accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("symlink target changed: %q err=%v", got, err)
	}
}

func TestPublishRejectsLexicalAndResolvedParentAliasesWithoutWrites(t *testing.T) {
	body := rootBodyForPublication(t)
	parent := privateRootTestDirectory(t)
	lexicalAlias := parent + string(os.PathSeparator) + "." + string(os.PathSeparator) + "root.json"
	if err := PublishCreateExclusive(lexicalAlias, body); err == nil || !strings.Contains(err.Error(), "lexically canonical") {
		t.Fatalf("lexical parent alias error=%v", err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("lexical alias mutated parent: entries=%v err=%v", entries, err)
	}

	base := canonicalRootTestBase(t)
	targetParent := filepath.Join(base, "resolved-parent")
	if err := os.Mkdir(targetParent, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "parent-alias")
	if err := os.Symlink(targetParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	if err := PublishCreateExclusive(filepath.Join(aliasParent, "root.json"), body); err == nil || !strings.Contains(err.Error(), "symlink or resolved alias") {
		t.Fatalf("resolved parent alias error=%v", err)
	}
	if entries, err := os.ReadDir(targetParent); err != nil || len(entries) != 0 {
		t.Fatalf("resolved alias target was mutated: entries=%v err=%v", entries, err)
	}

	ancestorTarget := filepath.Join(base, "ancestor-target")
	ancestorParent := filepath.Join(ancestorTarget, "parent")
	if err := os.Mkdir(ancestorTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ancestorParent, 0o700); err != nil {
		t.Fatal(err)
	}
	ancestorAlias := filepath.Join(base, "ancestor-alias")
	if err := os.Symlink(ancestorTarget, ancestorAlias); err != nil {
		t.Fatal(err)
	}
	if err := PublishCreateExclusive(filepath.Join(ancestorAlias, "parent", "root.json"), body); err == nil || !strings.Contains(err.Error(), "symlink or resolved alias") {
		t.Fatalf("resolved ancestor alias error=%v", err)
	}
	if entries, err := os.ReadDir(ancestorParent); err != nil || len(entries) != 0 {
		t.Fatalf("resolved ancestor target was mutated: entries=%v err=%v", entries, err)
	}
}

func TestPublishRejectsParentModeAndTamperedBody(t *testing.T) {
	body := rootBodyForPublication(t)
	base := canonicalRootTestBase(t)
	parent := filepath.Join(base, "public-parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0o755 {
		t.Fatalf("public parent info=%v err=%v", parentInfo, err)
	}
	if err := PublishCreateExclusive(filepath.Join(parent, "root.json"), body); err == nil || !strings.Contains(err.Error(), "mode-0700") {
		t.Fatalf("parent mode error=%v", err)
	}
	secureParent := privateRootTestDirectory(t)
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 1
	if err := PublishCreateExclusive(filepath.Join(secureParent, "root.json"), tampered); err == nil {
		t.Fatal("tampered root body was published")
	}
}

func TestPublishRecoversIncompleteSingleLinkStage(t *testing.T) {
	body := rootBodyForPublication(t)
	parent := privateRootTestDirectory(t)
	path := filepath.Join(parent, "root.json")
	tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
	if err := os.WriteFile(tempPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishCreateExclusive(path, body); err != nil {
		t.Fatalf("recover incomplete root stage: %v", err)
	}
	if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete root stage remains: %v", err)
	}
	published, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(published, body) {
		t.Fatalf("recovered root bytes differ: err=%v", err)
	}
}

func TestPublishRejectsUntrustedStageSymlinkAndHardlinkWithoutVictimWrites(t *testing.T) {
	body := rootBodyForPublication(t)
	t.Run("symlink", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		parent := filepath.Join(base, "private")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(base, "symlink-victim")
		original := []byte("symlink-victim-bytes")
		if err := os.WriteFile(victim, original, 0o600); err != nil {
			t.Fatal(err)
		}
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		if err := os.Symlink(victim, tempPath); err != nil {
			t.Fatal(err)
		}
		if err := PublishCreateExclusive(filepath.Join(parent, "root.json"), body); err == nil {
			t.Fatal("untrusted symlink stage was accepted")
		}
		actual, err := os.ReadFile(victim)
		if err != nil || !bytes.Equal(actual, original) {
			t.Fatalf("symlink stage victim changed: %q err=%v", actual, err)
		}
		if info, err := os.Lstat(tempPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("untrusted symlink stage was removed or replaced: info=%v err=%v", info, err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		parent := filepath.Join(base, "private")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(base, "hardlink-victim")
		original := []byte("hardlink-victim-bytes")
		if err := os.WriteFile(victim, original, 0o600); err != nil {
			t.Fatal(err)
		}
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		if err := os.Link(victim, tempPath); err != nil {
			t.Fatal(err)
		}
		if err := PublishCreateExclusive(filepath.Join(parent, "root.json"), body); err == nil {
			t.Fatal("untrusted hardlink stage was accepted")
		}
		actual, err := os.ReadFile(victim)
		victimInfo, victimErr := os.Lstat(victim)
		tempInfo, tempErr := os.Lstat(tempPath)
		if err != nil || !bytes.Equal(actual, original) || victimErr != nil || tempErr != nil ||
			!os.SameFile(victimInfo, tempInfo) || rootFileLinkCount(victimInfo) != 2 {
			t.Fatalf("hardlink stage victim changed: bytes=%q infos=%v/%v errors=%v/%v/%v", actual, victimInfo, tempInfo, err, victimErr, tempErr)
		}
	})
}

func TestConcurrentRootPublicationIsCreateExclusive(t *testing.T) {
	body := rootBodyForPublication(t)
	parent := privateRootTestDirectory(t)
	path := filepath.Join(parent, "root.json")
	const publishers = 8
	errorsByPublisher := make([]error, publishers)
	var wait sync.WaitGroup
	wait.Add(publishers)
	for index := range errorsByPublisher {
		go func(index int) {
			defer wait.Done()
			errorsByPublisher[index] = PublishCreateExclusive(path, body)
		}(index)
	}
	wait.Wait()
	successes := 0
	alreadyPublished := 0
	for _, err := range errorsByPublisher {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadyPublished):
			alreadyPublished++
		default:
			t.Fatalf("concurrent root publication error=%v", err)
		}
	}
	if successes != 1 || alreadyPublished != publishers-1 {
		t.Fatalf("concurrent root publication successes=%d alreadyPublished=%d", successes, alreadyPublished)
	}
	published, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(published, body) {
		t.Fatalf("concurrently published root differs: err=%v", err)
	}
}

func TestConcurrentConflictingRootPublicationHasOneExactWinner(t *testing.T) {
	firstBody := rootBodyForPublication(t)
	secondRequest := baseRequest(ScopeFinal, currentCatalogCount)
	secondRequest.RootID = "lyrics-root-conflicting"
	secondManifest, err := Assemble(secondRequest, rootResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := MarshalCanonical(secondManifest)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstBody, secondBody) {
		t.Fatal("conflicting publication fixtures unexpectedly match")
	}

	parent := privateRootTestDirectory(t)
	path := filepath.Join(parent, "root.json")
	bodies := [][]byte{firstBody, secondBody, firstBody, secondBody, firstBody, secondBody, firstBody, secondBody}
	errorsByPublisher := make([]error, len(bodies))
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(len(bodies))
	for index := range bodies {
		go func(index int) {
			defer wait.Done()
			<-start
			errorsByPublisher[index] = PublishCreateExclusive(path, bodies[index])
		}(index)
	}
	close(start)
	wait.Wait()

	successIndex := -1
	alreadyPublished := 0
	for index, err := range errorsByPublisher {
		switch {
		case err == nil:
			if successIndex != -1 {
				t.Fatalf("multiple conflicting publishers succeeded: %d and %d", successIndex, index)
			}
			successIndex = index
		case errors.Is(err, ErrAlreadyPublished):
			alreadyPublished++
		default:
			t.Fatalf("conflicting root publication error=%v", err)
		}
	}
	if successIndex == -1 || alreadyPublished != len(bodies)-1 {
		t.Fatalf("conflicting publication successIndex=%d alreadyPublished=%d", successIndex, alreadyPublished)
	}
	published, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(published, bodies[successIndex]) {
		t.Fatalf("conflicting publication did not preserve the winner's exact bytes: err=%v", err)
	}
}

func TestPublishRecoversAtomicCrashPair(t *testing.T) {
	body := rootBodyForPublication(t)
	parent := privateRootTestDirectory(t)
	path := filepath.Join(parent, "root.json")
	testHookAfterLink = func() error { return errors.New("injected root link crash") }
	t.Cleanup(func() { testHookAfterLink = nil })
	if err := PublishCreateExclusive(path, body); err == nil {
		t.Fatal("injected root crash did not fail")
	}
	finalInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
	tempInfo, err := os.Lstat(tempPath)
	if err != nil || !os.SameFile(finalInfo, tempInfo) || rootFileLinkCount(finalInfo) != 2 {
		t.Fatalf("crash pair final=%v temp=%v err=%v", finalInfo, tempInfo, err)
	}
	testHookAfterLink = nil
	if err := PublishCreateExclusive(path, body); err != nil {
		t.Fatalf("recover root crash pair: %v", err)
	}
	if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root crash temp remains: %v", err)
	}
	finalInfo, err = os.Lstat(path)
	if err != nil || rootFileLinkCount(finalInfo) != 1 {
		t.Fatalf("recovered root links=%d err=%v", rootFileLinkCount(finalInfo), err)
	}
}
