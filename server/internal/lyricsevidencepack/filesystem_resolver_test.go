package lyricsevidencepack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
)

func buildTestPack(t *testing.T, itemCount int) (string, Manifest, []lyricscontract.EvidenceRef) {
	t.Helper()
	items := make([]lyricssource.IndexEvidence, 0, itemCount)
	for index := 0; index < itemCount; index++ {
		items = append(items, testEvidence(t, index+1, 32))
	}
	refs := refsFor(items)
	directory := filepath.Join(canonicalTestRoot(t), "pack")
	manifest, err := Build(context.Background(), directory, refs, sliceExactSource{items: items})
	if err != nil {
		t.Fatal(err)
	}
	return directory, manifest, refs
}

func TestPublicationModesAndNoOverwrite(t *testing.T) {
	item := testEvidence(t, 1, 0)
	refs := []lyricscontract.EvidenceRef{evidenceRef(item)}
	directory := filepath.Join(canonicalTestRoot(t), "pack")
	manifest, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}})
	if err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || directoryInfo.Mode().Perm() != 0o700 || !directoryInfo.IsDir() {
		t.Fatalf("directory mode=%v err=%v", directoryInfo, err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(directory, ManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ManifestFileName, mustShardName(t, manifest.Shards[0])} {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || fileLinkCount(info) != 1 {
			t.Fatalf("%s mode=%v links=%d err=%v", name, info, fileLinkCount(info), err)
		}
	}
	if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("second publication error=%v", err)
	}
	manifestAfter, err := os.ReadFile(filepath.Join(directory, ManifestFileName))
	if err != nil || !bytes.Equal(manifestBefore, manifestAfter) {
		t.Fatalf("no-overwrite changed manifest err=%v", err)
	}
}

func TestConcurrentPublicationIsCreateExclusive(t *testing.T) {
	item := testEvidence(t, 1, 0)
	refs := []lyricscontract.EvidenceRef{evidenceRef(item)}
	directory := filepath.Join(canonicalTestRoot(t), "pack")
	const publishers = 8
	errorsByPublisher := make([]error, publishers)
	var wait sync.WaitGroup
	wait.Add(publishers)
	for index := range errorsByPublisher {
		go func() {
			defer wait.Done()
			_, errorsByPublisher[index] = Build(context.Background(), directory, refs, sliceExactSource{
				items: []lyricssource.IndexEvidence{item},
			})
		}()
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
			t.Fatalf("concurrent publication error=%v", err)
		}
	}
	if successes != 1 || alreadyPublished != publishers-1 {
		t.Fatalf("concurrent publication successes=%d alreadyPublished=%d", successes, alreadyPublished)
	}
	if _, err := OpenResolver(directory); err != nil {
		t.Fatalf("concurrently published pack is invalid: %v", err)
	}
}

