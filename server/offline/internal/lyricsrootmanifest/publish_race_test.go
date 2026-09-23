package lyricsrootmanifest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type rootEntrySnapshot struct {
	info   os.FileInfo
	body   []byte
	target string
}

type rootDirectorySnapshot map[string]rootEntrySnapshot

func snapshotRootDirectory(t *testing.T, path string) rootDirectorySnapshot {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(rootDirectorySnapshot, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		info, err := os.Lstat(entryPath)
		if err != nil {
			t.Fatal(err)
		}
		item := rootEntrySnapshot{info: info}
		switch {
		case info.Mode().IsRegular():
			item.body, err = os.ReadFile(entryPath)
		case info.Mode()&os.ModeSymlink != 0:
			item.target, err = os.Readlink(entryPath)
		}
		if err != nil {
			t.Fatal(err)
		}
		snapshot[entry.Name()] = item
	}
	return snapshot
}

func assertRootDirectorySnapshot(t *testing.T, path string, expected rootDirectorySnapshot) {
	t.Helper()
	actual := snapshotRootDirectory(t, path)
	if len(actual) != len(expected) {
		t.Fatalf("directory %s entry count changed: got=%d want=%d", path, len(actual), len(expected))
	}
	for name, want := range expected {
		got, found := actual[name]
		if !found {
			t.Fatalf("directory %s lost entry %q", path, name)
		}
		if !os.SameFile(want.info, got.info) || want.info.Mode() != got.info.Mode() ||
			rootFileLinkCount(want.info) != rootFileLinkCount(got.info) || !bytes.Equal(want.body, got.body) || want.target != got.target {
			t.Fatalf("directory %s entry %q changed", path, name)
		}
	}
}

func assertRootDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory %s was mutated: entries=%v err=%v", path, entries, err)
	}
}

func TestPublishFilesystemOperationsRejectParentSwapBeforeAnyOutsideMutation(t *testing.T) {
	body := rootBodyForPublication(t)
	for _, operation := range []string{rootFSCreate, rootFSWrite, rootFSLink, rootFSVerify, rootFSCleanup, rootFSSync} {
		t.Run(operation, func(t *testing.T) {
			base := canonicalRootTestBase(t)
			parent := filepath.Join(base, "parent")
			moved := filepath.Join(base, "validated-parent")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "root.json")
			triggered := false
			var outsideSnapshot rootDirectorySnapshot
			testHookBeforeFilesystemOperation = func(current, _ string) error {
				if triggered || current != operation {
					return nil
				}
				triggered = true
				if err := os.Rename(parent, moved); err != nil {
					return err
				}
				outsideSnapshot = snapshotRootDirectory(t, moved)
				return os.Mkdir(parent, 0o700)
			}
			t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
			if err := PublishCreateExclusive(path, body); err == nil {
				t.Fatal("publication succeeded after its pinned parent path was substituted")
			}
			if !triggered {
				t.Fatalf("operation hook %q was not reached", operation)
			}
			assertRootDirectoryEmpty(t, parent)
			assertRootDirectorySnapshot(t, moved, outsideSnapshot)
		})
	}
}

