package store

import (
	"encoding/json"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

func catalogMusicItemsForTest(t *testing.T, s *Store) map[int]model.CatalogMusicItem {
	t.Helper()
	response, err := s.CatalogMusic("", false, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	items := make(map[int]model.CatalogMusicItem, len(response.Items))
	for _, item := range response.Items {
		items[item.MusicID] = item
	}
	return items
}

func requireCatalogWithdrawn(t *testing.T, s *Store, want map[int]bool) {
	t.Helper()
	items := catalogMusicItemsForTest(t, s)
	for musicID, withdrawn := range want {
		item, ok := items[musicID]
		if !ok {
			t.Fatalf("catalog lacks music %d", musicID)
		}
		body, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if item.LyricsWithdrawn != withdrawn || strings.Contains(string(body), `"lyricsWithdrawn":true`) != withdrawn ||
			!withdrawn && strings.Contains(string(body), `"lyricsWithdrawn"`) {
			t.Fatalf("music %d withdrawn=%t want %t: %s", musicID, item.LyricsWithdrawn, withdrawn, body)
		}
	}
}

func TestCatalogMusicReportsLegacyLyricsWithdrawals(t *testing.T) {
	s := setupLyricsStore(t)
	if err := s.UpsertMusicCatalog([]MusicCatalogRecord{{MusicID: 30, JapaneseTitle: "未公開曲"}}); err != nil {
		t.Fatal(err)
	}
	revisions := map[int]int{}
	for _, musicID := range []int{10, 20, 30} {
		input := validLyrics()
		input.MusicID = musicID
		saved, _, err := s.SaveLyricsMutation(input, "editor")
		if err != nil {
			t.Fatal(err)
		}
		revisions[musicID] = saved.Revision
	}
	for _, musicID := range []int{10, 20} {
		if _, err := s.PublishLyrics(musicID, revisions[musicID]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UnpublishLyrics(musicID, revisions[musicID]); err != nil {
			t.Fatal(err)
		}
	}
	requireCatalogWithdrawn(t, s, map[int]bool{10: true, 20: true, 30: false})
	if _, err := s.PublishLyrics(20, revisions[20]); err != nil {
		t.Fatal(err)
	}
	requireCatalogWithdrawn(t, s, map[int]bool{10: true, 20: false, 30: false})
	if status := catalogMusicItemsForTest(t, s)[20].LyricsStatus; status != "published" {
		t.Fatalf("republished status=%q", status)
	}
	if item := catalogMusicItemsForTest(t, s)[20]; item.LyricsSourceV3 {
		t.Fatalf("legacy song reported as source-v3: %+v", item)
	}
}

func TestCatalogMusicReportsSourceV3LyricsWithdrawals(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	result, _ := publishLyricsDocumentForTest(t, s, lyricsDocumentTestRequest(), 0)
	requireCatalogWithdrawn(t, s, map[int]bool{lyricsDocumentTestMusicID: false})
	if item := catalogMusicItemsForTest(t, s)[lyricsDocumentTestMusicID]; !item.LyricsSourceV3 {
		t.Fatalf("source-v3 song not reported as source-v3: %+v", item)
	}
	if _, changed, err := s.SetSourceV3LyricsWithdrawn(lyricsDocumentTestMusicID, result.Revision, true, "admin"); err != nil || !changed {
		t.Fatalf("withdraw changed=%t err=%v", changed, err)
	}
	requireCatalogWithdrawn(t, s, map[int]bool{lyricsDocumentTestMusicID: true})
	if _, changed, err := s.SetSourceV3LyricsWithdrawn(lyricsDocumentTestMusicID, result.Revision, false, "admin"); err != nil || !changed {
		t.Fatalf("republish changed=%t err=%v", changed, err)
	}
	requireCatalogWithdrawn(t, s, map[int]bool{lyricsDocumentTestMusicID: false})
}
