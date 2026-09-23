package lyricsrecoverypublic

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func publicCandidateFixture() store.RecoveryPublicLyricsCandidate {
	states := []store.PublicLyricsAvailabilityState{
		store.PublicLyricsStateComplete,
		store.PublicLyricsStateGameOnly,
		store.PublicLyricsStateSatisfiedNoLyrics,
		store.PublicLyricsStateAmbiguous,
		store.PublicLyricsStateMissing,
		store.PublicLyricsStateIncomplete,
		store.PublicLyricsStateFailed,
	}
	candidate := store.RecoveryPublicLyricsCandidate{
		BatchSHA256: strings.Repeat("a", 64), RootSHA256: strings.Repeat("b", 64),
		Index:   store.PublicLyricsIndexDocument{Version: 2},
		Details: map[int]store.PublicLyricsDetailDocument{},
	}
	for index, state := range states {
		musicID := index + 1
		song := store.PublicLyricsIndexSong{
			MusicID: musicID, Revision: 1, UpdatedAt: "2026-08-05T00:00:00Z", State: state,
			Title: model.LocalizedTitle{Japanese: "曲"},
		}
		if state == store.PublicLyricsStateComplete {
			song.AvailableVersions = []string{"full"}
			candidate.Details[musicID] = publicDetailFixture(musicID, state, []string{"full"})
		}
		if state == store.PublicLyricsStateGameOnly {
			song.AvailableVersions = []string{"game"}
			candidate.Details[musicID] = publicDetailFixture(musicID, state, []string{"game"})
		}
		if state == store.PublicLyricsStateSatisfiedNoLyrics {
			song.NoLyricsReason = model.LyricsAvailabilityNoLyricsCatalogInstrumental
		}
		candidate.Index.Songs = append(candidate.Index.Songs, song)
	}
	return candidate
}

func publicDetailFixture(musicID int, state store.PublicLyricsAvailabilityState, versions []string) store.PublicLyricsDetailDocument {
	return store.PublicLyricsDetailDocument{
		Version: 2, MusicID: musicID, Revision: 1, UpdatedAt: "2026-08-05T00:00:00Z", State: state,
		Attributions: []store.PublicLyricsAttribution{{
			Provider: model.LyricsSourceProviderSekaipedia, Title: "曲", RevisionID: 1,
			RevisionURL: "https://www.sekaipedia.org/wiki/曲?oldid=1",
			LicenseName: "CC BY-SA 4.0", LicenseURL: "https://creativecommons.org/licenses/by-sa/4.0/",
		}},
		AvailableVersions: versions,
		Lines: []store.PublicLyricsLine{{
			ID: "line-1", Order: 0, Japanese: "歌", Chinese: "译", English: "",
			Segments: []model.LyricSegment{{
				Text: "歌", PerformerIDs: []int{}, Ruby: []model.LyricRubySpan{{Text: "歌", Reading: "うた"}},
			}},
		}},
	}
}

func publicV3CandidateFixture(t *testing.T) store.RecoveryPublicLyricsV3Candidate {
	t.Helper()
	body, err := os.ReadFile("../../../../contracts/public-lyrics/v3/detail-legacy-one-rendition.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := store.DecodePublicLyricsV3Detail(body)
	if err != nil {
		t.Fatal(err)
	}
	return store.RecoveryPublicLyricsV3Candidate{
		BatchSHA256: strings.Repeat("a", 64), RootSHA256: strings.Repeat("b", 64),
		Index: store.PublicLyricsV3IndexDocument{Version: 3, Songs: []store.PublicLyricsIndexSong{{
			MusicID: detail.MusicID, Revision: detail.Revision, UpdatedAt: detail.UpdatedAt,
			State: detail.State, Title: model.LocalizedTitle{Japanese: "V3 fixture"},
			AvailableVersions: []string{"full"},
		}}},
		Details: map[int]store.PublicLyricsV3DetailDocument{detail.MusicID: detail},
	}
}

func TestBuildBundleIsDeterministicAndClosesAllStates(t *testing.T) {
	candidate := publicCandidateFixture()
	first, err := BuildBundle(candidate, strings.Repeat("c", 64), 4096)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildBundle(candidate, strings.Repeat("c", 64), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.IndexBody, second.IndexBody) || !bytes.Equal(first.ManifestBody, second.ManifestBody) ||
		!bytes.Equal(first.ReceiptBody, second.ReceiptBody) || !reflect.DeepEqual(first.DetailBodies, second.DetailBodies) {
		t.Fatal("public candidate bundle is not deterministic")
	}
	if first.Manifest.CatalogCount != 7 || first.Manifest.DetailCount != 2 || first.Receipt.AssetCount != 3 {
		t.Fatalf("bundle counts manifest=%+v receipt=%+v", first.Manifest, first.Receipt)
	}
	for index, state := range publicStateOrder() {
		if first.Manifest.States[index] != (StateCount{State: state, Count: 1}) {
			t.Fatalf("state[%d]=%+v", index, first.Manifest.States[index])
		}
	}
	if err := ValidateManifest(first.Manifest); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReceipt(first.Receipt, first.Manifest, first.ManifestBody); err != nil {
		t.Fatal(err)
	}
}

func TestPublishExactWritesReceiptLastWithoutOverwrite(t *testing.T) {
	bundle, err := BuildBundle(publicCandidateFixture(), strings.Repeat("c", 64), 4096)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "candidate")
	if err := PublishExact(output, bundle); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{IndexPath, "music_1.json", "music_2.json", ManifestPath, ReceiptPath} {
		info, err := os.Lstat(filepath.Join(output, path))
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("asset %s mode=%v", path, info.Mode())
		}
	}
	var receipt Receipt
	body, err := os.ReadFile(filepath.Join(output, ReceiptPath))
	if err != nil || json.Unmarshal(body, &receipt) != nil || receipt.ReceiptSHA256 != bundle.Receipt.ReceiptSHA256 {
		t.Fatalf("receipt body=%q err=%v", body, err)
	}
	if err := PublishExact(output, bundle); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite error=%v", err)
	}
}

