package store

import (
	"context"
	"testing"

	"moesekai/server/internal/lyricscontract"
)

// seedRestoreRecoveryGraph installs a recovery import graph without touching the
// catalog or the editable lyrics of the target store.
func seedRestoreRecoveryGraph(t *testing.T, s *Store) {
	t.Helper()
	fixture := recoveryContentBackupFixture(t)
	recovery := LyricsContentExport{
		RecoverySourceEvidence:   fixture.RecoverySourceEvidence,
		RecoveryBatches:          fixture.RecoveryBatches,
		RecoveryItems:            fixture.RecoveryItems,
		RecoveryArtifacts:        fixture.RecoveryArtifacts,
		RecoveryArtifactEvidence: fixture.RecoveryArtifactEvidence,
		RecoveryContributions:    fixture.RecoveryContributions,
		AvailabilityDocuments:    fixture.AvailabilityDocuments,
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := restoreLyricsRecoveryContentTx(context.Background(), tx, recovery); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func restoreGraphRowCounts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{
		"song_lyrics_source_documents", "lyrics_recovery_import_batches", "lyrics_recovery_import_items",
		"lyrics_recovery_import_artifacts", "lyrics_recovery_import_artifact_evidence",
		"lyrics_recovery_source_evidence", "song_lyrics_availability_documents", "catalog_music",
	} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	return counts
}

func TestLegacyRestoreClearsSourceV3DocumentsAndRecoveryGraph(t *testing.T) {
	s := setupLyricsStore(t)
	document, evidenceByIdentity := renditionV3PersistenceDocument(t)
	if err := insertRenditionV3PersistenceGraph(t, s, document, evidenceByIdentity, []lyricscontract.RenditionTranslation{
		{RenditionKey: document.Renditions[0].RenditionKey},
		{RenditionKey: document.Renditions[1].RenditionKey},
	}); err != nil {
		t.Fatal(err)
	}
	seedRestoreRecoveryGraph(t, s)
	before := restoreGraphRowCounts(t, s)
	if before["song_lyrics_source_documents"] == 0 || before["lyrics_recovery_import_batches"] == 0 ||
		before["lyrics_recovery_source_evidence"] == 0 {
		t.Fatalf("fixture did not install a source-v3 document and a recovery graph: %+v", before)
	}

	if err := s.RestoreBackupContext(context.Background(), restoreContractCategories(), nil, nil,
		EventContentExport{}, LyricsContentExport{}, false, "operator"); err != nil {
		t.Fatalf("legacy restore over a source-v3 document: %v", err)
	}
	for table, count := range restoreGraphRowCounts(t, s) {
		if count != 0 {
			t.Fatalf("legacy restore left %d rows in %s", count, table)
		}
	}
	if _, err := s.ExportLyricsContent(); err != nil {
		t.Fatalf("export after legacy restore: %v", err)
	}
}

// localizationProjectionFixtureStore builds a store whose only song is an
// edited source-v3 document at localization revision 2, translated with the
// given prefix.
func localizationProjectionFixtureStore(t *testing.T, prefix string) *Store {
	t.Helper()
	s := setupLyricsStore(t)
	document, evidenceByIdentity := renditionV3PersistenceDocument(t)
	sekai := document.Renditions[0]
	if sekai.Full == nil {
		t.Fatal("fixture sekai rendition has no full side")
	}
	translations := make([]string, len(sekai.Full.Lines))
	for index := range translations {
		translations[index] = prefix + "-" + sekai.Full.Lines[index].ID
	}
	if err := insertRenditionV3PersistenceGraph(t, s, document, evidenceByIdentity, []lyricscontract.RenditionTranslation{
		{RenditionKey: sekai.RenditionKey, Translations: translations, TranslationCredit: "雪莹ちゃん"},
		{RenditionKey: document.Renditions[1].RenditionKey},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE song_lyrics_rendition_localizations SET revision=2`); err != nil {
		t.Fatal(err)
	}
	return s
}

func projectedLocalizationFirstLine(t *testing.T, s *Store) string {
	t.Helper()
	_, details, _, err := s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	detail, ok := details[10]
	if !ok || len(detail.Renditions) == 0 || detail.Renditions[0].Full == nil ||
		len(detail.Renditions[0].Full.Lines) == 0 {
		t.Fatalf("localization 10 not projected: %+v", details)
	}
	return detail.Renditions[0].Full.Lines[0].Chinese
}

func TestRestoreInvalidatesProjectionCacheAtAnUnchangedRevision(t *testing.T) {
	s := localizationProjectionFixtureStore(t, "初译")
	if got := projectedLocalizationFirstLine(t, s); got != "初译-full-000001" {
		t.Fatalf("first projection line = %q", got)
	}

	replacement, err := localizationProjectionFixtureStore(t, "改译").ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreBackupContext(context.Background(), restoreContractCategories(), nil, nil,
		EventContentExport{}, replacement, true, "operator"); err != nil {
		t.Fatal(err)
	}
	if got := projectedLocalizationFirstLine(t, s); got != "改译-full-000001" {
		t.Fatalf("projection after restore = %q, want the restored translation", got)
	}

	imported, err := localizationProjectionFixtureStore(t, "三译").ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportTranslationContentContext(context.Background(), nil, EventContentExport{}, imported); err != nil {
		t.Fatal(err)
	}
	if got := projectedLocalizationFirstLine(t, s); got != "三译-full-000001" {
		t.Fatalf("projection after content import = %q, want the imported translation", got)
	}
}
