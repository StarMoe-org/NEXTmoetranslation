package lyricsevidencepack

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsacquisition"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func canonicalTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(resolved)
	owner, ownerOK := fileOwner(info)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 ||
		!ownerOK || int(owner) != os.Geteuid() {
		t.Fatalf("canonical test root is not a direct effective-UID-owned mode-0700 directory: info=%v err=%v", info, err)
	}
	return resolved
}

type sliceExactSource struct {
	items []lyricssource.IndexEvidence
}

func (source sliceExactSource) ReplayByAcquisitionID(
	_ context.Context,
	acquisitionID lyricsacquisition.AcquisitionID,
) (lyricsacquisition.Acquisition, error) {
	for _, item := range source.items {
		acquired := testAcquisition(item)
		if acquired.AcquisitionID == acquisitionID {
			return acquired, nil
		}
	}
	return lyricsacquisition.Acquisition{}, lyricsacquisition.ErrAcquisitionNotFound
}

type fixedExactSource struct {
	acquired lyricsacquisition.Acquisition
}

func (source fixedExactSource) ReplayByAcquisitionID(
	context.Context,
	lyricsacquisition.AcquisitionID,
) (lyricsacquisition.Acquisition, error) {
	return source.acquired, nil
}

func testAcquisition(item lyricssource.IndexEvidence) lyricsacquisition.Acquisition {
	envelope, _ := json.Marshal(item)
	envelopeDigest := sha256.Sum256(envelope)
	acquisitionDigest := sha256.Sum256([]byte("test-acquisition-v1\x00" + item.EvidenceID))
	return lyricsacquisition.Acquisition{
		AcquisitionID: lyricsacquisition.AcquisitionID(hex.EncodeToString(acquisitionDigest[:])),
		Request:       lyricsacquisition.Request{Provider: string(item.Provider)},
		Evidence: lyricsacquisition.EvidenceProjection{
			EvidenceID: item.EvidenceID, Raw: append([]byte(nil), item.Raw...), RawSHA256: item.RawSHA256,
		},
		EvidenceEnvelope: append([]byte(nil), envelope...), EvidenceEnvelopeSHA256: hex.EncodeToString(envelopeDigest[:]),
		ReplayOnly: true,
	}
}

func testEvidence(t *testing.T, index, extraRawBytes int) lyricssource.IndexEvidence {
	t.Helper()
	pageID := 1000 + index
	revisionID := 2000 + index
	title := fmt.Sprintf("Evidence %06d", index)
	raw := []byte(fmt.Sprintf("exact-evidence-%06d-", index) + strings.Repeat("x", extraRawBytes))
	rawDigest := sha256.Sum256(raw)
	rawSHA256 := hex.EncodeToString(rawDigest[:])
	contentSHA1 := sha1.Sum(raw)
	fetchedAt := "2026-08-01T00:00:00Z"
	canonical := url.URL{Scheme: "https", Host: "vocaloid.fandom.com", Path: "/wiki/" + strings.ReplaceAll(title, " ", "_")}
	query := canonical.Query()
	query.Set("oldid", fmt.Sprintf("%d", revisionID))
	canonical.RawQuery = query.Encode()
	item := lyricssource.IndexEvidence{
		EvidenceID: lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
			model.LyricsSourceProviderVocaloidFandom, fmt.Sprintf("fetch:vocaloid-fandom:%d", pageID), fetchedAt, rawSHA256,
		),
		SHA256: rawSHA256, Kind: lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: model.LyricsSourceProviderVocaloidFandom, Origin: model.LyricsSourceOriginVocaloidFandom,
		PageID: pageID, RevisionID: revisionID, MediaWikiSHA1: hex.EncodeToString(contentSHA1[:]),
		Title: title, CanonicalURL: canonical.String(), Categories: []string{"Lyrics"}, FetchedAt: fetchedAt,
		Raw: raw, RawSHA256: rawSHA256,
	}
	if err := lyricssource.ValidateIndexEvidenceEnvelope(item); err != nil {
		t.Fatalf("test evidence is invalid: %v", err)
	}
	return item
}

