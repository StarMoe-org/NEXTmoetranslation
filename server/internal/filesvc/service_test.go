package filesvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func setupLegacyFileService(t *testing.T) *Service {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "legacy-filesvc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := store.New(database)
	es := store.NewEventStore(database)
	if _, err := s.ImportCategory("cards", model.Category{
		"prefix": {
			"こんにちは":     {Text: "你好", Source: model.SourceCN, Ids: []string{"1", "2"}},
			"A & B < C": {Text: "甲 & 乙 < 丙", Source: model.SourceHuman},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := es.ImportOrdered(42, model.EventStoryMeta{
		Source: "official_cn", Version: "1.0", LastUpdated: 1700000000,
	}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "scenario-1", Title: "标题 & <", TitleSource: model.SourceHuman,
		TalkKeys: []string{"zebra", "apple", "mango & lime"},
		TalkData: map[string]string{"zebra": "斑马", "apple": "苹果", "mango & lime": "芒果 & 青柠 <"},
	}}); err != nil {
		t.Fatal(err)
	}
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.Rebuild()
	initialProjection := svc.Status()
	if initialProjection.Generation != 1 || initialProjection.Pending || initialProjection.LastError != "" {
		t.Fatalf("initial projection status = %+v", initialProjection)
	}
	return svc
}

func TestInitialProjectionRetriesAfterFailOnce(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "projection-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.retryMin = 5 * time.Millisecond
	svc.retryMax = 10 * time.Millisecond
	rebuild := svc.rebuildAssetsFn
	var calls atomic.Int32
	svc.rebuildAssetsFn = func() error {
		if calls.Add(1) == 1 {
			return errors.New("internal projection detail")
		}
		return rebuild()
	}
	svc.Start()
	defer func() {
		svc.Stop()
		svc.Wait()
	}()
	deadline := time.Now().Add(time.Second)
	for svc.Status().Generation == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if status := svc.Status(); status.Generation != 1 || status.Pending || status.LastError != "" || calls.Load() != 2 {
		t.Fatalf("recovered status=%+v calls=%d", status, calls.Load())
	}
}

func TestStartReturnsWhileInitialProjectionIsBlockedAndStopWaits(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "projection-blocked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	entered := make(chan struct{})
	release := make(chan struct{})
	svc.rebuildAssetsFn = func() error {
		close(entered)
		<-release
		return nil
	}

	startReturned := make(chan struct{})
	go func() {
		svc.Start()
		close(startReturned)
	}()
	select {
	case <-startReturned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Start blocked on initial projection")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial projection worker did not start")
	}
	if status := svc.Status(); !status.Pending || status.Generation != 0 {
		t.Fatalf("blocked initial status = %+v", status)
	}

	svc.Stop()
	waitReturned := make(chan struct{})
	go func() {
		svc.Wait()
		close(waitReturned)
	}()
	select {
	case <-waitReturned:
		t.Fatal("Wait returned while the tracked projection was still running")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-waitReturned:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after initial projection completed")
	}
}

func TestProjectionRetriesFailOnceAfterSuccessUntilConverged(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "projection-post-start-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.debounce = time.Millisecond
	svc.retryMin = 5 * time.Millisecond
	svc.retryMax = 10 * time.Millisecond
	rebuild := svc.rebuildAssetsFn
	var calls atomic.Int32
	svc.rebuildAssetsFn = func() error {
		call := calls.Add(1)
		if call == 2 {
			return errors.New("post-start failure")
		}
		return rebuild()
	}
	svc.Start()
	defer func() {
		svc.Stop()
		svc.Wait()
	}()
	waitForProjection(t, svc, func(status ProjectionStatus) bool { return status.Generation == 1 && !status.Pending })
	svc.Trigger()
	waitForProjection(t, svc, func(status ProjectionStatus) bool { return status.Generation == 2 && !status.Pending })
	if calls.Load() != 3 || svc.Status().LastError != "" {
		t.Fatalf("post-start convergence status=%+v calls=%d", svc.Status(), calls.Load())
	}
}

func TestPersistentPostStartProjectionFailureStopsRetries(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "projection-stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.retryMin = 5 * time.Millisecond
	svc.retryMax = 10 * time.Millisecond
	svc.debounce = time.Millisecond
	rebuild := svc.rebuildAssetsFn
	var calls atomic.Int32
	svc.rebuildAssetsFn = func() error {
		if calls.Add(1) == 1 {
			return rebuild()
		}
		return errors.New("sensitive sqlite failure")
	}
	svc.Start()
	waitForProjection(t, svc, func(status ProjectionStatus) bool { return status.Generation == 1 && !status.Pending })
	svc.Trigger()
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < 3 {
		t.Fatalf("projection did not retry: calls=%d", calls.Load())
	}
	svc.Stop()
	svc.Stop()
	svc.Wait()
	stoppedCalls := calls.Load()
	time.Sleep(4 * svc.retryMax)
	if calls.Load() != stoppedCalls {
		t.Fatalf("projection ran after stop: before=%d after=%d", stoppedCalls, calls.Load())
	}
	status := svc.Status()
	if status.Generation != 1 || !status.Pending || status.LastError != "projection_generation_failed" || status.LastError == "sensitive sqlite failure" {
		t.Fatalf("persistent failure status = %+v", status)
	}
}

func waitForProjection(t *testing.T, svc *Service, ready func(ProjectionStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if status := svc.Status(); ready(status) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("projection did not reach expected state: %+v", svc.Status())
}

func TestLegacyPublicFileHTTPContract(t *testing.T) {
	svc := setupLegacyFileService(t)
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	tests := []struct {
		path    string
		fixture string
	}{
		{"/files/translation/cards.json", "cards.json"},
		{"/files/translation/cards.full.json", "cards.full.json"},
		{"/files/translation/eventStory/event_42.json", "event_42.json"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tt.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("..", "files", "testdata", "legacy", tt.fixture))
			if err != nil {
				t.Fatal(err)
			}
			want = bytes.TrimSuffix(want, []byte("\n"))
			if !bytes.Equal(got, want) {
				t.Fatalf("body mismatch\ngot:\n%s\nwant:\n%s", got, want)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := resp.Header.Get("Cache-Control"); got != "public, max-age=300, stale-while-revalidate=3600" {
				t.Fatalf("Cache-Control = %q", got)
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
				t.Fatalf("CORS = %q", got)
			}
			etag := resp.Header.Get("ETag")
			if len(etag) != 34 || etag[0] != '"' || etag[len(etag)-1] != '"' {
				t.Fatalf("strong ETag = %q", etag)
			}

			req, _ := http.NewRequest(http.MethodGet, ts.URL+tt.path, nil)
			req.Header.Set("If-None-Match", etag)
			notModified, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			notModified.Body.Close()
			if notModified.StatusCode != http.StatusNotModified {
				t.Fatalf("conditional status = %d", notModified.StatusCode)
			}

			headReq, _ := http.NewRequest(http.MethodHead, ts.URL+tt.path, nil)
			headResp, err := http.DefaultClient.Do(headReq)
			if err != nil {
				t.Fatal(err)
			}
			headResp.Body.Close()
			if headResp.StatusCode != http.StatusOK {
				t.Fatalf("HEAD status = %d", headResp.StatusCode)
			}
		})
	}

	t.Run("public lyrics cache headers", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/files/translation/lyrics/index.json")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "public, max-age=15, must-revalidate" {
			t.Fatalf("lyrics index Cache-Control = %q, want public, max-age=15, must-revalidate", got)
		}
	})

	localized, err := http.Get(ts.URL + "/files/v2/zh-CN/translation/eventStory/event_42.json")
	if err != nil {
		t.Fatal(err)
	}
	defer localized.Body.Close()
	localizedBody, err := io.ReadAll(localized.Body)
	if err != nil || localized.StatusCode != http.StatusOK {
		t.Fatalf("localized event status=%d err=%v", localized.StatusCode, err)
	}
	if bytes.Contains(localizedBody, []byte(`"revision"`)) {
		t.Fatalf("authenticated event revision leaked into public bytes: %s", localizedBody)
	}
}

