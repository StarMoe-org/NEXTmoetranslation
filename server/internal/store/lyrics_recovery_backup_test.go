package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
)

func TestLyricsRecoveryBackupRejectsNativeSourceV3WithLegacyEditableLyrics(t *testing.T) {
	document := publicV3TestDocument(t, publicV3TestRendition(
		"original", "original-source", model.LyricsSourceRenditionOriginal,
		true, false, false, false, "",
	))
	content, _ := publicV3RecoverySourceFixture(t, 797, document)
	content.Documents = []LyricsDocumentBackupRecord{{MusicID: 797, Revision: 1, UpdatedAt: 1786173174}}
	if err := validateRestoredLyricsSourceProvenance(content, map[int]bool{797: true}); err == nil ||
		!strings.Contains(err.Error(), "mixed source-v3 and legacy editable ownership") {
		t.Fatalf("backup validator accepted mixed native source-v3 storage: %v", err)
	}
}

func TestLyricsRecoveryBackupExportRejectsHistoricalMixedStorage(t *testing.T) {
	s := setupLyricsStore(t)
	if _, err := s.SaveLyrics(validLyrics(), "legacy-editor"); err != nil {
		t.Fatal(err)
	}
	document := publicV3TestDocument(t, publicV3TestRendition(
		"original", "original-source", model.LyricsSourceRenditionOriginal,
		true, false, false, false, "",
	))
	body, digest := publicV3TestDocumentBytes(t, document)
	if _, err := s.db.Exec(`DROP TRIGGER song_lyrics_source_v3_reject_legacy_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,?,?,?,?,?)`, 10, document.SchemaVersion, document.ReasonCode, string(body), digest,
		strings.Repeat("b", 64), int64(1786173174)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportLyricsContent(); err == nil ||
		!strings.Contains(err.Error(), "mixed source-v3 and legacy editable ownership") {
		t.Fatalf("backup export accepted mixed native source-v3 storage: %v", err)
	}
}

func TestLyricsRecoveryContentBackupRoundTripIsByteStableAndReplaceSafe(t *testing.T) {
	content := recoveryContentBackupFixture(t)
	want, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}

	destination := setupLyricsStore(t)
	for attempt := 1; attempt <= 2; attempt++ {
		if err := destination.ImportTranslationContent(nil, EventContentExport{}, content); err != nil {
			t.Fatalf("restore recovery content attempt %d: %v", attempt, err)
		}
	}
	gotContent, err := destination.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(gotContent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("recovery content restore was not byte-stable\nwant=%s\ngot=%s", want, got)
	}

	var counts struct {
		batches, items, evidence, artifacts, links, contributions, availability int
	}
	if err := destination.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM lyrics_recovery_import_batches),
		(SELECT COUNT(*) FROM lyrics_recovery_import_items),
		(SELECT COUNT(*) FROM lyrics_recovery_source_evidence),
		(SELECT COUNT(*) FROM lyrics_recovery_import_artifacts),
		(SELECT COUNT(*) FROM lyrics_recovery_import_artifact_evidence),
		(SELECT COUNT(*) FROM lyrics_recovery_import_component_contributions),
		(SELECT COUNT(*) FROM song_lyrics_availability_documents)`).Scan(
		&counts.batches, &counts.items, &counts.evidence, &counts.artifacts,
		&counts.links, &counts.contributions, &counts.availability); err != nil {
		t.Fatal(err)
	}
	if counts.batches != len(content.RecoveryBatches) || counts.items != len(content.RecoveryItems) ||
		counts.evidence != len(content.RecoverySourceEvidence) || counts.artifacts != len(content.RecoveryArtifacts) ||
		counts.links != len(content.RecoveryArtifactEvidence) ||
		counts.contributions != len(content.RecoveryContributions) ||
		counts.availability != len(content.AvailabilityDocuments) {
		t.Fatalf("restored recovery graph counts=%+v", counts)
	}
}

func TestLyricsRecoveryContentBackupAcceptsAuditedExternalPerformerAndRejectsCatalogCollision(t *testing.T) {
	valid := recoveryContentBackupFixture(t)
	valid.Segments[0].PerformerIDsJSON = "[1001]"
	destination := setupLyricsStore(t)
	if err := destination.ImportTranslationContent(nil, EventContentExport{}, valid); err != nil {
		t.Fatalf("restore audited lyrics-only performer: %v", err)
	}

	collision := cloneLyricsContentExport(t, valid)
	collision.Performers = append(collision.Performers, CatalogPerformerBackupRecord{
		PerformerID: 1001, NameJA: "衝突する外部歌唱者",
	})
	if err := setupLyricsStore(t).ImportTranslationContent(nil, EventContentExport{}, collision); err == nil ||
		!strings.Contains(err.Error(), "collides with a reserved lyrics-only performer") {
		t.Fatalf("reserved lyrics-only performer collision error=%v", err)
	}
}

func TestLyricsRecoveryContentBackupRejectsTamperingAtomically(t *testing.T) {
	valid := recoveryContentBackupFixture(t)
	tests := []struct {
		name string
		want string
		edit func(*LyricsContentExport)
	}{
		{name: "missing batch", want: "graph rows without a batch", edit: func(content *LyricsContentExport) {
			content.RecoveryBatches = nil
			content.SourceDocuments = nil
		}},
		{name: "music ids digest", want: "item coverage is incomplete", edit: func(content *LyricsContentExport) {
			content.RecoveryBatches[0].MusicIDsSHA256 = strings.Repeat("0", 64)
		}},
		{name: "selection digest", want: "evidence selection is invalid", edit: func(content *LyricsContentExport) {
			content.RecoveryBatches[0].SelectionSHA256 = strings.Repeat("0", 64)
		}},
		{name: "missing evidence", want: "has no exact parent", edit: func(content *LyricsContentExport) {
			content.RecoverySourceEvidence = content.RecoverySourceEvidence[1:]
		}},
		{name: "artifact scalar drift", want: "artifact", edit: func(content *LyricsContentExport) {
			content.RecoveryArtifacts[0].PageTitle = "tampered title"
		}},
		{name: "artifact identity drift", want: "artifact", edit: func(content *LyricsContentExport) {
			identity, err := model.DecodeLyricsSourceFixedIdentity([]byte(content.RecoveryArtifacts[0].FixedIdentityJSON))
			if err != nil {
				t.Fatal(err)
			}
			identity.Title = "tampered title"
			body, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(body)
			content.RecoveryArtifacts[0].PageTitle = identity.Title
			content.RecoveryArtifacts[0].FixedIdentityJSON = string(body)
			content.RecoveryArtifacts[0].FixedIdentitySHA256 = hex.EncodeToString(digest[:])
		}},
		{name: "missing contribution", want: "incomplete provenance", edit: func(content *LyricsContentExport) {
			content.RecoveryContributions = content.RecoveryContributions[1:]
		}},
		{name: "availability state", want: "availability document", edit: func(content *LyricsContentExport) {
			content.AvailabilityDocuments[0].State = string(model.LyricsAvailabilityStateFailed)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := cloneLyricsContentExport(t, valid)
			test.edit(&invalid)
			destination := setupLyricsStore(t)
			err := destination.ImportTranslationContent(nil, EventContentExport{}, invalid)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("restore error=%v want substring %q", err, test.want)
			}
			var batches, sourceDocuments int
			if queryErr := destination.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM lyrics_recovery_import_batches),
				(SELECT COUNT(*) FROM song_lyrics_source_documents)`).Scan(&batches, &sourceDocuments); queryErr != nil {
				t.Fatal(queryErr)
			}
			if batches != 0 || sourceDocuments != 0 {
				t.Fatalf("failed recovery restore wrote batches=%d sourceDocuments=%d", batches, sourceDocuments)
			}
		})
	}
}