func TestPublishFilesystemOperationsRejectAncestorSwapBeforeAnyOutsideMutation(t *testing.T) {
	body := rootBodyForPublication(t)
	for _, operation := range []string{rootFSCreate, rootFSWrite, rootFSLink, rootFSCleanup} {
		t.Run(operation, func(t *testing.T) {
			base := canonicalRootTestBase(t)
			ancestor := filepath.Join(base, "ancestor")
			parent := filepath.Join(ancestor, "parent")
			movedAncestor := filepath.Join(base, "validated-ancestor")
			movedParent := filepath.Join(movedAncestor, "parent")
			if err := os.Mkdir(ancestor, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			triggered := false
			var outsideSnapshot rootDirectorySnapshot
			testHookBeforeFilesystemOperation = func(current, _ string) error {
				if triggered || current != operation {
					return nil
				}
				triggered = true
				if err := os.Rename(ancestor, movedAncestor); err != nil {
					return err
				}
				outsideSnapshot = snapshotRootDirectory(t, movedParent)
				if err := os.Mkdir(ancestor, 0o700); err != nil {
					return err
				}
				return os.Mkdir(parent, 0o700)
			}
			t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
			if err := PublishCreateExclusive(filepath.Join(parent, "root.json"), body); err == nil {
				t.Fatal("publication succeeded after a pinned ancestor was substituted")
			}
			if !triggered {
				t.Fatalf("operation hook %q was not reached", operation)
			}
			assertRootDirectoryEmpty(t, parent)
			assertRootDirectorySnapshot(t, movedParent, outsideSnapshot)
		})
	}
}

func TestPublishToleratesLinkCountChurnOnPinnedDirectories(t *testing.T) {
	body := rootBodyForPublication(t)
	t.Run("ancestor", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		ancestor := filepath.Join(base, "ancestor")
		parent := filepath.Join(ancestor, "parent")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		churn := filepath.Join(base, "unrelated-directory")
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSOpen || name != ancestor {
				return nil
			}
			triggered = true
			return os.Mkdir(churn, 0o700)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		path := filepath.Join(parent, "root.json")
		if err := PublishCreateExclusive(path, body); err != nil {
			t.Fatalf("publish across unrelated ancestor link-count churn: %v", err)
		}
		if !triggered {
			t.Fatal("ancestor link-count churn hook was not reached")
		}
		assertRootRaceBytes(t, path, body)
		if info, err := os.Lstat(churn); err != nil || !info.IsDir() {
			t.Fatalf("unrelated ancestor entry changed: info=%v err=%v", info, err)
		}
	})

	t.Run("private parent before lock", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		ancestor := filepath.Join(base, "ancestor")
		parent := filepath.Join(ancestor, "parent")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		churn := filepath.Join(parent, "concurrent-owned-directory")
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSOpen || name != parent {
				return nil
			}
			triggered = true
			return os.Mkdir(churn, 0o700)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		path := filepath.Join(parent, "root.json")
		if err := PublishCreateExclusive(path, body); err != nil {
			t.Fatalf("publish across pre-lock parent link-count churn: %v", err)
		}
		if !triggered {
			t.Fatal("private parent link-count churn hook was not reached")
		}
		assertRootRaceBytes(t, path, body)
		if info, err := os.Lstat(churn); err != nil || !info.IsDir() {
			t.Fatalf("concurrent parent entry changed: info=%v err=%v", info, err)
		}
	})
}

func TestPublishPinnedTraversalRejectsParentAncestorAndSymlinkSwaps(t *testing.T) {
	body := rootBodyForPublication(t)
	t.Run("parent", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		ancestor := filepath.Join(base, "ancestor")
		parent := filepath.Join(ancestor, "parent")
		replacementParent := filepath.Join(ancestor, "replacement-parent")
		movedParent := filepath.Join(ancestor, "moved-parent")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(replacementParent, 0o700); err != nil {
			t.Fatal(err)
		}
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSOpen || name != parent {
				return nil
			}
			triggered = true
			if err := os.Rename(parent, movedParent); err != nil {
				return err
			}
			return os.Rename(replacementParent, parent)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(filepath.Join(parent, "root.json"), body); err == nil {
			t.Fatal("parent swap during pinned traversal was accepted")
		}
		if !triggered {
			t.Fatal("parent traversal hook was not reached")
		}
		assertRootDirectoryEmpty(t, parent)
		assertRootDirectoryEmpty(t, movedParent)
	})

	t.Run("ancestor", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		ancestor := filepath.Join(base, "ancestor")
		parent := filepath.Join(ancestor, "parent")
		replacementAncestor := filepath.Join(base, "replacement-ancestor")
		replacementParent := filepath.Join(replacementAncestor, "parent")
		movedAncestor := filepath.Join(base, "moved-ancestor")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(replacementAncestor, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(replacementParent, 0o700); err != nil {
			t.Fatal(err)
		}
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSOpen || name != ancestor {
				return nil
			}
			triggered = true
			if err := os.Rename(ancestor, movedAncestor); err != nil {
				return err
			}
			return os.Rename(replacementAncestor, ancestor)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(filepath.Join(parent, "root.json"), body); err == nil {
			t.Fatal("ancestor swap during pinned traversal was accepted")
		}
		if !triggered {
			t.Fatal("ancestor traversal hook was not reached")
		}
		assertRootDirectoryEmpty(t, filepath.Join(ancestor, "parent"))
		assertRootDirectoryEmpty(t, filepath.Join(movedAncestor, "parent"))
	})

	t.Run("ancestor symlink", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		ancestor := filepath.Join(base, "ancestor")
		parent := filepath.Join(ancestor, "parent")
		movedAncestor := filepath.Join(base, "moved-ancestor")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSOpen || name != ancestor {
				return nil
			}
			triggered = true
			if err := os.Rename(ancestor, movedAncestor); err != nil {
				return err
			}
			return os.Symlink(movedAncestor, ancestor)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(filepath.Join(parent, "root.json"), body); err == nil {
			t.Fatal("ancestor symlink swap during pinned traversal was accepted")
		}
		if !triggered {
			t.Fatal("ancestor symlink traversal hook was not reached")
		}
		assertRootDirectoryEmpty(t, filepath.Join(movedAncestor, "parent"))
	})
}