// A cross-origin caller must be able to read the status: the site's lyrics
// loader treats a readable 404 as "not published" and anything unreadable as a
// retryable failure.
func TestPublicFileErrorResponsesKeepCORS(t *testing.T) {
	svc := setupLegacyFileService(t)
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	missing, err := http.Get(ts.URL + "/files/translation/lyrics/music_999999.json")
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset status = %d", missing.StatusCode)
	}
	if got := missing.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("404 CORS = %q", got)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/files/translation/cards.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rejected.Body.Close()
	if rejected.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rejected.StatusCode)
	}
	if got := rejected.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("405 CORS = %q", got)
	}
}

func TestSetAssetsDefensivelyCopiesBodies(t *testing.T) {
	svc := &Service{assets: map[string]asset{}}
	body := []byte(`{"version":1}`)
	svc.SetAsset("data/search-index.json", body, "application/json; charset=utf-8")
	body[2] = 'X'

	req := httptest.NewRequest(http.MethodGet, "/files/data/search-index.json", nil)
	resp := httptest.NewRecorder()
	svc.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != `{"version":1}` {
		t.Fatalf("SetAsset retained a mutable caller buffer: status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestEmbeddedPublicLyricsOverlaySurvivesDatabaseRebuild(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "embedded-public-lyrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.Rebuild()

	read := func(path string) (int, []byte) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp := httptest.NewRecorder()
		svc.Handler().ServeHTTP(resp, req)
		return resp.Code, resp.Body.Bytes()
	}
	status, index := read("/files/translation/lyrics/index.json")
	if status != http.StatusOK {
		t.Fatalf("embedded lyrics index status=%d body=%s", status, index)
	}
	var document struct {
		Version int           `json:"version"`
		Songs   []interface{} `json:"songs"`
	}
	if err := json.Unmarshal(index, &document); err != nil {
		t.Fatal(err)
	}
	if document.Version != 3 || len(document.Songs) == 0 {
		t.Fatalf("embedded lyrics index version=%d songs=%d", document.Version, len(document.Songs))
	}
	if status, _ := read("/files/translation/lyrics/music_307.json"); status != http.StatusOK {
		t.Fatalf("embedded music_307 status=%d", status)
	}
	if status, _ := read("/files/translation/lyrics/music_682.json"); status != http.StatusNotFound {
		t.Fatalf("embedded incomplete music_682 status=%d", status)
	}
	for _, locale := range model.SupportedLocales {
		root := "/files/v2/" + locale + "/translation/lyrics/"
		if status, localizedIndex := read(root + "index.json"); status != http.StatusOK || !bytes.Equal(localizedIndex, index) {
			t.Fatalf("embedded locale index %s status=%d differs=%v", locale, status, !bytes.Equal(localizedIndex, index))
		}
		if status, _ := read(root + "music_307.json"); status != http.StatusOK {
			t.Fatalf("embedded locale music_307 %s status=%d", locale, status)
		}
		if status, _ := read(root + "music_682.json"); status != http.StatusNotFound {
			t.Fatalf("embedded locale incomplete music_682 %s status=%d", locale, status)
		}
	}

	const malformedMusicID = 99001
	const malformedPerformerID = 99002
	if err := s.UpsertMusicCatalog([]store.MusicCatalogRecord{{MusicID: malformedMusicID, JapaneseTitle: "壊れたDB投影"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: malformedPerformerID, JapaneseName: "試験歌唱者"}}); err != nil {
		t.Fatal(err)
	}
	saved, changed, err := s.SaveImportedLyricsMutation(model.SongLyrics{
		MusicID: malformedMusicID, Attribution: "Synthetic private database publication",
		SourceURL: "https://source.invalid/wiki/99001", SourcePageID: 99001, SourceRevisionID: 1,
		SourceSHA1: "0123456789abcdef0123456789abcdef01234567", SourceFetchedAt: "2026-08-11T00:00:00Z",
		Lines: []model.LyricLine{{
			ID: "malformed-db-line", Order: 0, Japanese: "壊れたDB投影",
			Segments: []model.LyricSegment{{Text: "壊れたDB投影", PerformerIDs: []int{malformedPerformerID}}},
		}},
	}, "fixture")
	if err != nil || !changed {
		t.Fatalf("save malformed DB fixture changed=%t err=%v", changed, err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE song_lyrics_publications SET payload_json='{' WHERE music_id=?`, malformedMusicID); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()
	status, malformedDBIndex := read("/files/translation/lyrics/index.json")
	if status != http.StatusOK || !bytes.Equal(malformedDBIndex, index) || svc.Status().LastError != "" {
		t.Fatalf("malformed DB publication blocked immutable bundle: status=%d projection=%+v", status, svc.Status())
	}

	svc.SetAsset("translation/lyrics/index.json", []byte(`{"version":1,"songs":[]}`), "application/json; charset=utf-8")
	svc.SetAsset("v2/zh-CN/translation/lyrics/index.json", []byte(`{"version":1,"songs":[]}`), "application/json; charset=utf-8")
	for _, path := range []string{
		"/files/translation/lyrics/index.json",
		"/files/v2/zh-CN/translation/lyrics/index.json",
	} {
		status, protectedIndex := read(path)
		if status != http.StatusOK || !bytes.Equal(protectedIndex, index) {
			t.Fatalf("external asset setter replaced immutable public lyrics %s: status=%d", path, status)
		}
	}
	svc.Rebuild()
	status, rebuiltIndex := read("/files/translation/lyrics/index.json")
	if status != http.StatusOK || !bytes.Equal(rebuiltIndex, index) {
		t.Fatalf("database rebuild did not retain immutable public lyrics overlay: status=%d", status)
	}
}

func TestDatabasePublicationsOverlayEmbeddedBundle(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "embedded-overlay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.Rebuild()

	read := func(path string) (int, []byte) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp := httptest.NewRecorder()
		svc.Handler().ServeHTTP(resp, req)
		return resp.Code, resp.Body.Bytes()
	}

	status, bundleIndexBytes := read("/files/translation/lyrics/index.json")
	if status != http.StatusOK {
		t.Fatalf("embedded lyrics index status=%d", status)
	}
	var bundleDocument store.PublicLyricsIndexDocument
	if err := json.Unmarshal(bundleIndexBytes, &bundleDocument); err != nil {
		t.Fatal(err)
	}
	bundleEntries := make(map[int]store.PublicLyricsIndexSong, len(bundleDocument.Songs))
	for _, song := range bundleDocument.Songs {
		bundleEntries[song.MusicID] = song
	}
	bundle307Status, bundle307Bytes := read("/files/translation/lyrics/music_307.json")
	if bundle307Status != http.StatusOK {
		t.Fatalf("bundle music_307 status=%d", bundle307Status)
	}
	bundleOneStatus, bundleOneBytes := read("/files/translation/lyrics/music_1.json")
	if bundleOneStatus != http.StatusOK {
		t.Fatalf("bundle music_1 status=%d", bundleOneStatus)
	}

	if err := s.UpsertMusicCatalog([]store.MusicCatalogRecord{
		{MusicID: 307, JapaneseTitle: "データベース新曲甲", ChineseTitle: "数据库新歌甲", EnglishTitle: "Database Song Alpha"},
		{MusicID: 682, JapaneseTitle: "データベース新曲乙", ChineseTitle: "数据库新歌乙", EnglishTitle: "Database Song Beta"},
		{MusicID: 99003, JapaneseTitle: "バンドル外新曲", ChineseTitle: "捆绑外新歌", EnglishTitle: "Off-Bundle Song"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 601, JapaneseName: "試験歌唱者"}}); err != nil {
		t.Fatal(err)
	}

	save := func(musicID, revision int, translation string) model.SongLyrics {
		t.Helper()
		input := model.SongLyrics{
			MusicID: musicID, Revision: revision, Attribution: "Synthetic overlay publication",
			SourceURL: fmt.Sprintf("https://source.invalid/wiki/%d", musicID), SourcePageID: musicID, SourceRevisionID: 1,
			SourceSHA1: "0123456789abcdef0123456789abcdef01234567", SourceFetchedAt: "2026-08-11T00:00:00Z",
			Lines: []model.LyricLine{{
				ID: "overlay-line", Order: 0, Japanese: "新しき歌", Chinese: translation,
				Segments: []model.LyricSegment{{Text: "新しき歌", PerformerIDs: []int{601}}},
			}},
		}
		var saved model.SongLyrics
		var changed bool
		var err error
		if revision == 0 {
			saved, changed, err = s.SaveImportedLyricsMutation(input, "fixture")
		} else {
			saved, changed, err = s.SaveLyricsMutation(input, "fixture")
		}
		if err != nil || !changed {
			t.Fatalf("save lyrics musicId=%d revision=%d changed=%t err=%v", musicID, revision, changed, err)
		}
		return saved
	}

	// A database publication at the bundle revision does not own the entry, so
	// the reviewed bundle base is preserved byte-for-byte.
	saved307 := save(307, 0, "数据库甲版一")
	if _, err := s.PublishLyrics(saved307.MusicID, saved307.Revision); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()
	if status, mergedIndex := read("/files/translation/lyrics/index.json"); status != http.StatusOK || !bytes.Equal(mergedIndex, bundleIndexBytes) {
		t.Fatalf("equal-revision database publication changed the reviewed bundle index: status=%d", status)
	}
	if status, body := read("/files/translation/lyrics/music_307.json"); status != http.StatusOK || !bytes.Equal(body, bundle307Bytes) {
		t.Fatalf("equal-revision database publication changed the reviewed bundle detail: status=%d", status)
	}

	// A newer complete database publication replaces the bundle entry and its
	// detail, mirrored to every locale.
	newer307 := save(307, 1, "数据库甲版二")
	if _, err := s.PublishLyrics(newer307.MusicID, newer307.Revision); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()
	status, mergedIndex := read("/files/translation/lyrics/index.json")
	if status != http.StatusOK {
		t.Fatalf("merged lyrics index status=%d", status)
	}
	if !bytes.HasSuffix(mergedIndex, []byte("\n")) {
		t.Fatalf("merged lyrics index lost its trailing newline")
	}
	var mergedDocument store.PublicLyricsIndexDocument
	if err := json.Unmarshal(mergedIndex, &mergedDocument); err != nil {
		t.Fatal(err)
	}
	if mergedDocument.Version != bundleDocument.Version || len(mergedDocument.Songs) != len(bundleDocument.Songs) {
		t.Fatalf("merged index version=%d songs=%d, bundle version=%d songs=%d",
			mergedDocument.Version, len(mergedDocument.Songs), bundleDocument.Version, len(bundleDocument.Songs))
	}
	mergedEntries := make(map[int]store.PublicLyricsIndexSong, len(mergedDocument.Songs))
	for _, song := range mergedDocument.Songs {
		mergedEntries[song.MusicID] = song
	}
	replaced := mergedEntries[307]
	if replaced.Revision != 2 || replaced.State != store.PublicLyricsStateComplete ||
		replaced.Title.Japanese != "データベース新曲甲" ||
		replaced.Title.Chinese != "数据库新歌甲" || replaced.Title.English != "Database Song Alpha" {
		t.Fatalf("merged entry for 307 = %+v", replaced)
	}
	if !reflect.DeepEqual(replaced.AvailableVersions, []string{"full"}) {
		t.Fatalf("merged entry for 307 availableVersions = %+v", replaced.AvailableVersions)
	}
	if _, err := store.DecodePublicLyricsV3Index(mergedIndex); err != nil {
		t.Fatalf("merged index must satisfy the strict v3 decoder: %v", err)
	}
	for position := 1; position < len(mergedDocument.Songs); position++ {
		if mergedDocument.Songs[position-1].MusicID >= mergedDocument.Songs[position].MusicID {
			t.Fatalf("merged index music ids must be strictly increasing at %d: %d >= %d",
				position, mergedDocument.Songs[position-1].MusicID, mergedDocument.Songs[position].MusicID)
		}
	}
	if !reflect.DeepEqual(mergedEntries[1], bundleEntries[1]) {
		t.Fatalf("bundle base entry for music 1 changed: %+v", mergedEntries[1])
	}
	status, merged307 := read("/files/translation/lyrics/music_307.json")
	if status != http.StatusOK || bytes.Equal(merged307, bundle307Bytes) {
		t.Fatalf("merged music_307 status=%d equalsBundle=%v", status, bytes.Equal(merged307, bundle307Bytes))
	}
	var mergedDetail struct {
		MusicID  int `json:"musicId"`
		Version  int `json:"version"`
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(merged307, &mergedDetail); err != nil {
		t.Fatal(err)
	}
	if mergedDetail.MusicID != 307 || mergedDetail.Version != 1 || mergedDetail.Revision != 2 {
		t.Fatalf("merged music_307 detail=%+v", mergedDetail)
	}
	for _, locale := range model.SupportedLocales {
		status, localized := read("/files/v2/" + locale + "/translation/lyrics/music_307.json")
		if status != http.StatusOK || !bytes.Equal(localized, merged307) {
			t.Fatalf("localized merged music_307 %s status=%d", locale, status)
		}
	}
	if status, body := read("/files/translation/lyrics/music_1.json"); status != http.StatusOK || !bytes.Equal(body, bundleOneBytes) {
		t.Fatalf("unrelated bundle detail music_1 changed: status=%d", status)
	}

	// A newer publication for a bundle-incomplete song replaces its index entry
	// and adds the detail the bundle never had.
	save(682, 0, "数据库乙版一")
	newer682 := save(682, 1, "数据库乙版二")
	if _, err := s.PublishLyrics(newer682.MusicID, newer682.Revision); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()
	status, _ = read("/files/translation/lyrics/music_682.json")
	if status != http.StatusOK {
		t.Fatalf("merged music_682 status=%d", status)
	}
	status, mergedIndex = read("/files/translation/lyrics/index.json")
	if status != http.StatusOK {
		t.Fatalf("merged 682 lyrics index status=%d", status)
	}
	var merged682Document store.PublicLyricsIndexDocument
	if err := json.Unmarshal(mergedIndex, &merged682Document); err != nil {
		t.Fatal(err)
	}
	for _, song := range merged682Document.Songs {
		if song.MusicID == 682 {
			if song.Revision != 2 || song.State != store.PublicLyricsStateComplete || song.Title.Japanese != "データベース新曲乙" {
				t.Fatalf("merged entry for 682 = %+v", song)
			}
			if !reflect.DeepEqual(song.AvailableVersions, []string{"full"}) {
				t.Fatalf("merged entry for 682 availableVersions = %+v", song.AvailableVersions)
			}
			break
		}
	}

	// A database song absent from the bundle is appended to the merged index
	// and its detail is served.
	savedNew := save(99003, 0, "捆绑外版本")
	if _, err := s.PublishLyrics(savedNew.MusicID, savedNew.Revision); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()
	status, mergedIndex = read("/files/translation/lyrics/index.json")
	if status != http.StatusOK {
		t.Fatalf("appended lyrics index status=%d", status)
	}
	var appendedDocument store.PublicLyricsIndexDocument
	if err := json.Unmarshal(mergedIndex, &appendedDocument); err != nil {
		t.Fatal(err)
	}
	if len(appendedDocument.Songs) != len(bundleDocument.Songs)+1 {
		t.Fatalf("merged index songs=%d, want %d", len(appendedDocument.Songs), len(bundleDocument.Songs)+1)
	}
	if _, err := store.DecodePublicLyricsV3Index(mergedIndex); err != nil {
		t.Fatalf("appended merged index must satisfy the strict v3 decoder: %v", err)
	}
	added := appendedDocument.Songs[len(appendedDocument.Songs)-1]
	if added.MusicID != 99003 || added.Revision != 1 || added.Title.Japanese != "バンドル外新曲" ||
		added.State != store.PublicLyricsStateComplete {
		t.Fatalf("appended index entry = %+v", added)
	}
	if !reflect.DeepEqual(added.AvailableVersions, []string{"full"}) {
		t.Fatalf("appended index entry availableVersions = %+v", added.AvailableVersions)
	}
	status, newDetail := read("/files/translation/lyrics/music_99003.json")
	if status != http.StatusOK || !bytes.Contains(newDetail, []byte(`"musicId": 99003`)) {
		t.Fatalf("appended music_99003 status=%d body=%s", status, newDetail)
	}
}

func TestDatabasePublicationOwnsEntry(t *testing.T) {
	complete := store.PublicLyricsIndexSong{MusicID: 1, Revision: 1, State: store.PublicLyricsStateComplete}
	gameOnly := store.PublicLyricsIndexSong{MusicID: 1, Revision: 1, State: store.PublicLyricsStateGameOnly}
	incomplete := store.PublicLyricsIndexSong{MusicID: 1, Revision: 1, State: store.PublicLyricsStateIncomplete}
	song := func(revision int, state store.PublicLyricsAvailabilityState) store.PublicLyricsIndexSong {
		return store.PublicLyricsIndexSong{MusicID: 1, Revision: revision, State: state}
	}
	tests := []struct {
		name      string
		database  store.PublicLyricsIndexSong
		bundle    store.PublicLyricsIndexSong
		inBundle  bool
		wantOwned bool
	}{
		{"absent from bundle is owned", incomplete, complete, false, true},
		{"newer complete replaces complete", song(2, store.PublicLyricsStateComplete), complete, true, true},
		{"equal complete keeps bundle entry", song(1, store.PublicLyricsStateComplete), complete, true, false},
		{"older complete keeps bundle entry", song(0, store.PublicLyricsStateComplete), complete, true, false},
		{"newer game_only replaces incomplete bundle entry", song(2, store.PublicLyricsStateGameOnly), incomplete, true, true},
		{"newer legacy v1 publication replaces bundle entry", song(2, ""), complete, true, true},
		{"newer draft never overrides reviewed complete", song(9, store.PublicLyricsStateIncomplete), complete, true, false},
		{"newer draft never overrides reviewed game_only", song(9, store.PublicLyricsStateIncomplete), gameOnly, true, false},
		{"newer failed never overrides reviewed complete", song(9, store.PublicLyricsStateFailed), complete, true, false},
		{"newer missing never overrides reviewed complete", song(9, store.PublicLyricsStateMissing), complete, true, false},
		{"newer ambiguous never overrides reviewed complete", song(9, store.PublicLyricsStateAmbiguous), complete, true, false},
		{"newer draft replaces non-complete bundle entry", song(9, store.PublicLyricsStateIncomplete), incomplete, true, true},
		{"equal draft keeps bundle entry", song(1, store.PublicLyricsStateIncomplete), incomplete, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := databasePublicationOwnsEntry(tt.database, tt.bundle, tt.inBundle); got != tt.wantOwned {
				t.Fatalf("databasePublicationOwnsEntry(%+v, %+v, %t) = %t, want %t",
					tt.database, tt.bundle, tt.inBundle, got, tt.wantOwned)
			}
		})
	}
}

func TestPublishedLyricsFilesAndAtomicRebuild(t *testing.T) {
	publicLocales := []string{"ja-JP", "zh-CN", "en-US"}
	firstMusicID := 41001
	laterMusicID := firstMusicID + 17
	performerID := 601

	database, err := db.Open(filepath.Join(t.TempDir(), "lyrics-files.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	catalog := []store.MusicCatalogRecord{
		{MusicID: firstMusicID, JapaneseTitle: "試験曲甲", ChineseTitle: "测试歌曲甲", EnglishTitle: "Test Song Alpha", IsNewlyWrittenMusic: true},
		{MusicID: laterMusicID, JapaneseTitle: "試験曲乙", ChineseTitle: "测试歌曲乙", EnglishTitle: "Test Song Beta"},
	}
	if err := s.UpsertMusicCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: performerID, JapaneseName: "試験歌唱者"}}); err != nil {
		t.Fatal(err)
	}
	inputs := []model.SongLyrics{
		{
			MusicID: firstMusicID, Revision: 0, Attribution: "Synthetic translation team alpha",
			SourceNote: "private alpha note", SourceURL: fmt.Sprintf("https://source.invalid/wiki/%d", firstMusicID), LicenseNote: "private alpha license",
			SourcePageID: 123, SourceRevisionID: 456, SourceSHA1: "0123456789abcdef0123456789abcdef01234567",
			SourceFetchedAt: "2026-07-23T00:00:00Z",
			Lines: []model.LyricLine{{
				ID: "source-alpha-1", Order: 0, Japanese: "甲を歌う", Chinese: "歌唱甲", English: "Sings alpha",
				Segments: []model.LyricSegment{{Text: "甲を歌う", PerformerIDs: []int{performerID}}},
			}},
		},
		{
			MusicID: laterMusicID, Revision: 0, Attribution: "Synthetic translation team beta",
			SourceNote: "private beta note", SourceURL: fmt.Sprintf("https://source.invalid/wiki/%d", laterMusicID), LicenseNote: "private beta license",
			SourcePageID: 789, SourceRevisionID: 987, SourceSHA1: "89abcdef0123456789abcdef0123456789abcdef",
			SourceFetchedAt: "2026-07-24T00:00:00Z",
			Lines: []model.LyricLine{{
				ID: "source-beta-1", Order: 0, Japanese: "乙を歌う", Chinese: "歌唱乙", English: "Sings beta",
				Segments: []model.LyricSegment{{Text: "乙を歌う", PerformerIDs: []int{performerID}}},
			}},
		},
	}
	if len(inputs) < 2 || inputs[0].MusicID >= inputs[1].MusicID {
		t.Fatal("failed rebuild fixture requires at least two publications in query order")
	}
	savedLyrics := make([]model.SongLyrics, 0, len(inputs))
	for _, input := range inputs {
		saved, changed, err := s.SaveImportedLyricsMutation(input, "editor")
		if err != nil || !changed {
			t.Fatalf("save lyrics musicId=%d changed=%t err=%v", input.MusicID, changed, err)
		}
		if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
			t.Fatalf("publish lyrics musicId=%d: %v", saved.MusicID, err)
		}
		savedLyrics = append(savedLyrics, saved)
	}

	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.publicLyrics = nil
	svc.Rebuild()

	readAsset := func(path string) ([]byte, string, int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		svc.Handler().ServeHTTP(rec, req)
		return rec.Body.Bytes(), rec.Header().Get("ETag"), rec.Code
	}
	type assetSnapshot struct {
		body []byte
		etag string
	}
	snapshotAllAssets := func() map[string]assetSnapshot {
		svc.mu.RLock()
		defer svc.mu.RUnlock()
		snapshot := make(map[string]assetSnapshot, len(svc.assets))
		for key, projected := range svc.assets {
			snapshot[key] = assetSnapshot{body: append([]byte(nil), projected.body...), etag: projected.etag}
		}
		return snapshot
	}
	canonicalRoot := "/files/translation/lyrics"
	localizedRoot := func(locale string) string { return "/files/v2/" + locale + "/translation/lyrics" }
	indexPath := func(root string) string { return root + "/index.json" }
	detailPath := func(root string, musicID int) string { return fmt.Sprintf("%s/music_%d.json", root, musicID) }
	canonicalIndexPath := indexPath(canonicalRoot)

	index, indexETag, status := readAsset(canonicalIndexPath)
	var indexDocument model.PublicLyricsIndex
	if status != http.StatusOK {
		t.Fatalf("lyrics index status=%d body=%s", status, index)
	}
	if err := json.Unmarshal(index, &indexDocument); err != nil {
		t.Fatalf("lyrics index JSON: %v\nbody=%s", err, index)
	}
	if indexDocument.Version != 1 || len(indexDocument.Songs) != len(savedLyrics) {
		t.Fatalf("lyrics index document=%+v body=%s", indexDocument, index)
	}
	for position, saved := range savedLyrics {
		if indexDocument.Songs[position].MusicID != saved.MusicID || indexDocument.Songs[position].Revision != saved.Revision {
			t.Fatalf("lyrics index song[%d]=%+v saved=%+v", position, indexDocument.Songs[position], saved)
		}
	}

	published := map[string]assetSnapshot{
		canonicalIndexPath: {body: append([]byte(nil), index...), etag: indexETag},
	}
	canonicalDetails := make(map[int]assetSnapshot, len(savedLyrics))
	for _, saved := range savedLyrics {
		path := detailPath(canonicalRoot, saved.MusicID)
		body, etag, detailStatus := readAsset(path)
		if detailStatus != http.StatusOK || !bytes.Contains(body, []byte(`"version": 1`)) {
			t.Fatalf("lyrics detail %s status=%d body=%s", path, detailStatus, body)
		}
		canonicalDetails[saved.MusicID] = assetSnapshot{body: append([]byte(nil), body...), etag: etag}
		published[path] = canonicalDetails[saved.MusicID]
	}
	firstSaved := savedLyrics[0]
	firstPrivateLineID := []byte(firstSaved.Lines[0].ID)
	for _, saved := range savedLyrics {
		detail := canonicalDetails[saved.MusicID].body
		attributionJSON, err := json.Marshal(saved.Attribution)
		if err != nil {
			t.Fatal(err)
		}
		attributionJSON = append([]byte(`"attribution": `), attributionJSON...)
		privateLineID := []byte(saved.Lines[0].ID)
		if !bytes.Contains(detail, attributionJSON) {
			t.Fatalf("public lyrics musicId=%d omitted attribution: %s", saved.MusicID, detail)
		}
		if !bytes.Contains(detail, []byte(`"id": "line-1"`)) || bytes.Contains(detail, privateLineID) {
			t.Fatalf("public lyrics musicId=%d did not replace private line identity: %s", saved.MusicID, detail)
		}
		for _, privateField := range []string{
			"status", "updatedBy", "sourceNote", "sourceUrl", "licenseNote", "sourcePageId", "sourceRevisionId", "sourceSha1", "sourceFetchedAt",
			saved.SourceNote, saved.SourceURL, saved.LicenseNote, saved.SourceSHA1,
		} {
			if bytes.Contains(detail, []byte(privateField)) {
				t.Fatalf("public lyrics musicId=%d leaked %q: %s", saved.MusicID, privateField, detail)
			}
		}
	}
	for _, locale := range publicLocales {
		root := localizedRoot(locale)
		localizedIndexPath := indexPath(root)
		localizedIndex, localizedIndexETag, localizedIndexStatus := readAsset(localizedIndexPath)
		if localizedIndexStatus != http.StatusOK || !bytes.Equal(localizedIndex, index) || localizedIndexETag != indexETag {
			t.Fatalf("localized lyrics index %s status=%d etag=%q body=%s", locale, localizedIndexStatus, localizedIndexETag, localizedIndex)
		}
		published[localizedIndexPath] = assetSnapshot{body: append([]byte(nil), localizedIndex...), etag: localizedIndexETag}
		for _, saved := range savedLyrics {
			localizedDetailPath := detailPath(root, saved.MusicID)
			localized, localizedETag, localizedStatus := readAsset(localizedDetailPath)
			canonical := canonicalDetails[saved.MusicID]
			if localizedStatus != http.StatusOK || !bytes.Equal(localized, canonical.body) || localizedETag != canonical.etag {
				t.Fatalf("localized lyrics %s musicId=%d status=%d etag=%q body=%s", locale, saved.MusicID, localizedStatus, localizedETag, localized)
			}
			published[localizedDetailPath] = assetSnapshot{body: append([]byte(nil), localized...), etag: localizedETag}
		}
	}
	initialAssets := snapshotAllAssets()
	actualLyricsKeys := make(map[string]bool, len(published))
	for key := range initialAssets {
		if strings.HasPrefix(key, "translation/lyrics/") || (strings.HasPrefix(key, "v2/") && strings.Contains(key, "/translation/lyrics/")) {
			actualLyricsKeys["/files/"+key] = true
		}
	}
	if len(actualLyricsKeys) != len(published) {
		t.Fatalf("public locale contract projected %d lyrics paths, want %d: %v", len(actualLyricsKeys), len(published), actualLyricsKeys)
	}
	for path := range published {
		if !actualLyricsKeys[path] {
			t.Fatalf("public locale contract omitted %s: %v", path, actualLyricsKeys)
		}
	}

	var storedPayload string
	if err := database.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, firstSaved.MusicID).Scan(&storedPayload); err != nil {
		t.Fatal(err)
	}
	privateLineIDJSON, err := json.Marshal(firstSaved.Lines[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyPayload := strings.Replace(storedPayload, `"id":"line-1"`, `"id":`+string(privateLineIDJSON), 1)
	if legacyPayload == storedPayload {
		t.Fatalf("could not construct legacy publication payload: %s", storedPayload)
	}
	if _, err := database.Exec(`UPDATE song_lyrics_publications SET payload_json=? WHERE music_id=?`, legacyPayload, firstSaved.MusicID); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()
	for path, previous := range published {
		current, currentETag, currentStatus := readAsset(path)
		if currentStatus != http.StatusOK || !bytes.Equal(current, previous.body) || currentETag != previous.etag {
			t.Fatalf("legacy stored identity changed %s: status=%d etag=%q body=%s", path, currentStatus, currentETag, current)
		}
		if strings.Contains(path, "/music_") && bytes.Contains(current, firstPrivateLineID) {
			t.Fatalf("legacy source line identity leaked through %s: %s", path, current)
		}
	}
	lastSuccessfulProjection := svc.Status()
	if lastSuccessfulProjection.Generation == 0 || lastSuccessfulProjection.Pending || lastSuccessfulProjection.LastSuccessAt == "" || lastSuccessfulProjection.LastError != "" {
		t.Fatalf("successful projection status = %+v", lastSuccessfulProjection)
	}
	lastSuccessfulAssets := snapshotAllAssets()

	var validCandidate model.PublicSongLyrics
	if err := json.Unmarshal([]byte(legacyPayload), &validCandidate); err != nil {
		t.Fatal(err)
	}
	candidateAttribution := fmt.Sprintf("candidate-attribution-%d", firstSaved.MusicID)
	candidateTitle := fmt.Sprintf("candidate-title-%d", firstSaved.MusicID)
	validCandidate.Attribution = candidateAttribution
	validCandidatePayload, err := json.Marshal(validCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE song_lyrics_publications SET payload_json=? WHERE music_id=?`, string(validCandidatePayload), firstSaved.MusicID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE catalog_music SET title_en=? WHERE music_id=?`, candidateTitle, firstSaved.MusicID); err != nil {
		t.Fatal(err)
	}
	laterSaved := savedLyrics[len(savedLyrics)-1]
	var invalidCandidate model.PublicSongLyrics
	if err := database.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, laterSaved.MusicID).Scan(&storedPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(storedPayload), &invalidCandidate); err != nil {
		t.Fatal(err)
	}
	invalidCandidate.Revision++
	wellFormedInvalidPayload, err := json.Marshal(invalidCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE song_lyrics_publications SET payload_json=? WHERE music_id=?`, string(wellFormedInvalidPayload), laterSaved.MusicID); err != nil {
		t.Fatal(err)
	}
	svc.Rebuild()

	afterFailedAssets := snapshotAllAssets()
	if len(afterFailedAssets) != len(lastSuccessfulAssets) {
		t.Fatalf("failed rebuild changed asset count: before=%d after=%d", len(lastSuccessfulAssets), len(afterFailedAssets))
	}
	for key, previous := range lastSuccessfulAssets {
		current, ok := afterFailedAssets[key]
		if !ok || !bytes.Equal(current.body, previous.body) || current.etag != previous.etag {
			t.Fatalf("failed candidate generation changed asset %s: present=%t etag=%q body=%s", key, ok, current.etag, current.body)
		}
		if bytes.Contains(current.body, []byte(candidateAttribution)) || bytes.Contains(current.body, []byte(candidateTitle)) {
			t.Fatalf("failed candidate generation leaked candidate data through %s: %s", key, current.body)
		}
	}
	for path, previous := range published {
		afterFailure, afterFailureETag, afterFailureStatus := readAsset(path)
		if afterFailureStatus != http.StatusOK || !bytes.Equal(afterFailure, previous.body) || afterFailureETag != previous.etag {
			t.Fatalf("failed candidate generation changed served %s: status=%d etag=%q body=%s", path, afterFailureStatus, afterFailureETag, afterFailure)
		}
	}
	failedProjection := svc.Status()
	if failedProjection.Generation != lastSuccessfulProjection.Generation || !failedProjection.Pending || failedProjection.LastError != "projection_generation_failed" || failedProjection.LastSuccessAt != lastSuccessfulProjection.LastSuccessAt {
		t.Fatalf("failed projection status = %+v, previous = %+v", failedProjection, lastSuccessfulProjection)
	}

	for _, saved := range savedLyrics {
		if _, err := s.UnpublishLyrics(saved.MusicID, saved.Revision, "admin"); err != nil {
			t.Fatalf("unpublish musicId=%d: %v", saved.MusicID, err)
		}
	}
	for path, previous := range published {
		beforeRebuild, beforeRebuildETag, beforeRebuildStatus := readAsset(path)
		if beforeRebuildStatus != http.StatusOK || !bytes.Equal(beforeRebuild, previous.body) || beforeRebuildETag != previous.etag {
			t.Fatalf("unpublish changed served projection before rebuild %s: status=%d etag=%q body=%s", path, beforeRebuildStatus, beforeRebuildETag, beforeRebuild)
		}
	}
	svc.Rebuild()
	recoveredProjection := svc.Status()
	if recoveredProjection.Generation != lastSuccessfulProjection.Generation+1 || recoveredProjection.Pending || recoveredProjection.LastError != "" || recoveredProjection.LastSuccessAt == "" {
		t.Fatalf("recovered projection status = %+v", recoveredProjection)
	}

	detailPaths := make([]string, 0, len(savedLyrics)*(len(publicLocales)+1))
	indexPaths := []string{canonicalIndexPath}
	for _, saved := range savedLyrics {
		detailPaths = append(detailPaths, detailPath(canonicalRoot, saved.MusicID))
	}
	for _, locale := range publicLocales {
		root := localizedRoot(locale)
		indexPaths = append(indexPaths, indexPath(root))
		for _, saved := range savedLyrics {
			detailPaths = append(detailPaths, detailPath(root, saved.MusicID))
		}
	}
	expectedUnpublishedKeys := make(map[string]bool, len(indexPaths))
	for _, path := range indexPaths {
		expectedUnpublishedKeys[strings.TrimPrefix(path, "/files/")] = true
	}
	unpublishedSnapshot := make(map[string]asset, len(indexPaths))
	unexpectedLyricsKeys := make([]string, 0)
	svc.mu.RLock()
	for key, projected := range svc.assets {
		if strings.HasPrefix(key, "translation/lyrics/") || (strings.HasPrefix(key, "v2/") && strings.Contains(key, "/translation/lyrics/")) {
			if expectedUnpublishedKeys[key] {
				unpublishedSnapshot["/files/"+key] = projected
			} else {
				unexpectedLyricsKeys = append(unexpectedLyricsKeys, key)
			}
		}
	}
	svc.mu.RUnlock()
	if len(unexpectedLyricsKeys) != 0 || len(unpublishedSnapshot) != len(indexPaths) {
		t.Fatalf("unpublish was not one complete asset swap: unexpected=%v indexes=%d/%d", unexpectedLyricsKeys, len(unpublishedSnapshot), len(indexPaths))
	}
	for _, path := range detailPaths {
		if _, _, detailStatus := readAsset(path); detailStatus != http.StatusNotFound {
			t.Fatalf("unpublished detail %s status=%d", path, detailStatus)
		}
	}
	var emptyIndex []byte
	var emptyIndexETag string
	for _, path := range indexPaths {
		projected := unpublishedSnapshot[path]
		var document model.PublicLyricsIndex
		if err := json.Unmarshal(projected.body, &document); err != nil {
			t.Fatalf("unpublished index %s is invalid JSON: %v\nbody=%s", path, err, projected.body)
		}
		if document.Version != 1 || len(document.Songs) != 0 {
			t.Fatalf("unpublished index %s = %+v", path, document)
		}
		body, bodyETag, indexStatus := readAsset(path)
		if indexStatus != http.StatusOK || !bytes.Equal(body, projected.body) || bodyETag != projected.etag {
			t.Fatalf("unpublished index %s status=%d etag=%q body=%s", path, indexStatus, bodyETag, body)
		}
		if path == canonicalIndexPath {
			emptyIndex = append([]byte(nil), body...)
			emptyIndexETag = bodyETag
			if bodyETag == indexETag {
				t.Fatalf("unpublished index retained published ETag %q", bodyETag)
			}
			continue
		}
		if !bytes.Equal(body, emptyIndex) || bodyETag != emptyIndexETag {
			t.Fatalf("unpublished locale index %s differs from canonical: etag=%q body=%s", path, bodyETag, body)
		}
	}
}

func TestRebuildWaitsForContentBoundary(t *testing.T) {
	svc := setupLegacyFileService(t)
	release := svc.store.LockContentExclusive()
	done := make(chan struct{})
	go func() {
		svc.Rebuild()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("rebuild crossed an active exclusive operation")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rebuild did not resume after content operation")
	}
}

func TestIncrementalEventRebuild(t *testing.T) {
	svc := setupLegacyFileService(t)

	// Update event 42
	if err := svc.events.UpdateLine(42, "1", "zebra", "新斑马", model.SourceHuman, "talk"); err != nil {
		t.Fatal(err)
	}

	if err := svc.RebuildEvent(42); err != nil {
		t.Fatalf("RebuildEvent failed: %v", err)
	}

	read := func(path string) ([]byte, int) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		svc.Handler().ServeHTTP(rec, req)
		return rec.Body.Bytes(), rec.Code
	}

	canonical, status := read("/files/translation/eventStory/event_42.json")
	if status != http.StatusOK {
		t.Fatalf("Get event_42.json status=%d", status)
	}
	if !strings.Contains(string(canonical), "新斑马") {
		t.Fatalf("expected '新斑马' in event_42.json, got: %s", string(canonical))
	}

	localized, status := read("/files/v2/zh-CN/translation/eventStory/event_42.json")
	if status != http.StatusOK {
		t.Fatalf("Get v2 zh-CN event_42.json status=%d", status)
	}
	if !strings.Contains(string(localized), "新斑马") {
		t.Fatalf("expected '新斑马' in localized event_42.json, got: %s", string(localized))
	}
}

func TestIncrementalCategoryRebuild(t *testing.T) {
	svc := setupLegacyFileService(t)

	// Update category cards
	if _, err := svc.store.UpdateEntry("cards", "prefix", "こんにちは", "您好呀", model.SourceHuman, "test-user"); err != nil {
		t.Fatal(err)
	}

	if err := svc.RebuildCategory("cards"); err != nil {
		t.Fatalf("RebuildCategory failed: %v", err)
	}

	read := func(path string) ([]byte, int) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		svc.Handler().ServeHTTP(rec, req)
		return rec.Body.Bytes(), rec.Code
	}

	flat, status := read("/files/translation/cards.json")
	if status != http.StatusOK {
		t.Fatalf("Get cards.json status=%d", status)
	}
	if !strings.Contains(string(flat), "您好呀") {
		t.Fatalf("expected '您好呀' in cards.json, got: %s", string(flat))
	}

	full, status := read("/files/translation/cards.full.json")
	if status != http.StatusOK {
		t.Fatalf("Get cards.full.json status=%d", status)
	}
	if !strings.Contains(string(full), "您好呀") {
		t.Fatalf("expected '您好呀' in cards.full.json, got: %s", string(full))
	}
}

