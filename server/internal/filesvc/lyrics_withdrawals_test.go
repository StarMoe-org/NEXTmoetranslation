package filesvc

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// publishBundleSongFromDatabase publishes music 307, which the reviewed
// embedded bundle also contains.
func publishBundleSongFromDatabase(t *testing.T, s *store.Store) int {
	t.Helper()
	if err := s.UpsertMusicCatalog([]store.MusicCatalogRecord{
		{MusicID: 307, JapaneseTitle: "DB新曲307", ChineseTitle: "DB新歌307", EnglishTitle: "DB Song 307"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 601, JapaneseName: "歌唱者"}}); err != nil {
		t.Fatal(err)
	}
	imported := model.SongLyrics{
		MusicID: 307, Revision: 0, Attribution: "withdrawal fixture",
		SourceURL: "https://source.invalid/wiki/307", SourcePageID: 307, SourceRevisionID: 1,
		SourceSHA1: "0123456789abcdef0123456789abcdef01234567", SourceFetchedAt: "2026-08-11T00:00:00Z",
		Lines: []model.LyricLine{{
			ID: "withdrawal-line", Order: 0, Japanese: "DB歌词", Chinese: "DB歌词一",
			Segments: []model.LyricSegment{{Text: "DB歌词", PerformerIDs: []int{601}}},
		}},
	}
	saved, changed, err := s.SaveImportedLyricsMutation(imported, "fixture")
	if err != nil || !changed {
		t.Fatalf("save imported lyrics 307: changed=%t err=%v", changed, err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	return saved.Revision
}

func indexContainsSong(t *testing.T, body []byte, musicID int) bool {
	t.Helper()
	var index store.PublicLyricsIndexDocument
	if err := json.Unmarshal(body, &index); err != nil {
		t.Fatalf("decode published index: %v", err)
	}
	for _, song := range index.Songs {
		if song.MusicID == musicID {
			return true
		}
	}
	return false
}

func TestUnpublishRemovesASongTheBundleContains(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "withdrawal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))

	read := func(path string) (int, []byte) {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		svc.Handler().ServeHTTP(response, request)
		return response.Code, response.Body.Bytes()
	}

	revision := publishBundleSongFromDatabase(t, s)
	svc.Rebuild()
	status, index := read("/files/translation/lyrics/index.json")
	if status != http.StatusOK || !indexContainsSong(t, index, 307) {
		t.Fatalf("published index status=%d contains307=%v", status, indexContainsSong(t, index, 307))
	}
	if detailStatus, _ := read("/files/translation/lyrics/music_307.json"); detailStatus != http.StatusOK {
		t.Fatalf("published detail status=%d", detailStatus)
	}

	if _, err := s.UnpublishLyrics(307, revision); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()

	status, index = read("/files/translation/lyrics/index.json")
	if status != http.StatusOK || indexContainsSong(t, index, 307) {
		t.Fatalf("withdrawn index status=%d still contains 307", status)
	}
	if detailStatus, _ := read("/files/translation/lyrics/music_307.json"); detailStatus != http.StatusNotFound {
		t.Fatalf("withdrawn detail status=%d, want 404", detailStatus)
	}
	if detailStatus, _ := read("/files/v2/zh-CN/translation/lyrics/music_307.json"); detailStatus != http.StatusNotFound {
		t.Fatalf("withdrawn locale-mirror detail status=%d, want 404", detailStatus)
	}
	if _, ok := svc.SongProvenance(307); ok {
		t.Fatal("withdrawn song still reports provenance")
	}

	if _, err := s.PublishLyrics(307, revision); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()

	status, index = read("/files/translation/lyrics/index.json")
	if status != http.StatusOK || !indexContainsSong(t, index, 307) {
		t.Fatalf("re-published index status=%d missing 307", status)
	}
	if detailStatus, _ := read("/files/translation/lyrics/music_307.json"); detailStatus != http.StatusOK {
		t.Fatalf("re-published detail status=%d", detailStatus)
	}
}

func TestBundleLoadFailureRebuildsFromDatabaseOnly(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "bundle-load-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.publicLyrics = nil
	svc.publicLyricsErr = errors.New("injected bundle load failure")

	publishBundleSongFromDatabase(t, s)
	if err := svc.rebuildAssetsContext(svc.ctx); err != nil {
		t.Fatalf("rebuild with an unloadable bundle failed: %v", err)
	}

	summary := svc.Status().LyricsSummary
	if !summary.Degraded || summary.DegradedReason == "" {
		t.Fatalf("expected degraded summary, got %+v", summary)
	}
	if summary.DBPublicationSongs != 1 || summary.BundleSongs != 0 || summary.TotalSongs != 1 {
		t.Fatalf("expected database-only counts, got %+v", summary)
	}

	request := httptest.NewRequest(http.MethodGet, "/files/translation/lyrics/music_307.json", nil)
	response := httptest.NewRecorder()
	svc.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("database-published detail status=%d, want 200", response.Code)
	}
}