func TestPublishLeafBoundaryRacesRejectSwapsSymlinksHardlinksAndSameBytes(t *testing.T) {
	body := rootBodyForPublication(t)
	t.Run("create exclusive", func(t *testing.T) {
		parent := privateRootTestDirectory(t)
		path := filepath.Join(parent, "root.json")
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		victim := []byte("create-race-victim")
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if operation == rootFSCreate && name == filepath.Base(tempPath) {
				testHookBeforeFilesystemOperation = nil
				return os.WriteFile(tempPath, victim, 0o600)
			}
			return nil
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("creation race was accepted")
		}
		assertRootRaceBytes(t, tempPath, victim)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("creation race published final root: %v", err)
		}
	})

	t.Run("link no overwrite", func(t *testing.T) {
		parent := privateRootTestDirectory(t)
		path := filepath.Join(parent, "root.json")
		victim := []byte("link-race-victim")
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if operation == rootFSLink && name == filepath.Base(path) {
				testHookBeforeFilesystemOperation = nil
				return os.WriteFile(path, victim, 0o600)
			}
			return nil
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); !errors.Is(err, ErrAlreadyPublished) {
			t.Fatalf("link race error=%v", err)
		}
		assertRootRaceBytes(t, path, victim)
	})

	t.Run("link staged same-byte inode swap", func(t *testing.T) {
		parent := privateRootTestDirectory(t)
		path := filepath.Join(parent, "root.json")
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		rescuedPath := filepath.Join(parent, "rescued-stage")
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSLink || name != filepath.Base(path) {
				return nil
			}
			triggered = true
			if err := os.Rename(tempPath, rescuedPath); err != nil {
				return err
			}
			return os.WriteFile(tempPath, body, 0o600)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("link accepted a same-byte replacement staging inode")
		}
		if !triggered {
			t.Fatal("link staging race hook was not reached")
		}
		assertRootRaceBytes(t, tempPath, body)
		assertRootRaceBytes(t, rescuedPath, body)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging inode swap created a final root: %v", err)
		}
	})

	t.Run("stage verification same-byte inode swap", func(t *testing.T) {
		parent := privateRootTestDirectory(t)
		path := filepath.Join(parent, "root.json")
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		rescuedPath := filepath.Join(parent, "rescued-stage")
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSVerify || name != filepath.Base(tempPath) {
				return nil
			}
			triggered = true
			if err := os.Rename(tempPath, rescuedPath); err != nil {
				return err
			}
			return os.WriteFile(tempPath, body, 0o600)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("verification accepted a same-byte replacement inode")
		}
		if !triggered {
			t.Fatal("verification race hook was not reached")
		}
		assertRootRaceBytes(t, tempPath, body)
		assertRootRaceBytes(t, rescuedPath, body)
	})

	t.Run("cleanup same-byte inode swap", func(t *testing.T) {
		parent := privateRootTestDirectory(t)
		path := filepath.Join(parent, "root.json")
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		rescuedPath := filepath.Join(parent, "rescued-stage")
		if err := os.WriteFile(tempPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(tempPath, path); err != nil {
			t.Fatal(err)
		}
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if operation == rootFSCleanup && name == filepath.Base(tempPath) {
				testHookBeforeFilesystemOperation = nil
				if err := os.Rename(tempPath, rescuedPath); err != nil {
					return err
				}
				return os.WriteFile(tempPath, body, 0o600)
			}
			return nil
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("cleanup accepted a same-byte replacement inode")
		}
		assertRootRaceBytes(t, tempPath, body)
		assertRootRaceBytes(t, rescuedPath, body)
		assertRootRaceBytes(t, path, body)
	})

	t.Run("final verification same-byte inode swap", func(t *testing.T) {
		parent := privateRootTestDirectory(t)
		path := filepath.Join(parent, "root.json")
		rescuedPath := filepath.Join(parent, "rescued-final")
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSVerify || name != filepath.Base(path) {
				return nil
			}
			triggered = true
			if err := os.Rename(path, rescuedPath); err != nil {
				return err
			}
			return os.WriteFile(path, body, 0o600)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("final verification accepted a same-byte replacement inode")
		}
		if !triggered {
			t.Fatal("final verification hook was not reached")
		}
		assertRootRaceBytes(t, path, body)
		assertRootRaceBytes(t, rescuedPath, body)
	})

	t.Run("linked pair sync same-byte final swap", func(t *testing.T) {
		parent := privateRootTestDirectory(t)
		path := filepath.Join(parent, "root.json")
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		rescuedPath := filepath.Join(parent, "rescued-linked-final")
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSSync || name != filepath.Base(path) {
				return nil
			}
			triggered = true
			if err := os.Rename(path, rescuedPath); err != nil {
				return err
			}
			return os.WriteFile(path, body, 0o600)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("linked-pair sync accepted a same-byte final inode swap")
		}
		if !triggered {
			t.Fatal("linked-pair sync hook was not reached")
		}
		assertRootRaceBytes(t, path, body)
		assertRootRaceBytes(t, rescuedPath, body)
		assertRootRaceBytes(t, tempPath, body)
	})

	t.Run("committed sync same-byte final swap", func(t *testing.T) {
		parent := privateRootTestDirectory(t)
		path := filepath.Join(parent, "root.json")
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		rescuedPath := filepath.Join(parent, "rescued-committed-final")
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if triggered || operation != rootFSSync || name != filepath.Base(tempPath) {
				return nil
			}
			if _, finalErr := os.Lstat(path); finalErr != nil {
				return nil
			}
			if _, tempErr := os.Lstat(tempPath); !errors.Is(tempErr, os.ErrNotExist) {
				return nil
			}
			triggered = true
			if err := os.Rename(path, rescuedPath); err != nil {
				return err
			}
			return os.WriteFile(path, body, 0o600)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("committed sync accepted a same-byte final inode swap")
		}
		if !triggered {
			t.Fatal("committed sync hook was not reached")
		}
		assertRootRaceBytes(t, path, body)
		assertRootRaceBytes(t, rescuedPath, body)
		if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("committed sync recreated the stage: %v", err)
		}
	})

	t.Run("symlink before write", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		parent := filepath.Join(base, "private")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "root.json")
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		rescuedPath := filepath.Join(parent, "rescued-empty-stage")
		victimPath := filepath.Join(base, "outside-victim")
		victim := []byte("outside-victim-bytes")
		if err := os.WriteFile(victimPath, victim, 0o600); err != nil {
			t.Fatal(err)
		}
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if operation == rootFSWrite && name == filepath.Base(tempPath) {
				testHookBeforeFilesystemOperation = nil
				if err := os.Rename(tempPath, rescuedPath); err != nil {
					return err
				}
				return os.Symlink(victimPath, tempPath)
			}
			return nil
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("symlink stage swap before write was accepted")
		}
		assertRootRaceBytes(t, victimPath, victim)
		if info, err := os.Lstat(rescuedPath); err != nil || info.Size() != 0 {
			t.Fatalf("rescued pre-write stage changed: info=%v err=%v", info, err)
		}
	})

	t.Run("hardlink before write", func(t *testing.T) {
		base := canonicalRootTestBase(t)
		parent := filepath.Join(base, "private")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "root.json")
		tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
		outsideAlias := filepath.Join(base, "outside-hardlink")
		testHookBeforeFilesystemOperation = func(operation, name string) error {
			if operation == rootFSWrite && name == filepath.Base(tempPath) {
				testHookBeforeFilesystemOperation = nil
				return os.Link(tempPath, outsideAlias)
			}
			return nil
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if err := PublishCreateExclusive(path, body); err == nil {
			t.Fatal("hardlink stage race before write was accepted")
		}
		tempInfo, tempErr := os.Lstat(tempPath)
		aliasInfo, aliasErr := os.Lstat(outsideAlias)
		if tempErr != nil || aliasErr != nil || !os.SameFile(tempInfo, aliasInfo) || tempInfo.Size() != 0 ||
			rootFileLinkCount(tempInfo) != 2 {
			t.Fatalf("hardlink race wrote outside root: temp=%v alias=%v errors=%v/%v", tempInfo, aliasInfo, tempErr, aliasErr)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("hardlink pre-write race published final root: %v", err)
		}
	})
}