func TestFullRebuildKeepsConcurrentIncrementalPublication(t *testing.T) {
	svc := setupLegacyFileService(t)
	read := func(path string) (string, int) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		svc.Handler().ServeHTTP(rec, req)
		return rec.Body.String(), rec.Code
	}

	for attempt := 0; attempt < 5; attempt++ {
		text := fmt.Sprintf("您好呀%d", attempt)
		rebuilt := make(chan error, 1)
		go func() { rebuilt <- svc.rebuildAssetsContext(context.Background()) }()

		// The edit and its incremental publication land while the full rebuild
		// is still generating from its earlier read.
		if _, err := svc.store.UpdateEntry("cards", "prefix", "こんにちは", text, model.SourceHuman, "test-user"); err != nil {
			t.Fatal(err)
		}
		if err := svc.RebuildCategory("cards"); err != nil {
			t.Fatalf("RebuildCategory failed: %v", err)
		}
		if err := <-rebuilt; err != nil {
			t.Fatalf("full rebuild failed: %v", err)
		}

		for _, path := range []string{
			"/files/translation/cards.json",
			"/files/translation/cards.full.json",
			"/files/v2/zh-CN/translation/cards.json",
		} {
			body, status := read(path)
			if status != http.StatusOK || !strings.Contains(body, text) {
				t.Fatalf("attempt %d: full rebuild discarded the incremental publication of %s: status=%d body=%s",
					attempt, path, status, body)
			}
		}
	}
}

