package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

func TestPublicLyricsV3StrictDecodersRejectNestedTampering(t *testing.T) {
	root := "../../../contracts/public-lyrics/v3"
	validDetail := readPublicLyricsV3Contract(t, root+"/detail-legacy-one-rendition.fixture.json")
	if _, err := DecodePublicLyricsV3Detail(validDetail); err != nil {
		t.Fatal(err)
	}
	validIndex := readPublicLyricsV3Contract(t, root+"/index.fixture.json")
	if _, err := DecodePublicLyricsV3Index(validIndex); err != nil {
		t.Fatal(err)
	}

	mutateDetail := func(mutate func(map[string]any)) []byte {
		t.Helper()
		var document map[string]any
		if err := json.Unmarshal(validDetail, &document); err != nil {
			t.Fatal(err)
		}
		mutate(document)
		body, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	firstRendition := func(document map[string]any) map[string]any {
		return document["renditions"].([]any)[0].(map[string]any)
	}
	firstSegment := func(document map[string]any) map[string]any {
		full := firstRendition(document)["full"].(map[string]any)
		line := full["lines"].([]any)[0].(map[string]any)
		return line["segments"].([]any)[0].(map[string]any)
	}

	tampered := map[string][]byte{
		"unknown nested field": mutateDetail(func(document map[string]any) {
			firstSegment(document)["private"] = true
		}),
		"Han without reading": mutateDetail(func(document map[string]any) {
			ruby := firstSegment(document)["ruby"].([]any)
			delete(ruby[0].(map[string]any), "reading")
		}),
		"reading on kana": mutateDetail(func(document map[string]any) {
			ruby := firstSegment(document)["ruby"].([]any)
			ruby[1].(map[string]any)["reading"] = "う"
		}),
		"reading without kana": mutateDetail(func(document map[string]any) {
			ruby := firstSegment(document)["ruby"].([]any)
			ruby[0].(map[string]any)["reading"] = "ー"
		}),
		"mixed Han and kana base": mutateDetail(func(document map[string]any) {
			segment := firstSegment(document)
			ruby := segment["ruby"].([]any)
			ruby[0].(map[string]any)["text"] = "歌う"
			segment["ruby"] = ruby[:1]
		}),
		"wrong revision host": mutateDetail(func(document map[string]any) {
			provenance := firstRendition(document)["provenance"].([]any)
			provenance[0].(map[string]any)["revisionUrl"] = "https://example.invalid/wiki/Test?oldid=1201"
		}),
		"wrong provider license": mutateDetail(func(document map[string]any) {
			provenance := firstRendition(document)["provenance"].([]any)
			provenance[0].(map[string]any)["licenseName"] = "CC0"
		}),
		"missing Full text provenance": mutateDetail(func(document map[string]any) {
			rendition := firstRendition(document)
			provenance := rendition["provenance"].([]any)
			rendition["provenance"] = provenance[1:]
		}),
		"missing relation provenance": mutateDetail(func(document map[string]any) {
			rendition := firstRendition(document)
			provenance := rendition["provenance"].([]any)
			rendition["provenance"] = append(provenance[:1], provenance[2:]...)
		}),
		"mutable Moegirl URL": mutateDetail(func(document map[string]any) {
			provenance := firstRendition(document)["provenance"].([]any)
			attribution := provenance[0].(map[string]any)
			attribution["provider"] = "moegirl"
			attribution["revisionUrl"] = "https://moegirl.icu/wiki/Public_Test"
			attribution["licenseName"] = "CC BY-NC-SA 3.0"
			attribution["licenseUrl"] = "https://creativecommons.org/licenses/by-nc-sa/3.0/"
		}),
		"null optional object": mutateDetail(func(document map[string]any) {
			firstRendition(document)["translationCredits"] = nil
		}),
		"trailing JSON":        append(append([]byte(nil), validDetail...), []byte(` {}`)...),
		"duplicate root field": bytes.Replace(validDetail, []byte(`"version": 3`), []byte(`"version": 3, "version": 3`), 1),
	}
	for name, body := range tampered {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePublicLyricsV3Detail(body); err == nil {
				t.Fatal("tampered detail was accepted")
			}
		})
	}

	projectionFixture := readPublicLyricsV3Contract(t, root+"/detail-full-game-projection.fixture.json")
	mutateProjection := func(mutate func(map[string]any)) []byte {
		t.Helper()
		var document map[string]any
		if err := json.Unmarshal(projectionFixture, &document); err != nil {
			t.Fatal(err)
		}
		mutate(document)
		body, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for name, body := range map[string][]byte{
		"projection translation drift": mutateProjection(func(document map[string]any) {
			rendition := document["renditions"].([]any)[0].(map[string]any)
			fullLine := rendition["full"].(map[string]any)["lines"].([]any)[0].(map[string]any)
			gameLine := rendition["game"].(map[string]any)["lines"].([]any)[0].(map[string]any)
			fullLine["zh-CN"] = "一致"
			gameLine["zh-CN"] = "漂移"
		}),
		"projection relation order drift": mutateProjection(func(document map[string]any) {
			rendition := document["renditions"].([]any)[0].(map[string]any)
			rendition["relation"].(map[string]any)["lineIds"] = []any{"full-000003", "full-000001"}
			gameLines := rendition["game"].(map[string]any)["lines"].([]any)
			gameLines[0].(map[string]any)["japanese"] = "進もう"
			gameLines[1].(map[string]any)["japanese"] = "歌う"
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePublicLyricsV3Detail(body); err == nil {
				t.Fatal("tampered exact projection was accepted")
			}
		})
	}

	var emptyIndex PublicLyricsV3IndexDocument
	emptyIndex.Version = 3
	emptyIndex.Songs = []PublicLyricsIndexSong{}
	if _, err := EncodePublicLyricsV3Index(emptyIndex); err == nil {
		t.Fatal("empty Public v3 index was accepted")
	}
	var index map[string]any
	if err := json.Unmarshal(validIndex, &index); err != nil {
		t.Fatal(err)
	}
	index["songs"].([]any)[0].(map[string]any)["private"] = true
	unknownIndex, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePublicLyricsV3Index(unknownIndex); err == nil {
		t.Fatal("unknown index field was accepted")
	}
	if err := json.Unmarshal(validIndex, &index); err != nil {
		t.Fatal(err)
	}
	index["songs"].([]any)[0].(map[string]any)["title"].(map[string]any)["zh-CN"] = nil
	nullIndex, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePublicLyricsV3Index(nullIndex); err == nil {
		t.Fatal("null optional index field was accepted")
	}
}

