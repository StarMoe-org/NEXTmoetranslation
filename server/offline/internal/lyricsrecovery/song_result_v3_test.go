package lyricsrecovery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricscompose"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
)

func recoveryV3ReplayFixture(t *testing.T) ReplayResult {
	t.Helper()
	sekaiSource := noRomajiReplayFixture("歌唱者-01", "星乃一歌")
	sekaiFull := cloneLyricsSourceFull(sekaiSource.Composition.Full)
	sekaiGame := cloneLyricsSourceFull(sekaiSource.Composition.Full)
	sekaiGame.Lines = []model.LyricsSourceFullLine{sekaiGame.Lines[0], sekaiGame.Lines[2]}
	for index := range sekaiGame.Lines {
		sekaiGame.Lines[index].ID = "game-00000" + string(rune('1'+index))
	}
	vocaloidSource := noRomajiReplayFixture("歌唱者-21", "初音ミク")
	vocaloidGame := cloneLyricsSourceFull(vocaloidSource.Composition.Full)
	vocaloidGame.Version = model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER"}
	for index := range vocaloidGame.Lines {
		vocaloidGame.Lines[index].ID = "game-00000" + string(rune('1'+index))
	}

	const sekaiOutcome = "outcome-sekaipedia-42-peer-sekai"
	const vocaloidOutcome = "outcome-moegirl-42-peer-vocaloid"
	bindRecoveryV3ReadingEvidence(&sekaiFull, sekaiOutcome, "sekai", model.LyricsSourceRenditionSideFull)
	bindRecoveryV3ReadingEvidence(&sekaiGame, sekaiOutcome, "sekai", model.LyricsSourceRenditionSideGame)
	bindRecoveryV3ReadingEvidence(&vocaloidGame, vocaloidOutcome, "vocaloid", model.LyricsSourceRenditionSideGame)
	sekaiRef := model.LyricsSourceComponentRef{RenditionKey: sekaiOutcome}
	vocaloidRef := model.LyricsSourceComponentRef{RenditionKey: vocaloidOutcome}
	renditions := []model.LyricsSourceRendition{
		{
			RenditionKey: "sekai", SourceKind: model.LyricsSourceRenditionSekai,
			SourceTabPaths:        []model.LyricsSourceTabPath{{"Full Version"}, {"Game Version", "SEKAI"}},
			ReasonCode:            model.LyricsSourceVersionReasonTaggedFullAndGame,
			SourcePerformerIDs:    []string{"歌唱者-01"},
			FullPerformerEvidence: model.LyricsSourcePerformerEvidenceSourceComplete,
			GamePerformerEvidence: model.LyricsSourcePerformerEvidenceSourceComplete,
			Full:                  &sekaiFull, Game: &sekaiGame,
			Relation: model.LyricsSourceRenditionRelation{
				Kind: model.LyricsSourceRenditionRelationExactProjection, FullRenditionKey: "sekai",
				LineIDs: []string{"full-000001", "full-000003"},
			},
			Provenance: model.LyricsSourceRenditionProvenance{
				FullText: &sekaiRef, FullPerformerSegmentation: &sekaiRef, FullRuby: &sekaiRef,
				GameText: &sekaiRef, GamePerformerSegmentation: &sekaiRef, GameRuby: &sekaiRef,
				RelationEvidence: sekaiRef, VersionEvidence: sekaiRef,
			},
		},
		{
			RenditionKey: "vocaloid", SourceKind: model.LyricsSourceRenditionVocaloid,
			SourceTabPaths:        []model.LyricsSourceTabPath{{"Game Version", "VIRTUAL SINGER"}},
			ReasonCode:            model.LyricsSourceVersionReasonTaggedGameOnly,
			SourcePerformerIDs:    []string{"歌唱者-21"},
			FullPerformerEvidence: model.LyricsSourcePerformerEvidenceNone,
			GamePerformerEvidence: model.LyricsSourcePerformerEvidenceSourceComplete,
			Game:                  &vocaloidGame,
			Relation:              model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
			Provenance: model.LyricsSourceRenditionProvenance{
				GameText: &vocaloidRef, GamePerformerSegmentation: &vocaloidRef, GameRuby: &vocaloidRef,
				RelationEvidence: vocaloidRef, VersionEvidence: vocaloidRef,
			},
		},
	}
	if err := model.ValidateLyricsSourceRenditionSetPayload(renditions); err != nil {
		t.Fatalf("valid recovery v3 rendition fixture: %v", err)
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(renditions)
	if err != nil {
		t.Fatal(err)
	}
	sekaiEvidence := lyricscontract.EvidenceRef{
		Provider: model.LyricsSourceProviderSekaipedia, AcquisitionID: strings.Repeat("1", 64),
		EvidenceID: "revision:sekaipedia:42:420:" + strings.Repeat("2", 64),
		SHA256:     strings.Repeat("3", 64), EnvelopeSHA256: strings.Repeat("4", 64),
	}
	vocaloidEvidence := lyricscontract.EvidenceRef{
		Provider: model.LyricsSourceProviderMoegirl, AcquisitionID: strings.Repeat("5", 64),
		EvidenceID: "revision:moegirl:42:421:" + strings.Repeat("6", 64),
		SHA256:     strings.Repeat("7", 64), EnvelopeSHA256: strings.Repeat("8", 64),
	}
	evidenceByOutcome := map[string][]lyricscontract.EvidenceRef{
		sekaiOutcome:    {sekaiEvidence},
		vocaloidOutcome: {vocaloidEvidence},
	}
	componentGroups := make([]RenditionComponentEvidence, 0, len(renditions))
	groupByKey := map[string]int{}
	for _, binding := range bindings {
		groupIndex, found := groupByKey[binding.RenditionKey]
		if !found {
			groupIndex = len(componentGroups)
			groupByKey[binding.RenditionKey] = groupIndex
			componentGroups = append(componentGroups, RenditionComponentEvidence{RenditionKey: binding.RenditionKey})
		}
		componentGroups[groupIndex].Components = append(componentGroups[groupIndex].Components, RenditionComponentEvidenceRef{
			Component: binding.Component, OutcomeID: binding.FixedIdentityKey,
			Evidence: cloneEvidenceRefs(evidenceByOutcome[binding.FixedIdentityKey]),
		})
	}
	selected := []lyricscontract.EvidenceRef{sekaiEvidence, vocaloidEvidence}
	sort.Slice(selected, func(left, right int) bool { return selected[left].EvidenceID < selected[right].EvidenceID })
	selectedSources := []string{sekaiOutcome, vocaloidOutcome}
	sort.Strings(selectedSources)
	return ReplayResult{
		MusicID: 42,
		Providers: []ProviderReplay{
			{Artifact: lyricsoutcomeartifact.Artifact{
				Provider: model.LyricsSourceProviderSekaipedia, OutcomeID: sekaiOutcome,
				ArtifactSHA256: strings.Repeat("9", 64),
			}, EvidenceRefs: []lyricscontract.EvidenceRef{sekaiEvidence}},
			{Artifact: lyricsoutcomeartifact.Artifact{
				Provider: model.LyricsSourceProviderMoegirl, OutcomeID: vocaloidOutcome,
				ArtifactSHA256: strings.Repeat("a", 64),
			}, EvidenceRefs: []lyricscontract.EvidenceRef{vocaloidEvidence}},
		},
		Composition: &lyricscompose.FixedArtifactComposition{
			Renditions: model.CloneLyricsSourceRenditions(renditions), SelectedSourceKeys: selectedSources,
		},
		Selected: selected, RenditionComponents: componentGroups,
	}
}

func bindRecoveryV3ReadingEvidence(
	full *model.LyricsSourceFull,
	fixedIdentityKey string,
	renditionKey string,
	side model.LyricsSourceRenditionSide,
) {
	if full == nil {
		return
	}
	for lineIndex := range full.Lines {
		for segmentIndex := range full.Lines[lineIndex].Segments {
			for spanIndex := range full.Lines[lineIndex].Segments[segmentIndex].Ruby {
				span := &full.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex]
				if span.Reading == "" {
					continue
				}
				span.ReadingEvidence = &model.LyricsSourceReadingEvidence{
					Kind:             model.LyricsSourceReadingEvidenceExplicitSourceKana,
					FixedIdentityKey: fixedIdentityKey, RenditionKey: renditionKey, Side: side,
					SourceRowOrdinal: lineIndex + 1, SourceSegmentOrdinal: segmentIndex + 1,
				}
			}
		}
	}
}