func TestPublishExactV3FailureLeavesNoPartialOutput(t *testing.T) {
	bundle, err := BuildV3Bundle(publicV3CandidateFixture(t), strings.Repeat("c", 64), 4096)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	output := filepath.Join(parent, "v3-candidate")
	publishBundleBeforeRenameHook = func(staging string) error {
		if _, statErr := os.Stat(filepath.Join(staging, ReceiptPath)); statErr != nil {
			t.Fatalf("staged receipt missing before injected failure: %v", statErr)
		}
		return errors.New("injected publication failure")
	}
	t.Cleanup(func() { publishBundleBeforeRenameHook = nil })
	if err := PublishExactV3(output, bundle); err == nil || !strings.Contains(err.Error(), "injected publication failure") {
		t.Fatalf("injected publication error=%v", err)
	}
	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Fatalf("failed publication left output=%v", statErr)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".public-candidate-staging-") {
			t.Fatalf("failed publication left staging directory %q", entry.Name())
		}
	}
}

func TestBuildV3BundleClosesStrictDetailAndPublishBoundaries(t *testing.T) {
	bundle, err := BuildV3Bundle(publicV3CandidateFixture(t), strings.Repeat("c", 64), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Manifest.PublicLyricsVersion != 3 || bundle.Manifest.CatalogCount != 1 || bundle.Manifest.DetailCount != 1 {
		t.Fatalf("v3 bundle envelope=%+v", bundle.Manifest)
	}
	output := filepath.Join(t.TempDir(), "v3-candidate")
	if err := PublishExactV3(output, bundle); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{IndexPath, "music_10.json", ManifestPath, ReceiptPath} {
		if info, statErr := os.Stat(filepath.Join(output, path)); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("v3 asset %s stat=%v info=%v", path, statErr, info)
		}
	}
}

func TestBuildV3BundleRejectsIndexOverflow(t *testing.T) {
	candidate := publicV3CandidateFixture(t)
	candidate.Index.Songs = make([]store.PublicLyricsIndexSong, model.PublicLyricsMaxIndexEntries+1)
	if _, err := BuildV3Bundle(candidate, strings.Repeat("c", 64), 4096); err == nil ||
		!strings.Contains(err.Error(), "public v3 candidate envelope is invalid") {
		t.Fatalf("v3 index overflow error=%v", err)
	}
}