func TestPublicLyricsV3TreatsIdeographicZeroAsPlainNumericText(t *testing.T) {
	root := "../../../contracts/public-lyrics/v3"
	var document map[string]any
	if err := json.Unmarshal(readPublicLyricsV3Contract(t, root+"/detail-legacy-one-rendition.fixture.json"), &document); err != nil {
		t.Fatal(err)
	}
	rendition := document["renditions"].([]any)[0].(map[string]any)
	line := rendition["full"].(map[string]any)["lines"].([]any)[0].(map[string]any)
	segment := line["segments"].([]any)[0].(map[string]any)
	line["japanese"] = "〇"
	segment["text"] = "〇"
	segment["ruby"] = []any{map[string]any{"text": "〇"}}

	plain, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePublicLyricsV3Detail(plain); err != nil {
		t.Fatalf("plain ideographic zero was rejected: %v", err)
	}

	segment["ruby"].([]any)[0].(map[string]any)["reading"] = "れい"
	annotated, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePublicLyricsV3Detail(annotated); err == nil {
		t.Fatal("numeric ideographic zero received a ruby reading")
	}
}

func publicV3RecoverySourceFixture(t *testing.T, musicID int, document model.LyricsSourceDocument) (LyricsContentExport, string) {
	t.Helper()
	const batchSHA = "f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0"
	const rootSHA = "e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"
	body, digest := publicV3TestDocumentBytes(t, document)
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		t.Fatal(err)
	}
	contributions := make([]LyricsRecoveryContributionBackupRecord, len(bindings))
	for index, binding := range bindings {
		contributions[index] = LyricsRecoveryContributionBackupRecord{
			BatchSHA256: batchSHA, MusicID: musicID,
			Component: binding.ComponentKey, RenditionKey: binding.FixedIdentityKey,
		}
	}
	artifacts := make([]LyricsRecoveryArtifactBackupRecord, len(document.FixedIdentities))
	for index, identity := range document.FixedIdentities {
		identityBody, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		artifacts[index] = LyricsRecoveryArtifactBackupRecord{
			BatchSHA256: batchSHA, MusicID: musicID, RenditionKey: identity.RenditionKey,
			FixedIdentityJSON: string(identityBody),
		}
	}
	return LyricsContentExport{
		Music: []CatalogMusicBackupRecord{{MusicID: musicID, TitleJA: "Localized metadata fixture"}},
		SourceDocuments: []LyricsSourceDocumentBackupRecord{{
			DocumentID: 41, MusicID: musicID, SchemaVersion: model.LyricsSourceDocumentSchemaVersionV3,
			DocumentJSON: string(body), DocumentSHA256: digest, ManifestBatchSHA256: batchSHA,
			CreatedAt: 1785456000,
		}},
		RecoveryBatches: []LyricsRecoveryBatchBackupRecord{{
			BatchSHA256: batchSHA, RootSHA256: rootSHA, CatalogCount: 1,
		}},
		RecoveryItems: []LyricsRecoveryItemBackupRecord{{
			BatchSHA256: batchSHA, MusicID: musicID, State: string(PublicLyricsStateComplete), DocumentSHA256: digest,
		}},
		RecoveryArtifacts: artifacts, RecoveryContributions: contributions,
	}, batchSHA
}