func legacyComparableRecoveryV3Full(full *model.LyricsSourceFull) *model.LyricsSourceFull {
	if full == nil {
		return nil
	}
	cloned := cloneLyricsSourceFull(*full)
	for lineIndex := range cloned.Lines {
		for segmentIndex := range cloned.Lines[lineIndex].Segments {
			for spanIndex := range cloned.Lines[lineIndex].Segments[segmentIndex].Ruby {
				cloned.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = nil
			}
		}
	}
	return &cloned
}

func TestReplayV3BindsCanonicalPeerRenditionComponents(t *testing.T) {
	fixture := recoveryV3ReplayFixture(t)
	providers := append([]ProviderReplay{}, fixture.Providers...)
	for index := range providers {
		providers[index].Outcome.Status = lyricsprovideroutcome.StatusCandidate
		providers[index].Fixed = &lyricssource.FixedRevision{}
	}
	bound, err := bindReplayComposition(ReplayResult{MusicID: fixture.MusicID, Providers: providers}, *fixture.Composition)
	if err != nil {
		t.Fatal(err)
	}
	if len(bound.RenditionComponents) != 2 || !componentEvidenceEmpty(bound.Components) || len(bound.Selected) != 2 {
		t.Fatalf("bound peer rendition evidence=%+v", bound)
	}
	componentCount := 0
	for _, rendition := range bound.RenditionComponents {
		componentCount += len(rendition.Components)
	}
	if componentCount != 13 || !reflect.DeepEqual(bound.RenditionComponents, fixture.RenditionComponents) {
		t.Fatalf("canonical peer rendition components=%+v", bound.RenditionComponents)
	}
	if _, err := NewSongResult(bound); err != nil {
		t.Fatalf("bound peer rendition result: %v", err)
	}

	mixed := *fixture.Composition
	mixed.ReasonCode = model.LyricsSourceVersionReasonTaggedFullAndGame
	if _, err := bindReplayComposition(ReplayResult{MusicID: fixture.MusicID, Providers: providers}, mixed); err == nil {
		t.Fatal("mixed singular/plural replay composition was accepted")
	}
}

