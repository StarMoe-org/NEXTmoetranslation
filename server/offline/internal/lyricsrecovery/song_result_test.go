package lyricsrecovery

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

func TestSongResultStrictCanonicalNoRomanizationAndTamperChecks(t *testing.T) {
	result := testCompleteSongResult(t)
	body, err := MarshalSongResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(body)), "romaji") || strings.Contains(strings.ToLower(string(body)), "romanization") {
		t.Fatal("canonical song result crossed the no-romanization boundary")
	}
	decoded, err := DecodeSongResult(body)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := MarshalSongResult(decoded)
	if err != nil || !bytes.Equal(body, roundTrip) {
		t.Fatalf("song result canonical round trip drifted: err=%v", err)
	}

	schemaField := []byte(fmt.Sprintf(`"schemaVersion":%d`, result.SchemaVersion))
	duplicateSchemaField := []byte(fmt.Sprintf(`"schemaVersion":%d,"schemaVersion":%d`, result.SchemaVersion, result.SchemaVersion))
	hostile := map[string][]byte{
		"duplicate": bytes.Replace(body, schemaField, duplicateSchemaField, 1),
		"unknown":   bytes.Replace(body, []byte(`{"schemaVersion"`), []byte(`{"unknown":1,"schemaVersion"`), 1),
		"romanization field": bytes.Replace(body, []byte(`{"schemaVersion"`),
			[]byte(`{"romaji":"forbidden","schemaVersion"`), 1),
		"trailing": append(append([]byte{}, body...), []byte("\n{}")...),
		"utf8":     append(append([]byte{}, body...), 0xff),
		"depth":    []byte(`{"x":[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}`),
	}
	for name, candidate := range hostile {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSongResult(candidate); err == nil {
				t.Fatal("hostile song result was accepted")
			}
		})
	}
	if _, err := DecodeSongResult(bytes.Repeat([]byte{'x'}, MaxSongResultBytes+1)); err == nil {
		t.Fatal("oversized song result was accepted")
	}

	tampered := cloneSongResult(result)
	tampered.MusicID++
	if err := ValidateSongResult(tampered); err == nil {
		t.Fatal("tampered song result retained a valid digest")
	}
	cloned := cloneSongResult(result)
	if result.SchemaVersion == SongResultSchemaVersionV3 {
		fullIndex := -1
		for index := range result.Renditions {
			if result.Renditions[index].Full != nil && len(result.Renditions[index].Full.Lines) > 0 {
				fullIndex = index
				break
			}
		}
		if fullIndex < 0 {
			t.Fatal("complete song result v3 has no Full side to clone")
		}
		cloned.Renditions[fullIndex].Full.Lines[0].Text = "mutated clone"
		if result.Renditions[fullIndex].Full.Lines[0].Text == cloned.Renditions[fullIndex].Full.Lines[0].Text {
			t.Fatal("song result rendition Full clone aliases authoritative line storage")
		}
	} else {
		cloned.Full.Lines[0].Text = "mutated clone"
		if result.Full.Lines[0].Text == cloned.Full.Lines[0].Text {
			t.Fatal("song result Full clone aliases authoritative line storage")
		}
	}

	unreferenced := cloneSongResult(result)
	unreferenced.ResultSHA256 = ""
	unreferenced.SelectedEvidence = append(unreferenced.SelectedEvidence, lyricscontract.EvidenceRef{
		Provider:      model.LyricsSourceProviderSekaipedia,
		AcquisitionID: strings.Repeat("9", 64), EvidenceID: "revision:sekaipedia:999:999:" + strings.Repeat("8", 64),
		SHA256: strings.Repeat("8", 64), EnvelopeSHA256: strings.Repeat("7", 64),
	})
	sort.Slice(unreferenced.SelectedEvidence, func(left, right int) bool {
		return unreferenced.SelectedEvidence[left].EvidenceID < unreferenced.SelectedEvidence[right].EvidenceID
	})
	if err := validateSongResult(unreferenced, false); err == nil {
		t.Fatal("selected evidence outside the exact component union was accepted")
	}

	nullComponent := cloneSongResult(result)
	nullComponent.ResultSHA256 = ""
	if result.SchemaVersion == SongResultSchemaVersionV3 {
		nullComponent.Renditions[0].Components[0].Evidence = nil
	} else {
		nullComponent.Components.Ruby = nil
	}
	if err := validateSongResult(nullComponent, false); err == nil {
		t.Fatal("null component evidence array was accepted")
	}
}