func TestPublishNowBypassesDebounce(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "publish-now.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.SetDebounce(time.Hour) // very long debounce
	svc.Start()
	defer func() {
		svc.Stop()
		svc.Wait()
	}()

	// Wait for initial generation 1
	deadline := time.Now().Add(time.Second)
	for svc.Status().Generation == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Trigger PublishNow immediately
	svc.PublishNow()

	deadline = time.Now().Add(time.Second)
	for svc.Status().Generation < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if gen := svc.Status().Generation; gen < 2 {
		t.Fatalf("expected generation >= 2 after PublishNow with 1h debounce, got %d", gen)
	}
}

func TestAwaitPublishedPublishesADebouncedRequestAtOnce(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "await-published.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.SetDebounce(time.Hour)

	// Without a worker there is nothing to wait for: nothing pending answers
	// true, a pending request answers false at once.
	if !svc.AwaitPublished(context.Background(), time.Minute) {
		t.Fatal("nothing pending was not reported as published")
	}
	svc.Trigger()
	started := time.Now()
	if svc.AwaitPublished(context.Background(), time.Minute) || time.Since(started) > time.Second {
		t.Fatalf("a request without a worker waited %s or was reported as published", time.Since(started))
	}

	svc.Start()
	defer func() {
		svc.Stop()
		svc.Wait()
	}()
	if !svc.AwaitPublished(context.Background(), 10*time.Second) {
		t.Fatal("the initial generation was not awaited")
	}
	generation := svc.Status().Generation
	// A debounced request would wait an hour; the await publishes it at once.
	svc.Trigger()
	if status := svc.Status(); !status.Pending {
		t.Fatalf("the debounced request is not pending: %+v", status)
	}
	if !svc.AwaitPublished(context.Background(), 10*time.Second) {
		t.Fatal("the debounced request was not published")
	}
	if status := svc.Status(); status.Generation <= generation || status.Pending {
		t.Fatalf("status after the await = %+v, generation before %d", status, generation)
	}

	svc.Trigger()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if svc.AwaitPublished(canceled, 10*time.Second) {
		t.Fatal("a canceled wait reported the pending request as published")
	}
}