func TestSongResultV3RoundTripCanonicalHashAndV2Compatibility(t *testing.T) {
	result, err := NewSongResult(recoveryV3ReplayFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != SongResultSchemaVersionV3 || result.State != lyricscontract.CoverageComplete ||
		len(result.Renditions) != 2 || result.Renditions[0].RenditionKey != "sekai" ||
		result.Renditions[1].RenditionKey != "vocaloid" || result.Full != nil || result.Renditions[1].Full != nil {
		t.Fatalf("peer rendition song result=%+v", result)
	}
	body, err := MarshalSongResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, []byte(`{"schemaVersion":3,"canonicalEncoding":`)) ||
		bytes.Contains(body, []byte(`"alternateVocals"`)) || bytes.Contains(body, []byte(`"full":null`)) {
		t.Fatalf("song result v3 canonical wire shape drifted: %s", body)
	}
	decoded, err := DecodeSongResult(body)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := MarshalSongResult(decoded)
	if err != nil || !bytes.Equal(body, roundTrip) {
		t.Fatalf("song result v3 canonical round trip drifted: err=%v", err)
	}
	hostile := map[string][]byte{
		"duplicate":               bytes.Replace(body, []byte(`"schemaVersion":3`), []byte(`"schemaVersion":3,"schemaVersion":3`), 1),
		"unknown":                 bytes.Replace(body, []byte(`{"schemaVersion"`), []byte(`{"unknown":1,"schemaVersion"`), 1),
		"legacy zero field":       bytes.Replace(body, []byte(`"providerOutcomes"`), []byte(`"components":{},"providerOutcomes"`), 1),
		"noncanonical whitespace": append([]byte("\n"), body...),
		"trailing":                append(append([]byte{}, body...), []byte("\n{}")...),
	}
	for name, candidate := range hostile {
		t.Run("decode "+name, func(t *testing.T) {
			if _, err := DecodeSongResult(candidate); err == nil {
				t.Fatal("hostile song result v3 was accepted")
			}
		})
	}
	bodyDigest := sha256.Sum256(body)
	const expectedV3BodySHA256 = "681c0535c35c80c5bb0048e99efa1d7cabd7bbf710c36af4be4b076f4ba1eae8"
	const expectedV3ResultSHA256 = "cf57159358f7fc4ce75a6de044ac3d605ad272b05106d207723cc43b425f0e13"
	if got := hex.EncodeToString(bodyDigest[:]); got != expectedV3BodySHA256 || result.ResultSHA256 != expectedV3ResultSHA256 {
		t.Fatalf("song result v3 canonical body SHA-256=%s resultSha256=%s", got, result.ResultSHA256)
	}

	cloned := cloneSongResult(result)
	cloned.Renditions[0].Full.Lines[0].Text = "mutated clone"
	cloned.Renditions[0].Components[0].Evidence[0].SHA256 = strings.Repeat("b", 64)
	if result.Renditions[0].Full.Lines[0].Text == cloned.Renditions[0].Full.Lines[0].Text ||
		result.Renditions[0].Components[0].Evidence[0].SHA256 == cloned.Renditions[0].Components[0].Evidence[0].SHA256 {
		t.Fatal("song result v3 clone aliases rendition payload or evidence")
	}

	v2, err := NewSongResult(noRomajiReplayFixture("歌唱者-01", "星乃一歌"))
	if err != nil {
		t.Fatal(err)
	}
	v2Body, err := MarshalSongResult(v2)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(v2Body, []byte(`"renditions"`)) {
		t.Fatal("song result v2 canonical bytes gained renditions")
	}
	v2BodyDigest := sha256.Sum256(v2Body)
	const expectedV2BodySHA256 = "f27f43ad49fbb9b9f4bbf34684a2a5142224a80e25c7bd78c0f7a2c1cc7cdf6b"
	const expectedV2ResultSHA256 = "152ec7c51e4a9567b42e070c5a01845d6e7633c352684f68103699807500a46f"
	if got := hex.EncodeToString(v2BodyDigest[:]); got != expectedV2BodySHA256 || v2.ResultSHA256 != expectedV2ResultSHA256 {
		t.Fatalf("song result v2 canonical body SHA-256=%s resultSha256=%s", got, v2.ResultSHA256)
	}
	v2Decoded, err := DecodeSongResult(v2Body)
	if err != nil {
		t.Fatal(err)
	}
	v2RoundTrip, err := MarshalSongResult(v2Decoded)
	if err != nil || !bytes.Equal(v2Body, v2RoundTrip) {
		t.Fatalf("song result v2 canonical bytes drifted: err=%v", err)
	}
	upgraded, err := UpconvertSongResultV2(v2Decoded)
	if err != nil {
		t.Fatal(err)
	}
	upgradedFullWithoutEvidence := legacyComparableRecoveryV3Full(upgraded.Renditions[0].Full)
	if upgraded.SchemaVersion != SongResultSchemaVersionV3 || len(upgraded.Renditions) != 1 ||
		upgraded.Renditions[0].RenditionKey != "sekai" || upgraded.Renditions[0].Full == nil ||
		!reflect.DeepEqual(*upgradedFullWithoutEvidence, *v2Decoded.Full) ||
		upgraded.Renditions[0].Relation.Kind != model.LyricsSourceRenditionRelationNone ||
		!reflect.DeepEqual(upgraded.Renditions[0].Translations, v2Decoded.Translations) {
		t.Fatalf("lossless song result v2 up-conversion=%+v", upgraded)
	}
	if got, err := MarshalSongResult(v2Decoded); err != nil || !bytes.Equal(got, v2Body) {
		t.Fatal("song result v2 input was mutated during up-conversion")
	}
}