func TestPublishCleanupRejectsLateHardlinkWithoutRemovingStage(t *testing.T) {
	body := rootBodyForPublication(t)
	parent := privateRootTestDirectory(t)
	path := filepath.Join(parent, "root.json")
	tempPath := filepath.Join(parent, ".root.json.lyrics-root-v1.tmp")
	aliasPath := filepath.Join(canonicalRootTestBase(t), "late-hardlink")
	testHookAfterLink = func() error { return os.Link(path, aliasPath) }
	t.Cleanup(func() { testHookAfterLink = nil })
	if err := PublishCreateExclusive(path, body); err == nil {
		t.Fatal("cleanup accepted a late hardlink")
	}
	finalInfo, finalErr := os.Lstat(path)
	tempInfo, tempErr := os.Lstat(tempPath)
	aliasInfo, aliasErr := os.Lstat(aliasPath)
	if finalErr != nil || tempErr != nil || aliasErr != nil || !os.SameFile(finalInfo, tempInfo) ||
		!os.SameFile(finalInfo, aliasInfo) || rootFileLinkCount(finalInfo) != 3 {
		t.Fatalf("cleanup mutated the raced root stage: final=%v temp=%v alias=%v errors=%v/%v/%v",
			finalInfo, tempInfo, aliasInfo, finalErr, tempErr, aliasErr)
	}
}

func assertRootRaceBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("race victim %s changed: size=%d want=%d err=%v", path, len(actual), len(expected), err)
	}
}