func TestSongResultV1CanonicalDigestRemainsCompatible(t *testing.T) {
	v1, err := NewSongResult(noRomajiReplayFixture("歌唱者-01", "星乃一歌"))
	if err != nil {
		t.Fatal(err)
	}
	v1.SchemaVersion = SongResultSchemaVersionV1
	v1.CanonicalEncoding = SongResultCanonicalEncodingV1
	v1.DigestAlgorithm = SongResultDigestAlgorithmV1
	v1.NoLyricsReason = ""
	v1.Game = nil
	v1.AlternateVocals = nil
	v1.Components.GameText = nil
	v1.Components.AlternateVocals = nil
	v1.ResultSHA256 = ""
	digest, err := songResultDigest(v1)
	if err != nil {
		t.Fatal(err)
	}
	v1.ResultSHA256 = digest
	body, err := MarshalSongResult(v1)
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibited := range []string{`"noLyricsReason"`, `"game"`, `"gameText"`} {
		if bytes.Contains(body, []byte(prohibited)) {
			t.Fatalf("v1 canonical bytes gained v2 field %s", prohibited)
		}
	}
	decoded, err := DecodeSongResult(body)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := MarshalSongResult(decoded)
	if err != nil || !bytes.Equal(body, roundTrip) || decoded.SchemaVersion != SongResultSchemaVersionV1 {
		t.Fatalf("v1 song-result compatibility drifted: err=%v", err)
	}
}