func TestThreeLayerOverlayProvenanceTracking(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "provenance-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.Rebuild()

	// 1. Check initial bundle baseline provenance
	status := svc.Status()
	if status.LyricsSummary.Degraded {
		t.Fatalf("unexpected degraded initial status: %+v", status)
	}
	if status.LyricsSummary.BundleSongs == 0 || status.LyricsSummary.TotalSongs != status.LyricsSummary.BundleSongs {
		t.Fatalf("initial bundle songs count mismatch: %+v", status.LyricsSummary)
	}

	p307, ok := svc.SongProvenance(307)
	if !ok || p307.Source != "bundle" || p307.Revision != 1 || p307.State != "complete" || !p307.HasDetail {
		t.Fatalf("expected bundle provenance for 307, got: %+v, ok=%v", p307, ok)
	}

	p682, ok := svc.SongProvenance(682)
	if !ok || p682.Source != "bundle" || p682.Revision != 1 || p682.State != "incomplete" || p682.HasDetail {
		t.Fatalf("expected incomplete bundle provenance for 682, got: %+v, ok=%v", p682, ok)
	}

	// 2. Layer 2: DB Publication overlay
	if err := s.UpsertMusicCatalog([]store.MusicCatalogRecord{
		{MusicID: 307, JapaneseTitle: "DB新曲307", ChineseTitle: "DB新歌307", EnglishTitle: "DB Song 307"},
		{MusicID: 682, JapaneseTitle: "あなたしか見えないの", ChineseTitle: "眼中仅有你一人", EnglishTitle: "Anata Shika Mienai no"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 601, JapaneseName: "歌唱者"}}); err != nil {
		t.Fatal(err)
	}

	input0 := model.SongLyrics{
		MusicID: 307, Revision: 0, Attribution: "Layer 2 publication",
		SourceURL: "https://source.invalid/wiki/307", SourcePageID: 307, SourceRevisionID: 1,
		SourceSHA1: "0123456789abcdef0123456789abcdef01234567", SourceFetchedAt: "2026-08-11T00:00:00Z",
		Lines: []model.LyricLine{{
			ID: "overlay-line", Order: 0, Japanese: "DB歌词", Chinese: "DB歌词一",
			Segments: []model.LyricSegment{{Text: "DB歌词", PerformerIDs: []int{601}}},
		}},
	}
	saved0, changed, err := s.SaveImportedLyricsMutation(input0, "fixture")
	if err != nil || !changed {
		t.Fatalf("save imported lyrics 307: changed=%t err=%v", changed, err)
	}
	input1 := model.SongLyrics{
		MusicID: 307, Revision: saved0.Revision, Attribution: "Layer 2 publication",
		SourceURL: "https://source.invalid/wiki/307", SourcePageID: 307, SourceRevisionID: 1,
		SourceSHA1: "0123456789abcdef0123456789abcdef01234567", SourceFetchedAt: "2026-08-11T00:00:00Z",
		Lines: []model.LyricLine{{
			ID: "overlay-line", Order: 0, Japanese: "DB歌词", Chinese: "DB歌词二",
			Segments: []model.LyricSegment{{Text: "DB歌词", PerformerIDs: []int{601}}},
		}},
	}
	saved1, changed, err := s.SaveLyricsMutation(input1, "fixture")
	if err != nil || !changed {
		t.Fatalf("save lyrics mutation 307: changed=%t err=%v", changed, err)
	}
	if _, err := s.PublishLyrics(saved1.MusicID, saved1.Revision); err != nil {
		t.Fatal(err)
	}

	svc.Rebuild()

	status = svc.Status()
	if status.LyricsSummary.DBPublicationSongs != 1 {
		t.Fatalf("expected 1 DBPublicationSong, got: %+v", status.LyricsSummary)
	}
	p307After, ok := svc.SongProvenance(307)
	if !ok || p307After.Source != "db_publication" || p307After.Revision != 2 {
		t.Fatalf("expected db_publication provenance for 307, got: %+v, ok=%v", p307After, ok)
	}

	// 3. Layer 3: Localization projection overlay (for 682)
	if _, err := database.Exec(db.MigrationV32Song682TranslationEditionsSQL); err != nil {
		t.Fatalf("apply migration 32 failed: %v", err)
	}
	if _, err := database.Exec(db.MigrationV33Song682TranslationQEDCorrectionSQL); err != nil {
		t.Fatalf("apply migration 33 failed: %v", err)
	}
	if _, err := database.Exec(db.MigrationV34Song682TranslationMirrorSyncSQL); err != nil {
		t.Fatalf("apply migration 34 failed: %v", err)
	}

	svc.Rebuild()

	status = svc.Status()
	if status.LyricsSummary.LocalizationSongs != 1 {
		t.Fatalf("expected 1 LocalizationSong, got: %+v", status.LyricsSummary)
	}
	if status.LyricsSummary.DBPublicationSongs != 1 {
		t.Fatalf("expected 1 DBPublicationSong, got: %+v", status.LyricsSummary)
	}
	p682Loc, ok := svc.SongProvenance(682)
	if !ok || p682Loc.Source != "localization_projection" || p682Loc.Revision != 10 || p682Loc.State != "complete" || !p682Loc.HasDetail {
		t.Fatalf("expected localization_projection provenance for 682, got: %+v, ok=%v", p682Loc, ok)
	}

	// 4. Nonexistent song
	if _, ok := svc.SongProvenance(999999); ok {
		t.Fatal("expected false for nonexistent song provenance")
	}
}

