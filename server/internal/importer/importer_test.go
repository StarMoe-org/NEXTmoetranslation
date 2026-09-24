package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func openTestStores(t *testing.T, name string) (*store.Store, *store.EventStore) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return store.New(database), store.NewEventStore(database)
}

// writeExport materializes a legacy translations directory with one entry per
// category and returns its path.
func writeExport(t *testing.T) string {
	t.Helper()
	s, es := openTestStores(t, "export.db")
	for _, category := range model.SupportedCategories {
		if _, err := s.ImportCategory(category, model.Category{"name": {
			category + "-jp": {Text: category + "-zh", Source: model.SourceHuman, Ids: []string{"1"}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := es.ImportOrdered(42, model.EventStoryMeta{
		Source: "official_cn", Version: "1.0", LastUpdated: 1700000000,
	}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "scenario-1", Title: "标题", TitleSource: model.SourceHuman,
		TalkKeys: []string{"一"}, TalkData: map[string]string{"一": "第一句"},
		TalkSources: map[string]string{"一": model.SourceHuman},
	}}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := files.NewGenerator(s, es, root).WriteAll(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "translation")
}

func removeCategoryFiles(t *testing.T, dir, category string, suffixes ...string) {
	t.Helper()
	for _, suffix := range suffixes {
		if err := os.Remove(filepath.Join(dir, category+suffix)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportExportPredatingGachaInfoRestoresItEmpty(t *testing.T) {
	dir := writeExport(t)
	removeCategoryFiles(t, dir, "gachaInfo", ".json", ".full.json")

	payload, result, err := ReadDir(dir)
	if err != nil {
		t.Fatalf("pre-gachaInfo export rejected: %v", err)
	}
	gachaInfo, ok := payload.Categories["gachaInfo"]
	if !ok || gachaInfo == nil || len(gachaInfo) != 0 {
		t.Fatalf("gachaInfo payload = %#v present=%v, want empty category", gachaInfo, ok)
	}
	if result.Categories != len(model.SupportedCategories) || result.Entries != len(model.SupportedCategories)-1 {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "gachaInfo") {
		t.Fatalf("missing gachaInfo warning: %v", result.Warnings)
	}
	if _, _, err := ReadSeedDir(t.Context(), dir); err != nil {
		t.Fatalf("pre-gachaInfo seed rejected: %v", err)
	}

	dest, destEvents := openTestStores(t, "restored.db")
	if _, err := dest.ImportCategory("gachaInfo", model.Category{"description": {
		"stale-jp": {Text: "stale-zh", Source: model.SourceHuman},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportDir(dir, dest, destEvents); err != nil {
		t.Fatalf("import pre-gachaInfo export: %v", err)
	}
	restored, err := dest.CategoryData("gachaInfo")
	if err != nil || len(restored) != 0 {
		t.Fatalf("restored gachaInfo = %#v err=%v, want replaced by empty", restored, err)
	}
	cards, err := dest.CategoryData("cards")
	if err != nil || cards["name"]["cards-jp"].Text != "cards-zh" {
		t.Fatalf("restored cards = %#v err=%v", cards, err)
	}
}

func TestImportAcceptsEmptyGachaInfoProjection(t *testing.T) {
	dir := writeExport(t)
	for _, suffix := range []string{".json", ".full.json"} {
		if err := os.WriteFile(filepath.Join(dir, "gachaInfo"+suffix), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ReadDir(dir); err != nil {
		t.Fatalf("export taken before the first gachaInfo sync rejected: %v", err)
	}
}

func TestImportRejectsHalfPresentGachaInfoAndMissingRequiredCategory(t *testing.T) {
	halfPresent := writeExport(t)
	removeCategoryFiles(t, halfPresent, "gachaInfo", ".full.json")
	if _, _, err := ReadDir(halfPresent); err == nil || !strings.Contains(err.Error(), "missing gachaInfo.full.json") {
		t.Fatalf("half-present gachaInfo error = %v", err)
	}

	missingRequired := writeExport(t)
	removeCategoryFiles(t, missingRequired, "gacha", ".json", ".full.json")
	if _, _, err := ReadDir(missingRequired); err == nil || !strings.Contains(err.Error(), "missing gacha.json") {
		t.Fatalf("missing gacha error = %v", err)
	}

	emptyRequired := writeExport(t)
	for _, suffix := range []string{".json", ".full.json"} {
		if err := os.WriteFile(filepath.Join(emptyRequired, "gacha"+suffix), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ReadDir(emptyRequired); err == nil || !strings.Contains(err.Error(), "restore category gacha is empty") {
		t.Fatalf("empty gacha error = %v", err)
	}
}
