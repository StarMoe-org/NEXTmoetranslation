package lyricsrecoveryimport

import (
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
	"moesekai/server/offline/internal/lyricsrecovery"
)

func TestV3OneRenditionVocaloidRebindsFixedIdentity(t *testing.T) {
	const outcomeID = "outcome:sekaipedia:1:fixture"
	full := v3AdapterTestFull("vocaloid", "full-000001", "line-a", outcomeID, "vocaloid", model.LyricsSourceRenditionSideFull)
	result := lyricsrecovery.SongResultRendition{
		RenditionKey: "vocaloid",
		SourceKind:   model.LyricsSourceRenditionVocaloid,
		Full:         full,
		Components: []lyricsrecovery.RenditionComponentEvidenceRef{
			{Component: model.LyricsSourceRenditionComponentFullText, OutcomeID: outcomeID},
			{Component: model.LyricsSourceRenditionComponentFullRuby, OutcomeID: outcomeID},
		},
	}

	rendering, err := sourceRenditionFromResult(result)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := v3FixedIdentityKey(outcomeID, "vocaloid")
	if len(wantKey) != len("fixed-")+64 || !strings.HasPrefix(wantKey, "fixed-") || strings.Contains(wantKey, ":") {
		t.Fatalf("derived fixed identity key %q is not a bounded persisted key", wantKey)
	}
	if rendering.Provenance.FullText == nil || rendering.Provenance.FullRuby == nil ||
		rendering.Provenance.FullText.RenditionKey != wantKey || rendering.Provenance.FullRuby.RenditionKey != wantKey {
		t.Fatalf("Vocaloid component refs were not rebound to %q: %+v", wantKey, rendering.Provenance)
	}
	gotEvidence := rendering.Full.Lines[0].Segments[0].Ruby[0].ReadingEvidence
	if gotEvidence == nil || gotEvidence.FixedIdentityKey != wantKey || gotEvidence.RenditionKey != "vocaloid" {
		t.Fatalf("Vocaloid reading evidence was not rebound: %+v", gotEvidence)
	}
	originalEvidence := result.Full.Lines[0].Segments[0].Ruby[0].ReadingEvidence
	if originalEvidence == nil || originalEvidence.FixedIdentityKey != outcomeID {
		t.Fatalf("source result was mutated: %+v", originalEvidence)
	}
}

func TestRenditionTranslationsFromResultPreservesIndependentGamePeer(t *testing.T) {
	result := lyricsrecovery.SongResult{Renditions: []lyricsrecovery.SongResultRendition{{
		RenditionKey: "sekai", Translations: []string{"主译文"},
		PeerTranslations: []lyricsrecovery.SongResultPeerTranslation{{
			Side: "game", Locale: "zh-CN", Translations: []string{"游戏译文"},
		}},
	}}}
	got := renditionTranslationsFromResult(result)
	if len(got) != 1 || got[0].RenditionKey != "sekai" ||
		!reflect.DeepEqual(got[0].Translations, []string{"主译文"}) || len(got[0].PeerTranslations) != 1 ||
		got[0].PeerTranslations[0].Side != "game" || got[0].PeerTranslations[0].Locale != "zh-CN" ||
		!reflect.DeepEqual(got[0].PeerTranslations[0].Translations, []string{"游戏译文"}) {
		t.Fatalf("mapped peer translations=%+v", got)
	}
	result.Renditions[0].PeerTranslations[0].Translations[0] = "mutated"
	if got[0].PeerTranslations[0].Translations[0] != "游戏译文" {
		t.Fatal("mapped peer translations alias the recovery result")
	}
}