func TestResolverRejectsTamperSymlinkAndModeDrift(t *testing.T) {
	t.Run("tamper", func(t *testing.T) {
		directory, manifest, _ := buildTestPack(t, 1)
		path := filepath.Join(directory, mustShardName(t, manifest.Shards[0]))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body[len(body)/2] ^= 1
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenResolver(directory); err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("tampered shard error=%v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		directory, manifest, _ := buildTestPack(t, 1)
		name := mustShardName(t, manifest.Shards[0])
		path := filepath.Join(directory, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(canonicalTestRoot(t), "target.json")
		if err := os.WriteFile(target, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenResolver(directory); err == nil || !strings.Contains(err.Error(), "invalid entry") {
			t.Fatalf("symlink shard error=%v", err)
		}
	})
	t.Run("mode", func(t *testing.T) {
		directory, manifest, _ := buildTestPack(t, 1)
		path := filepath.Join(directory, mustShardName(t, manifest.Shards[0]))
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenResolver(directory); err == nil || !strings.Contains(err.Error(), "mode-0600") {
			t.Fatalf("mode drift error=%v", err)
		}
	})
}

func TestPublicationRecoversCrashPairs(t *testing.T) {
	item := testEvidence(t, 1, 0)
	refs := []lyricscontract.EvidenceRef{evidenceRef(item)}
	t.Cleanup(func() {
		testHookAfterLink = nil
		testHookBeforeManifest = nil
	})
	t.Run("incomplete shard stage", func(t *testing.T) {
		referenceDir := filepath.Join(canonicalTestRoot(t), "reference")
		manifest, err := Build(context.Background(), referenceDir, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}})
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		name := mustShardName(t, manifest.Shards[0])
		if err := os.WriteFile(filepath.Join(directory, "."+name+".tmp"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err != nil {
			t.Fatalf("recover incomplete shard stage: %v", err)
		}
		assertNoTemporaryEntries(t, directory)
	})
	t.Run("incomplete manifest stage", func(t *testing.T) {
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "."+ManifestFileName+".tmp"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err != nil {
			t.Fatalf("recover incomplete manifest stage: %v", err)
		}
		assertNoTemporaryEntries(t, directory)
	})
	t.Run("shard link pair", func(t *testing.T) {
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		failed := false
		testHookAfterLink = func(label string) error {
			if !failed && strings.HasPrefix(label, "evidence shard") {
				failed = true
				return errors.New("injected shard link crash")
			}
			return nil
		}
		if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err == nil {
			t.Fatal("injected shard crash did not fail")
		}
		testHookAfterLink = nil
		if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err != nil {
			t.Fatalf("recover shard pair: %v", err)
		}
		assertNoTemporaryEntries(t, directory)
	})
	t.Run("before manifest", func(t *testing.T) {
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		testHookBeforeManifest = func() error { return errors.New("injected pre-manifest crash") }
		if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err == nil {
			t.Fatal("injected pre-manifest crash did not fail")
		}
		testHookBeforeManifest = nil
		if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err != nil {
			t.Fatalf("recover pre-manifest crash: %v", err)
		}
		assertNoTemporaryEntries(t, directory)
	})
	t.Run("manifest link pair", func(t *testing.T) {
		directory := filepath.Join(canonicalTestRoot(t), "pack")
		testHookAfterLink = func(label string) error {
			if label == "evidence pack manifest" {
				return errors.New("injected manifest link crash")
			}
			return nil
		}
		if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err == nil {
			t.Fatal("injected manifest crash did not fail")
		}
		testHookAfterLink = nil
		if _, err := Build(context.Background(), directory, refs, sliceExactSource{items: []lyricssource.IndexEvidence{item}}); err != nil {
			t.Fatalf("recover manifest pair: %v", err)
		}
		assertNoTemporaryEntries(t, directory)
	})
}

func TestResolverValidatesShardsOnceAndHydratesByExactID(t *testing.T) {
	items := []lyricssource.IndexEvidence{
		testEvidence(t, 1, 80), testEvidence(t, 2, 80), testEvidence(t, 3, 80), testEvidence(t, 4, 80),
	}
	refs := refsFor(items)
	directory := filepath.Join(canonicalTestRoot(t), "pack")
	limits := buildLimits{shardRaw: 220, shardEncoded: 4 << 10, totalRaw: 1 << 20, totalEncoded: 1 << 20, items: 16}
	manifest, err := buildWithLimits(context.Background(), directory, refs, sliceExactSource{items: items}, limits)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := OpenResolver(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolver.ValidatedShardCount(); got != len(manifest.Shards) || got < 2 {
		t.Fatalf("validated shard count=%d manifests=%d", got, len(manifest.Shards))
	}
	for iteration := 0; iteration < 100; iteration++ {
		for _, ref := range refs {
			item, err := resolver.HydrateExact(ref)
			if err != nil || item.EvidenceID != ref.EvidenceID {
				t.Fatalf("hydrate %q: item=%+v err=%v", ref.EvidenceID, item, err)
			}
		}
	}
	if got := resolver.ValidatedShardCount(); got != len(manifest.Shards) {
		t.Fatalf("hydration repeated shard validation: %d", got)
	}
	first, err := resolver.HydrateID(refs[0].EvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	first.Raw[0] ^= 1
	first.Categories[0] = "Mutated"
	again, err := resolver.HydrateID(refs[0].EvidenceID)
	if err != nil || bytes.Equal(first.Raw, again.Raw) || again.Categories[0] == "Mutated" {
		t.Fatalf("hydrated defensive copy drift: first=%+v again=%+v err=%v", first, again, err)
	}
	wrong := refs[0]
	wrong.SHA256 = strings.Repeat("f", 64)
	if _, err := resolver.HydrateExact(wrong); err == nil {
		t.Fatal("resolver accepted wrong exact digest")
	}
}

func TestEmptyPackHasExplicitZeroUnion(t *testing.T) {
	directory := filepath.Join(canonicalTestRoot(t), "pack")
	manifest, err := Build(context.Background(), directory, []lyricscontract.EvidenceRef{}, sliceExactSource{items: []lyricssource.IndexEvidence{}})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Totals.ItemCount != 0 || manifest.Totals.ShardCount != 0 || len(manifest.Shards) != 0 {
		t.Fatalf("empty manifest=%+v", manifest)
	}
	resolver, err := OpenResolver(directory)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.ValidatedShardCount() != 0 || resolver.ValidateSelected([]lyricscontract.EvidenceRef{}) != nil {
		t.Fatalf("empty resolver=%+v", resolver)
	}
}

func mustShardName(t *testing.T, shard ShardManifest) string {
	t.Helper()
	name, err := ShardFileName(shard.Ordinal, shard.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func assertNoTemporaryEntries(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary entry remained after recovery: %s", entry.Name())
		}
	}
}
