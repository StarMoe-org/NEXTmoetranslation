package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"moesekai/server/internal/files"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func TestLyricsSourceImportSaveWithoutProducerHeader(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
	}}); err != nil {
		t.Fatal(err)
	}
	h.api.lyricsSrc = fakeLyricsSource{preview: lyricssource.Preview{
		CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
		FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
	}}
	previewResponse := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
		"musicId": 10, "pageId": 12, "revisionId": 34,
	})
	defer previewResponse.Body.Close()
	var preview lyricssource.Preview
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil || preview.ImportToken == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	draft := apiLyrics()
	draft.SourceURL = preview.CanonicalURL
	draft.SourcePageID = preview.PageID
	draft.SourceRevisionID = preview.RevisionID
	draft.SourceSHA1 = preview.SHA1
	draft.SourceFetchedAt = preview.FetchedAt
	draft.Lines = []model.LyricLine{{
		ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞",
		Segments: []model.LyricSegment{{Text: "歌詞", PerformerIDs: []int{}, Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}},
	}}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	request["sourceImportToken"] = preview.ImportToken

	response := doJSON(t, http.MethodPut, h.server.URL+"/api/editor/v1/lyrics/save", h.token, request)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("headerless verified import status=%d body=%s", response.StatusCode, body)
	}
	var saved model.SongLyrics
	if err := json.NewDecoder(response.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.SourceRevisionID != 34 || saved.SourceSHA1 != sourceSHA1 {
		t.Fatalf("headerless verified import saved=%+v", saved)
	}
	h.api.lyricsImportMu.Lock()
	_, stillPresent := h.api.lyricsImports[preview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if stillPresent {
		t.Fatal("headerless verified import did not consume its grant")
	}
}

// synchronousFileService rebuilds the real projection on PublishNow so the
// test can read the served assets right after the handler returns.
type synchronousFileService struct {
	*filesvc.Service
	publishNow int
}

func (f *synchronousFileService) PublishNow() {
	f.publishNow++
	f.Service.Rebuild()
}

func TestSourceV3UnpublishAndPublishToggleTheServedSong(t *testing.T) {
	const musicID = 990765
	h := setupLegacyAPI(t)
	seedAPISourceV3Lyrics(t, h, musicID)
	// The localization projection only serves songs edited after the batch.
	if _, err := h.db.Exec(`UPDATE song_lyrics_rendition_localizations SET revision=2`); err != nil {
		t.Fatal(err)
	}
	service := &synchronousFileService{Service: filesvc.New(h.store, h.events, files.NewGenerator(h.store, h.events, ""))}
	h.api.SetFileService(service)
	service.Rebuild()

	read := func(path string) (int, []byte) {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		return response.Code, response.Body.Bytes()
	}
	served := func() bool {
		t.Helper()
		status, body := read("/files/translation/lyrics/index.json")
		if status != http.StatusOK {
			t.Fatalf("index status=%d", status)
		}
		var index store.PublicLyricsIndexDocument
		if err := json.Unmarshal(body, &index); err != nil {
			t.Fatal(err)
		}
		for _, song := range index.Songs {
			if song.MusicID == musicID {
				return true
			}
		}
		return false
	}
	detailPath := "/files/translation/lyrics/music_990765.json"
	if !served() {
		t.Fatal("seeded source-v3 song is not served before unpublish")
	}
	if status, _ := read(detailPath); status != http.StatusOK {
		t.Fatalf("detail status before unpublish=%d", status)
	}

	current, err := h.store.GetLyricsRenditionDocument(musicID)
	if err != nil {
		t.Fatal(err)
	}
	publication := func(path string, revision int) (int, map[string]any) {
		t.Helper()
		response := authorizedRequest(t, h, http.MethodPost, path, map[string]any{"musicId": musicID, "revision": revision})
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}

	status, body := publication("/api/editor/v1/lyrics/unpublish", current.Revision)
	if status != http.StatusOK || body["musicId"] != float64(musicID) || body["revision"] != float64(current.Revision) {
		t.Fatalf("unpublish status=%d body=%v", status, body)
	}
	if withdrawals, err := h.store.PublicLyricsWithdrawals(); err != nil || !withdrawals[musicID] {
		t.Fatalf("unpublish withdrawals=%v err=%v", withdrawals, err)
	}
	if service.publishNow != 1 || served() {
		t.Fatalf("after unpublish publishNow=%d served=%v", service.publishNow, served())
	}
	if status, _ := read(detailPath); status != http.StatusNotFound {
		t.Fatalf("detail status after unpublish=%d", status)
	}

	if status, _ := publication("/api/editor/v1/lyrics/unpublish", current.Revision); status != http.StatusOK || service.publishNow != 1 {
		t.Fatalf("repeated unpublish status=%d publishNow=%d", status, service.publishNow)
	}
	status, body = publication("/api/editor/v1/lyrics/publish", current.Revision+1)
	current409, _ := body["current"].(map[string]any)
	if status != http.StatusConflict || body["error"] != "revision_conflict" || current409["revision"] != float64(current.Revision) {
		t.Fatalf("stale publish status=%d body=%v", status, body)
	}

	status, body = publication("/api/editor/v1/lyrics/publish", current.Revision)
	if status != http.StatusOK || body["musicId"] != float64(musicID) {
		t.Fatalf("publish status=%d body=%v", status, body)
	}
	if withdrawals, err := h.store.PublicLyricsWithdrawals(); err != nil || withdrawals[musicID] {
		t.Fatalf("publish withdrawals=%v err=%v", withdrawals, err)
	}
	if service.publishNow != 2 || !served() {
		t.Fatalf("after publish publishNow=%d served=%v", service.publishNow, served())
	}
	if status, _ := read(detailPath); status != http.StatusOK {
		t.Fatalf("detail status after publish=%d", status)
	}
}

// pjsk.moe validates a database-overlaid legacy detail with an exact key
// allowlist (web/src/lib/lyrics.ts validateDocument, isLineV1, isSegmentV1)
// and drops the whole song on any extra key.
func TestServedLegacyLyricsDetailStaysInsidePjskMoeV1Allowlist(t *testing.T) {
	const musicID = 990766
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{MusicID: musicID, JapaneseTitle: "許可リスト"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "歌唱者"}}); err != nil {
		t.Fatal(err)
	}
	saved, _, err := h.store.SaveImportedLyricsMutation(model.SongLyrics{
		MusicID: musicID, Attribution: "allowlist fixture", SourceNote: "private note", LicenseNote: "private license",
		SourceURL: "https://vocaloid.fandom.com/wiki/Allowlist_Song?oldid=4321", SourcePageID: 99, SourceRevisionID: 4321,
		SourceSHA1: "0123456789abcdef0123456789abcdef01234567", SourceFetchedAt: "2026-08-11T00:00:00Z",
		Lines: []model.LyricLine{{
			ID: "private-line-id", Order: 0, Japanese: "歌詞", Chinese: "歌词", StanzaBreakBefore: false,
			Segments: []model.LyricSegment{{Text: "歌詞", PerformerIDs: []int{1}, Ruby: []model.LyricRubySpan{{Text: "歌詞", Reading: "かし"}}}},
		}},
	}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.PublishLyrics(musicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	service := filesvc.New(h.store, h.events, files.NewGenerator(h.store, h.events, ""))
	service.Rebuild()
	request := httptest.NewRequest(http.MethodGet, "/files/translation/lyrics/music_990766.json", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("served detail status=%d", response.Code)
	}
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	assertOnlyKeys := func(what string, object map[string]json.RawMessage, allowed ...string) {
		t.Helper()
		allow := map[string]bool{}
		for _, key := range allowed {
			allow[key] = true
		}
		for key := range object {
			if !allow[key] {
				t.Fatalf("%s carries key %q outside the pjsk.moe v1 allowlist: %s", what, key, response.Body.Bytes())
			}
		}
	}
	assertOnlyKeys("detail", detail, "version", "musicId", "revision", "updatedAt", "attribution", "attributions", "lines")
	if string(detail["version"]) != "1" || detail["attributions"] == nil {
		t.Fatalf("served legacy detail version=%s attributions=%s", detail["version"], detail["attributions"])
	}
	var lines []map[string]json.RawMessage
	if err := json.Unmarshal(detail["lines"], &lines); err != nil || len(lines) != 1 {
		t.Fatalf("served lines=%s err=%v", detail["lines"], err)
	}
	assertOnlyKeys("line", lines[0], "id", "order", "japanese", "zh-CN", "en-US", "stanzaBreakBefore", "segments")
	var segments []map[string]json.RawMessage
	if err := json.Unmarshal(lines[0]["segments"], &segments); err != nil || len(segments) != 1 {
		t.Fatalf("served segments=%s err=%v", lines[0]["segments"], err)
	}
	assertOnlyKeys("segment", segments[0], "text", "performerIds")
	var attributions []map[string]json.RawMessage
	if err := json.Unmarshal(detail["attributions"], &attributions); err != nil || len(attributions) != 1 {
		t.Fatalf("served attributions=%s err=%v", detail["attributions"], err)
	}
	assertOnlyKeys("attribution", attributions[0], "provider", "title", "revisionId", "revisionUrl", "licenseName", "licenseUrl")
}
