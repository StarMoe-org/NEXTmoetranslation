package lyricscompose

import (
	"fmt"
	"reflect"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
)

func fixedPeerRenditionInputs(t *testing.T) []FixedArtifactInput {
	t.Helper()
	sekai := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "peer-sekai-source",
		logicalRendition: "full-sekai", revisionTimestamp: "2026-08-01T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		reason:  model.LyricsSourceVersionReasonTaggedFullAndGame,
		texts:   []string{"歌う", "未来へ", "進もう"}, gamePositions: []int{0, 2},
		performerID: "ichika", performerName: "Hoshino Ichika", reading: "うた",
	})
	sekaiDocument := *sekai.Fixed.Document
	sekaiGame := *cloneModelFull(&sekaiDocument.Full)
	sekaiGame.Lines = []model.LyricsSourceFullLine{sekaiGame.Lines[0], sekaiGame.Lines[2]}
	for index := range sekaiGame.Lines {
		sekaiGame.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	sekaiRef := model.LyricsSourceComponentRef{RenditionKey: sekai.SourceKey}
	sekaiDocument.Game = &sekaiGame
	sekaiDocument.Provenance.GameText = &sekaiRef
	sekaiV3, err := model.UpconvertLyricsSourceDocumentV2(sekaiDocument)
	if err != nil {
		t.Fatal(err)
	}
	sekaiV3.Renditions[0].SourceTabPaths = []model.LyricsSourceTabPath{
		{"Full Version"}, {"Game Version", "SEKAI"},
	}
	if err := model.ValidateLyricsSourceDocument(sekaiV3); err != nil {
		t.Fatal(err)
	}
	sekai.Fixed.Document = &sekaiV3
	sekai.Fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, sekaiV3.FixedIdentities...)

	vocaloid := newFixedDocumentInput(fixedDocumentFixture{
		provider: model.LyricsSourceProviderSekaipedia, sourceKey: "peer-vocaloid-source",
		logicalRendition: "game-vocaloid", revisionTimestamp: "2026-08-01T00:00:00Z",
		version: model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER"},
		reason:  model.LyricsSourceVersionReasonUntaggedFullOnly,
		texts:   []string{"歌う", "未来へ"}, performerID: "miku", performerName: "Hatsune Miku",
		reading: "うた", privateReview: true,
	})
	vocaloidDocument := *vocaloid.Fixed.Document
	vocaloidGame := *cloneModelFull(&vocaloidDocument.Full)
	for index := range vocaloidGame.Lines {
		vocaloidGame.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	vocaloidRef := vocaloidDocument.Provenance.FullText
	vocaloidDocument.ReasonCode = model.LyricsSourceVersionReasonTaggedGameOnly
	vocaloidDocument.Full = model.LyricsSourceFull{}
	vocaloidDocument.Game = &vocaloidGame
	vocaloidDocument.Provenance.FullText = model.LyricsSourceComponentRef{}
	vocaloidDocument.Provenance.GameText = &vocaloidRef
	vocaloidDocument.GameProjection = nil
	vocaloidDocument.Provenance.GameProjection = nil
	vocaloid.Fixed.VersionReason = model.LyricsSourceVersionReasonTaggedGameOnly
	vocaloidV3, err := model.UpconvertLyricsSourceDocumentV2(vocaloidDocument)
	if err != nil {
		t.Fatal(err)
	}
	vocaloidV3.Renditions[0].SourceTabPaths = []model.LyricsSourceTabPath{
		{"Game Version", "VIRTUAL SINGER"},
	}
	if err := model.ValidateLyricsSourceDocument(vocaloidV3); err != nil {
		t.Fatal(err)
	}
	vocaloid.Fixed.Document = &vocaloidV3
	vocaloid.Fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, vocaloidV3.FixedIdentities...)
	return []FixedArtifactInput{sekai, vocaloid}
}

