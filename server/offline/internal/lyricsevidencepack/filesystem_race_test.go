package lyricsevidencepack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
)

func TestEvidencePublicationOperationsStayBoundToLockedDirectory(t *testing.T) {
	item := testEvidence(t, 1, 0)
	refs := []lyricscontract.EvidenceRef{evidenceRef(item)}
	for _, operation := range []string{evidenceFSCreate, evidenceFSLink, evidenceFSVerify, evidenceFSCleanup, evidenceFSSync} {
		t.Run(operation, func(t *testing.T) {
			base := canonicalTestRoot(t)
			directory := filepath.Join(base, "pack")
			moved := filepath.Join(base, "validated-pack")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
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
			if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err == nil {
				t.Fatal("evidence pack publication succeeded after directory substitution")
			}
			if !triggered {
				t.Fatalf("operation hook %q was not reached", operation)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("replacement pack directory was mutated at %q: entries=%v err=%v", operation, entries, err)
			}
		})
	}
}

func TestEvidenceResolverInventoryStaysBoundToLockedDirectory(t *testing.T) {
	directory, _, _ := buildTestPack(t, 1)
	moved := directory + "-validated"
	triggered := false
	testHookBeforeFilesystemOperation = func(operation, _ string) error {
		if triggered || operation != evidenceFSList {
			return nil
		}
		triggered = true
		if err := os.Rename(directory, moved); err != nil {
			return err
		}
		return os.Mkdir(directory, 0o700)
	}
	t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
	if _, err := OpenResolver(directory); err == nil {
		t.Fatal("resolver accepted a substituted pack path during exact inventory")
	}
	if !triggered {
		t.Fatal("inventory race hook was not reached")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("replacement pack directory was listed or mutated: entries=%v err=%v", entries, err)
	}
}

func TestEvidenceBuildRechecksExactInventoryAfterFinalSync(t *testing.T) {
	item := testEvidence(t, 1, 0)
	refs := []lyricscontract.EvidenceRef{evidenceRef(item)}
	directory := filepath.Join(canonicalTestRoot(t), "pack")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	extraPath := filepath.Join(directory, "unexpected-entry")
	listCount := 0
	injected := false
	testHookBeforeFilesystemOperation = func(operation, _ string) error {
		if operation == evidenceFSList {
			listCount++
			return nil
		}
		if operation == evidenceFSSync && listCount >= 2 && !injected {
			injected = true
			return os.WriteFile(extraPath, []byte("unexpected"), 0o600)
		}
		return nil
	}
	t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
	if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err == nil {
		t.Fatal("build accepted an entry injected at the final directory sync boundary")
	}
	if !injected {
		t.Fatal("final inventory sync race hook was not reached")
	}
	if body, err := os.ReadFile(extraPath); err != nil || !bytes.Equal(body, []byte("unexpected")) {
		t.Fatalf("unexpected inventory entry was altered: body=%q err=%v", body, err)
	}
}

func TestEvidenceCleanupRejectsLateHardlinkWithoutRemovingStage(t *testing.T) {
	item := testEvidence(t, 1, 0)
	refs := []lyricscontract.EvidenceRef{evidenceRef(item)}
	plans, _, err := planShards([]lyricssource.IndexEvidence{item}, refs, defaultBuildLimits)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%v err=%v", plans, err)
	}
	name, err := ShardFileName(plans[0].manifest.Ordinal, plans[0].manifest.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(canonicalTestRoot(t), "pack")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(directory, name)
	tempPath := filepath.Join(directory, "."+name+".tmp")
	aliasPath := filepath.Join(directory, "late-hardlink")
	triggered := false
	testHookAfterLink = func(string) error {
		if triggered {
			return nil
		}
		triggered = true
		return os.Link(finalPath, aliasPath)
	}
	t.Cleanup(func() { testHookAfterLink = nil })
	if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err == nil {
		t.Fatal("evidence cleanup accepted a late hardlink")
	}
	finalInfo, finalErr := os.Lstat(finalPath)
	tempInfo, tempErr := os.Lstat(tempPath)
	aliasInfo, aliasErr := os.Lstat(aliasPath)
	if !triggered || finalErr != nil || tempErr != nil || aliasErr != nil || !os.SameFile(finalInfo, tempInfo) ||
		!os.SameFile(finalInfo, aliasInfo) || fileLinkCount(finalInfo) != 3 {
		t.Fatalf("cleanup mutated the raced evidence stage: triggered=%t final=%v temp=%v alias=%v errors=%v/%v/%v",
			triggered, finalInfo, tempInfo, aliasInfo, finalErr, tempErr, aliasErr)
	}
}

