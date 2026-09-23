package lyricsoutcomeartifact

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactDirectoryCreationStaysBoundToValidatedParent(t *testing.T) {
	base := privateTempDir(t)
	parent := filepath.Join(base, "parent")
	moved := filepath.Join(base, "validated-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, "outcomes")
	triggered := false
	testHookBeforeFilesystemOperation = func(operation, name string) error {
		if triggered || operation != artifactFSCreate || name != "outcomes" {
			return nil
		}
		triggered = true
		if err := os.Rename(parent, moved); err != nil {
			return err
		}
		return os.Mkdir(parent, 0o700)
	}
	t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
	if err := CreatePrivateDirectory(directory); err == nil {
		t.Fatal("directory creation succeeded after parent substitution")
	}
	if !triggered {
		t.Fatal("directory creation race hook was not reached")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("replacement parent was mutated: entries=%v err=%v", entries, err)
	}
}

func TestArtifactPublicationOperationsStayBoundToLockedDirectory(t *testing.T) {
	artifact := testArtifact(t)
	for _, operation := range []string{artifactFSCreate, artifactFSLink, artifactFSVerify, artifactFSCleanup, artifactFSSync} {
		t.Run(operation, func(t *testing.T) {
			base := privateTempDir(t)
			directory := filepath.Join(base, "outcomes")
			if err := CreatePrivateDirectory(directory); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(base, "validated-outcomes")
			triggered := false
			testHookBeforeFilesystemOperation = func(current, _ string) error {
				if triggered || current != operation {
					return nil
				}
				triggered = true
				if err := os.Rename(directory, moved); err != nil {
					return err
				}
				return os.Mkdir(directory, 0o700)
			}
			t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
			if _, err := PublishCreateExclusive(directory, artifact); err == nil {
				t.Fatal("artifact publication succeeded after directory substitution")
			}
			if !triggered {
				t.Fatalf("operation hook %q was not reached", operation)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("replacement outcome directory was mutated at %q: entries=%v err=%v", operation, entries, err)
			}
		})
	}
}

func TestArtifactCleanupRejectsLateHardlinkWithoutRemovingStage(t *testing.T) {
	base := privateTempDir(t)
	directory := filepath.Join(base, "outcomes")
	if err := CreatePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	artifact := testArtifact(t)
	name, err := FileName(artifact)
	if err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(directory, name)
	tempPath := filepath.Join(directory, "."+name+".tmp")
	aliasPath := filepath.Join(directory, "late-hardlink")
	testHookAfterLink = func() error { return os.Link(finalPath, aliasPath) }
	t.Cleanup(func() { testHookAfterLink = nil })
	if _, err := PublishCreateExclusive(directory, artifact); err == nil {
		t.Fatal("artifact cleanup accepted a late hardlink")
	}
	finalInfo, finalErr := os.Lstat(finalPath)
	tempInfo, tempErr := os.Lstat(tempPath)
	aliasInfo, aliasErr := os.Lstat(aliasPath)
	if finalErr != nil || tempErr != nil || aliasErr != nil || !os.SameFile(finalInfo, tempInfo) ||
		!os.SameFile(finalInfo, aliasInfo) || fileLinkCount(finalInfo) != 3 {
		t.Fatalf("cleanup mutated the raced stage: final=%v temp=%v alias=%v errors=%v/%v/%v",
			finalInfo, tempInfo, aliasInfo, finalErr, tempErr, aliasErr)
	}
}

func TestArtifactVerificationRejectsInodeSwapAtOpenBoundary(t *testing.T) {
	base := privateTempDir(t)
	directory := filepath.Join(base, "outcomes")
	if err := CreatePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	artifact := testArtifact(t)
	path, err := PublishCreateExclusive(directory, artifact)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rescued := filepath.Join(directory, "rescued-artifact")
	triggered := false
	testHookBeforeFilesystemOperation = func(operation, name string) error {
		if triggered || operation != artifactFSVerify || name != filepath.Base(path) {
			return nil
		}
		triggered = true
		if err := os.Rename(path, rescued); err != nil {
			return err
		}
		return os.WriteFile(path, body, 0o600)
	}
	t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
	if _, err := Open(path); err == nil {
		t.Fatal("artifact Open accepted an inode swap between inspection and descriptor open")
	}
	if !triggered {
		t.Fatal("artifact verification race hook was not reached")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("replacement artifact changed: err=%v", err)
	}
}