func TestComposeFixedArtifactsV3PreservesPeerRenditionsAndCanonicalComponents(t *testing.T) {
	inputs := fixedPeerRenditionInputs(t)
	composition, err := ComposeFixedArtifacts(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(composition.Renditions) != 2 || composition.ReasonCode != "" || len(composition.Full.Lines) != 0 ||
		composition.Game != nil || composition.GameProjection != nil ||
		!reflect.DeepEqual(composition.SelectedSourceKeys, []string{"peer-sekai-source", "peer-vocaloid-source"}) {
		t.Fatalf("peer composition=%+v", composition)
	}
	sekai, vocaloid := composition.Renditions[0], composition.Renditions[1]
	if sekai.RenditionKey != "sekai" || sekai.SourceKind != model.LyricsSourceRenditionSekai ||
		sekai.Full == nil || sekai.Game == nil || sekai.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection ||
		!reflect.DeepEqual(sekai.Relation.LineIDs, []string{"full-000001", "full-000003"}) {
		t.Fatalf("SEKAI peer rendition=%+v", sekai)
	}
	if vocaloid.RenditionKey != "vocaloid" || vocaloid.SourceKind != model.LyricsSourceRenditionVocaloid ||
		vocaloid.Full != nil || vocaloid.Game == nil || vocaloid.Relation.Kind != model.LyricsSourceRenditionRelationNone {
		t.Fatalf("VIRTUAL SINGER peer rendition=%+v", vocaloid)
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(composition.Renditions)
	if err != nil || len(bindings) != 13 {
		t.Fatalf("peer component enumeration=%+v err=%v", bindings, err)
	}
}

func TestComposeFixedArtifactsV3IsIndependentOfInputOrder(t *testing.T) {
	inputs := fixedPeerRenditionInputs(t)
	forward, err := ComposeFixedArtifacts(inputs)
	if err != nil {
		t.Fatal(err)
	}
	reverse := []FixedArtifactInput{inputs[1], inputs[0]}
	backward, err := ComposeFixedArtifacts(reverse)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, backward) {
		t.Fatalf("peer composition changed with input order\nforward=%+v\nbackward=%+v", forward, backward)
	}
}

func TestComposeFixedArtifactsV3FailsClosedOnConflictingPeerPayload(t *testing.T) {
	inputs := fixedPeerRenditionInputs(t)
	conflict := inputs[0]
	conflict.SourceKey = "peer-sekai-conflict"
	conflictDocument := *conflict.Fixed.Document
	conflictDocument.Renditions = model.CloneLyricsSourceRenditions(conflictDocument.Renditions)
	line := conflictDocument.Renditions[0].Full.Lines[0]
	line.Text = conflictDocument.Renditions[0].Full.Lines[1].Text
	line.Segments = append([]model.LyricsSourceSegment{}, conflictDocument.Renditions[0].Full.Lines[1].Segments...)
	line.TrailingPerformerIDs = append([]string{}, conflictDocument.Renditions[0].Full.Lines[1].TrailingPerformerIDs...)
	conflictDocument.Renditions[0].Full.Lines[0] = line
	conflict.Fixed.Document = &conflictDocument
	if _, err := ComposeFixedArtifacts([]FixedArtifactInput{inputs[0], conflict, inputs[1]}); err == nil {
		t.Fatal("conflicting peer rendition payload was accepted")
	}
}

func TestComposeFixedArtifactsV3RestoresPartialVocaloidGameSegmentationThroughLegacyBridge(t *testing.T) {
	inputs := fixedPeerRenditionInputs(t)
	vocaloid := inputs[1]
	document := *vocaloid.Fixed.Document
	document.Renditions = model.CloneLyricsSourceRenditions(document.Renditions)
	rendition := &document.Renditions[0]
	rendition.PrivateReview = nil
	rendition.GamePerformerEvidence = model.LyricsSourcePerformerEvidenceSourcePartial
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("partial Vocaloid Game fixture: %v", err)
	}
	vocaloid.Fixed.Document = &document
	vocaloid.Fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, document.FixedIdentities...)

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{vocaloid})
	if err != nil {
		t.Fatal(err)
	}
	if len(composition.Renditions) != 1 {
		t.Fatalf("partial Vocaloid composition renditions=%d", len(composition.Renditions))
	}
	rendition = &composition.Renditions[0]
	if rendition.SourceKind != model.LyricsSourceRenditionVocaloid || rendition.Full != nil || rendition.Game == nil ||
		rendition.GamePerformerEvidence != model.LyricsSourcePerformerEvidenceSourcePartial ||
		rendition.Provenance.GamePerformerSegmentation == nil || len(rendition.Game.Performers) != 1 ||
		len(rendition.Game.Lines[0].Segments[0].PerformerIDs) == 0 {
		t.Fatalf("partial Vocaloid Game segmentation was not restored: %+v", rendition)
	}
}