func TestCanonicalSourceV3RecoveryV3RoundTripPreservesPeerContract(t *testing.T) {
	fixture := recoveryV3ReplayFixture(t)
	const sekaiOutcome = "outcome-sekaipedia-42-peer-sekai"
	const vocaloidOutcome = "outcome-moegirl-42-peer-vocaloid"
	source := model.LyricsSourceDocument{
		SchemaVersion: model.LyricsSourceDocumentSchemaVersionV3,
		FixedIdentities: []model.LyricsSourceFixedIdentity{
			{
				Provider: model.LyricsSourceProviderSekaipedia, Origin: model.LyricsSourceOriginSekaipedia,
				PageID: 42, RevisionID: 420, SHA1: strings.Repeat("b", 40), Title: "Peer SEKAI",
				CanonicalURL:      "https://www.sekaipedia.org/wiki/Peer_SEKAI?oldid=420",
				RevisionTimestamp: "2026-08-07T08:00:00Z", FetchedAt: "2026-08-07T08:01:00Z",
				Categories: []string{}, Section: "Full and Game", RenditionKey: sekaiOutcome,
				CompositionRenditionKey: "sekai", VersionReason: model.LyricsSourceVersionReasonTaggedFullAndGame,
				IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
					EvidenceID: "search:sekaipedia:42", SHA256: strings.Repeat("c", 64),
				}},
			},
			{
				Provider: model.LyricsSourceProviderMoegirl, Origin: model.LyricsSourceOriginMoegirl,
				PageID: 43, RevisionID: 421, SHA1: strings.Repeat("d", 40), Title: "Peer VOCALOID",
				CanonicalURL: "https://moegirl.icu/wiki/Peer_VOCALOID?oldid=421",
				FetchedAt:    "2026-08-07T08:01:00Z", Categories: []string{}, Section: "Game",
				RenditionKey: vocaloidOutcome, CompositionRenditionKey: "vocaloid",
				VersionReason: model.LyricsSourceVersionReasonTaggedGameOnly,
				IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
					EvidenceID: "search:moegirl:43", SHA256: strings.Repeat("e", 64),
				}},
			},
		},
		Renditions: model.CloneLyricsSourceRenditions(fixture.Composition.Renditions),
	}
	if err := model.ValidateLyricsSourceDocument(source); err != nil {
		t.Fatalf("canonical source v3 fixture: %v", err)
	}
	sourceBody, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	decodedSource, err := model.DecodeLyricsSourceDocument(sourceBody)
	if err != nil {
		t.Fatal(err)
	}
	sourceRoundTrip, err := json.Marshal(decodedSource)
	if err != nil || !bytes.Equal(sourceBody, sourceRoundTrip) {
		t.Fatalf("canonical source v3 round trip drifted: %v", err)
	}

	fixture.Composition.Renditions = model.CloneLyricsSourceRenditions(decodedSource.Renditions)
	result, err := NewSongResult(fixture)
	if err != nil {
		t.Fatal(err)
	}
	resultBody, err := MarshalSongResult(result)
	if err != nil {
		t.Fatal(err)
	}
	decodedResult, err := DecodeSongResult(resultBody)
	if err != nil {
		t.Fatal(err)
	}
	resultRoundTrip, err := MarshalSongResult(decodedResult)
	if err != nil || !bytes.Equal(resultBody, resultRoundTrip) {
		t.Fatalf("canonical recovery v3 round trip drifted: %v", err)
	}
	restored := make([]model.LyricsSourceRendition, len(decodedResult.Renditions))
	for index, rendition := range decodedResult.Renditions {
		restored[index], err = modelRenditionFromSongResult(rendition)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(decodedSource.Renditions, restored) {
		t.Fatalf("source-v3/recovery-v3 peer contract drifted: source=%+v recovery=%+v", decodedSource.Renditions, restored)
	}
}