func TestSongResultV2PreservesGameOnlyWithoutProvisionalFull(t *testing.T) {
	replay := noRomajiReplayFixture("ichika", "Hoshino Ichika")
	game := replay.Composition.Full
	for index := range game.Lines {
		game.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	replay.Composition.Full = model.LyricsSourceFull{}
	replay.Composition.Game = &game
	replay.Composition.GameProjection = nil
	replay.Composition.ReasonCode = model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid
	replay.Composition.Components.FullText = ""
	replay.Composition.Components.GameText = "selected-source"
	replay.Components.FullText = []lyricscontract.EvidenceRef{}
	replay.Components.GameText = cloneEvidenceRefs(replay.Selected)

	result, err := NewSongResult(replay)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != SongResultSchemaVersionV2 || result.State != lyricscontract.CoverageGameOnly ||
		result.Full != nil || result.Game == nil || len(result.Game.Lines) != len(game.Lines) ||
		result.GameProjection != nil || result.NoLyricsReason != "" || len(result.Components.FullText) != 0 ||
		len(result.Components.GameText) != 1 || result.ReasonCode != model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid {
		t.Fatalf("Game-only song result=%+v", result)
	}
	body, err := MarshalSongResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"state":"game_only"`)) || !bytes.Contains(body, []byte(`"full":null`)) ||
		!bytes.Contains(body, []byte(`"game":`)) {
		t.Fatalf("Game-only canonical bytes do not explicitly avoid fake Full: %s", body)
	}
	cloned := cloneSongResult(result)
	cloned.Game.Lines[0].Text = "mutated"
	if result.Game.Lines[0].Text == cloned.Game.Lines[0].Text {
		t.Fatal("Game-only clone aliases authoritative Game line storage")
	}
}

func TestSongResultAcceptsEvidenceBoundAuthoritativeVocaloidSegmentation(t *testing.T) {
	replay := noRomajiReplayFixture("歌唱者-21", "初音ミク")
	replay.Composition.Full.Version = model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"}
	replay.Composition.Full.Performers = append(replay.Composition.Full.Performers, model.LyricsSourcePerformer{
		PerformerID: "歌唱者-22", Name: "鏡音リン", Color: "#FFCC11",
	})
	replay.Composition.Full.Lines[0].Segments = []model.LyricsSourceSegment{
		{Text: "歌", PerformerIDs: []string{"歌唱者-21"}, Ruby: []model.LyricsSourceRubySpan{{Text: "歌", Reading: "うた"}}},
		{Text: "う", PerformerIDs: []string{"歌唱者-22"}, Ruby: []model.LyricsSourceRubySpan{{Text: "う"}}},
	}
	replay.Composition.Full.Lines[0].TrailingPerformerIDs = []string{"歌唱者-21", "歌唱者-22"}

	result, err := NewSongResult(replay)
	if err != nil {
		t.Fatalf("source-proven VIRTUAL SINGER song result: %v", err)
	}
	if result.Full == nil || result.Full.Version.Kind != "vocaloid" || len(result.Full.Performers) != 2 ||
		len(result.Full.Lines[0].Segments) != 2 || len(result.Components.PerformerSegmentation) == 0 {
		t.Fatalf("source-proven VIRTUAL SINGER segmentation was not preserved: %+v", result)
	}
	body, err := MarshalSongResult(result)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSongResult(body)
	if err != nil || decoded.Full == nil || len(decoded.Full.Lines[0].Segments) != 2 {
		t.Fatalf("canonical VIRTUAL SINGER song-result round trip: decoded=%+v err=%v", decoded, err)
	}

	gameReplay := replay
	gameComposition := *replay.Composition
	game := gameComposition.Full
	for index := range game.Lines {
		game.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	gameComposition.Full = model.LyricsSourceFull{}
	gameComposition.Game = &game
	gameComposition.GameProjection = nil
	gameComposition.ReasonCode = model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid
	gameComposition.Components.FullText = ""
	gameComposition.Components.GameText = "selected-source"
	gameReplay.Composition = &gameComposition
	gameReplay.Components.FullText = []lyricscontract.EvidenceRef{}
	gameReplay.Components.GameText = cloneEvidenceRefs(gameReplay.Selected)
	gameResult, err := NewSongResult(gameReplay)
	if err != nil || gameResult.Game == nil || len(gameResult.Game.Lines[0].Segments) != 2 ||
		len(gameResult.Components.PerformerSegmentation) == 0 {
		t.Fatalf("source-proven VIRTUAL SINGER Game-only result=%+v err=%v", gameResult, err)
	}

	withoutEvidence := replay
	withoutEvidence.Components.PerformerSegmentation = []lyricscontract.EvidenceRef{}
	if _, err := NewSongResult(withoutEvidence); err == nil ||
		!strings.Contains(err.Error(), "vocaloid-only Full must not contain performer metadata") {
		t.Fatalf("VIRTUAL SINGER segmentation without component evidence error=%v", err)
	}
}

func TestSongResultV2CatalogInstrumentalIsSatisfiedWithoutLyrics(t *testing.T) {
	replay := noRomajiReplayFixture("ichika", "Hoshino Ichika")
	replay.Instrumental = true
	replay.Composition = nil

	result, err := NewSongResult(replay)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != lyricscontract.CoverageSatisfiedNoLyrics ||
		result.NoLyricsReason != NoLyricsReasonCatalogInstrumental || result.ReasonCode != "" ||
		result.Full != nil || result.Game != nil || result.GameProjection != nil || result.Translations != nil ||
		len(result.SelectedEvidence) != 0 || !componentEvidenceEmpty(result.Components) || len(result.ProviderOutcomes) != 1 {
		t.Fatalf("catalog instrumental song result=%+v", result)
	}
	if _, err := MarshalSongResult(result); err != nil {
		t.Fatal(err)
	}

	conflict := replay
	conflict.Composition = noRomajiReplayFixture("ichika", "Hoshino Ichika").Composition
	if _, err := NewSongResult(conflict); err == nil {
		t.Fatal("catalog instrumental accepted a recovered lyrics composition")
	}
}

func TestSongResultCrashPairRecovery(t *testing.T) {
	result := testCompleteSongResult(t)
	body, err := MarshalSongResult(result)
	if err != nil {
		t.Fatal(err)
	}
	root := privateRecoveryTempDir(t)
	name, err := SongResultFileName(result)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	tempPath := filepath.Join(root, "."+name+".lyrics-recovery-v2.tmp")
	if err := os.WriteFile(tempPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(tempPath, path); err != nil {
		t.Fatal(err)
	}
	if err := PublishSongResult(path, result); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered crash-pair staging path remains: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || privateLinkCount(info) != 1 || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovered crash-pair final identity is invalid: info=%v err=%v", info, err)
	}
}

func TestSongResultPrivatePublicationRejectsOverwriteModeAndSymlink(t *testing.T) {
	result := testCompleteSongResult(t)
	root := privateRecoveryTempDir(t)
	name, err := SongResultFileName(result)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	if err := PublishSongResult(path, result); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("published song result identity is invalid: info=%v err=%v", info, err)
	}
	opened, err := OpenSongResult(path)
	if err != nil || opened.ResultSHA256 != result.ResultSHA256 {
		t.Fatalf("open published song result: err=%v", err)
	}
	if err := PublishSongResult(path, result); !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("no-overwrite song result error=%v", err)
	}
	link := filepath.Join(root, "song-result-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSongResult(link); err == nil {
		t.Fatal("symlink song result was accepted")
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSongResult(path); err == nil {
		t.Fatal("wrong-mode song result was accepted")
	}
}

func testCompleteSongResult(t *testing.T) SongResult {
	t.Helper()
	ctx := t.Context()
	root := privateRecoveryTempDir(t)
	binding := fixtureCatalogBinding()
	catalog, _, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	ledger, err := lyricsacquisition.CreateLedger(ctx, filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	parent := fixtureParentRoot(t, ctx, root, ledger, binding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := catalog.MusicIdentity(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	providers, _, err := AcquireSong(ctx, 2, identity, runtime, ledger, fixtureProviderTransports(t))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplaySong(ctx, 2, identity, plan.Versions.ProviderPolicy, runtime, ledger, providers)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewSongResult(replayed)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
