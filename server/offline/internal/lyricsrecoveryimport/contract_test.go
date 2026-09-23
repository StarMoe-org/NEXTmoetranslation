package lyricsrecoveryimport

import (
	"bytes"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsstaging"
)

func textFreeManifestFixture(t *testing.T) Manifest {
	t.Helper()
	states := []lyricscontract.CoverageState{
		lyricscontract.CoverageSatisfiedNoLyrics,
		lyricscontract.CoverageAmbiguous,
		lyricscontract.CoverageMissing,
		lyricscontract.CoverageIncomplete,
	}
	items := make([]Item, len(states))
	for index, state := range states {
		availabilityState := model.LyricsAvailabilityState(state)
		reason := model.LyricsSourceVersionReasonVersionConflict
		noLyricsReason := ""
		if state == lyricscontract.CoverageSatisfiedNoLyrics {
			reason = ""
			noLyricsReason = model.LyricsAvailabilityNoLyricsCatalogInstrumental
		}
		document := model.LyricsAvailabilityDocument{
			SchemaVersion: model.LyricsAvailabilityDocumentSchemaVersion,
			State:         availabilityState, ReasonCode: reason, NoLyricsReason: noLyricsReason,
			FixedIdentities: []model.LyricsSourceFixedIdentity{},
		}
		documentSHA, err := AvailabilityDocumentSHA256(document)
		if err != nil {
			t.Fatal(err)
		}
		items[index] = Item{
			MusicID: index + 1, JapaneseTitle: "試験曲", CatalogFingerprint: strings.Repeat(string('a'+rune(index)), 64),
			TargetMusicID: index + 1, AssociationMusicIDs: []int{}, State: state,
			ResultSHA256: strings.Repeat(string('1'+rune(index)), 64),
			Availability: &document, AvailabilityDocumentSHA256: documentSHA,
		}
	}
	musicIDsSHA, err := lyricscontract.OrderedMusicIDsSHA256([]int{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	coverage := lyricscontract.Coverage{
		Total: 4, SatisfiedNoLyrics: 1, Ambiguous: 1, Missing: 1, Incomplete: 1,
	}
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Root: RootBinding{
			SchemaVersion: lyricscontract.SchemaVersionV2, RootID: "root-text-free-fixture",
			RootSHA256: strings.Repeat("f", 64), CatalogCount: 4, MusicIDsSHA256: musicIDsSHA, Coverage: coverage,
		},
		Items: items,
	}
	digest, err := manifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.BatchSHA256 = digest
	return manifest
}

func TestTextFreeRecoveryImportManifestRoundTrip(t *testing.T) {
	manifest := textFreeManifestFixture(t)
	if err := ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalCanonical(decoded)
	if err != nil || !bytes.Equal(body, second) {
		t.Fatalf("canonical round trip err=%v", err)
	}
	lower := bytes.ToLower(body)
	for _, forbidden := range [][]byte{[]byte("romaji"), []byte("romanization"), []byte(`"full"`), []byte(`"game"`)} {
		if bytes.Contains(lower, forbidden) {
			t.Fatalf("text-free manifest leaked %q", forbidden)
		}
	}
}

func TestRecoveryImportManifestCloneDeepCopiesPeerTranslations(t *testing.T) {
	manifest := textFreeManifestFixture(t)
	manifest.Items[0].Draft = &lyricsstaging.Draft{RenditionTranslations: []lyricscontract.RenditionTranslation{{
		RenditionKey: "sekai", PeerTranslations: []lyricscontract.RenditionPeerTranslation{{
			Side: "game", Locale: "zh-CN", Translations: []string{"游戏译文"},
		}},
	}}}
	cloned := cloneManifest(manifest)
	cloned.Items[0].Draft.RenditionTranslations[0].PeerTranslations[0].Translations[0] = "mutated"
	if got := manifest.Items[0].Draft.RenditionTranslations[0].PeerTranslations[0].Translations[0]; got != "游戏译文" {
		t.Fatalf("peer translation clone aliased input: %q", got)
	}
}

func TestRecoveryImportManifestRejectsStateAndDigestDrift(t *testing.T) {
	manifest := textFreeManifestFixture(t)
	manifest.Root.Coverage.Missing++
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("coverage drift was accepted")
	}

	manifest = textFreeManifestFixture(t)
	manifest.Items[2].Availability.State = model.LyricsAvailabilityStateFailed
	manifest.BatchSHA256 = ""
	digest, _ := manifestDigest(manifest)
	manifest.BatchSHA256 = digest
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("availability state drift was accepted")
	}

	manifest = textFreeManifestFixture(t)
	manifest.Items[0].AvailabilityDocumentSHA256 = strings.Repeat("0", 64)
	manifest.BatchSHA256 = ""
	digest, _ = manifestDigest(manifest)
	manifest.BatchSHA256 = digest
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("availability digest drift was accepted")
	}
}