func evidenceRef(item lyricssource.IndexEvidence) lyricscontract.EvidenceRef {
	ref, err := EvidenceRefFromAcquisition(testAcquisition(item))
	if err != nil {
		panic(err)
	}
	return ref
}

func refsFor(items []lyricssource.IndexEvidence) []lyricscontract.EvidenceRef {
	refs := make([]lyricscontract.EvidenceRef, len(items))
	for index, item := range items {
		refs[index] = evidenceRef(item)
	}
	sort.Slice(refs, func(left, right int) bool { return refs[left].EvidenceID < refs[right].EvidenceID })
	return refs
}

func TestDecodeSelectionRejectsStrictJSONBoundaries(t *testing.T) {
	selection := Selection{SchemaVersion: SchemaVersionV1, Evidence: []lyricscontract.EvidenceRef{}}
	body, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	deep := []byte(`{"schemaVersion":1,"evidence":[],"unknown":` + strings.Repeat("[", MaxJSONDepth+1) + "0" + strings.Repeat("]", MaxJSONDepth+1) + `}`)
	for name, mutated := range map[string][]byte{
		"duplicate":       bytes.Replace(body, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1),
		"unknown":         bytes.Replace(body, []byte(`"evidence":[]`), []byte(`"evidence":[],"romaji":[]`), 1),
		"trailing value":  append(append([]byte(nil), body...), []byte(`{}`)...),
		"trailing space":  append(append([]byte(nil), body...), ' '),
		"invalid UTF-8":   append([]byte{0xff}, body...),
		"lone surrogate":  []byte(`{"schemaVersion":1,"evidence":[],"unknown":"\uD800"}`),
		"excessive depth": deep,
		"oversized":       bytes.Repeat([]byte{' '}, MaxManifestBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSelection(mutated); err == nil {
				t.Fatalf("hostile %s JSON was accepted", name)
			}
		})
	}
}