func publicV3TestRendition(
	key, identityKey string,
	kind model.LyricsSourceRenditionKind,
	hasFull, hasGame, exactProjection, legacyReading bool,
	performerID string,
) model.LyricsSourceRendition {
	label := map[model.LyricsSourceRenditionKind]string{
		model.LyricsSourceRenditionOriginal:  "Original Version",
		model.LyricsSourceRenditionSekai:     "SEKAI Version",
		model.LyricsSourceRenditionVocaloid:  "VIRTUAL SINGER",
		model.LyricsSourceRenditionAlternate: "Alternate Vocal Test",
	}[kind]
	ref := model.LyricsSourceComponentRef{RenditionKey: identityKey}
	rendition := model.LyricsSourceRendition{
		RenditionKey:          key,
		SourceKind:            kind,
		ReasonCode:            model.LyricsSourceVersionReasonUntaggedFullOnly,
		SourceTabPaths:        []model.LyricsSourceTabPath{{label}},
		FullPerformerEvidence: model.LyricsSourcePerformerEvidenceNone,
		GamePerformerEvidence: model.LyricsSourcePerformerEvidenceNone,
		Relation:              model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
		Provenance: model.LyricsSourceRenditionProvenance{
			RelationEvidence: ref,
			VersionEvidence:  ref,
		},
	}
	if kind == model.LyricsSourceRenditionAlternate {
		rendition.SourceTabPaths = []model.LyricsSourceTabPath{{"Alternate Vocal", label}}
	}
	if hasFull && hasGame {
		rendition.SourceTabPaths = []model.LyricsSourceTabPath{{"Full Version", label}, {"Game Version", label}}
	} else if hasGame {
		rendition.SourceTabPaths = []model.LyricsSourceTabPath{{"Game Version", label}}
	}
	if hasFull {
		rendition.Full = publicV3TestSide(key, identityKey, kind, label, model.LyricsSourceRenditionSideFull, legacyReading, performerID)
		rendition.Provenance.FullText = publicV3TestRef(identityKey)
	}
	if hasGame {
		rendition.Game = publicV3TestSide(key, identityKey, kind, label, model.LyricsSourceRenditionSideGame, legacyReading, performerID)
		rendition.Provenance.GameText = publicV3TestRef(identityKey)
	}
	if legacyReading {
		if hasFull {
			rendition.Provenance.FullRuby = publicV3TestRef(identityKey)
		}
		if hasGame {
			rendition.Provenance.GameRuby = publicV3TestRef(identityKey)
		}
	}
	if performerID != "" {
		rendition.SourcePerformerIDs = []string{performerID}
		rendition.PrivateReview = &model.LyricsSourcePrivateReview{
			PerformerSegmentationEvidence: model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured,
		}
		if hasFull {
			rendition.FullPerformerEvidence = model.LyricsSourcePerformerEvidenceSourceComplete
			rendition.Provenance.FullPerformerSegmentation = publicV3TestRef(identityKey)
		}
		if hasGame {
			rendition.GamePerformerEvidence = model.LyricsSourcePerformerEvidenceSourceComplete
			rendition.Provenance.GamePerformerSegmentation = publicV3TestRef(identityKey)
		}
	}
	switch {
	case hasFull && hasGame && exactProjection:
		rendition.ReasonCode = model.LyricsSourceVersionReasonTaggedFullAndGame
		rendition.Relation = model.LyricsSourceRenditionRelation{
			Kind:             model.LyricsSourceRenditionRelationExactProjection,
			FullRenditionKey: key,
			LineIDs:          []string{"full-000001"},
		}
	case !hasFull && hasGame:
		rendition.ReasonCode = model.LyricsSourceVersionReasonTaggedGameOnly
	case hasFull:
		rendition.ReasonCode = model.LyricsSourceVersionReasonUntaggedFullOnly
	}
	return rendition
}

