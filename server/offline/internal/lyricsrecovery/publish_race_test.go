package lyricsrecovery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryPublicationOperationsStayBoundToLockedParent(t *testing.T) {
	result := testCompleteSongResult(t)
	name, err := SongResultFileName(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{recoveryFSCreate, recoveryFSLink, recoveryFSVerify, recoveryFSCleanup, recoveryFSSync} {
		t.Run(operation, func(t *testing.T) {
			base := privateRecoveryTempDir(t)
			parent := filepath.Join(base, "results")
			moved := filepath.Join(base, "validated-results")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, name)
			triggered := false
			testHookBeforePublishFSOperation = func(current, _ string) error {
				if triggered || current != operation {
					return nil
				}
				triggered = true
				if err := os.Rename(parent, moved); err != nil {
					return err
				}
				return os.Mkdir(parent, 0o700)
			}
			t.Cleanup(func() { testHookBeforePublishFSOperation = nil })
			if err := PublishSongResult(path, result); err == nil {
				t.Fatal("recovery publication succeeded after parent substitution")
			}
			if !triggered {
				t.Fatalf("operation hook %q was not reached", operation)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("replacement recovery parent was mutated at %q: entries=%v err=%v", operation, entries, err)
			}
		})
	}
}

func TestRecoveryCleanupRejectsLateHardlinkWithoutRemovingStage(t *testing.T) {
	result := testCompleteSongResult(t)
	name, err := SongResultFileName(result)
	if err != nil {
		t.Fatal(err)
	}
	parent := privateRecoveryTempDir(t)
	path := filepath.Join(parent, name)
	tempPath := filepath.Join(parent, "."+name+".lyrics-recovery-v2.tmp")
	aliasPath := filepath.Join(parent, "late-hardlink")
	testHookAfterPublishLink = func() error { return os.Link(path, aliasPath) }
	t.Cleanup(func() { testHookAfterPublishLink = nil })
	if err := PublishSongResult(path, result); err == nil {
		t.Fatal("recovery cleanup accepted a late hardlink")
	}
	finalInfo, finalErr := os.Lstat(path)
	tempInfo, tempErr := os.Lstat(tempPath)
	aliasInfo, aliasErr := os.Lstat(aliasPath)
	if finalErr != nil || tempErr != nil || aliasErr != nil || !os.SameFile(finalInfo, tempInfo) ||
		!os.SameFile(finalInfo, aliasInfo) || privateLinkCount(finalInfo) != 3 {
		t.Fatalf("cleanup mutated the raced recovery stage: final=%v temp=%v alias=%v errors=%v/%v/%v",
			finalInfo, tempInfo, aliasInfo, finalErr, tempErr, aliasErr)
	}
}

func TestRecoveryVerificationRejectsInodeSwapAtOpenBoundary(t *testing.T) {
	result := testCompleteSongResult(t)
	name, err := SongResultFileName(result)
	if err != nil {
		t.Fatal(err)
	}
	parent := privateRecoveryTempDir(t)
	path := filepath.Join(parent, name)
	if err := PublishSongResult(path, result); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rescued := filepath.Join(parent, "rescued-result")
	triggered := false
	testHookBeforePublishFSOperation = func(operation, current string) error {
		if triggered || operation != recoveryFSVerify || current != name {
			return nil
		}
		triggered = true
		if err := os.Rename(path, rescued); err != nil {
			return err
		}
		return os.WriteFile(path, body, 0o600)
	}
	t.Cleanup(func() { testHookBeforePublishFSOperation = nil })
	if _, err := OpenSongResult(path); err == nil {
		t.Fatal("recovery Open accepted an inode swap between inspection and descriptor open")
	}
	if !triggered {
		t.Fatal("recovery verification race hook was not reached")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("replacement recovery result changed: err=%v", err)
	}
}
