package store

import (
	"context"
	"path/filepath"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/model"
)

func restoreContractCategories() map[string]model.Category {
	categories := make(map[string]model.Category, len(model.SupportedCategories))
	for _, category := range model.SupportedCategories {
		categories[category] = model.Category{}
	}
	return categories
}

func embeddedLyricsEditorSeedLedgerCounts(t *testing.T, s *Store) (int, int, int) {
	t.Helper()
	var items, batches, catalog int
	if err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedded_lyrics_editor_seed_items),
		(SELECT COUNT(*) FROM embedded_lyrics_editor_seed_batches),
		(SELECT COUNT(*) FROM catalog_music)`).Scan(&items, &batches, &catalog); err != nil {
		t.Fatal(err)
	}
	return items, batches, catalog
}

func TestRestoreClearsEmbeddedLyricsEditorSeedLedgerInBothBranches(t *testing.T) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name            string
		additivePresent bool
	}{
		{name: "additive", additivePresent: true},
		{name: "legacy", additivePresent: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database, err := db.Open(filepath.Join(t.TempDir(), "embedded-seed-restore.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			s := New(database)
			seedEmbeddedLyricsEditorCatalog(t, database, bundle, false)
			seedEmbeddedLyricsEditorLegacyPerformers(t, database, bundle)
			if _, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle); err != nil {
				t.Fatal(err)
			}

			if err := s.RestoreBackupContext(context.Background(), restoreContractCategories(), nil, nil,
				EventContentExport{}, LyricsContentExport{}, testCase.additivePresent, "operator"); err != nil {
				t.Fatalf("restore with an applied embedded seed ledger: %v", err)
			}
			if items, batches, catalog := embeddedLyricsEditorSeedLedgerCounts(t, s); items != 0 || batches != 0 || catalog != 0 {
				t.Fatalf("after restore seed items=%d batches=%d catalog=%d", items, batches, catalog)
			}

			// The ledger is rebuilt from the embedded bundle once the restored
			// catalog is back, which is what makes clearing it honest.
			seedEmbeddedLyricsEditorCatalog(t, database, bundle, false)
			seedEmbeddedLyricsEditorLegacyPerformers(t, database, bundle)
			result, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
			if err != nil {
				t.Fatalf("replay embedded seed after restore: %v", err)
			}
			if result.Inserted != embeddedlyricsseed.ExpectedCatalogCount || result.Replayed != 0 {
				t.Fatalf("embedded seed replay after restore result=%+v", result)
			}
			if items, batches, _ := embeddedLyricsEditorSeedLedgerCounts(t, s); items != embeddedlyricsseed.ExpectedCatalogCount || batches != 1 {
				t.Fatalf("replayed ledger items=%d batches=%d", items, batches)
			}
		})
	}
}