func TestLyricsRecoveryContentBackupRealDatabaseRoundTrip(t *testing.T) {
	sourcePath := os.Getenv("MOESEKAI_RECOVERY_BACKUP_DB")
	if sourcePath == "" {
		t.Skip("operator-only real recovery backup round trip")
	}
	copiedSourcePath := filepath.Join(t.TempDir(), "source-v24.db")
	copyRecoveryBackupDatabase(t, sourcePath, copiedSourcePath)
	sourceDB, err := db.Open(copiedSourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceStore := New(sourceDB)
	content, err := sourceStore.ExportLyricsContent()
	if err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	if err := sourceDB.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 256<<20 {
		t.Fatalf("real recovery lyrics content exceeds bounded backup limit: %d", len(body))
	}

	destinationPath := filepath.Join(t.TempDir(), "destination-v24.db")
	destinationDB, err := db.Open(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destinationDB.Close()
	destinationStore := New(destinationDB)
	if err := destinationStore.ImportTranslationContent(nil, EventContentExport{}, content); err != nil {
		t.Fatal(err)
	}
	restored, err := destinationStore.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	restoredBody, err := json.MarshalIndent(restored, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, restoredBody) {
		t.Fatalf("real recovery backup round trip changed canonical lyrics content: before=%x after=%x",
			sha256.Sum256(body), sha256.Sum256(restoredBody))
	}
	var integrity string
	var foreignKeyViolations int
	if err := destinationDB.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if err := destinationDB.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" || foreignKeyViolations != 0 {
		t.Fatalf("restored database integrity=%q foreignKeyViolations=%d", integrity, foreignKeyViolations)
	}
	t.Logf("real recovery backup round trip: music=%d items=%d evidence=%d JSON=%d bytes SHA256=%x",
		len(content.Music), len(content.RecoveryItems), len(content.RecoverySourceEvidence), len(body), sha256.Sum256(body))
}

func copyRecoveryBackupDatabase(t *testing.T, sourcePath, destinationPath string) {
	t.Helper()
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		destination.Close()
		t.Fatal(err)
	}
	if err := destination.Sync(); err != nil {
		destination.Close()
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
}

func recoveryContentBackupFixture(t *testing.T) LyricsContentExport {
	t.Helper()
	_, content := setupContentBackupEvidenceGraph(t)
	content.Publications = []LyricsPublicationBackupRecord{}

	batchSHA := recoveryBackupTestSHA("batch")
	rootSHA := recoveryBackupTestSHA("root")
	musicIDs := make([]int, len(content.Music))
	for index, music := range content.Music {
		musicIDs[index] = music.MusicID
	}
	sort.Ints(musicIDs)
	musicIDsSHA, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
	if err != nil {
		t.Fatal(err)
	}

	createdAt := content.SourceDocuments[0].CreatedAt
	content.SourceDocuments[0].ManifestBatchSHA256 = batchSHA
	completeMusicID := content.SourceDocuments[0].MusicID
	items := make([]LyricsRecoveryItemBackupRecord, 0, len(content.Music))
	availability := []LyricsAvailabilityDocumentBackupRecord{}
	for _, music := range content.Music {
		item := LyricsRecoveryItemBackupRecord{
			BatchSHA256: batchSHA, MusicID: music.MusicID, JapaneseTitle: music.TitleJA,
			CatalogFingerprint: music.LyricsCatalogFingerprint, TargetMusicID: music.MusicID,
			AssociationMusicIDsJSON: "[]", ResultSHA256: recoveryBackupTestSHA("result:" + music.TitleJA),
			CreatedAt: createdAt,
		}
		if music.MusicID == completeMusicID {
			item.State = string(lyricscontract.CoverageComplete)
			item.DraftSHA256 = recoveryBackupTestSHA("draft")
			item.DocumentSHA256 = content.SourceDocuments[0].DocumentSHA256
		} else {
			document := model.LyricsAvailabilityDocument{
				SchemaVersion:   model.LyricsAvailabilityDocumentSchemaVersion,
				State:           model.LyricsAvailabilityStateMissing,
				ReasonCode:      model.LyricsSourceVersionReasonVersionConflict,
				FixedIdentities: []model.LyricsSourceFixedIdentity{},
			}
			body, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(body)
			item.State = string(lyricscontract.CoverageMissing)
			item.AvailabilityDocumentSHA256 = hex.EncodeToString(digest[:])
			availability = append(availability, LyricsAvailabilityDocumentBackupRecord{
				AvailabilityDocumentID: int64(len(availability) + 1), BatchSHA256: batchSHA,
				MusicID: music.MusicID, SchemaVersion: document.SchemaVersion, State: string(document.State),
				ReasonCode: string(document.ReasonCode), DocumentJSON: string(body),
				DocumentSHA256: item.AvailabilityDocumentSHA256, ResultSHA256: item.ResultSHA256,
				CreatedAt: createdAt,
			})
		}
		items = append(items, item)
	}
	sort.Slice(items, func(left, right int) bool { return items[left].MusicID < items[right].MusicID })

	recoveryEvidence := make([]LyricsRecoverySourceEvidenceBackupRecord, len(content.SourceIndexEvidence))
	selection := make([]lyricscontract.EvidenceRef, len(content.SourceIndexEvidence))
	var rawByteCount int64
	for index, record := range content.SourceIndexEvidence {
		acquisitionID := recoveryBackupTestSHA("acquisition:" + record.Provider + ":" + record.EvidenceID)
		envelopeSHA := recoveryBackupTestSHA("envelope:" + record.Provider + ":" + record.EvidenceID)
		recoveryEvidence[index] = LyricsRecoverySourceEvidenceBackupRecord{
			Provider: record.Provider, EvidenceID: record.EvidenceID, SHA256: record.SHA256,
			AcquisitionID: acquisitionID, EnvelopeSHA256: envelopeSHA, Kind: record.Kind, Origin: record.Origin,
			PageID: record.PageID, RevisionID: record.RevisionID, RevisionTimestamp: record.RevisionTimestamp,
			MediaWikiSHA1: record.MediaWikiSHA1, PageTitle: record.PageTitle,
			CanonicalRevisionURL: record.CanonicalRevisionURL, CategoriesJSON: record.CategoriesJSON,
			CanonicalRequestURL: record.CanonicalRequestURL, FetchedAt: record.FetchedAt,
			RawBytes: append([]byte(nil), record.RawBytes...), RawByteCount: record.RawByteCount,
			RawSHA256: record.RawSHA256, CreatedAt: createdAt,
		}
		selection[index] = lyricscontract.EvidenceRef{
			Provider: model.LyricsSourceProvider(record.Provider), AcquisitionID: acquisitionID,
			EvidenceID: record.EvidenceID, SHA256: record.SHA256, EnvelopeSHA256: envelopeSHA,
		}
		rawByteCount += int64(record.RawByteCount)
	}
	sort.Slice(recoveryEvidence, func(left, right int) bool {
		if recoveryEvidence[left].Provider != recoveryEvidence[right].Provider {
			return recoveryEvidence[left].Provider < recoveryEvidence[right].Provider
		}
		return recoveryEvidence[left].EvidenceID < recoveryEvidence[right].EvidenceID
	})
	sort.Slice(selection, func(left, right int) bool { return selection[left].EvidenceID < selection[right].EvidenceID })
	selectionSHA, err := lyricscontract.OrderedSelectionSHA256(selection)
	if err != nil {
		t.Fatal(err)
	}

	recoveryArtifacts := make([]LyricsRecoveryArtifactBackupRecord, len(content.SourceArtifacts))
	for index, record := range content.SourceArtifacts {
		recoveryArtifacts[index] = LyricsRecoveryArtifactBackupRecord{
			BatchSHA256: batchSHA, MusicID: completeMusicID, Provider: record.Provider,
			RenditionKey: record.RenditionKey, Origin: record.Origin, PageID: record.PageID,
			RevisionID: record.RevisionID, RevisionTimestamp: record.RevisionTimestamp,
			MediaWikiSHA1: record.MediaWikiSHA1, PageTitle: record.PageTitle,
			CanonicalRevisionURL: record.CanonicalRevisionURL, FetchedAt: record.FetchedAt,
			CategoriesJSON: record.CategoriesJSON, Section: record.Section,
			CompositionRenditionKey: record.CompositionRenditionKey, VersionReason: record.VersionReason,
			IndexEvidenceRefsJSON: record.IndexEvidenceRefsJSON, FixedIdentityJSON: record.FixedIdentityJSON,
			FixedIdentitySHA256: record.FixedIdentitySHA256, RawByteCount: record.RawByteCount,
			RawWikitextSHA256: record.RawWikitextSHA256, ArtifactSHA256: record.ArtifactSHA256,
			CreatedAt: createdAt,
		}
	}
	recoveryLinks := make([]LyricsRecoveryArtifactEvidenceBackupRecord, len(content.SourceArtifactEvidence))
	for index, record := range content.SourceArtifactEvidence {
		recoveryLinks[index] = LyricsRecoveryArtifactEvidenceBackupRecord{
			BatchSHA256: batchSHA, MusicID: completeMusicID, RenditionKey: record.RenditionKey,
			Position: record.Position, Provider: record.Provider, EvidenceID: record.EvidenceID, SHA256: record.SHA256,
		}
	}
	recoveryContributions := make([]LyricsRecoveryContributionBackupRecord, len(content.SourceContributions))
	for index, record := range content.SourceContributions {
		recoveryContributions[index] = LyricsRecoveryContributionBackupRecord{
			BatchSHA256: batchSHA, MusicID: completeMusicID, Component: record.Component,
			RenditionKey: record.RenditionKey, ContributionSHA256: record.ContributionSHA256,
		}
	}

	coverage := lyricscontract.Coverage{
		Total: len(content.Music), Complete: 1, Missing: len(content.Music) - 1,
		ProviderOutcomeRefCount: len(content.Music), SelectionRefCount: len(selection),
		UniqueAcquisitionCount: len(selection), UniqueEvidenceCount: len(selection),
	}
	coverageJSON, err := json.Marshal(coverage)
	if err != nil {
		t.Fatal(err)
	}
	content.RecoveryBatches = []LyricsRecoveryBatchBackupRecord{{
		BatchSHA256: batchSHA, SchemaVersion: 1, RootSchemaVersion: lyricscontract.SchemaVersionV2,
		RootID: "recovery-backup-roundtrip", RootSHA256: rootSHA, CatalogCount: len(content.Music),
		MusicIDsSHA256: musicIDsSHA, CoverageJSON: string(coverageJSON),
		EvidenceReceiptSHA256: recoveryBackupTestSHA("receipt"), PackSHA256: recoveryBackupTestSHA("pack"),
		SelectionSHA256: selectionSHA, EvidenceCount: len(selection), ShardCount: 1,
		RawByteCount: rawByteCount, EncodedByteCount: rawByteCount + int64(len(selection)),
		Actor: "recovery-backup-test", CreatedAt: createdAt,
	}}
	content.RecoveryItems = items
	content.RecoverySourceEvidence = recoveryEvidence
	content.RecoveryArtifacts = recoveryArtifacts
	content.RecoveryArtifactEvidence = recoveryLinks
	content.RecoveryContributions = recoveryContributions
	content.AvailabilityDocuments = availability
	content.SourceArtifacts = []LyricsSourceArtifactBackupRecord{}
	content.SourceIndexEvidence = []LyricsSourceIndexEvidenceBackupRecord{}
	content.SourceArtifactEvidence = []LyricsSourceArtifactEvidenceBackupRecord{}
	content.SourceContributions = []LyricsSourceContributionBackupRecord{}
	return content
}

func recoveryBackupTestSHA(value string) string {
	digest := sha256.Sum256([]byte("lyrics-recovery-backup-test-v1\x00" + value))
	return hex.EncodeToString(digest[:])
}
