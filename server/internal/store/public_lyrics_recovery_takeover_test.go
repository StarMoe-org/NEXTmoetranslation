package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"moesekai/server/internal/model"
)

type recoveryPublicCandidatesForTest struct {
	v3 RecoveryPublicLyricsV3Candidate
	v2 RecoveryPublicLyricsCandidate
	v4 RecoveryPublicLyricsV4Candidate
}

func recoveryPublicCandidatesOf(t *testing.T, s *Store, batchSHA256 string) recoveryPublicCandidatesForTest {
	t.Helper()
	var candidates recoveryPublicCandidatesForTest
	var err error
	if candidates.v3, err = s.RecoveryPublicLyricsV3(batchSHA256); err != nil {
		t.Fatalf("recovery Public v3 candidate: %v", err)
	}
	if candidates.v2, err = s.RecoveryPublicLyricsV2Compatibility(batchSHA256); err != nil {
		t.Fatalf("recovery Public v2 compatibility candidate: %v", err)
	}
	if candidates.v4, err = s.RecoveryPublicLyricsV4(batchSHA256); err != nil {
		t.Fatalf("recovery Public v4 candidate: %v", err)
	}
	return candidates
}

func TestRecoveryPublicCandidatesOfATakenOverBatchKeepTheLedgerContent(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	s := fixture.store
	before := recoveryPublicCandidatesOf(t, s, fixture.batchSHA)
	if _, owns := before.v3.Details[10]; !owns {
		t.Fatalf("fixture v3 candidate has no detail for the complete item: %+v", before.v3.Index)
	}
	for _, musicID := range []int{10, 20} {
		takeOverRecoverySongForTest(t, s, musicID)
	}
	if takeovers := recoveryTakeoverRowsForTest(t, s); len(takeovers) != 2 || !takeovers[10].superseded {
		t.Fatalf("takeovers=%+v", takeovers)
	}
	after := recoveryPublicCandidatesOf(t, s, fixture.batchSHA)
	if !reflect.DeepEqual(after.v3, before.v3) {
		t.Fatalf("v3 candidate changed by the takeover\nbefore=%+v\nafter=%+v", before.v3, after.v3)
	}
	if !reflect.DeepEqual(after.v2, before.v2) {
		t.Fatalf("v2 compatibility candidate changed by the takeover\nbefore=%+v\nafter=%+v", before.v2, after.v2)
	}
	if !reflect.DeepEqual(after.v4, before.v4) {
		t.Fatalf("v4 candidate changed by the takeover\nbefore=%+v\nafter=%+v", before.v4, after.v4)
	}

	restored := restoreSeededContentBackup(t, mustExportLyricsContent(t, s))
	if again := recoveryPublicCandidatesOf(t, restored, fixture.batchSHA); !reflect.DeepEqual(again, before) {
		t.Fatal("candidates of the restored taken-over backup differ from the pre-takeover candidates")
	}
}

func TestRecoveryPublicCandidatesOfATakenOverTranslatedItemKeepTheLedgerTranslations(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	s := fixture.store
	seedRecoveryLedgerTranslations(t, fixture, false)
	before := recoveryPublicCandidatesOf(t, s, fixture.batchSHA)
	translated := 0
	for _, rendition := range before.v3.Details[10].Renditions {
		if rendition.TranslationCredits != nil && rendition.Full != nil && rendition.Full.Lines[0].Chinese != "" {
			translated++
		}
	}
	if translated != len(before.v3.Details[10].Renditions) || translated == 0 {
		t.Fatalf("fixture candidate is not translated as seeded: %+v", before.v3.Details[10])
	}
	for _, musicID := range []int{10, 20} {
		takeOverRecoverySongForTest(t, s, musicID)
	}
	after := recoveryPublicCandidatesOf(t, s, fixture.batchSHA)
	if !reflect.DeepEqual(after.v3, before.v3) {
		t.Fatalf("v3 candidate changed by the takeover\nbefore=%+v\nafter=%+v", before.v3.Details[10], after.v3.Details[10])
	}
	if !reflect.DeepEqual(after.v2, before.v2) {
		t.Fatalf("v2 compatibility candidate changed by the takeover\nbefore=%+v\nafter=%+v", before.v2, after.v2)
	}
	if !reflect.DeepEqual(after.v4, before.v4) {
		t.Fatalf("v4 candidate changed by the takeover\nbefore=%+v\nafter=%+v", before.v4.Details[10], after.v4.Details[10])
	}
	restored := restoreSeededContentBackup(t, mustExportLyricsContent(t, s))
	if again := recoveryPublicCandidatesOf(t, restored, fixture.batchSHA); !reflect.DeepEqual(again, before) {
		t.Fatal("candidates of the restored taken-over backup differ from the pre-takeover candidates")
	}
}

func TestRecoveryPublicCandidatesOfATakenOverItemKeepTheLedgerTranslationEditions(t *testing.T) {
	ledger, body := ledgerLocalizationsWithEditionForTest(t)
	before := recoveryPublicCandidatesOf(t, ledger.store, ledger.batchSHA)
	if editions := len(before.v4.Details[10].TranslationEditions); editions != 2 {
		t.Fatalf("fixture v4 candidate editions=%d", editions)
	}
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	seedRecoveryLedgerTranslations(t, fixture, false)
	for _, musicID := range []int{10, 20} {
		takeOverRecoverySongForTest(t, fixture.store, musicID)
	}
	content := mustExportLyricsContent(t, fixture.store)
	content.RecoveryTakeovers[0].SupersededDocument.LocalizationsJSON = body
	restored := restoreSeededContentBackup(t, content)
	if after := recoveryPublicCandidatesOf(t, restored, fixture.batchSHA); !reflect.DeepEqual(after, before) {
		t.Fatalf("candidates of the taken-over item differ from the pre-takeover candidates\nbefore=%+v\nafter=%+v",
			before.v4.Details[10], after.v4.Details[10])
	}
}