func TestEvidencePublicationOperationBoundaryLeafSwapsPreserveVictims(t *testing.T) {
	item := testEvidence(t, 1, 0)
	refs := []lyricscontract.EvidenceRef{evidenceRef(item)}
	plans, _, err := planShards([]lyricssource.IndexEvidence{item}, refs, defaultBuildLimits)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans=%v err=%v", plans, err)
	}
	name, err := ShardFileName(plans[0].manifest.Ordinal, plans[0].manifest.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	var shardBody bytes.Buffer
	if err := writeShard(&shardBody, plans[0].manifest.Ordinal, []lyricssource.IndexEvidence{item}); err != nil {
		t.Fatal(err)
	}
	source := sliceExactSource{items: []lyricssource.IndexEvidence{item}}

	t.Run("create exclusive", func(t *testing.T) {
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		tempPath := filepath.Join(directory, "."+name+".tmp")
		finalPath := filepath.Join(directory, name)
		victim := []byte("create-race-victim")
		testHookBeforeFilesystemOperation = func(operation, currentName string) error {
			if operation == evidenceFSCreate && currentName == filepath.Base(tempPath) {
				testHookBeforeFilesystemOperation = nil
				return os.WriteFile(tempPath, victim, 0o600)
			}
			return nil
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if _, err := Build(context.Background(), directory, refs, source); err == nil {
			t.Fatal("publication accepted a staging create race")
		}
		assertFileBytes(t, tempPath, victim)
		if _, err := os.Lstat(finalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("create race published a final shard: %v", err)
		}
	})

	t.Run("link no overwrite", func(t *testing.T) {
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		finalPath := filepath.Join(directory, name)
		victim := []byte("link-race-victim")
		testHookBeforeFilesystemOperation = func(operation, currentName string) error {
			if operation == evidenceFSLink && currentName == name {
				testHookBeforeFilesystemOperation = nil
				return os.WriteFile(finalPath, victim, 0o600)
			}
			return nil
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if _, err := Build(context.Background(), directory, refs, source); err == nil {
			t.Fatal("publication overwrote a concurrently appearing final shard")
		}
		assertFileBytes(t, finalPath, victim)
	})

	t.Run("same-byte staged inode replacement", func(t *testing.T) {
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		tempPath := filepath.Join(directory, "."+name+".tmp")
		rescuedPath := filepath.Join(directory, "rescued-stage")
		finalPath := filepath.Join(directory, name)
		triggered := false
		testHookBeforeFilesystemOperation = func(operation, currentName string) error {
			if triggered || operation != evidenceFSLink || currentName != name {
				return nil
			}
			triggered = true
			if err := os.Rename(tempPath, rescuedPath); err != nil {
				return err
			}
			return os.WriteFile(tempPath, shardBody.Bytes(), 0o600)
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if _, err := Build(context.Background(), directory, refs, source); err == nil {
			t.Fatal("publication accepted a same-byte staging inode replacement")
		}
		if !triggered {
			t.Fatal("same-byte staged replacement hook was not reached")
		}
		assertFileBytes(t, tempPath, shardBody.Bytes())
		if _, err := os.Lstat(finalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("same-byte staged replacement published a final shard: %v", err)
		}
	})

	t.Run("cleanup inode replacement", func(t *testing.T) {
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		tempPath := filepath.Join(directory, "."+name+".tmp")
		rescuedPath := filepath.Join(directory, "rescued-stage")
		victim := []byte("cleanup-race-victim")
		testHookBeforeFilesystemOperation = func(operation, currentName string) error {
			if operation == evidenceFSCleanup && currentName == filepath.Base(tempPath) {
				testHookBeforeFilesystemOperation = nil
				if err := os.Rename(tempPath, rescuedPath); err != nil {
					return err
				}
				return os.WriteFile(tempPath, victim, 0o600)
			}
			return nil
		}
		t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
		if _, err := Build(context.Background(), directory, refs, source); err == nil {
			t.Fatal("publication cleanup unlinked a replacement victim")
		}
		assertFileBytes(t, tempPath, victim)
	})
}

func TestEvidenceVerificationRejectsSameByteInodeReplacementAtOpenBoundary(t *testing.T) {
	directory, _, _ := buildTestPack(t, 1)
	path := filepath.Join(directory, ManifestFileName)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rescued := filepath.Join(directory, "rescued-manifest")
	triggered := false
	testHookBeforeFilesystemOperation = func(operation, name string) error {
		if triggered || operation != evidenceFSVerify || name != ManifestFileName {
			return nil
		}
		triggered = true
		if err := os.Rename(path, rescued); err != nil {
			return err
		}
		return os.WriteFile(path, body, 0o600)
	}
	t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
	if _, err := OpenResolver(directory); err == nil {
		t.Fatal("resolver accepted a manifest inode swap between inspection and descriptor open")
	}
	if !triggered {
		t.Fatal("manifest verification race hook was not reached")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("replacement manifest changed: err=%v", err)
	}
}

func TestEvidenceRejectsLexicalParentAliases(t *testing.T) {
	item := testEvidence(t, 1, 0)
	refs := []lyricscontract.EvidenceRef{evidenceRef(item)}
	source := sliceExactSource{items: []lyricssource.IndexEvidence{item}}

	t.Run("direct parent symlink", func(t *testing.T) {
		root := canonicalTestRoot(t)
		targetParent := filepath.Join(root, "target-parent")
		aliasParent := filepath.Join(root, "alias-parent")
		if err := os.Mkdir(targetParent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(targetParent, aliasParent); err != nil {
			t.Fatal(err)
		}
		if _, err := Build(context.Background(), filepath.Join(aliasParent, "pack"), refs, source); err == nil ||
			!strings.Contains(err.Error(), "lexical parent") {
			t.Fatalf("parent symlink error=%v", err)
		}
		assertDirectoryEmpty(t, targetParent, "parent-symlink target")
	})

	t.Run("resolved ancestor alias", func(t *testing.T) {
		root := canonicalTestRoot(t)
		realAncestor := filepath.Join(root, "real-ancestor")
		realParent := filepath.Join(realAncestor, "parent")
		aliasAncestor := filepath.Join(root, "alias-ancestor")
		if err := os.Mkdir(realAncestor, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realAncestor, aliasAncestor); err != nil {
			t.Fatal(err)
		}
		if _, err := Build(context.Background(), filepath.Join(aliasAncestor, "parent", "pack"), refs, source); err == nil ||
			!strings.Contains(err.Error(), "lexical parent") {
			t.Fatalf("resolved ancestor alias error=%v", err)
		}
		assertDirectoryEmpty(t, realParent, "resolved-alias target")
	})

	t.Run("resolver resolved parent alias", func(t *testing.T) {
		root := canonicalTestRoot(t)
		realParent := filepath.Join(root, "real-parent")
		aliasParent := filepath.Join(root, "alias-parent")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(realParent, "pack")
		if _, err := Build(context.Background(), directory, refs, source); err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(directory, ManifestFileName)
		before, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, aliasParent); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenResolver(filepath.Join(aliasParent, "pack")); err == nil ||
			!strings.Contains(err.Error(), "lexical parent") {
			t.Fatalf("resolver parent alias error=%v", err)
		}
		after, err := os.ReadFile(manifestPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("resolver alias attempt changed the reviewed manifest: err=%v", err)
		}
	})
}

func TestEvidencePublicationRejectsAncestorSwapBeforeFirstWrite(t *testing.T) {
	item := testEvidence(t, 1, 0)
	root := canonicalTestRoot(t)
	reviewed := filepath.Join(root, "reviewed")
	parent := filepath.Join(reviewed, "parent")
	directory := filepath.Join(parent, "pack")
	moved := filepath.Join(root, "moved-reviewed")
	if err := os.Mkdir(reviewed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	triggered := false
	testHookBeforeFilesystemOperation = func(operation, name string) error {
		if triggered || operation != evidenceFSCreate || !strings.HasSuffix(name, ".tmp") {
			return nil
		}
		triggered = true
		if err := os.Rename(reviewed, moved); err != nil {
			return err
		}
		if err := os.Mkdir(reviewed, 0o700); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(reviewed, "parent"), 0o700); err != nil {
			return err
		}
		return os.Mkdir(filepath.Join(reviewed, "parent", "pack"), 0o700)
	}
	t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
	if _, err := Build(context.Background(), directory, []lyricscontract.EvidenceRef{evidenceRef(item)}, sliceExactSource{
		items: []lyricssource.IndexEvidence{item},
	}); err == nil {
		t.Fatal("publication accepted an ancestor swap")
	}
	if !triggered {
		t.Fatal("ancestor-swap hook was not reached")
	}
	assertDirectoryEmpty(t, filepath.Join(moved, "parent", "pack"), "detached reviewed pack")
	assertDirectoryEmpty(t, directory, "replacement pack")
}

func TestEvidencePublicationRejectsLeafSwapBeforeFirstWrite(t *testing.T) {
	item := testEvidence(t, 1, 0)
	root := canonicalTestRoot(t)
	parent := filepath.Join(root, "parent")
	directory := filepath.Join(parent, "pack")
	moved := filepath.Join(parent, "moved-pack")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	triggered := false
	testHookBeforeFilesystemOperation = func(operation, name string) error {
		if triggered || operation != evidenceFSCreate || !strings.HasSuffix(name, ".tmp") {
			return nil
		}
		triggered = true
		if err := os.Rename(directory, moved); err != nil {
			return err
		}
		return os.Mkdir(directory, 0o700)
	}
	t.Cleanup(func() { testHookBeforeFilesystemOperation = nil })
	if _, err := Build(context.Background(), directory, []lyricscontract.EvidenceRef{evidenceRef(item)}, sliceExactSource{
		items: []lyricssource.IndexEvidence{item},
	}); err == nil {
		t.Fatal("publication accepted a leaf-directory swap")
	}
	if !triggered {
		t.Fatal("leaf-swap hook was not reached")
	}
	assertDirectoryEmpty(t, moved, "detached pack leaf")
	assertDirectoryEmpty(t, directory, "replacement pack leaf")
}

func TestEvidencePublicationNeverWritesThroughLeafSymlinkOutsideReviewedTree(t *testing.T) {
	item := testEvidence(t, 1, 0)
	root := canonicalTestRoot(t)
	reviewed := filepath.Join(root, "reviewed")
	outside := filepath.Join(root, "outside-reviewed-tree")
	if err := os.Mkdir(reviewed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(reviewed, "pack")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), filepath.Join(reviewed, "pack"), []lyricscontract.EvidenceRef{evidenceRef(item)}, sliceExactSource{
		items: []lyricssource.IndexEvidence{item},
	}); err == nil {
		t.Fatal("publication followed an output leaf symlink")
	}
	assertDirectoryEmpty(t, outside, "outside reviewed tree")
}

func assertDirectoryEmpty(t *testing.T, path, label string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("%s was mutated: entries=%v err=%v", label, entries, err)
	}
}

func assertFileBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("race victim %s changed: body=%q err=%v", path, actual, err)
	}
}