func TestComposeFixedArtifactsV3RestoresPartialVocaloidFullSegmentationThroughLegacyBridge(t *testing.T) {
	inputs := fixedPeerRenditionInputs(t)
	vocaloid := inputs[1]
	document := *vocaloid.Fixed.Document
	document.Renditions = model.CloneLyricsSourceRenditions(document.Renditions)
	rendition := &document.Renditions[0]
	full := cloneModelFull(rendition.Game)
	if full == nil {
		t.Fatal("Vocaloid fixture has no Game side to promote")
	}
	for lineIndex := range full.Lines {
		for segmentIndex := range full.Lines[lineIndex].Segments {
			for spanIndex := range full.Lines[lineIndex].Segments[segmentIndex].Ruby {
				evidence := full.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence
				if evidence == nil {
					continue
				}
				copy := *evidence
				copy.Side = model.LyricsSourceRenditionSideFull
				full.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = &copy
			}
		}
	}
	gameText := *rendition.Provenance.GameText
	gamePerformer := *rendition.Provenance.GamePerformerSegmentation
	gameRuby := *rendition.Provenance.GameRuby
	rendition.Full = full
	rendition.Game = nil
	rendition.ReasonCode = model.LyricsSourceVersionReasonUntaggedFullOnly
	rendition.SourceTabPaths = []model.LyricsSourceTabPath{{"Full Version", "VIRTUAL SINGER"}}
	rendition.PrivateReview = nil
	rendition.FullPerformerEvidence = model.LyricsSourcePerformerEvidenceSourcePartial
	rendition.GamePerformerEvidence = model.LyricsSourcePerformerEvidenceNone
	rendition.Provenance.FullText = &gameText
	rendition.Provenance.FullPerformerSegmentation = &gamePerformer
	rendition.Provenance.FullRuby = &gameRuby
	rendition.Provenance.GameText = nil
	rendition.Provenance.GamePerformerSegmentation = nil
	rendition.Provenance.GameRuby = nil
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("partial Vocaloid Full fixture: %v", err)
	}
	vocaloid.Fixed.Document = &document
	vocaloid.Fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, document.FixedIdentities...)

	composition, err := ComposeFixedArtifacts([]FixedArtifactInput{vocaloid})
	if err != nil {
		t.Fatal(err)
	}
	if len(composition.Renditions) != 1 {
		t.Fatalf("partial Vocaloid Full composition renditions=%d", len(composition.Renditions))
	}
	rendition = &composition.Renditions[0]
	if rendition.SourceKind != model.LyricsSourceRenditionVocaloid || rendition.Full == nil || rendition.Game != nil ||
		rendition.FullPerformerEvidence != model.LyricsSourcePerformerEvidenceSourcePartial ||
		rendition.Provenance.FullPerformerSegmentation == nil || len(rendition.Full.Performers) != 1 ||
		len(rendition.Full.Lines[0].Segments[0].PerformerIDs) == 0 {
		t.Fatalf("partial Vocaloid Full segmentation was not restored: %+v", rendition)
	}
}

func TestNormalizedFixedSourcePerformerIDsRetainsFixedExternalNenerobo(t *testing.T) {
	inputs := fixedPeerRenditionInputs(t)
	full := cloneModelFull(inputs[0].Fixed.Document.Renditions[0].Full)
	if full == nil || len(full.Performers) != 1 {
		t.Fatalf("unexpected Full fixture=%+v", full)
	}
	const sourceID = "外部歌唱者-04"
	full.Performers[0] = model.LyricsSourcePerformer{PerformerID: sourceID, Name: "Nenerobo"}
	for lineIndex := range full.Lines {
		for segmentIndex := range full.Lines[lineIndex].Segments {
			for performerIndex := range full.Lines[lineIndex].Segments[segmentIndex].PerformerIDs {
				full.Lines[lineIndex].Segments[segmentIndex].PerformerIDs[performerIndex] = sourceID
			}
		}
		for performerIndex := range full.Lines[lineIndex].TrailingPerformerIDs {
			full.Lines[lineIndex].TrailingPerformerIDs[performerIndex] = sourceID
		}
	}
	normalized, err := lyricscontract.NormalizePersistedPerformerMetadata(*full)
	if err != nil || len(normalized.Performers) != 1 || normalized.Performers[0].PerformerID != sourceID ||
		normalized.Performers[0].Name != "Nenerobo" {
		t.Fatalf("fixed external performer normalization=%+v err=%v", normalized.Performers, err)
	}
	got, err := normalizedFixedSourcePerformerIDs(model.LyricsSourceRendition{
		SourcePerformerIDs: []string{sourceID}, Full: full,
	})
	if err != nil || !reflect.DeepEqual(got, []string{sourceID}) {
		t.Fatalf("fixed external source roster=%v err=%v", got, err)
	}
}