func TestPublishExactV3RejectsAssetAndStrictBodyTampering(t *testing.T) {
	original, err := BuildV3Bundle(publicV3CandidateFixture(t), strings.Repeat("c", 64), 4096)
	if err != nil {
		t.Fatal(err)
	}

	mutatedIndex := original
	index, err := store.DecodePublicLyricsV3Index(original.IndexBody)
	if err != nil {
		t.Fatal(err)
	}
	index.Songs[0].Title.Japanese = "篡改"
	mutatedIndex.IndexBody = mustCanonical(index)
	indexOutput := filepath.Join(t.TempDir(), "index-tamper")
	if err := PublishExactV3(indexOutput, mutatedIndex); err == nil || !strings.Contains(err.Error(), "index bytes do not match the manifest") {
		t.Fatalf("mutated v3 index error=%v", err)
	}
	if _, statErr := os.Lstat(indexOutput); !os.IsNotExist(statErr) {
		t.Fatalf("mutated v3 index created output: %v", statErr)
	}

	mutatedDetail := original
	mutatedDetail.DetailBodies = map[int][]byte{10: append([]byte(nil), original.DetailBodies[10]...)}
	detail, err := store.DecodePublicLyricsV3Detail(mutatedDetail.DetailBodies[10])
	if err != nil {
		t.Fatal(err)
	}
	if detail.Renditions[0].TranslationCredits == nil {
		detail.Renditions[0].TranslationCredits = &store.PublicLyricsV3TranslationCredits{}
	}
	detail.Renditions[0].TranslationCredits.Translation = "篡改署名"
	mutatedDetail.DetailBodies[10] = mustCanonical(detail)
	detailOutput := filepath.Join(t.TempDir(), "detail-tamper")
	if err := PublishExactV3(detailOutput, mutatedDetail); err == nil || !strings.Contains(err.Error(), "music 10 bytes do not match the manifest") {
		t.Fatalf("mutated v3 detail error=%v", err)
	}
	if _, statErr := os.Lstat(detailOutput); !os.IsNotExist(statErr) {
		t.Fatalf("mutated v3 detail created output: %v", statErr)
	}

	extraDetail := original
	extraDetail.DetailBodies = map[int][]byte{
		10:  append([]byte(nil), original.DetailBodies[10]...),
		999: append([]byte(nil), original.DetailBodies[10]...),
	}
	extraOutput := filepath.Join(t.TempDir(), "extra-detail")
	if err := PublishExactV3(extraOutput, extraDetail); err == nil || !strings.Contains(err.Error(), "detail bodies do not close") {
		t.Fatalf("extra v3 detail error=%v", err)
	}
	if _, statErr := os.Lstat(extraOutput); !os.IsNotExist(statErr) {
		t.Fatalf("extra v3 detail created output: %v", statErr)
	}

	unknown := map[string]any{}
	if err := json.Unmarshal(original.DetailBodies[10], &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["private"] = true
	unknownBody, err := json.MarshalIndent(unknown, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	unknownBody = append(unknownBody, '\n')
	strictTamper := rebindV3BundleDetail(original, 10, unknownBody)
	strictOutput := filepath.Join(t.TempDir(), "strict-detail-tamper")
	if err := PublishExactV3(strictOutput, strictTamper); err == nil || !strings.Contains(err.Error(), "exact strict v3 detail") {
		t.Fatalf("strict v3 detail error=%v", err)
	}
	if _, statErr := os.Lstat(strictOutput); !os.IsNotExist(statErr) {
		t.Fatalf("strict v3 detail created output: %v", statErr)
	}
}

func rebindV3BundleDetail(bundle Bundle, musicID int, body []byte) Bundle {
	result := bundle
	result.DetailBodies = map[int][]byte{}
	for id, original := range bundle.DetailBodies {
		result.DetailBodies[id] = append([]byte(nil), original...)
	}
	result.DetailBodies[musicID] = append([]byte(nil), body...)
	for index := range result.Manifest.Details {
		if result.Manifest.Details[index].MusicID == musicID {
			result.Manifest.Details[index] = newAsset(detailPath(musicID), musicID, body)
		}
	}
	assets := append([]Asset{result.Manifest.Index}, result.Manifest.Details...)
	result.Manifest.ContentSHA256 = contentDigest(assets)
	result.ManifestBody = mustCanonical(result.Manifest)
	result.Receipt.ManifestSHA256 = digestBytes(result.ManifestBody)
	result.Receipt.ManifestBytes = int64(len(result.ManifestBody))
	result.Receipt.ContentSHA256 = result.Manifest.ContentSHA256
	result.Receipt.ReceiptSHA256, _ = receiptDigest(result.Receipt)
	result.ReceiptBody = mustCanonical(result.Receipt)
	return result
}

func TestBuildBundleRejectsDetailStateDrift(t *testing.T) {
	candidate := publicCandidateFixture()
	candidate.Details[1] = publicDetailFixture(1, store.PublicLyricsStateGameOnly, []string{"game"})
	if _, err := BuildBundle(candidate, strings.Repeat("c", 64), 4096); err == nil ||
		!strings.Contains(err.Error(), "identities differ") {
		t.Fatalf("state drift error=%v", err)
	}
}

func TestPublishExactRejectsBodiesMutatedAfterBuild(t *testing.T) {
	original, err := BuildBundle(publicCandidateFixture(), strings.Repeat("c", 64), 4096)
	if err != nil {
		t.Fatal(err)
	}

	mutatedIndex := original
	mutatedIndex.IndexBody = append([]byte(nil), original.IndexBody...)
	mutatedIndex.IndexBody[0] ^= 1
	indexOutput := filepath.Join(t.TempDir(), "index-tamper")
	if err := PublishExact(indexOutput, mutatedIndex); err == nil || !strings.Contains(err.Error(), "index bytes") {
		t.Fatalf("mutated index error=%v", err)
	}
	if _, err := os.Lstat(indexOutput); !os.IsNotExist(err) {
		t.Fatalf("mutated index created output: %v", err)
	}

	mutatedDetail := original
	mutatedDetail.DetailBodies = make(map[int][]byte, len(original.DetailBodies))
	for musicID, body := range original.DetailBodies {
		mutatedDetail.DetailBodies[musicID] = append([]byte(nil), body...)
	}
	mutatedDetail.DetailBodies[1][0] ^= 1
	detailOutput := filepath.Join(t.TempDir(), "detail-tamper")
	if err := PublishExact(detailOutput, mutatedDetail); err == nil || !strings.Contains(err.Error(), "music 1 bytes") {
		t.Fatalf("mutated detail error=%v", err)
	}
	if _, err := os.Lstat(detailOutput); !os.IsNotExist(err) {
		t.Fatalf("mutated detail created output: %v", err)
	}
}
