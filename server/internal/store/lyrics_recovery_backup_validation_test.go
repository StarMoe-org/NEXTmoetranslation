package store

import (
	"strings"
	"testing"
)

func TestRecoveryBackupCoverageSurvivesCatalogGrowth(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	if err := fixture.store.UpsertMusicCatalog([]MusicCatalogRecord{
		{MusicID: 30, JapaneseTitle: "新着曲", ChineseTitle: "新到曲", EnglishTitle: "Newly Added"},
	}); err != nil {
		t.Fatal(err)
	}

	export, err := fixture.store.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export after the upstream catalog grew: %v", err)
	}
	if len(export.RecoveryBatches) != 1 || export.RecoveryBatches[0].CatalogCount != 2 || len(export.Music) != 3 {
		t.Fatalf("export batches=%d catalogCount=%d music=%d", len(export.RecoveryBatches),
			export.RecoveryBatches[0].CatalogCount, len(export.Music))
	}
	restored := setupLyricsStore(t)
	if err := restored.UpsertMusicCatalog([]MusicCatalogRecord{
		{MusicID: 30, JapaneseTitle: "新着曲", ChineseTitle: "新到曲", EnglishTitle: "Newly Added"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, export); err != nil {
		t.Fatalf("restore backup taken after catalog growth: %v", err)
	}

	documentIDs := make(map[int]bool, len(export.Documents))
	for _, document := range export.Documents {
		documentIDs[document.MusicID] = true
	}
	musicIDs := make(map[int]bool, len(export.Music))
	for _, music := range export.Music {
		musicIDs[music.MusicID] = true
	}
	inconsistent := export
	inconsistent.RecoveryBatches = append([]LyricsRecoveryBatchBackupRecord(nil), export.RecoveryBatches...)
	inconsistent.RecoveryBatches[0].CatalogCount = 3
	err = validateRestoredLyricsRecoveryProvenance(inconsistent, documentIDs, musicIDs)
	if err == nil || !strings.Contains(err.Error(), "coverage is invalid") {
		t.Fatalf("batch disagreeing with its own coverage error=%v", err)
	}
}