func TestDBPublicationFailureSetsDegradedAndFailsOpen(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "fail-open-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.Rebuild()

	read := func(path string) (int, []byte) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp := httptest.NewRecorder()
		svc.Handler().ServeHTTP(resp, req)
		return resp.Code, resp.Body.Bytes()
	}

	status, bundleIndex := read("/files/translation/lyrics/index.json")
	if status != http.StatusOK {
		t.Fatalf("initial bundle index status=%d", status)
	}

	// Corrupt database publications table so PublishedLyricsJSON fails
	if _, err := database.Exec(`DROP TABLE song_lyrics_publications`); err != nil {
		t.Fatal(err)
	}

	svc.Rebuild()

	// Should fail-open: return HTTP 200 with bundle index bytes
	status, afterIndex := read("/files/translation/lyrics/index.json")
	if status != http.StatusOK || !bytes.Equal(afterIndex, bundleIndex) {
		t.Fatalf("failed to serve bundle bytes on degraded: status=%d equal=%v", status, bytes.Equal(afterIndex, bundleIndex))
	}

	projStatus := svc.Status()
	if !projStatus.LyricsSummary.Degraded || projStatus.LyricsSummary.DegradedReason == "" {
		t.Fatalf("expected degraded status with reason, got: %+v", projStatus.LyricsSummary)
	}
	// Verify that the overall service generation did NOT fail (Generation > 0, LastError == "")
	if projStatus.Generation == 0 || projStatus.LastError != "" {
		t.Fatalf("fail-open should not fail service projection status: %+v", projStatus)
	}
}