func mustExportLyricsContent(t *testing.T, s *Store) LyricsContentExport {
	t.Helper()
	content, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestRecoveryPublicCandidatesOfATakenOverLegacyV2ItemCarryOnlyTheLedgerText(t *testing.T) {
	const batchSHA = "abababababababababababababababababababababababababababababababab"
	const createdAt = 1786173174
	rendition := publicV3TestRendition(
		"original", "original-source", model.LyricsSourceRenditionOriginal,
		true, false, false, true, "",
	)
	v3Rendition := publicV3TestDocument(t, rendition).Renditions[0]
	legacyDocument := model.LyricsSourceDocument{
		SchemaVersion:   model.LyricsSourceDocumentSchemaVersionV2,
		ReasonCode:      v3Rendition.ReasonCode,
		FixedIdentities: append([]model.LyricsSourceFixedIdentity(nil), publicV3TestDocument(t, rendition).FixedIdentities...),
		Full:            *model.CloneLyricsSourceFull(v3Rendition.Full),
		Provenance: model.LyricsSourceComponentProvenance{
			FullText:        *v3Rendition.Provenance.FullText,
			Ruby:            v3Rendition.Provenance.FullRuby,
			VersionEvidence: v3Rendition.Provenance.VersionEvidence,
		},
	}
	for lineIndex := range legacyDocument.Full.Lines {
		for segmentIndex := range legacyDocument.Full.Lines[lineIndex].Segments {
			for spanIndex := range legacyDocument.Full.Lines[lineIndex].Segments[segmentIndex].Ruby {
				legacyDocument.Full.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = nil
			}
		}
	}
	documentBody, documentSHA := publicV3TestDocumentBytes(t, legacyDocument)
	identityBody, err := json.Marshal(legacyDocument.FixedIdentities[0])
	if err != nil {
		t.Fatal(err)
	}
	contributions := []LyricsRecoveryContributionBackupRecord{}
	for component, identityKey := range publicLyricsSourceComponentRefs(legacyDocument) {
		contributions = append(contributions, LyricsRecoveryContributionBackupRecord{
			BatchSHA256: batchSHA, MusicID: 795, Component: component, RenditionKey: identityKey,
		})
	}
	// The takeover deleted the superseded document and its editable rows; the
	// replacing editor document and its localization belong to no batch.
	content := LyricsContentExport{
		Music: []CatalogMusicBackupRecord{{MusicID: 795, TitleJA: "Legacy v2"}},
		SourceDocuments: []LyricsSourceDocumentBackupRecord{{
			DocumentID: 42, MusicID: 795, SchemaVersion: model.LyricsSourceDocumentSchemaVersionV3,
			DocumentJSON: "{}", DocumentSHA256: documentSHA[:63] + "0", CreatedAt: createdAt + 10,
		}},
		RenditionLocalizations: []LyricsRenditionLocalizationBackupRecord{{
			DocumentID: 42, RenditionKey: "original", Locale: "zh-CN", TranslationCredit: "编辑器译者",
			Revision: 3, UpdatedAt: createdAt + 10,
		}},
		RecoveryBatches: []LyricsRecoveryBatchBackupRecord{{BatchSHA256: batchSHA, RootSHA256: documentSHA, CatalogCount: 1}},
		RecoveryItems: []LyricsRecoveryItemBackupRecord{{
			BatchSHA256: batchSHA, MusicID: 795, State: string(PublicLyricsStateComplete), DocumentSHA256: documentSHA,
		}},
		RecoveryArtifacts: []LyricsRecoveryArtifactBackupRecord{{
			BatchSHA256: batchSHA, MusicID: 795, RenditionKey: legacyDocument.FixedIdentities[0].RenditionKey,
			FixedIdentityJSON: string(identityBody),
		}},
		RecoveryContributions: contributions,
		RecoveryTakeovers: []LyricsRecoveryTakeoverBackupRecord{{
			MusicID: 795, BatchSHA256: batchSHA, ItemState: string(PublicLyricsStateComplete),
			SupersededDocument: &LyricsRecoveryTakeoverDocumentBackupRecord{
				SchemaVersion: model.LyricsSourceDocumentSchemaVersionV2, ReasonCode: string(legacyDocument.ReasonCode),
				DocumentJSON: string(documentBody), DocumentSHA256: documentSHA, CreatedAt: createdAt,
			},
			TakenOverAt: createdAt + 10, TakenOverBy: "document-admin",
		}},
	}
	candidate, err := buildRecoveryPublicLyricsV3Candidate(content, batchSHA)
	if err != nil {
		t.Fatal(err)
	}
	detail := candidate.Details[795]
	if detail.Revision != 1 || detail.UpdatedAt != formatTimestamp(createdAt) || len(detail.Renditions) != 1 ||
		detail.Renditions[0].Full == nil || detail.Renditions[0].TranslationCredits != nil {
		t.Fatalf("taken-over legacy v2 detail=%+v", detail)
	}
	line := detail.Renditions[0].Full.Lines[0]
	if line.Japanese != legacyDocument.Full.Lines[0].Text || line.Chinese != "" || line.English != "" {
		t.Fatalf("taken-over legacy v2 line=%+v", line)
	}
	if _, err := buildRecoveryPublicLyricsV2CompatibilityCandidate(content, candidate); err != nil {
		t.Fatalf("v2 compatibility candidate: %v", err)
	}
	if _, err := buildRecoveryPublicLyricsV4Candidate(content, batchSHA); err != nil {
		t.Fatalf("v4 candidate: %v", err)
	}
}