func TestSongResultV2UpconversionPreservesFullGameAndGameOnlyShapes(t *testing.T) {
	fullGameReplay := noRomajiReplayFixture("歌唱者-01", "星乃一歌")
	fullGame := cloneLyricsSourceFull(fullGameReplay.Composition.Full)
	fullGame.Lines = []model.LyricsSourceFullLine{fullGame.Lines[0], fullGame.Lines[2]}
	fullGame.Lines[0].ID = "game-000001"
	fullGame.Lines[1].ID = "game-000002"
	fullGameReplay.Composition.Game = &fullGame
	fullGameReplay.Composition.GameProjection = &model.LyricsSourceGameProjection{
		LineIDs: []string{"full-000001", "full-000003"},
	}
	fullGameReplay.Composition.ReasonCode = model.LyricsSourceVersionReasonTaggedFullAndGame
	fullGameReplay.Composition.Components.GameText = "selected-source"
	fullGameReplay.Composition.Components.GameProjection = "selected-source"
	fullGameReplay.Components.GameText = cloneEvidenceRefs(fullGameReplay.Selected)
	fullGameReplay.Components.GameProjection = cloneEvidenceRefs(fullGameReplay.Selected)
	fullGameV2, err := NewSongResult(fullGameReplay)
	if err != nil {
		t.Fatal(err)
	}
	fullGameV3, err := UpconvertSongResultV2(fullGameV2)
	if err != nil {
		t.Fatal(err)
	}
	fullGameV3FullWithoutEvidence := legacyComparableRecoveryV3Full(fullGameV3.Renditions[0].Full)
	fullGameV3GameWithoutEvidence := legacyComparableRecoveryV3Full(fullGameV3.Renditions[0].Game)
	if len(fullGameV3.Renditions) != 1 || fullGameV3.Renditions[0].Full == nil ||
		fullGameV3.Renditions[0].Game == nil ||
		!reflect.DeepEqual(*fullGameV3FullWithoutEvidence, *fullGameV2.Full) ||
		!reflect.DeepEqual(*fullGameV3GameWithoutEvidence, *fullGameV2.Game) ||
		fullGameV3.Renditions[0].Relation.Kind != model.LyricsSourceRenditionRelationExactProjection ||
		!reflect.DeepEqual(fullGameV3.Renditions[0].Relation.LineIDs, fullGameV2.GameProjection.LineIDs) {
		t.Fatalf("Full+Game song result up-conversion=%+v", fullGameV3)
	}

	gameOnlyReplay := noRomajiReplayFixture("歌唱者-01", "星乃一歌")
	gameOnly := cloneLyricsSourceFull(gameOnlyReplay.Composition.Full)
	for index := range gameOnly.Lines {
		gameOnly.Lines[index].ID = []string{"game-000001", "game-000002", "game-000003"}[index]
	}
	gameOnlyReplay.Composition.Full = model.LyricsSourceFull{}
	gameOnlyReplay.Composition.Game = &gameOnly
	gameOnlyReplay.Composition.GameProjection = nil
	gameOnlyReplay.Composition.ReasonCode = model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid
	gameOnlyReplay.Composition.Components.FullText = ""
	gameOnlyReplay.Composition.Components.GameText = "selected-source"
	gameOnlyReplay.Components.FullText = []lyricscontract.EvidenceRef{}
	gameOnlyReplay.Components.GameText = cloneEvidenceRefs(gameOnlyReplay.Selected)
	gameOnlyV2, err := NewSongResult(gameOnlyReplay)
	if err != nil {
		t.Fatal(err)
	}
	gameOnlyV3, err := UpconvertSongResultV2(gameOnlyV2)
	if err != nil {
		t.Fatal(err)
	}
	gameOnlyV3GameWithoutEvidence := legacyComparableRecoveryV3Full(gameOnlyV3.Renditions[0].Game)
	if len(gameOnlyV3.Renditions) != 1 || gameOnlyV3.Renditions[0].Full != nil ||
		gameOnlyV3.Renditions[0].Game == nil ||
		!reflect.DeepEqual(*gameOnlyV3GameWithoutEvidence, *gameOnlyV2.Game) ||
		gameOnlyV3.Renditions[0].Relation.Kind != model.LyricsSourceRenditionRelationNone {
		t.Fatalf("Game-only song result up-conversion=%+v", gameOnlyV3)
	}
}