func TestMatchesV3SelectedAcquisitionBindsAcquisitionID(t *testing.T) {
	selected := lyricscontract.EvidenceRef{
		Provider: model.LyricsSourceProviderSekaipedia, AcquisitionID: strings.Repeat("a", 64),
		EvidenceID: "revision:sekaipedia:1:2:" + strings.Repeat("b", 64),
		SHA256:     strings.Repeat("c", 64), EnvelopeSHA256: strings.Repeat("d", 64),
	}
	acquisition := lyricsoutcomeartifact.AcquisitionRef{
		AcquisitionID: selected.AcquisitionID, EvidenceID: selected.EvidenceID,
		SHA256: selected.SHA256, EnvelopeSHA256: selected.EnvelopeSHA256,
	}
	if !matchesV3SelectedAcquisition(selected, selected.Provider, acquisition) {
		t.Fatal("matching exact acquisition was rejected")
	}
	acquisition.AcquisitionID = strings.Repeat("e", 64)
	if matchesV3SelectedAcquisition(selected, selected.Provider, acquisition) {
		t.Fatal("acquisition identity drift was accepted")
	}
}

func TestExactPublicV3RenditionRejectsFlattenedOrAmbiguousResults(t *testing.T) {
	const outcomeID = "outcome:moegirl:1:fixture"
	candidate := lyricsoutcomeartifact.CandidateIdentity{
		RenditionKey: "full-vocaloid", VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
	}
	full := v3AdapterTestFull("vocaloid", "full-000001", "line-a", outcomeID, "vocaloid", model.LyricsSourceRenditionSideFull)
	valid := lyricsrecovery.SongResult{Renditions: []lyricsrecovery.SongResultRendition{{
		RenditionKey: "vocaloid", SourceKind: model.LyricsSourceRenditionVocaloid,
		ReasonCode: candidate.VersionReason, Full: full,
		Relation: model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
		Components: []lyricsrecovery.RenditionComponentEvidenceRef{{
			Component: model.LyricsSourceRenditionComponentVersion, OutcomeID: outcomeID,
		}},
		Translations: []string{"译文"},
	}}}
	if rendition, err := exactPublicV3Rendition(valid, outcomeID, candidate); err != nil || rendition == nil {
		t.Fatalf("valid exact-public rendition rejected: rendition=%v err=%v", rendition, err)
	}
	for name, mutate := range map[string]func(*lyricsrecovery.SongResult){
		"flattened game": func(result *lyricsrecovery.SongResult) {
			result.Renditions[0].Game = full
		},
		"ambiguous families": func(result *lyricsrecovery.SongResult) {
			result.Renditions = append(result.Renditions, result.Renditions[0])
		},
		"wrong family": func(result *lyricsrecovery.SongResult) {
			result.Renditions[0].RenditionKey = "sekai"
		},
		"peer translation": func(result *lyricsrecovery.SongResult) {
			result.Renditions[0].PeerTranslations = []lyricsrecovery.SongResultPeerTranslation{{
				Side: "game", Locale: "zh-CN", Translations: []string{"forbidden"},
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := valid
			result.Renditions = append([]lyricsrecovery.SongResultRendition(nil), valid.Renditions...)
			mutate(&result)
			if _, err := exactPublicV3Rendition(result, outcomeID, candidate); err == nil {
				t.Fatal("invalid exact-public rendition was accepted")
			}
		})
	}
}

func TestCompareV3ProjectionAcceptsSEKAIExactProjection(t *testing.T) {
	const outcomeID = "outcome:sekaipedia:2:fixture"
	full := v3AdapterTestFull("sekai", "full-000001", "line-a", outcomeID, "sekai", model.LyricsSourceRenditionSideFull)
	game := v3AdapterTestFull("sekai", "game-000001", "line-a", outcomeID, "sekai", model.LyricsSourceRenditionSideGame)
	result := lyricsrecovery.SongResult{Renditions: []lyricsrecovery.SongResultRendition{{
		RenditionKey: "sekai", SourceKind: model.LyricsSourceRenditionSekai,
		ReasonCode: model.LyricsSourceVersionReasonTaggedFullAndGame,
		Full:       full, Game: game,
		Relation: model.LyricsSourceRenditionRelation{
			Kind: model.LyricsSourceRenditionRelationExactProjection, FullRenditionKey: "sekai",
			LineIDs: []string{"full-000001"},
		},
		Components: []lyricsrecovery.RenditionComponentEvidenceRef{
			{Component: model.LyricsSourceRenditionComponentFullRuby, OutcomeID: outcomeID},
			{Component: model.LyricsSourceRenditionComponentGameRuby, OutcomeID: outcomeID},
			{Component: model.LyricsSourceRenditionComponentVersion, OutcomeID: outcomeID},
		},
	}}}
	projection := lyricssource.SekaipediaRecoveryProjection{
		RenditionKey: "full-sekai", ReasonCode: model.LyricsSourceVersionReasonTaggedFullAndGame,
		Full: *legacyComparableV3Full(full), Game: legacyComparableV3Full(game),
		GameProjection: &model.LyricsSourceGameProjection{LineIDs: []string{"full-000001"}},
	}
	candidate := lyricsoutcomeartifact.CandidateIdentity{
		RenditionKey: "full-sekai", VersionReason: model.LyricsSourceVersionReasonTaggedFullAndGame,
	}
	if err := compareV3Projection(result, outcomeID, candidate, projection); err != nil {
		t.Fatalf("exact SEKAI projection was rejected: %v", err)
	}
	projection.Game.Lines[0].Text = "drift"
	if err := compareV3Projection(result, outcomeID, candidate, projection); err == nil {
		t.Fatal("SEKAI Game text drift was accepted")
	}
	projection.Game = legacyComparableV3Full(game)
	projection.GameProjection.LineIDs[0] = "full-999999"
	if err := compareV3Projection(result, outcomeID, candidate, projection); err == nil {
		t.Fatal("SEKAI exact relation drift was accepted")
	}
}

func TestCompareV3ProjectionAcceptsREMTwoPeers(t *testing.T) {
	const outcomeID = "outcome:sekaipedia:765:fixture"
	sekaiFull := v3AdapterTestFull("sekai", "full-000001", "line-a", outcomeID, "sekai", model.LyricsSourceRenditionSideFull)
	sekaiGame := v3AdapterTestFull("sekai", "game-000001", "line-a", outcomeID, "sekai", model.LyricsSourceRenditionSideGame)
	vocaloidGame := v3AdapterTestFull("vocaloid", "game-000001", "line-b", outcomeID, "vocaloid", model.LyricsSourceRenditionSideGame)
	result := lyricsrecovery.SongResult{Renditions: []lyricsrecovery.SongResultRendition{
		{
			RenditionKey: "sekai", SourceKind: model.LyricsSourceRenditionSekai,
			ReasonCode: model.LyricsSourceVersionReasonTaggedFullAndGame,
			Full:       sekaiFull, Game: sekaiGame,
			Relation: model.LyricsSourceRenditionRelation{
				Kind: model.LyricsSourceRenditionRelationExactProjection, FullRenditionKey: "sekai",
				LineIDs: []string{"full-000001"},
			},
			Components: []lyricsrecovery.RenditionComponentEvidenceRef{
				{Component: model.LyricsSourceRenditionComponentFullRuby, OutcomeID: outcomeID},
				{Component: model.LyricsSourceRenditionComponentGameRuby, OutcomeID: outcomeID},
				{Component: model.LyricsSourceRenditionComponentVersion, OutcomeID: outcomeID},
			},
		},
		{
			RenditionKey: "vocaloid", SourceKind: model.LyricsSourceRenditionVocaloid,
			ReasonCode: model.LyricsSourceVersionReasonTaggedGameOnly,
			Game:       vocaloidGame,
			Relation:   model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
			Components: []lyricsrecovery.RenditionComponentEvidenceRef{
				{Component: model.LyricsSourceRenditionComponentGameRuby, OutcomeID: outcomeID},
				{Component: model.LyricsSourceRenditionComponentVersion, OutcomeID: outcomeID},
			},
		},
	}}
	projection := lyricssource.SekaipediaRecoveryProjection{
		RenditionKey: "full-sekai", ReasonCode: model.LyricsSourceVersionReasonTaggedFullAndGame,
		Full: *legacyComparableV3Full(sekaiFull), Game: legacyComparableV3Full(sekaiGame),
		GameProjection: &model.LyricsSourceGameProjection{LineIDs: []string{"full-000001"}},
	}
	candidate := lyricsoutcomeartifact.CandidateIdentity{
		RenditionKey: "full-sekai", VersionReason: model.LyricsSourceVersionReasonTaggedFullAndGame,
	}

	if err := compareV3Projection(result, outcomeID, candidate, projection); err != nil {
		t.Fatalf("REM peer projection was rejected: %v", err)
	}
	sekaiRendering, err := sourceRenditionFromResult(result.Renditions[0])
	if err != nil {
		t.Fatal(err)
	}
	vocaloidRendering, err := sourceRenditionFromResult(result.Renditions[1])
	if err != nil {
		t.Fatal(err)
	}
	sekaiKey := sekaiRendering.Provenance.VersionEvidence.RenditionKey
	vocaloidKey := vocaloidRendering.Provenance.VersionEvidence.RenditionKey
	if sekaiKey == vocaloidKey || sekaiKey != v3FixedIdentityKey(outcomeID, "sekai") ||
		vocaloidKey != v3FixedIdentityKey(outcomeID, "vocaloid") {
		t.Fatalf("shared outcome was not split by logical family: sekai=%q vocaloid=%q", sekaiKey, vocaloidKey)
	}
}

func TestCompareV3ProjectionAcceptsGameOnly(t *testing.T) {
	const outcomeID = "outcome:sekaipedia:3:fixture"
	game := v3AdapterTestFull("vocaloid", "game-000001", "line-a", outcomeID, "vocaloid", model.LyricsSourceRenditionSideGame)
	result := lyricsrecovery.SongResult{Renditions: []lyricsrecovery.SongResultRendition{{
		RenditionKey: "vocaloid", SourceKind: model.LyricsSourceRenditionVocaloid,
		ReasonCode: model.LyricsSourceVersionReasonTaggedGameOnly,
		Game:       game,
		Relation:   model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
		Components: []lyricsrecovery.RenditionComponentEvidenceRef{
			{Component: model.LyricsSourceRenditionComponentGameRuby, OutcomeID: outcomeID},
			{Component: model.LyricsSourceRenditionComponentVersion, OutcomeID: outcomeID},
		},
	}}}
	projection := lyricssource.SekaipediaRecoveryProjection{
		RenditionKey: "game-vocaloid", ReasonCode: model.LyricsSourceVersionReasonTaggedGameOnly,
		Game: legacyComparableV3Full(game),
	}
	candidate := lyricsoutcomeartifact.CandidateIdentity{
		RenditionKey: "game-vocaloid", VersionReason: model.LyricsSourceVersionReasonTaggedGameOnly,
	}

	if err := compareV3Projection(result, outcomeID, candidate, projection); err != nil {
		t.Fatalf("Game-only projection was rejected: %v", err)
	}
}

func TestCompareV3ProjectionRejectsWrongFamily(t *testing.T) {
	const outcomeID = "outcome:sekaipedia:4:fixture"
	full := v3AdapterTestFull("vocaloid", "full-000001", "line-a", outcomeID, "vocaloid", model.LyricsSourceRenditionSideFull)
	result := lyricsrecovery.SongResult{Renditions: []lyricsrecovery.SongResultRendition{{
		RenditionKey: "vocaloid", SourceKind: model.LyricsSourceRenditionVocaloid,
		ReasonCode: model.LyricsSourceVersionReasonUntaggedFullOnly,
		Full:       full,
		Relation:   model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
		Components: []lyricsrecovery.RenditionComponentEvidenceRef{{
			Component: model.LyricsSourceRenditionComponentVersion, OutcomeID: outcomeID,
		}},
	}}}
	projection := lyricssource.SekaipediaRecoveryProjection{
		RenditionKey: "full-sekai", ReasonCode: model.LyricsSourceVersionReasonUntaggedFullOnly,
		Full: *legacyComparableV3Full(full),
	}
	candidate := lyricsoutcomeartifact.CandidateIdentity{
		RenditionKey: "full-sekai", VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
	}

	if err := compareV3Projection(result, outcomeID, candidate, projection); err == nil ||
		!strings.Contains(err.Error(), "matching rendition family") {
		t.Fatalf("wrong-family projection error=%v", err)
	}
}

func v3AdapterTestFull(
	kind string,
	lineID string,
	text string,
	outcomeID string,
	renditionKey string,
	side model.LyricsSourceRenditionSide,
) *model.LyricsSourceFull {
	return &model.LyricsSourceFull{
		Version:              model.LyricsSourceVersion{Kind: kind, Label: strings.ToUpper(kind)},
		Performers:           []model.LyricsSourcePerformer{},
		RubyGeneratorVersion: "sekaipedia-ruby-kana-v2",
		Lines: []model.LyricsSourceFullLine{{
			ID: lineID, Text: text,
			Segments: []model.LyricsSourceSegment{{
				Text: text, PerformerIDs: []string{},
				Ruby: []model.LyricsSourceRubySpan{{
					Text: text, Reading: "かな",
					ReadingEvidence: &model.LyricsSourceReadingEvidence{
						Kind:             model.LyricsSourceReadingEvidenceExplicitSourceKana,
						FixedIdentityKey: outcomeID, RenditionKey: renditionKey, Side: side,
						SourceRowOrdinal: 1, SourceSegmentOrdinal: 1,
					},
				}},
			}},
			TrailingPerformerIDs: []string{},
		}},
	}
}

func TestComparableV3ProjectionFullPreservesExactRubyWithoutSourceComponent(t *testing.T) {
	full := v3AdapterTestFull("sekai", "full-000001", "line-a", "outcome:fixture", "sekai", model.LyricsSourceRenditionSideFull)
	projection := legacyComparableV3Full(full)
	rendition := lyricsrecovery.SongResultRendition{Components: []lyricsrecovery.RenditionComponentEvidenceRef{{
		Component: model.LyricsSourceRenditionComponentFullText, OutcomeID: "outcome:fixture",
	}}}
	comparable, err := comparableV3ProjectionFull(projection, full, rendition, model.LyricsSourceRenditionSideFull)
	if err != nil {
		t.Fatal(err)
	}
	span := comparable.Lines[0].Segments[0].Ruby[0]
	if comparable.RubyGeneratorVersion == "" || span.Reading == "" || span.ReadingEvidence != nil {
		t.Fatalf("exact reparsed ruby was not preserved independently of source identity evidence: %+v", comparable)
	}
	if !reflect.DeepEqual(comparable, projection) {
		t.Fatal("projection normalization changed exact text or ruby without performer metadata to rebind")
	}
}

func TestLegacyComparableV3FullPreservesAllNonIdentityFields(t *testing.T) {
	full := v3AdapterTestFull("sekai", "full-000001", "line-a", "outcome:fixture", "sekai", model.LyricsSourceRenditionSideFull)
	comparable := legacyComparableV3Full(full)
	if comparable == nil || comparable.Lines[0].Segments[0].Ruby[0].ReadingEvidence != nil {
		t.Fatalf("identity-only reading evidence was not removed: %+v", comparable)
	}
	withoutEvidence := model.CloneLyricsSourceFull(full)
	withoutEvidence.Lines[0].Segments[0].Ruby[0].ReadingEvidence = nil
	if !reflect.DeepEqual(comparable, withoutEvidence) {
		t.Fatal("legacy comparison changed non-identity text, ruby, or relation input fields")
	}
	if full.Lines[0].Segments[0].Ruby[0].ReadingEvidence == nil {
		t.Fatal("legacy comparison mutated the recovery result")
	}
}