func TestBuildDeterministicallyShardsByEvidenceID(t *testing.T) {
	items := []lyricssource.IndexEvidence{
		testEvidence(t, 4, 120), testEvidence(t, 1, 120), testEvidence(t, 3, 120), testEvidence(t, 2, 120),
	}
	selected := refsFor(items)
	limits := buildLimits{shardRaw: 300, shardEncoded: 4 << 10, totalRaw: 1 << 20, totalEncoded: 1 << 20, items: 16}
	firstDir := filepath.Join(canonicalTestRoot(t), "pack")
	first, err := buildWithLimits(context.Background(), firstDir, selected, sliceExactSource{items: items}, limits)
	if err != nil {
		t.Fatal(err)
	}
	reversed := append([]lyricssource.IndexEvidence(nil), items...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	secondDir := filepath.Join(canonicalTestRoot(t), "pack")
	second, err := buildWithLimits(context.Background(), secondDir, selected, sliceExactSource{items: reversed}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.PackSHA256 != second.PackSHA256 || len(first.Shards) < 2 {
		t.Fatalf("deterministic manifests differ:\nfirst=%+v\nsecond=%+v", first, second)
	}
	unsigned := first
	unsigned.PackSHA256 = ""
	unsignedBody, err := json.Marshal(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	packDigest := sha256.New()
	_, _ = packDigest.Write([]byte("moesekai-lyrics-evidence-pack-manifest-v1\x00"))
	_, _ = packDigest.Write(unsignedBody)
	if got := hex.EncodeToString(packDigest.Sum(nil)); got != first.PackSHA256 {
		t.Fatalf("streamed pack digest=%s canonical digest=%s", first.PackSHA256, got)
	}
	canonicalBody, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if size, err := streamCanonicalJSON(first, io.Discard); err != nil || size != len(canonicalBody) {
		t.Fatalf("streamed manifest size=%d canonical size=%d err=%v", size, len(canonicalBody), err)
	}
	seen := make(map[string]struct{})
	for index, shard := range first.Shards {
		if shard.Ordinal != index {
			t.Fatalf("shard ordinal=%d index=%d", shard.Ordinal, index)
		}
		name, _ := ShardFileName(shard.Ordinal, shard.SHA256)
		firstBody, err := os.ReadFile(filepath.Join(firstDir, name))
		if err != nil {
			t.Fatal(err)
		}
		secondBody, err := os.ReadFile(filepath.Join(secondDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstBody, secondBody) {
			t.Fatalf("shard %d changed with source order", index)
		}
		for _, ref := range shard.Items {
			if _, duplicate := seen[ref.EvidenceID]; duplicate {
				t.Fatalf("evidence %q assigned twice", ref.EvidenceID)
			}
			seen[ref.EvidenceID] = struct{}{}
		}
	}
	if len(seen) != len(selected) {
		t.Fatalf("shard union=%d selected=%d", len(seen), len(selected))
	}
}

func TestBuildProvesExactUnionBeforePublication(t *testing.T) {
	first := testEvidence(t, 1, 0)
	second := testEvidence(t, 2, 0)
	firstRef := evidenceRef(first)
	conflicting := firstRef
	conflicting.SHA256 = strings.Repeat("f", 64)
	for name, test := range map[string]struct {
		selected []lyricscontract.EvidenceRef
		source   ExactAcquisitionSource
		want     string
	}{
		"missing": {
			selected: refsFor([]lyricssource.IndexEvidence{first, second}),
			source:   sliceExactSource{items: []lyricssource.IndexEvidence{first}}, want: "not found",
		},
		"duplicate": {selected: []lyricscontract.EvidenceRef{firstRef, firstRef}, source: sliceExactSource{items: []lyricssource.IndexEvidence{first}}, want: "duplicate"},
		"conflict":  {selected: []lyricscontract.EvidenceRef{firstRef, conflicting}, source: sliceExactSource{items: []lyricssource.IndexEvidence{first}}, want: "conflicting"},
		"wrong exact acquisition": {
			selected: []lyricscontract.EvidenceRef{firstRef}, source: fixedExactSource{acquired: testAcquisition(second)}, want: "does not match",
		},
	} {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(canonicalTestRoot(t), "pack")
			_, err := Build(context.Background(), output, test.selected, test.source)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("exact-union error=%v, want %q", err, test.want)
			}
			if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid union created output directory: %v", statErr)
			}
		})
	}

	output := filepath.Join(canonicalTestRoot(t), "pack")
	manifest, err := Build(context.Background(), output, []lyricscontract.EvidenceRef{firstRef}, sliceExactSource{
		items: []lyricssource.IndexEvidence{second, first},
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Totals.ItemCount != 1 || len(manifest.Selected) != 1 || manifest.Selected[0] != firstRef {
		t.Fatalf("unselected ledger acquisition entered exact pack union: %+v", manifest)
	}
}

func TestCanonicalEnvelopeAndShardRejectRomajiFields(t *testing.T) {
	item := testEvidence(t, 1, 0)
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	mutated := bytes.Replace(body, []byte(`"rawSha256"`), []byte(`"romaji":"forbidden","rawSha256"`), 1)
	if _, err := DecodeCanonicalEnvelope(mutated); err == nil {
		t.Fatal("evidence envelope accepted a romaji field")
	}
	output := filepath.Join(canonicalTestRoot(t), "pack")
	manifest, err := Build(context.Background(), output, []lyricscontract.EvidenceRef{evidenceRef(item)}, sliceExactSource{items: []lyricssource.IndexEvidence{item}})
	if err != nil {
		t.Fatal(err)
	}
	manifestBody, err := MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := ShardFileName(0, manifest.Shards[0].SHA256)
	shardBody, err := os.ReadFile(filepath.Join(output, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"romaji"`, `"romanization"`, `"ruby"`} {
		if bytes.Contains(manifestBody, []byte(forbidden)) || bytes.Contains(shardBody, []byte(forbidden)) {
			t.Fatalf("pack emitted forbidden field %s", forbidden)
		}
	}
	shardMutated := bytes.Replace(shardBody, []byte(`"rawSha256"`), []byte(`"romaji":"forbidden","rawSha256"`), 1)
	if _, err := decodeShard(shardMutated); err == nil {
		t.Fatal("evidence shard accepted a romaji field")
	}
}