func TestSongResultV3CapturesIndependentGamePeerTranslations(t *testing.T) {
	fixture := recoveryV3ReplayFixture(t)
	rendition := fixture.Composition.Renditions[0]
	rendition.Relation = model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone}
	fixture.Composition.Renditions[0] = rendition
	fixture.Providers[0].Fixed = &lyricssource.FixedRevision{
		RenditionKey: "full-sekai",
		Extraction: lyricssource.Extraction{Lines: []lyricssource.StructuredLine{
			{Japanese: rendition.Full.Lines[0].Text},
			{Japanese: rendition.Full.Lines[1].Text},
			{Japanese: rendition.Full.Lines[2].Text},
		}},
		Translations: []string{"完整一", "完整二", "完整三"},
	}
	const gameOutcome = "outcome-sekaipedia-42-peer-sekai-game"
	gameEvidence := lyricscontract.EvidenceRef{
		Provider: model.LyricsSourceProviderVocaloidFandom, AcquisitionID: strings.Repeat("b", 64),
		EvidenceID: "revision:sekaipedia:42:422:" + strings.Repeat("c", 64),
		SHA256:     strings.Repeat("d", 64), EnvelopeSHA256: strings.Repeat("e", 64),
	}
	fixture.Providers = append(fixture.Providers, ProviderReplay{
		Artifact: lyricsoutcomeartifact.Artifact{
			Provider: model.LyricsSourceProviderVocaloidFandom, OutcomeID: gameOutcome,
			ArtifactSHA256: strings.Repeat("f", 64),
		},
		Fixed: &lyricssource.FixedRevision{
			RenditionKey: "game-sekai",
			Extraction: lyricssource.Extraction{Lines: []lyricssource.StructuredLine{
				{Japanese: rendition.Game.Lines[0].Text},
				{Japanese: rendition.Game.Lines[1].Text},
			}},
			Translations: []string{"游戏一", "游戏二"},
		},
		EvidenceRefs: []lyricscontract.EvidenceRef{gameEvidence},
	})
	fixture.Composition.Renditions[0].Provenance.GameText = &model.LyricsSourceComponentRef{RenditionKey: gameOutcome}
	fixture.Selected = append(fixture.Selected, gameEvidence)
	sort.Slice(fixture.Selected, func(left, right int) bool {
		return fixture.Selected[left].EvidenceID < fixture.Selected[right].EvidenceID
	})
	for renditionIndex := range fixture.RenditionComponents {
		if fixture.RenditionComponents[renditionIndex].RenditionKey != rendition.RenditionKey {
			continue
		}
		for componentIndex := range fixture.RenditionComponents[renditionIndex].Components {
			component := &fixture.RenditionComponents[renditionIndex].Components[componentIndex]
			if component.Component == model.LyricsSourceRenditionComponentGameText ||
				component.Component == model.LyricsSourceRenditionComponentRelation {
				component.OutcomeID = gameOutcome
				component.Evidence = []lyricscontract.EvidenceRef{gameEvidence}
			}
		}
	}
	result, err := NewSongResult(fixture)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Renditions[0]
	if !reflect.DeepEqual(got.Translations, []string{"完整一", "完整二", "完整三"}) ||
		len(got.PeerTranslations) != 1 || got.PeerTranslations[0].Side != "game" ||
		got.PeerTranslations[0].Locale != "zh-CN" ||
		!reflect.DeepEqual(got.PeerTranslations[0].Translations, []string{"游戏一", "游戏二"}) {
		t.Fatalf("independent Game translations=%+v", got)
	}
	body, err := MarshalSongResult(result)
	if err != nil || !bytes.Contains(body, []byte(`"peerTranslations":[{"side":"game","locale":"zh-CN","translations":["游戏一","游戏二"]}]`)) {
		t.Fatalf("peer translations canonical body=%s err=%v", body, err)
	}
	decoded, err := DecodeSongResult(body)
	if err != nil || !reflect.DeepEqual(decoded.Renditions[0].PeerTranslations, got.PeerTranslations) {
		t.Fatalf("peer translation round trip=%+v err=%v", decoded.Renditions, err)
	}
	unknown := bytes.Replace(body, []byte(`"side":"game"`), []byte(`"side":"game","unknown":true`), 1)
	duplicate := bytes.Replace(body, []byte(`"side":"game"`), []byte(`"side":"game","side":"game"`), 1)
	empty := bytes.Replace(body,
		[]byte(`"peerTranslations":[{"side":"game","locale":"zh-CN","translations":["游戏一","游戏二"]}]`),
		[]byte(`"peerTranslations":[]`), 1)
	for name, hostile := range map[string][]byte{"unknown nested field": unknown, "duplicate nested field": duplicate, "explicit empty peers": empty} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSongResult(hostile); err == nil {
				t.Fatal("non-canonical peer translation JSON was accepted")
			}
		})
	}
}