func publicV3TestSide(
	renditionKey, identityKey string,
	kind model.LyricsSourceRenditionKind,
	label string,
	side model.LyricsSourceRenditionSide,
	legacyReading bool,
	performerID string,
) *model.LyricsSourceFull {
	evidence := &model.LyricsSourceReadingEvidence{
		Kind:             model.LyricsSourceReadingEvidenceDeterministicDictionary,
		GeneratorVersion: "public-v3-test",
	}
	if legacyReading {
		evidence = &model.LyricsSourceReadingEvidence{
			Kind:                 model.LyricsSourceReadingEvidenceLegacyV2Component,
			FixedIdentityKey:     identityKey,
			RenditionKey:         renditionKey,
			Side:                 side,
			SourceRowOrdinal:     1,
			SourceSegmentOrdinal: 1,
		}
	}
	performers := []model.LyricsSourcePerformer{}
	performerIDs := []string{}
	if performerID != "" {
		performers = append(performers, model.LyricsSourcePerformer{
			PerformerID: performerID,
			Name:        "Test Performer",
			Color:       "#39C5BB",
		})
		performerIDs = append(performerIDs, performerID)
	}
	return &model.LyricsSourceFull{
		Version:              model.LyricsSourceVersion{Kind: string(kind), Label: label},
		Performers:           performers,
		RubyGeneratorVersion: "public-v3-test",
		Lines: []model.LyricsSourceFullLine{{
			ID:   string(side) + "-000001",
			Text: "歌う",
			Segments: []model.LyricsSourceSegment{{
				Text:         "歌う",
				PerformerIDs: performerIDs,
				Ruby: []model.LyricsSourceRubySpan{
					{Text: "歌", Reading: "うた", ReadingEvidence: evidence},
					{Text: "う"},
				},
			}},
			TrailingPerformerIDs: []string{},
		}},
	}
}

func publicV3TestRef(identityKey string) *model.LyricsSourceComponentRef {
	return &model.LyricsSourceComponentRef{RenditionKey: identityKey}
}

func publicV3TestDocument(t *testing.T, renditions ...model.LyricsSourceRendition) model.LyricsSourceDocument {
	t.Helper()
	identities := make([]model.LyricsSourceFixedIdentity, 0, len(renditions))
	seen := map[string]bool{}
	for index, rendition := range renditions {
		bindings, err := model.EnumerateLyricsSourceRenditionComponents([]model.LyricsSourceRendition{rendition})
		if err != nil {
			t.Fatalf("rendition fixture %q: %v", rendition.RenditionKey, err)
		}
		for _, binding := range bindings {
			if seen[binding.FixedIdentityKey] {
				continue
			}
			seen[binding.FixedIdentityKey] = true
			identities = append(identities, publicV3TestIdentity(binding.FixedIdentityKey, 1200+index))
		}
	}
	document := model.LyricsSourceDocument{
		SchemaVersion:   model.LyricsSourceDocumentSchemaVersionV3,
		FixedIdentities: identities,
		Renditions:      model.CloneLyricsSourceRenditions(renditions),
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("source v3 document fixture: %v", err)
	}
	return document
}

func publicV3TestIdentity(key string, revisionID int) model.LyricsSourceFixedIdentity {
	return model.LyricsSourceFixedIdentity{
		Provider:          model.LyricsSourceProviderSekaipedia,
		Origin:            model.LyricsSourceOriginSekaipedia,
		PageID:            revisionID + 100,
		RevisionID:        revisionID,
		SHA1:              strings.Repeat("a", 40),
		Title:             "Public v3 test " + key,
		CanonicalURL:      "https://www.sekaipedia.org/wiki/Public_v3_test_" + key + "?oldid=" + jsonNumber(revisionID),
		RevisionTimestamp: "2026-07-31T00:00:00Z",
		FetchedAt:         "2026-07-31T00:01:00Z",
		Categories:        []string{"Test"},
		Section:           "Lyrics",
		RenditionKey:      key,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "test-" + key,
			SHA256:     strings.Repeat("b", 64),
		}},
	}
}


func publicV3TestDocumentBytes(t *testing.T, document model.LyricsSourceDocument) ([]byte, string) {
	t.Helper()
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return body, hex.EncodeToString(digest[:])
}

func jsonNumber(value int) string {
	body, _ := json.Marshal(value)
	return string(body)
}

func mustExportLyricsContent(t *testing.T, s *Store) LyricsContentExport {
	t.Helper()
	content, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	return content
}
