package api

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// recordingFileService counts the immediate publications a handler requests.
type recordingFileService struct {
	publishNow atomic.Int64
	categories atomic.Int64
}

func (f *recordingFileService) RebuildEvent(int) error { return nil }

func (f *recordingFileService) RebuildSideStory(string, string) error { return nil }

func (f *recordingFileService) RebuildCategory(string) error {
	f.categories.Add(1)
	return nil
}

func (f *recordingFileService) PublishNow() { f.publishNow.Add(1) }

func (f *recordingFileService) Status() filesvc.ProjectionStatus { return filesvc.ProjectionStatus{} }

func (f *recordingFileService) SongProvenance(int) (filesvc.SongProvenance, bool) {
	return filesvc.SongProvenance{}, false
}

func setupRecordingFileService(t *testing.T) (*legacyAPIHarness, *recordingFileService) {
	t.Helper()
	h := setupLegacyAPI(t)
	recorder := &recordingFileService{}
	h.api.SetFileService(recorder)
	return h, recorder
}

func TestTranslationEditionMutationsPublishImmediately(t *testing.T) {
	h, recorder := setupRecordingFileService(t)
	seedAPISourceV3Lyrics(t, h, 765)

	detail := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/detail?musicId=765", nil)
	defer detail.Body.Close()
	var document store.LyricsRenditionDocument
	if err := json.NewDecoder(detail.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}

	mutations := []map[string]any{
		{"musicId": 765, "operation": "create", "editionKey": "alternate", "label": "另一译本"},
		{"musicId": 765, "operation": "rename", "editionKey": "alternate", "label": "改名译本"},
		{"musicId": 765, "operation": "set-default", "editionKey": "alternate"},
	}
	revision := document.Revision
	for _, mutation := range mutations {
		before := recorder.publishNow.Load()
		mutation["revision"] = revision
		response := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/translation-editions", mutation)
		body := store.LyricsRenditionDocument{}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("%v edition status=%d", mutation["operation"], response.StatusCode)
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		revision = body.Revision
		if got := recorder.publishNow.Load(); got != before+1 {
			t.Fatalf("%v edition requested %d immediate publications, want 1", mutation["operation"], got-before)
		}
	}
}

func TestMusicTitleBatchPublishesImmediately(t *testing.T) {
	h, recorder := setupRecordingFileService(t)
	if _, err := h.store.ImportCategory("music", model.Category{
		"title": {"はじめまして": {Text: "初次见面", Source: model.SourceCN, Ids: []string{"307"}}},
	}); err != nil {
		t.Fatal(err)
	}

	batch := func(category, field, key, text string) {
		t.Helper()
		snapshotResponse := authorizedRequest(t, h, http.MethodGet, "/api/category/snapshot?category="+category+"&locale=zh-CN", nil)
		defer snapshotResponse.Body.Close()
		if snapshotResponse.StatusCode != http.StatusOK {
			t.Fatalf("%s snapshot status=%d", category, snapshotResponse.StatusCode)
		}
		var snapshot model.CategoryLocaleSnapshot
		if err := json.NewDecoder(snapshotResponse.Body).Decode(&snapshot); err != nil {
			t.Fatal(err)
		}
		response := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/category/batch", map[string]any{
			"category": category, "locale": model.LocaleChinese, "baseRevision": snapshot.Revision,
			"updates": []map[string]string{{"field": field, "key": key, "text": text, "source": "human"}},
		})
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s/%s batch status=%d", category, field, response.StatusCode)
		}
	}

	batch("cards", "prefix", "cn-key", "不影响歌词索引")
	if got := recorder.publishNow.Load(); got != 0 {
		t.Fatalf("a cards batch requested %d immediate publications, want 0", got)
	}

	batch("music", "title", "はじめまして", "初次见面（修订）")
	if got := recorder.publishNow.Load(); got != 1 {
		t.Fatalf("a music/title batch requested %d immediate publications, want 1", got)
	}
}

func TestUnpublishRecordsAndPublishClearsTheWithdrawalMarker(t *testing.T) {
	h, recorder := setupRecordingFileService(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{
		{MusicID: 307, JapaneseTitle: "撤下测试", ChineseTitle: "撤下测试", EnglishTitle: "Withdrawal fixture"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 601, JapaneseName: "歌唱者"}}); err != nil {
		t.Fatal(err)
	}
	saved, changed, err := h.store.SaveImportedLyricsMutation(model.SongLyrics{
		MusicID: 307, Revision: 0, Attribution: "withdrawal fixture",
		SourceURL: "https://source.invalid/wiki/307", SourcePageID: 307, SourceRevisionID: 1,
		SourceSHA1: "0123456789abcdef0123456789abcdef01234567", SourceFetchedAt: "2026-08-11T00:00:00Z",
		Lines: []model.LyricLine{{
			ID: "withdrawal-line", Order: 0, Japanese: "歌词", Chinese: "歌词一",
			Segments: []model.LyricSegment{{Text: "歌词", PerformerIDs: []int{601}}},
		}},
	}, "fixture")
	if err != nil || !changed {
		t.Fatalf("seed lyrics: changed=%t err=%v", changed, err)
	}
	if _, err := h.store.PublishLyrics(307, saved.Revision); err != nil {
		t.Fatal(err)
	}

	unpublished := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/unpublish", map[string]any{
		"musicId": 307, "revision": saved.Revision,
	})
	defer unpublished.Body.Close()
	if unpublished.StatusCode != http.StatusOK {
		t.Fatalf("unpublish status=%d", unpublished.StatusCode)
	}
	withdrawals, err := h.store.PublicLyricsWithdrawals()
	if err != nil || !withdrawals[307] {
		t.Fatalf("unpublish did not record a withdrawal: %v err=%v", withdrawals, err)
	}
	if got := recorder.publishNow.Load(); got != 1 {
		t.Fatalf("unpublish requested %d immediate publications, want 1", got)
	}

	republished := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/publish", map[string]any{
		"musicId": 307, "revision": saved.Revision,
	})
	defer republished.Body.Close()
	if republished.StatusCode != http.StatusOK {
		t.Fatalf("publish status=%d", republished.StatusCode)
	}
	withdrawals, err = h.store.PublicLyricsWithdrawals()
	if err != nil || withdrawals[307] {
		t.Fatalf("publish did not clear the withdrawal: %v err=%v", withdrawals, err)
	}
}