func TestSongResultV3RejectsInvalidPeerRenditionAndEvidenceShapes(t *testing.T) {
	base, err := NewSongResult(recoveryV3ReplayFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	removeComponent := func(result *SongResult, kind model.LyricsSourceRenditionComponentKind) {
		components := result.Renditions[0].Components
		for index, component := range components {
			if component.Component == kind {
				result.Renditions[0].Components = append(components[:index:index], components[index+1:]...)
				return
			}
		}
	}
	tests := map[string]func(*SongResult){
		"non canonical rendition order": func(result *SongResult) {
			result.Renditions[0], result.Renditions[1] = result.Renditions[1], result.Renditions[0]
		},
		"duplicate rendition key": func(result *SongResult) {
			result.Renditions[1].RenditionKey = result.Renditions[0].RenditionKey
		},
		"duplicate source tab path": func(result *SongResult) {
			result.Renditions[1].SourceTabPaths[0] = append(model.LyricsSourceTabPath{}, result.Renditions[0].SourceTabPaths[0]...)
		},
		"unknown source kind": func(result *SongResult) { result.Renditions[0].SourceKind = "other" },
		"cross rendition projection": func(result *SongResult) {
			result.Renditions[0].Relation.FullRenditionKey = result.Renditions[1].RenditionKey
		},
		"unknown relation": func(result *SongResult) { result.Renditions[0].Relation.Kind = "approximate" },
		"none relation projection data": func(result *SongResult) {
			result.Renditions[1].Relation.LineIDs = []string{"full-000001"}
		},
		"duplicate exact projection line ID": func(result *SongResult) {
			result.Renditions[0].Relation.LineIDs[1] = result.Renditions[0].Relation.LineIDs[0]
		},
		"non exact projection": func(result *SongResult) {
			game := result.Renditions[0].Game
			lineID := game.Lines[0].ID
			game.Lines[0] = result.Renditions[0].Full.Lines[1]
			game.Lines[0].ID = lineID
		},
		"missing component ref": func(result *SongResult) {
			removeComponent(result, model.LyricsSourceRenditionComponentFullText)
		},
		"duplicate component ref": func(result *SongResult) {
			component := result.Renditions[0].Components[0]
			result.Renditions[0].Components = append(result.Renditions[0].Components, component)
		},
		"non canonical component order": func(result *SongResult) {
			result.Renditions[0].Components[0], result.Renditions[0].Components[1] =
				result.Renditions[0].Components[1], result.Renditions[0].Components[0]
		},
		"unknown outcome": func(result *SongResult) {
			result.Renditions[0].Components[0].OutcomeID = "missing-outcome"
		},
		"missing exact evidence list": func(result *SongResult) {
			result.Renditions[0].Components[0].Evidence = nil
		},
		"evidence provider mismatch": func(result *SongResult) {
			result.Renditions[0].Components[0].Evidence[0].Provider = model.LyricsSourceProviderMoegirl
		},
		"evidence outside selected union": func(result *SongResult) {
			result.Renditions[0].Components[0].Evidence[0].SHA256 = strings.Repeat("b", 64)
		},
		"unreferenced selected evidence": func(result *SongResult) {
			result.SelectedEvidence = append(result.SelectedEvidence, lyricscontract.EvidenceRef{
				Provider: model.LyricsSourceProviderSekaipedia, AcquisitionID: strings.Repeat("b", 64),
				EvidenceID: "revision:sekaipedia:42:422:" + strings.Repeat("c", 64),
				SHA256:     strings.Repeat("d", 64), EnvelopeSHA256: strings.Repeat("e", 64),
			})
			sort.Slice(result.SelectedEvidence, func(left, right int) bool {
				return result.SelectedEvidence[left].EvidenceID < result.SelectedEvidence[right].EvidenceID
			})
		},
		"legacy singular field": func(result *SongResult) {
			result.ReasonCode = model.LyricsSourceVersionReasonTaggedFullAndGame
		},
		"legacy empty component field": func(result *SongResult) {
			result.Components.FullText = []lyricscontract.EvidenceRef{}
		},
		"coverage state disagrees with renditions": func(result *SongResult) {
			result.State = lyricscontract.CoverageGameOnly
		},
		"exact projection peer translation": func(result *SongResult) {
			result.Renditions[0].PeerTranslations = []SongResultPeerTranslation{{
				Side: "game", Locale: "zh-CN", Translations: []string{"非法一", "非法二"},
			}}
		},
		"invalid peer locale": func(result *SongResult) {
			result.Renditions[1].PeerTranslations = []SongResultPeerTranslation{{
				Side: "game", Locale: "en-US", Translations: []string{"非法一", "非法二", "非法三"},
			}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			result := cloneSongResult(base)
			result.ResultSHA256 = ""
			mutate(&result)
			if err := validateSongResult(result, false); err == nil {
				t.Fatal("invalid song result v3 was accepted")
			}
		})
	}
}
