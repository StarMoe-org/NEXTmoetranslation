package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/internal/publiclyricsbundle"
	"moesekai/server/internal/store"
)

type fakeLyricsSource struct {
	candidates []lyricssource.Candidate
	preview    lyricssource.Preview
	err        error
	previewFn  func(context.Context, lyricssource.MusicIdentity, int, int) (lyricssource.Preview, error)
}

func (f fakeLyricsSource) Search(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
	return f.candidates, f.err
}

func (f fakeLyricsSource) Preview(ctx context.Context, identity lyricssource.MusicIdentity, pageID, revisionID int) (lyricssource.Preview, error) {
	if f.previewFn != nil {
		return f.previewFn(ctx, identity, pageID, revisionID)
	}
	return f.preview, f.err
}

func TestLyricsSourceIdentityPreservesRoleBoundCatalogCredits(t *testing.T) {
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", ProducerMetadata: "producer metadata",
		Lyricist: "作詞者", Composer: "作曲者", Arranger: "編曲者",
	}}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	identity, ok := h.api.lyricsSourceIdentity(recorder, 10)
	if !ok || recorder.Code != http.StatusOK {
		t.Fatalf("identity ok=%v status=%d body=%s", ok, recorder.Code, recorder.Body.String())
	}
	if identity.MusicID != 10 || identity.JapaneseTitle != "新曲" || identity.ProducerMetadata != "作詞者 | 作曲者 | 編曲者" ||
		identity.Lyricist != "作詞者" || identity.Composer != "作曲者" || identity.Arranger != "編曲者" {
		t.Fatalf("identity=%+v", identity)
	}
}

func seedLyricsCatalog(t *testing.T, h *legacyAPIHarness) {
	t.Helper()
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{
		{MusicID: 10, JapaneseTitle: "新曲", ChineseTitle: "新歌", EnglishTitle: "New Song", IsNewlyWrittenMusic: true},
		{MusicID: 20, JapaneseTitle: "旧曲", IsNewlyWrittenMusic: false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{
		{PerformerID: 1, JapaneseName: "初音ミク", ChineseName: "初音未来", EnglishName: "Hatsune Miku"},
	}); err != nil {
		t.Fatal(err)
	}
}

func strictLyricsSave(t *testing.T, h *legacyAPIHarness, bearer string, body any) *http.Response {
	t.Helper()
	return strictLyricsSaveAt(t, h.server.URL+"/api/editor/v1/lyrics/save", bearer, body, h.api.editorGate.Status())
}

func strictLyricsSaveAt(t *testing.T, url, bearer string, body any, status editorgate.Status) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, url, strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(loadedProducerStateHeader, loadedState(status))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func apiLyrics() model.SongLyrics {
	return model.SongLyrics{
		MusicID: 10, Revision: 0, Status: "draft", Attribution: "MoeSeka translation team", SourceNote: "manual",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "初音歌う", Chinese: "初音歌唱", English: "Miku sings",
			Segments: []model.LyricSegment{{Text: "初音", PerformerIDs: []int{1}}, {Text: "歌う", PerformerIDs: []int{1}}},
		}},
	}
}

func TestCatalogMusicAddsEmbeddedRuntimeLyricsWithoutChangingDBStatus(t *testing.T) {
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{
		{MusicID: 307, JapaneseTitle: "おこちゃま戦争"},
		{MusicID: 682, JapaneseTitle: "あなたしか見えないの"},
		{MusicID: 999999, JapaneseTitle: "Bundle 外の曲"},
	}); err != nil {
		t.Fatal(err)
	}

	response := authorizedRequest(t, h, http.MethodGet, "/api/catalog/music?newlyWritten=false&limit=100", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("catalog status=%d", response.StatusCode)
	}
	var result model.CatalogMusicResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	items := make(map[int]model.CatalogMusicItem, len(result.Items))
	for _, item := range result.Items {
		items[item.MusicID] = item
	}
	complete := items[307]
	if complete.LyricsStatus != "" || complete.RuntimeLyrics == nil || complete.RuntimeLyrics.ReleaseID != publiclyricsbundle.ReleaseID ||
		!complete.RuntimeLyrics.ImmutableOverlay || complete.RuntimeLyrics.State != "complete" || !complete.RuntimeLyrics.HasDetail ||
		complete.RuntimeLyrics.Revision != 1 || complete.RuntimeLyrics.UpdatedAt != "2026-08-08T13:24:16Z" ||
		!reflect.DeepEqual(complete.RuntimeLyrics.AvailableVersions, []string{"full", "game"}) ||
		complete.RuntimeLyrics.BatchSHA256 != publiclyricsbundle.BatchSHA256 || complete.RuntimeLyrics.RootSHA256 != publiclyricsbundle.RootSHA256 {
		t.Fatalf("complete catalog item=%+v", complete)
	}
	incomplete := items[682]
	if incomplete.RuntimeLyrics == nil {
		t.Fatalf("incomplete catalog item has no runtime metadata: %+v", incomplete)
	}
	if incomplete.LyricsStatus != "" || incomplete.RuntimeLyrics.State != "incomplete" ||
		incomplete.RuntimeLyrics.HasDetail || incomplete.RuntimeLyrics.AvailableVersions == nil || len(incomplete.RuntimeLyrics.AvailableVersions) != 0 {
		t.Fatalf("incomplete catalog item=%+v runtime=%+v", incomplete, *incomplete.RuntimeLyrics)
	}
	outside := items[999999]
	if outside.LyricsStatus != "" || outside.RuntimeLyrics != nil {
		t.Fatalf("non-bundle catalog item=%+v", outside)
	}

	saved, err := h.store.SaveLyrics(model.SongLyrics{MusicID: 307, Status: "draft", Lines: []model.LyricLine{{
		ID: "line-1", Order: 0, Japanese: "歌", Segments: []model.LyricSegment{{Text: "歌", PerformerIDs: []int{}}},
	}}}, "alice")
	if err != nil || saved.Revision != 1 {
		t.Fatalf("save source draft=%+v err=%v", saved, err)
	}
	response = authorizedRequest(t, h, http.MethodGet, "/api/catalog/music?newlyWritten=false&limit=100", nil)
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Items {
		if item.MusicID == 307 {
			if item.LyricsStatus != "draft" || item.RuntimeLyrics == nil || item.RuntimeLyrics.State != "complete" {
				t.Fatalf("DB draft/runtime overlay item=%+v", item)
			}
			return
		}
	}
	t.Fatal("music 307 missing after DB draft save")
}

type provenanceFileService struct {
	fakeFileService
	provenance map[int]filesvc.SongProvenance
}

func (f *provenanceFileService) SongProvenance(musicID int) (filesvc.SongProvenance, bool) {
	p, ok := f.provenance[musicID]
	return p, ok
}

func TestCatalogMusicRuntimeLyricsFollowsLivePublicProjection(t *testing.T) {
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{
		{MusicID: 307, JapaneseTitle: "おこちゃま戦争"},
		{MusicID: 682, JapaneseTitle: "あなたしか見えないの"},
		{MusicID: 999999, JapaneseTitle: "Bundle 外の曲"},
	}); err != nil {
		t.Fatal(err)
	}
	// 307 stays served from the embedded bundle, 682 was withdrawn from the
	// public mirror, 999999 is a database publication outside the bundle.
	h.api.SetFileService(&provenanceFileService{provenance: map[int]filesvc.SongProvenance{
		307: {MusicID: 307, Source: filesvc.SongSourceBundle, Revision: 1, State: "complete",
			AvailableVersions: []string{"full", "game"}, UpdatedAt: "2026-08-08T13:24:16Z", HasDetail: true},
		999999: {MusicID: 999999, Source: filesvc.SongSourceDBPublication, Revision: 3, State: "complete",
			AvailableVersions: []string{"full"}, UpdatedAt: "2026-09-22T20:19:58Z", HasDetail: true},
	}})

	response := authorizedRequest(t, h, http.MethodGet, "/api/catalog/music?newlyWritten=false&limit=100", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("catalog status=%d", response.StatusCode)
	}
	var result model.CatalogMusicResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	items := make(map[int]model.CatalogMusicItem, len(result.Items))
	for _, item := range result.Items {
		items[item.MusicID] = item
	}
	bundled := items[307].RuntimeLyrics
	if bundled == nil || !bundled.ImmutableOverlay || bundled.Source != "bundle" || bundled.ReleaseID != publiclyricsbundle.ReleaseID ||
		bundled.BatchSHA256 != publiclyricsbundle.BatchSHA256 || bundled.State != "complete" || !bundled.HasDetail ||
		!reflect.DeepEqual(bundled.AvailableVersions, []string{"full", "game"}) {
		t.Fatalf("bundle-served item runtime=%+v", bundled)
	}
	if withdrawn := items[682].RuntimeLyrics; withdrawn != nil {
		t.Fatalf("withdrawn bundle song still advertises a public mirror: %+v", *withdrawn)
	}
	published := items[999999].RuntimeLyrics
	if published == nil || published.ImmutableOverlay || published.Source != "db_publication" || published.ReleaseID != "" ||
		published.BatchSHA256 != "" || published.Revision != 3 || published.State != "complete" ||
		!reflect.DeepEqual(published.AvailableVersions, []string{"full"}) {
		t.Fatalf("database-published item runtime=%+v", published)
	}
}

func TestLyricsAPIContractAndRBAC(t *testing.T) {
	h := setupLegacyAPI(t)
	seedLyricsCatalog(t, h)

	catalog := authorizedRequest(t, h, http.MethodGet, "/api/catalog/music", nil)
	defer catalog.Body.Close()
	var catalogResult model.CatalogMusicResponse
	if err := json.NewDecoder(catalog.Body).Decode(&catalogResult); err != nil {
		t.Fatal(err)
	}
	if len(catalogResult.Items) != 1 || catalogResult.Items[0].MusicID != 10 {
		t.Fatalf("default catalog = %+v", catalogResult)
	}
	characters := authorizedRequest(t, h, http.MethodGet, "/api/catalog/characters", nil)
	defer characters.Body.Close()
	var characterResult model.CatalogPerformerResponse
	if err := json.NewDecoder(characters.Body).Decode(&characterResult); err != nil {
		t.Fatal(err)
	}
	if len(characterResult.Items) != 1 || characterResult.Items[0].PerformerID != 1 {
		t.Fatalf("character catalog = %+v", characterResult)
	}

	createEditor := authorizedRequest(t, h, http.MethodPost, "/api/admin/users", map[string]string{
		"username": "editor", "password": "strong-password-123", "role": auth.RoleEditor,
	})
	createEditor.Body.Close()
	login := doJSON(t, http.MethodPost, h.server.URL+"/api/auth/login", "", map[string]string{"username": "editor", "password": "strong-password-123"})
	defer login.Body.Close()
	var editorLogin struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(login.Body).Decode(&editorLogin); err != nil {
		t.Fatal(err)
	}

	save := strictAs(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", editorLogin.Token, apiLyrics())
	defer save.Body.Close()
	if save.StatusCode != http.StatusOK {
		t.Fatalf("editor save status = %d", save.StatusCode)
	}
	var saved model.SongLyrics
	if err := json.NewDecoder(save.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.Status != "draft" {
		t.Fatalf("saved lyrics = %+v", saved)
	}

	editorPublish := strictAs(t, h, http.MethodPost, "/api/editor/v1/lyrics/publish", editorLogin.Token, map[string]int{
		"musicId": 10, "revision": 1,
	})
	defer editorPublish.Body.Close()
	if editorPublish.StatusCode != http.StatusForbidden {
		t.Fatalf("editor publish status = %d", editorPublish.StatusCode)
	}

	adminPublish := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/publish", map[string]int{
		"musicId": 10, "revision": 1,
	})
	defer adminPublish.Body.Close()
	if adminPublish.StatusCode != http.StatusOK {
		t.Fatalf("admin publish status = %d", adminPublish.StatusCode)
	}
	if err := json.NewDecoder(adminPublish.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "published" {
		t.Fatalf("publish result = %+v", saved)
	}

	list := authorizedRequest(t, h, http.MethodGet, "/api/lyrics", nil)
	defer list.Body.Close()
	var listed model.LyricsListResponse
	if err := json.NewDecoder(list.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.Items[0].Status != "published" {
		t.Fatalf("lyrics list = %+v", listed)
	}
}

type fakeFileService struct {
	publishNowCalls int
}

func (f *fakeFileService) RebuildEvent(eventID int) error        { return nil }
func (f *fakeFileService) RebuildCategory(category string) error { return nil }
func (f *fakeFileService) PublishNow()                           { f.publishNowCalls++ }
func (f *fakeFileService) Status() filesvc.ProjectionStatus      { return filesvc.ProjectionStatus{} }
func (f *fakeFileService) SongProvenance(musicID int) (filesvc.SongProvenance, bool) {
	return filesvc.SongProvenance{}, false
}

func TestLyricsPublicationTriggersProjectionPublishNow(t *testing.T) {
	h := setupLegacyAPI(t)
	seedLyricsCatalog(t, h)

	save := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", apiLyrics())
	save.Body.Close()
	if save.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d", save.StatusCode)
	}

	fake := &fakeFileService{}
	h.api.SetFileService(fake)

	published := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/publish", map[string]int{
		"musicId": 10, "revision": 1,
	})
	published.Body.Close()
	if published.StatusCode != http.StatusOK {
		t.Fatalf("publish status = %d", published.StatusCode)
	}
	if fake.publishNowCalls != 1 {
		t.Fatalf("PublishNow calls after publish = %d, want 1", fake.publishNowCalls)
	}

	// A no-op publication of the same revision does not request another rebuild.
	noop := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/publish", map[string]int{
		"musicId": 10, "revision": 1,
	})
	noop.Body.Close()
	if noop.StatusCode != http.StatusOK {
		t.Fatalf("noop publish status = %d", noop.StatusCode)
	}
	if fake.publishNowCalls != 1 {
		t.Fatalf("PublishNow calls after noop publish = %d, want 1", fake.publishNowCalls)
	}

	unpublished := authorizedRequest(t, h, http.MethodPost, "/api/editor/v1/lyrics/unpublish", map[string]int{
		"musicId": 10, "revision": 1,
	})
	unpublished.Body.Close()
	if unpublished.StatusCode != http.StatusOK {
		t.Fatalf("unpublish status = %d", unpublished.StatusCode)
	}
	if fake.publishNowCalls != 2 {
		t.Fatalf("PublishNow calls after unpublish = %d, want 2", fake.publishNowCalls)
	}
}

func TestPrivateLyricsDetailAvoidsHTMLExpansionBeyondSekaiTextBodyCap(t *testing.T) {
	h := setupLegacyAPI(t)
	seedLyricsCatalog(t, h)
	const lineCount = 200
	text := strings.Repeat("<", 6000)
	draft := model.SongLyrics{MusicID: 10, Status: "draft", Lines: make([]model.LyricLine, lineCount)}
	for index := range draft.Lines {
		draft.Lines[index] = model.LyricLine{
			ID: fmt.Sprintf("html-%d", index), Order: index, Japanese: text, Chinese: "", English: "",
			Segments: []model.LyricSegment{{
				Text: text, PerformerIDs: []int{1}, Ruby: []model.LyricRubySpan{{Text: text}},
			}},
		}
	}
	saved, err := h.store.SaveLyrics(draft, "alice")
	if err != nil {
		t.Fatal(err)
	}
	htmlEscaped, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(htmlEscaped) <= maxJSONBodyBytes {
		t.Fatalf("HTML-escaped control payload=%d bytes, want more than %d", len(htmlEscaped), maxJSONBodyBytes)
	}

	response := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/detail?musicId=10", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("private lyrics detail status=%d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxJSONBodyBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > maxJSONBodyBytes {
		t.Fatalf("non-HTML-escaped private lyrics response=%d bytes, cap=%d", len(body), maxJSONBodyBytes)
	}
	if bytes.Contains(body, []byte(`\u003c`)) || !bytes.Contains(body, []byte("<<<")) {
		t.Fatal("private lyrics response did not preserve literal HTML-sensitive characters")
	}
	var decoded model.SongLyrics
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode private lyrics response: %v", err)
	}
	if len(decoded.Lines) != lineCount || decoded.Lines[0].Japanese != text {
		t.Fatalf("decoded private lyrics response lost content: lines=%d", len(decoded.Lines))
	}
}

func TestLyricsAPIOrdinarySaveRejectsManagedSourceURLWithoutImportGrant(t *testing.T) {
	h := setupLegacyAPI(t)
	seedLyricsCatalog(t, h)
	for _, sourceURL := range []string{
		"https://vocaloid.fandom.com/wiki/Song",
		"https://vocaloid.fandom.com/wiki/Song?oldid=123",
		"https://vocaloid.wikia.com/wiki/Song",
		"HTTP://VOCALOID.WIKIA.COM./wiki/Song",
	} {
		draft := apiLyrics()
		draft.SourceURL = sourceURL
		response := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", draft)
		defer response.Body.Close()
		if response.StatusCode != http.StatusUnprocessableEntity {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("managed source URL %q status=%d body=%s", sourceURL, response.StatusCode, body)
		}
		var contract struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(response.Body).Decode(&contract); err != nil || contract.Error != "source_drift" {
			t.Fatalf("managed source URL %q body=%+v err=%v", sourceURL, contract, err)
		}
	}

	external := apiLyrics()
	external.SourceURL = "https://projectsekai.fandom.com/wiki/Song"
	response := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", external)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("other Fandom source status=%d body=%s", response.StatusCode, body)
	}
}

func TestLyricsAPIConflictAndValidationShapes(t *testing.T) {
	h := setupLegacyAPI(t)
	seedLyricsCatalog(t, h)
	first := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", apiLyrics())
	first.Body.Close()

	stale := apiLyrics()
	stale.Lines[0].English = "changed"
	conflict := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", stale)
	defer conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d", conflict.StatusCode)
	}
	var conflictBody struct {
		Error   string           `json:"error"`
		Current model.SongLyrics `json:"current"`
	}
	if err := json.NewDecoder(conflict.Body).Decode(&conflictBody); err != nil {
		t.Fatal(err)
	}
	if conflictBody.Error != "revision_conflict" || conflictBody.Current.Revision != 1 {
		t.Fatalf("conflict body = %+v", conflictBody)
	}

	mismatch := apiLyrics()
	mismatch.MusicID = 20
	mismatch.Lines[0].Segments[1].Text = "wrong"
	invalid := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", mismatch)
	defer invalid.Body.Close()
	if invalid.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("validation status = %d", invalid.StatusCode)
	}
	var validation struct {
		Error   string   `json:"error"`
		Details []string `json:"details"`
	}
	if err := json.NewDecoder(invalid.Body).Decode(&validation); err != nil {
		t.Fatal(err)
	}
	if validation.Error != "segment_mismatch" || len(validation.Details) == 0 {
		t.Fatalf("validation body = %+v", validation)
	}
}

func TestLyricsSourcePreviewContract(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "初音ミク"}}); err != nil {
		t.Fatal(err)
	}
	h.api.lyricsSrc = fakeLyricsSource{
		candidates: []lyricssource.Candidate{{PageID: 12, Title: "新曲", RevisionID: 34, SHA1: sourceSHA1}},
		preview: lyricssource.Preview{
			CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
			FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
		},
	}
	search := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/source/search?musicId=10", nil)
	defer search.Body.Close()
	if search.StatusCode != http.StatusOK {
		t.Fatalf("source search status = %d", search.StatusCode)
	}
	var candidates struct {
		Items []lyricssource.Candidate `json:"items"`
	}
	if err := json.NewDecoder(search.Body).Decode(&candidates); err != nil {
		t.Fatal(err)
	}
	if len(candidates.Items) != 1 || candidates.Items[0].PageID != 12 {
		t.Fatalf("source candidates = %+v", candidates)
	}
	preview := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
		"musicId": 10, "pageId": 12, "revisionId": 34,
	})
	defer preview.Body.Close()
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("source preview status = %d", preview.StatusCode)
	}
	var result lyricssource.Preview
	if err := json.NewDecoder(preview.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.PageID != 12 || result.RevisionID != 34 || result.SHA1 != sourceSHA1 ||
		len(result.Lines) != 1 || result.Lines[0].Japanese != "歌詞" || result.ImportToken == "" {
		t.Fatalf("source preview = %+v", result)
	}
	draft := apiLyrics()
	draft.SourceURL = result.CanonicalURL
	draft.SourcePageID = result.PageID
	draft.SourceRevisionID = result.RevisionID
	draft.SourceSHA1 = result.SHA1
	draft.SourceFetchedAt = result.FetchedAt
	draft.Lines = []model.LyricLine{{
		ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞", Chinese: "", English: "",
		Segments: []model.LyricSegment{{Text: "歌詞", PerformerIDs: []int{}, Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}},
	}}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var importDraft map[string]any
	if err := json.Unmarshal(payload, &importDraft); err != nil {
		t.Fatal(err)
	}
	importDraft["sourceImportToken"] = result.ImportToken
	save := strictLyricsSave(t, h, h.token, importDraft)
	defer save.Body.Close()
	if save.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(save.Body)
		t.Fatalf("source-preview draft save status=%d body=%s", save.StatusCode, body)
	}
	var saved model.SongLyrics
	if err := json.NewDecoder(save.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.SourceURL != result.CanonicalURL || saved.SourceRevisionID != result.RevisionID {
		t.Fatalf("saved source provenance = %+v", saved)
	}
	h.api.lyricsImportMu.Lock()
	_, tokenStillPresent := h.api.lyricsImports[result.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if tokenStillPresent {
		t.Fatal("successful imported save did not consume its one-time import grant")
	}
	replay := strictLyricsSave(t, h, h.token, importDraft)
	replay.Body.Close()
	if replay.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("consumed import token replay status = %d", replay.StatusCode)
	}
	frozen := saved
	frozen.SourceRevisionID++
	frozen.Lines[0].ID = "replacement-line"
	frozen.Lines[0].Japanese = "別の歌詞"
	frozen.Lines[0].Segments[0].Text = "別の歌詞"
	frozenSave := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", frozen)
	defer frozenSave.Body.Close()
	if frozenSave.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("saved source identity was not frozen, status=%d", frozenSave.StatusCode)
	}
	var auditCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE user='alice' AND action IN ('lyrics.source.search', 'lyrics.source.preview')`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 2 {
		t.Fatalf("source audit count = %d", auditCount)
	}
}

func TestLyricsSourcePreviewRejectsMismatchedRequestedIdentityBeforeGrant(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	for name, mutate := range map[string]func(*lyricssource.Preview){
		"page":     func(preview *lyricssource.Preview) { preview.PageID++ },
		"revision": func(preview *lyricssource.Preview) { preview.RevisionID++ },
	} {
		t.Run(name, func(t *testing.T) {
			h := setupLegacyAPI(t)
			if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
				MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
			}}); err != nil {
				t.Fatal(err)
			}
			preview := lyricssource.Preview{
				CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
				FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
			}
			mutate(&preview)
			h.api.lyricsSrc = fakeLyricsSource{preview: preview}
			response := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
				"musicId": 10, "pageId": 12, "revisionId": 34,
			})
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadGateway {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("mismatched %s identity status=%d body=%s", name, response.StatusCode, body)
			}
			h.api.lyricsImportMu.Lock()
			grantCount := len(h.api.lyricsImports)
			h.api.lyricsImportMu.Unlock()
			if grantCount != 0 {
				t.Fatalf("mismatched %s identity issued %d grants", name, grantCount)
			}
			var auditCount int
			if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.source.preview'`).Scan(&auditCount); err != nil {
				t.Fatal(err)
			}
			if auditCount != 0 {
				t.Fatalf("mismatched %s identity recorded %d preview audits", name, auditCount)
			}
		})
	}
}

func TestLyricsSourcePreviewRejectsUnsafeCanonicalURLBeforeGrant(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	for name, canonicalURL := range map[string]string{
		"external origin":          "https://example.invalid/source?oldid=34",
		"managed subdomain":        "https://vocaloid.fandom.com.example.invalid/wiki/Song?oldid=34",
		"managed trailing dot":     "https://vocaloid.fandom.com./wiki/Song?oldid=34",
		"managed HTTP":             "http://vocaloid.fandom.com/wiki/Song?oldid=34",
		"managed non-default port": "https://vocaloid.fandom.com:444/wiki/Song?oldid=34",
		"managed missing oldid":    "https://vocaloid.fandom.com/wiki/Song",
		"managed mismatched oldid": "https://vocaloid.fandom.com/wiki/Song?oldid=35",
		"managed duplicate oldid":  "https://vocaloid.fandom.com/wiki/Song?oldid=34&oldid=34",
		"managed extra query":      "https://vocaloid.fandom.com/wiki/Song?oldid=34&diff=prev",
		"managed fragment":         "https://vocaloid.fandom.com/wiki/Song?oldid=34#Lyrics",
		"legacy HTTP":              "http://vocaloid.wikia.com/wiki/Song?oldid=34",
		"legacy non-default port":  "https://vocaloid.wikia.com:444/wiki/Song?oldid=34",
		"credential URL":           "https://user:secret@vocaloid.fandom.com/wiki/Song?oldid=34",
	} {
		t.Run(name, func(t *testing.T) {
			h := setupLegacyAPI(t)
			if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
				MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
			}}); err != nil {
				t.Fatal(err)
			}
			h.api.lyricsSrc = fakeLyricsSource{preview: lyricssource.Preview{
				CanonicalURL: canonicalURL, PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
				FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
			}}
			response := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
				"musicId": 10, "pageId": 12, "revisionId": 34,
			})
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadGateway {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("unsafe canonical URL %q status=%d body=%s", canonicalURL, response.StatusCode, body)
			}
			h.api.lyricsImportMu.Lock()
			grantCount := len(h.api.lyricsImports)
			h.api.lyricsImportMu.Unlock()
			if grantCount != 0 {
				t.Fatalf("unsafe canonical URL %q issued %d grants", canonicalURL, grantCount)
			}
			var auditCount int
			if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.source.preview'`).Scan(&auditCount); err != nil {
				t.Fatal(err)
			}
			if auditCount != 0 {
				t.Fatalf("unsafe canonical URL %q recorded %d preview audits", canonicalURL, auditCount)
			}
		})
	}
}

func TestLyricsSourcePreviewRejectsNoncanonicalFakeSHA1BeforeGrant(t *testing.T) {
	for name, sha1 := range map[string]string{
		"opaque":    "source-sha1",
		"uppercase": "0123456789ABCDEF0123456789ABCDEF01234567",
		"non-hex":   "g123456789abcdef0123456789abcdef01234567",
	} {
		t.Run(name, func(t *testing.T) {
			h := setupLegacyAPI(t)
			if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
				MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
			}}); err != nil {
				t.Fatal(err)
			}
			h.api.lyricsSrc = fakeLyricsSource{preview: lyricssource.Preview{
				CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sha1,
				FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
			}}
			response := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
				"musicId": 10, "pageId": 12, "revisionId": 34,
			})
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadGateway {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("noncanonical fake SHA1 status=%d body=%s", response.StatusCode, body)
			}
			h.api.lyricsImportMu.Lock()
			grantCount := len(h.api.lyricsImports)
			h.api.lyricsImportMu.Unlock()
			if grantCount != 0 {
				t.Fatalf("noncanonical fake SHA1 issued %d grants", grantCount)
			}
		})
	}
}

func TestLyricsSaveRejectsUnverifiedOrTamperedSourceProvenance(t *testing.T) {
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
	draft := apiLyrics()
	draft.SourceURL = "https://vocaloid.fandom.com/wiki/Song?oldid=34"
	draft.SourcePageID = 12
	draft.SourceRevisionID = 34
	draft.SourceSHA1 = sourceSHA1
	draft.SourceFetchedAt = "2026-07-22T12:00:00Z"
	draft.Lines = []model.LyricLine{{ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞", Segments: []model.LyricSegment{{Text: "歌詞", Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}}}}
	unverified := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", draft)
	unverified.Body.Close()
	if unverified.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unverified source save status = %d", unverified.StatusCode)
	}
	for _, fetchedAt := range []string{"1970-01-01T00:00:00Z", "1969-12-31T23:59:59Z"} {
		bypass := draft
		bypass.SourceFetchedAt = fetchedAt
		response := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/lyrics/save", bypass)
		response.Body.Close()
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("unverified source save with sourceFetchedAt=%q status = %d", fetchedAt, response.StatusCode)
		}
	}

	previewResponse := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
		"musicId": 10, "pageId": 12, "revisionId": 34,
	})
	defer previewResponse.Body.Close()
	var preview lyricssource.Preview
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var tampered map[string]any
	if err := json.Unmarshal(payload, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["sourceImportToken"] = preview.ImportToken
	tamperedLines := tampered["lines"].([]any)
	tamperedLines[0].(map[string]any)["japanese"] = "改ざん"
	tamperedSave := strictLyricsSave(t, h, h.token, tampered)
	tamperedSave.Body.Close()
	if tamperedSave.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("tampered source save status = %d", tamperedSave.StatusCode)
	}
	h.api.lyricsImportMu.Lock()
	_, tamperedGrantStillPresent := h.api.lyricsImports[preview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if tamperedGrantStillPresent {
		t.Fatal("deterministic source drift did not consume import grant")
	}

	noncanonicalPreviewResponse := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
		"musicId": 10, "pageId": 12, "revisionId": 34,
	})
	defer noncanonicalPreviewResponse.Body.Close()
	var noncanonicalPreview lyricssource.Preview
	if err := json.NewDecoder(noncanonicalPreviewResponse.Body).Decode(&noncanonicalPreview); err != nil {
		t.Fatal(err)
	}
	payload, err = json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var noncanonical map[string]any
	if err := json.Unmarshal(payload, &noncanonical); err != nil {
		t.Fatal(err)
	}
	noncanonical["sourceImportToken"] = noncanonicalPreview.ImportToken
	noncanonical["sourceSha1"] = "source-sha1"
	noncanonicalSave := strictLyricsSave(t, h, h.token, noncanonical)
	noncanonicalSave.Body.Close()
	if noncanonicalSave.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("verified import with noncanonical sourceSha1 status = %d", noncanonicalSave.StatusCode)
	}
	h.api.lyricsImportMu.Lock()
	_, noncanonicalGrantStillPresent := h.api.lyricsImports[noncanonicalPreview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if noncanonicalGrantStillPresent {
		t.Fatal("deterministic noncanonical source drift did not consume import grant")
	}
}

func TestLyricsImportGrantRejectsCatalogIdentityDriftAfterPreview(t *testing.T) {
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
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "改名曲", IsNewlyWrittenMusic: true, ProducerMetadata: "new producer",
	}}); err != nil {
		t.Fatal(err)
	}
	draft := apiLyrics()
	draft.SourceURL = preview.CanonicalURL
	draft.SourcePageID = preview.PageID
	draft.SourceRevisionID = preview.RevisionID
	draft.SourceSHA1 = preview.SHA1
	draft.SourceFetchedAt = preview.FetchedAt
	draft.Lines = []model.LyricLine{{ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞", Segments: []model.LyricSegment{{Text: "歌詞", Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}}}}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	request["sourceImportToken"] = preview.ImportToken
	response := strictLyricsSave(t, h, h.token, request)
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("catalog-drift import status=%d body=%s", response.StatusCode, body)
	}
	var contract struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&contract); err != nil || contract.Error != "source_drift" {
		t.Fatalf("catalog-drift body=%+v err=%v", contract, err)
	}
	h.api.lyricsImportMu.Lock()
	_, present := h.api.lyricsImports[preview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if present {
		t.Fatal("catalog identity drift did not consume import grant")
	}
}

func TestLyricsImportValidationFailureReleasesGrantForRetry(t *testing.T) {
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
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	draft := apiLyrics()
	draft.SourceURL = preview.CanonicalURL
	draft.SourcePageID = preview.PageID
	draft.SourceRevisionID = preview.RevisionID
	draft.SourceSHA1 = preview.SHA1
	draft.SourceFetchedAt = preview.FetchedAt
	draft.Lines = []model.LyricLine{{
		ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞",
		Segments: []model.LyricSegment{{Text: "歌詞", PerformerIDs: []int{999}, Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}},
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
	invalid := strictLyricsSave(t, h, h.token, request)
	invalid.Body.Close()
	if invalid.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("retryable validation status = %d", invalid.StatusCode)
	}
	h.api.lyricsImportMu.Lock()
	released, present := h.api.lyricsImports[preview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if !present || released.claimID != "" {
		t.Fatalf("validation failure did not release grant: present=%v grant=%+v", present, released)
	}
	requestLines := request["lines"].([]any)
	segments := requestLines[0].(map[string]any)["segments"].([]any)
	segments[0].(map[string]any)["performerIds"] = []any{}
	retry := strictLyricsSave(t, h, h.token, request)
	defer retry.Body.Close()
	if retry.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(retry.Body)
		t.Fatalf("released validation retry status=%d body=%s", retry.StatusCode, body)
	}
}

func TestLyricsImportPostSaveCommitInvariantIsReportedWithoutFalseFailure(t *testing.T) {
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
	if previewResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(previewResponse.Body)
		t.Fatalf("preview status=%d body=%s", previewResponse.StatusCode, body)
	}
	var preview lyricssource.Preview
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	draft := apiLyrics()
	draft.SourceURL = preview.CanonicalURL
	draft.SourcePageID = preview.PageID
	draft.SourceRevisionID = preview.RevisionID
	draft.SourceSHA1 = preview.SHA1
	draft.SourceFetchedAt = preview.FetchedAt
	draft.Lines = []model.LyricLine{{
		ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞", Segments: []model.LyricSegment{{Text: "歌詞", Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}},
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

	var reports []string
	h.api.lyricsInvariantReporter = func(format string, args ...any) {
		reports = append(reports, fmt.Sprintf(format, args...))
	}
	corruptClaimOnCommit := make(chan struct{})
	h.store.OnChange(func() {
		h.api.lyricsImportMu.Lock()
		if grant, ok := h.api.lyricsImports[preview.ImportToken]; ok {
			grant.claimID = "unexpected-claim-owner"
			h.api.lyricsImports[preview.ImportToken] = grant
		}
		h.api.lyricsImportMu.Unlock()
		close(corruptClaimOnCommit)
	})

	response := strictLyricsSave(t, h, h.token, request)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("post-commit invariant response status=%d body=%s", response.StatusCode, body)
	}
	select {
	case <-corruptClaimOnCommit:
	default:
		t.Fatal("post-commit invariant hook did not run")
	}
	var saved model.SongLyrics
	if err := json.NewDecoder(response.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 {
		t.Fatalf("post-commit invariant response revision=%d", saved.Revision)
	}
	loaded, err := h.store.GetLyrics(10)
	if err != nil || loaded.Revision != 1 {
		t.Fatalf("post-commit invariant durable lyrics=%+v err=%v", loaded, err)
	}
	h.api.lyricsImportMu.Lock()
	_, replayable := h.api.lyricsImports[preview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if replayable {
		t.Fatal("post-commit invariant left the import token replayable")
	}
	if len(reports) != 0 {
		t.Fatalf("post-commit hook ran before grant consumption: reports=%v", reports)
	}
}

func TestLyricsImportGrantIsConsumedBeforeBlockingChangeHook(t *testing.T) {
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
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	draft := apiLyrics()
	draft.SourceURL = preview.CanonicalURL
	draft.SourcePageID = preview.PageID
	draft.SourceRevisionID = preview.RevisionID
	draft.SourceSHA1 = preview.SHA1
	draft.SourceFetchedAt = preview.FetchedAt
	draft.Lines = []model.LyricLine{{ID: "source-1", Order: 0, Japanese: "歌詞", Segments: []model.LyricSegment{{Text: "歌詞", Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}}}}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	request["sourceImportToken"] = preview.ImportToken
	hookStarted := make(chan struct{})
	allowHook := make(chan struct{})
	h.store.OnChange(func() {
		close(hookStarted)
		<-allowHook
	})
	responseCh := make(chan *http.Response, 1)
	go func() { responseCh <- strictLyricsSave(t, h, h.token, request) }()
	<-hookStarted
	h.api.lyricsImportMu.Lock()
	_, present := h.api.lyricsImports[preview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if present {
		close(allowHook)
		response := <-responseCh
		response.Body.Close()
		t.Fatal("blocking change hook retained a committed import grant")
	}
	close(allowHook)
	response := <-responseCh
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("blocking hook save status=%d body=%s", response.StatusCode, body)
	}
}

func TestLyricsImportDatabaseFailureReleasesGrantForRetry(t *testing.T) {
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
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	draft := apiLyrics()
	draft.SourceURL = preview.CanonicalURL
	draft.SourcePageID = preview.PageID
	draft.SourceRevisionID = preview.RevisionID
	draft.SourceSHA1 = preview.SHA1
	draft.SourceFetchedAt = preview.FetchedAt
	draft.Lines = []model.LyricLine{{ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞", Segments: []model.LyricSegment{{Text: "歌詞", Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}}}}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	request["sourceImportToken"] = preview.ImportToken
	if _, err := h.db.Exec(`CREATE TRIGGER fail_imported_lyrics_audit BEFORE INSERT ON audit_log
		WHEN NEW.action='lyrics.save' BEGIN SELECT RAISE(ABORT, 'import save failed'); END`); err != nil {
		t.Fatal(err)
	}
	failed := strictLyricsSave(t, h, h.token, request)
	failed.Body.Close()
	if failed.StatusCode != http.StatusInternalServerError {
		t.Fatalf("database failure status = %d", failed.StatusCode)
	}
	h.api.lyricsImportMu.Lock()
	released, present := h.api.lyricsImports[preview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if !present || released.claimID != "" {
		t.Fatalf("database failure did not release grant: present=%v grant=%+v", present, released)
	}
	if _, err := h.db.Exec(`DROP TRIGGER fail_imported_lyrics_audit`); err != nil {
		t.Fatal(err)
	}
	retry := strictLyricsSave(t, h, h.token, request)
	defer retry.Body.Close()
	if retry.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(retry.Body)
		t.Fatalf("database retry status=%d body=%s", retry.StatusCode, body)
	}
}

func TestLyricsImportStoreSourceDriftConsumesGrant(t *testing.T) {
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
	if previewResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(previewResponse.Body)
		t.Fatalf("source preview status=%d body=%s", previewResponse.StatusCode, body)
	}
	var preview lyricssource.Preview
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}

	// Removing the catalog identity after preview forces the store to return a
	// deterministic source_drift rather than a correctable draft validation
	// error. The capability must be consumed, not released for retry.
	if _, err := h.db.Exec(`DELETE FROM catalog_music WHERE music_id=10`); err != nil {
		t.Fatal(err)
	}
	draft := apiLyrics()
	draft.SourceURL = preview.CanonicalURL
	draft.SourcePageID = preview.PageID
	draft.SourceRevisionID = preview.RevisionID
	draft.SourceSHA1 = preview.SHA1
	draft.SourceFetchedAt = preview.FetchedAt
	draft.Lines = []model.LyricLine{{
		ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞", Segments: []model.LyricSegment{{Text: "歌詞", Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}},
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
	failed := strictLyricsSave(t, h, h.token, request)
	defer failed.Body.Close()
	if failed.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(failed.Body)
		t.Fatalf("store source_drift status=%d body=%s", failed.StatusCode, body)
	}
	var contract struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(failed.Body).Decode(&contract); err != nil {
		t.Fatal(err)
	}
	if contract.Error != "source_drift" {
		t.Fatalf("store source drift error=%q", contract.Error)
	}
	h.api.lyricsImportMu.Lock()
	_, present := h.api.lyricsImports[preview.ImportToken]
	h.api.lyricsImportMu.Unlock()
	if present {
		t.Fatal("store source_drift released rather than consumed the import grant")
	}
}

func TestConcurrentLyricsImportGrantsForSameMusicHaveOneWinner(t *testing.T) {
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

	previewToken := func() string {
		t.Helper()
		response := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
			"musicId": 10, "pageId": 12, "revisionId": 34,
		})
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("preview status=%d body=%s", response.StatusCode, body)
		}
		var preview lyricssource.Preview
		if err := json.NewDecoder(response.Body).Decode(&preview); err != nil {
			t.Fatal(err)
		}
		return preview.ImportToken
	}
	payload := func(token, lineID string) map[string]any {
		draft := apiLyrics()
		draft.SourceURL = "https://vocaloid.fandom.com/wiki/Song?oldid=34"
		draft.SourcePageID = 12
		draft.SourceRevisionID = 34
		draft.SourceSHA1 = sourceSHA1
		draft.SourceFetchedAt = "2026-07-22T12:00:00Z"
		draft.Lines = []model.LyricLine{{
			ID: lineID, Order: 0, Japanese: "歌詞", Segments: []model.LyricSegment{{Text: "歌詞", Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}},
		}}
		body, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		result["sourceImportToken"] = token
		return result
	}

	tokens := []string{previewToken(), previewToken()}
	start := make(chan struct{})
	type importResponse struct {
		status int
		body   string
	}
	responses := make(chan importResponse, len(tokens))
	var wg sync.WaitGroup
	for index, token := range tokens {
		wg.Add(1)
		go func(index int, token string) {
			defer wg.Done()
			<-start
			response := strictLyricsSave(t, h, h.token, payload(token, fmt.Sprintf("wiki-12-34-%d", index+1)))
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			responses <- importResponse{status: response.StatusCode, body: string(body)}
		}(index, token)
	}
	close(start)
	wg.Wait()
	close(responses)

	successes, rejected := 0, 0
	var responseBodies []string
	for response := range responses {
		responseBodies = append(responseBodies, response.body)
		switch response.status {
		case http.StatusOK:
			successes++
		case http.StatusUnprocessableEntity:
			rejected++
		default:
			t.Fatalf("unexpected concurrent import status=%d bodies=%v", response.status, responseBodies)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("concurrent imports successes=%d rejected=%d bodies=%v", successes, rejected, responseBodies)
	}
	loaded, err := h.store.GetLyrics(10)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 1 || len(loaded.Lines) != 1 || (loaded.Lines[0].ID != "wiki-12-34-1" && loaded.Lines[0].ID != "wiki-12-34-2") {
		t.Fatalf("concurrent imported document = %+v", loaded)
	}
	h.api.lyricsImportMu.Lock()
	remaining := len(h.api.lyricsImports)
	h.api.lyricsImportMu.Unlock()
	if remaining != 0 {
		t.Fatalf("deterministically ineligible concurrent import retained %d grants", remaining)
	}
}

func TestLyricsImportGrantIsFirstSaveOnlyAndBoundToUserMusicAndTTL(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{
		{MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer"},
		{MusicID: 11, JapaneseTitle: "別曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer"},
	}); err != nil {
		t.Fatal(err)
	}
	h.api.lyricsSrc = fakeLyricsSource{preview: lyricssource.Preview{
		CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
		FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
	}}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "初音ミク"}}); err != nil {
		t.Fatal(err)
	}
	newAdmin, err := h.api.auth.CreateUser("other-admin", "strong-password-456", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, _, err := h.api.auth.IssueToken(newAdmin)
	if err != nil {
		t.Fatal(err)
	}

	previewToken := func() string {
		t.Helper()
		response := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
			"musicId": 10, "pageId": 12, "revisionId": 34,
		})
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("preview status = %d", response.StatusCode)
		}
		var preview lyricssource.Preview
		if err := json.NewDecoder(response.Body).Decode(&preview); err != nil {
			t.Fatal(err)
		}
		return preview.ImportToken
	}
	payload := func(token string, musicID, revision int) map[string]any {
		t.Helper()
		draft := apiLyrics()
		draft.MusicID = musicID
		draft.Revision = revision
		draft.SourceURL = "https://vocaloid.fandom.com/wiki/Song?oldid=34"
		draft.SourcePageID = 12
		draft.SourceRevisionID = 34
		draft.SourceSHA1 = sourceSHA1
		draft.SourceFetchedAt = "2026-07-22T12:00:00Z"
		draft.Lines = []model.LyricLine{{
			ID: "wiki-12-34-1", Order: 0, Japanese: "歌詞",
			Segments: []model.LyricSegment{{Text: "歌詞", Ruby: []model.LyricRubySpan{{Text: "歌詞"}}}},
		}}
		body, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		result["sourceImportToken"] = token
		return result
	}
	assertRejected := func(name, token string, musicID, revision int, bearer string) {
		t.Helper()
		response := strictLyricsSave(t, h, bearer, payload(token, musicID, revision))
		defer response.Body.Close()
		if response.StatusCode != http.StatusUnprocessableEntity {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("%s status=%d body=%s", name, response.StatusCode, body)
		}
	}

	assertRejected("unknown token", "unknown-import-token", 10, 0, h.token)
	assertRejected("cross user", previewToken(), 10, 0, otherToken)
	assertRejected("cross music", previewToken(), 11, 0, h.token)

	expired := previewToken()
	h.api.lyricsImportMu.Lock()
	grant := h.api.lyricsImports[expired]
	grant.expiresAt = time.Now().Add(-time.Second)
	h.api.lyricsImports[expired] = grant
	h.api.lyricsImportMu.Unlock()
	assertRejected("expired", expired, 10, 0, h.token)

	firstSaveOnly := previewToken()
	assertRejected("nonzero revision", firstSaveOnly, 10, 1, h.token)
	h.api.lyricsImportMu.Lock()
	_, grantStillPresent := h.api.lyricsImports[firstSaveOnly]
	h.api.lyricsImportMu.Unlock()
	if grantStillPresent {
		t.Fatal("deterministic nonzero-revision rejection did not consume import grant")
	}

	_, err = h.store.SaveLyrics(apiLyrics(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	existingPreview := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
		"musicId": 10, "pageId": 12, "revisionId": 34,
	})
	defer existingPreview.Body.Close()
	if existingPreview.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(existingPreview.Body)
		t.Fatalf("existing-document preview status=%d body=%s", existingPreview.StatusCode, body)
	}
}

func TestLyricsSourcePreviewRejectsDocumentCreatedDuringFetchWithoutGrant(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{
		PerformerID: 1, JapaneseName: "初音ミク",
	}}); err != nil {
		t.Fatal(err)
	}
	h.api.lyricsSrc = fakeLyricsSource{previewFn: func(context.Context, lyricssource.MusicIdentity, int, int) (lyricssource.Preview, error) {
		if _, err := h.store.SaveLyrics(apiLyrics(), "alice"); err != nil {
			t.Fatal(err)
		}
		return lyricssource.Preview{
			CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
			FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
		}, nil
	}}
	response := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
		"musicId": 10, "pageId": 12, "revisionId": 34,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("document-raced preview status=%d body=%s", response.StatusCode, body)
	}
	var contract struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&contract); err != nil {
		t.Fatal(err)
	}
	if contract.Error != "source_drift" {
		t.Fatalf("document-raced preview error=%q", contract.Error)
	}
	h.api.lyricsImportMu.Lock()
	grantCount := len(h.api.lyricsImports)
	h.api.lyricsImportMu.Unlock()
	if grantCount != 0 {
		t.Fatalf("document-raced preview issued %d grants", grantCount)
	}
	var auditCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.source.preview'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 0 {
		t.Fatalf("document-raced preview recorded %d preview audits", auditCount)
	}
}

func TestLyricsSourcePreviewRejectsCatalogIdentityChangeDuringFetch(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
	}}); err != nil {
		t.Fatal(err)
	}
	h.api.lyricsSrc = fakeLyricsSource{previewFn: func(_ context.Context, identity lyricssource.MusicIdentity, _, _ int) (lyricssource.Preview, error) {
		if identity.JapaneseTitle != "新曲" || identity.ProducerMetadata != "producer" {
			t.Fatalf("preview identity=%+v", identity)
		}
		if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
			MusicID: 10, JapaneseTitle: "改名曲", IsNewlyWrittenMusic: true, ProducerMetadata: "new producer",
		}}); err != nil {
			t.Fatal(err)
		}
		return lyricssource.Preview{
			CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
			FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
		}, nil
	}}
	response := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
		"musicId": 10, "pageId": 12, "revisionId": 34,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("catalog-raced preview status=%d body=%s", response.StatusCode, body)
	}
	var contract struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&contract); err != nil {
		t.Fatal(err)
	}
	if contract.Error != "source_drift" {
		t.Fatalf("catalog-raced preview error=%q", contract.Error)
	}
	h.api.lyricsImportMu.Lock()
	grantCount := len(h.api.lyricsImports)
	h.api.lyricsImportMu.Unlock()
	if grantCount != 0 {
		t.Fatalf("catalog-raced preview issued %d grants", grantCount)
	}
}

func TestLyricsSourcePreviewRejectsProducerStateChangeDuringFetch(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
	}}); err != nil {
		t.Fatal(err)
	}
	h.api.lyricsSrc = fakeLyricsSource{previewFn: func(context.Context, lyricssource.MusicIdentity, int, int) (lyricssource.Preview, error) {
		release, err := h.api.editorGate.BeginProducer()
		if err != nil {
			t.Fatal(err)
		}
		release()
		return lyricssource.Preview{
			CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
			FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
		}, nil
	}}
	response := authorizedRequest(t, h, http.MethodPost, "/api/lyrics/source/preview", map[string]int{
		"musicId": 10, "pageId": 12, "revisionId": 34,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("producer-raced preview status=%d body=%s", response.StatusCode, body)
	}
	h.api.lyricsImportMu.Lock()
	grantCount := len(h.api.lyricsImports)
	h.api.lyricsImportMu.Unlock()
	if grantCount != 0 {
		t.Fatalf("producer-raced preview issued %d grants", grantCount)
	}
}

func TestLyricsImportGrantClaimReleaseCommitAndProducerBinding(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	status := h.api.editorGate.Status()
	preview := lyricssource.Preview{
		CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
		FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
	}
	token, err := h.api.issueLyricsImportGrant("alice", 10, preview, status)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := h.api.claimLyricsImportGrant(token, "alice", 10, status)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.api.claimLyricsImportGrant(token, "alice", 10, status); !errors.Is(err, errLyricsImportGrantBusy) {
		t.Fatalf("concurrent same-token claim error=%v", err)
	}
	if !h.api.releaseLyricsImportGrant(claim) {
		t.Fatal("release did not match active claim")
	}
	retry, err := h.api.claimLyricsImportGrant(token, "alice", 10, status)
	if err != nil {
		t.Fatalf("released grant was not retryable: %v", err)
	}
	if !h.api.commitLyricsImportGrant(retry) {
		t.Fatal("commit did not consume matching claim")
	}
	if _, err := h.api.claimLyricsImportGrant(token, "alice", 10, status); !errors.Is(err, errLyricsImportGrantInvalid) {
		t.Fatalf("committed grant replay error=%v", err)
	}

	producerToken, err := h.api.issueLyricsImportGrant("alice", 10, preview, status)
	if err != nil {
		t.Fatal(err)
	}
	releaseProducer, err := h.api.editorGate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	releaseProducer()
	if _, err := h.api.claimLyricsImportGrant(producerToken, "alice", 10, h.api.editorGate.Status()); !errors.Is(err, errLyricsImportGrantInvalid) {
		t.Fatalf("producer-mismatched claim error=%v", err)
	}
	h.api.lyricsImportMu.Lock()
	_, remains := h.api.lyricsImports[producerToken]
	h.api.lyricsImportMu.Unlock()
	if remains {
		t.Fatal("producer mismatch did not terminally consume grant")
	}
}

func TestLyricsImportGrantCapacityNeverEvictsClaimedEntries(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	status := h.api.editorGate.Status()
	preview := lyricssource.Preview{
		CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
		FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
	}
	claims := make([]lyricsImportClaim, 0, maxLyricsImportTokens)
	for index := 0; index < maxLyricsImportTokens; index++ {
		token, err := h.api.issueLyricsImportGrant("alice", 10, preview, status)
		if err != nil {
			t.Fatalf("issue grant %d: %v", index, err)
		}
		claim, err := h.api.claimLyricsImportGrant(token, "alice", 10, status)
		if err != nil {
			t.Fatalf("claim grant %d: %v", index, err)
		}
		claims = append(claims, claim)
	}
	if _, err := h.api.issueLyricsImportGrant("alice", 10, preview, status); !errors.Is(err, errLyricsImportCapacity) {
		t.Fatalf("all-claimed capacity error=%v", err)
	}
	h.api.lyricsImportMu.Lock()
	remaining := len(h.api.lyricsImports)
	h.api.lyricsImportMu.Unlock()
	if remaining != maxLyricsImportTokens {
		t.Fatalf("claimed capacity entries=%d, want %d", remaining, maxLyricsImportTokens)
	}
	if !h.api.releaseLyricsImportGrant(claims[0]) {
		t.Fatal("could not release claimed capacity entry")
	}
	if _, err := h.api.issueLyricsImportGrant("alice", 10, preview, status); err != nil {
		t.Fatalf("unclaimed capacity entry was not evictable: %v", err)
	}
}

func TestLyricsSourceFailureIsSanitized(t *testing.T) {
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", IsNewlyWrittenMusic: true, ProducerMetadata: "producer",
	}}); err != nil {
		t.Fatal(err)
	}
	h.api.lyricsSrc = fakeLyricsSource{err: errors.New("secret upstream response")}
	response := authorizedRequest(t, h, http.MethodGet, "/api/lyrics/source/search?musicId=10", nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "secret upstream response") ||
		!strings.Contains(string(body), "source_unavailable") {
		t.Fatalf("sanitized source failure status=%d body=%s", response.StatusCode, body)
	}
	for header, values := range response.Header {
		for _, value := range values {
			if strings.Contains(value, "secret upstream response") {
				t.Fatalf("upstream failure leaked through header %s: %q", header, value)
			}
		}
	}
}

func TestLyricsImportGrantCatalogCurrentIncludesCredits(t *testing.T) {
	const sourceSHA1 = "0123456789abcdef0123456789abcdef01234567"
	h := setupLegacyAPI(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", ProducerMetadata: "producer",
		Lyricist: "作詞", Composer: "作曲", Arranger: "編曲",
	}}); err != nil {
		t.Fatal(err)
	}
	status := h.api.editorGate.Status()
	preview := lyricssource.Preview{
		CanonicalURL: "https://vocaloid.fandom.com/wiki/Song?oldid=34", PageID: 12, RevisionID: 34, SHA1: sourceSHA1,
		FetchedAt: "2026-07-22T12:00:00Z", Lines: []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
	}
	identity := lyricssource.MusicIdentity{
		MusicID: 10, JapaneseTitle: "新曲", ProducerMetadata: "作詞 | 作曲 | 編曲",
		Lyricist: "作詞", Composer: "作曲", Arranger: "編曲",
		PerformerSegmentationPolicy: lyricssource.PerformerSegmentationDisabled,
	}
	token, err := h.api.issueLyricsImportGrant("alice", 10, preview, status, identity)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := h.api.claimLyricsImportGrant(token, "alice", 10, status)
	if err != nil {
		t.Fatal(err)
	}
	current, checked, err := h.api.lyricsImportGrantCatalogCurrent(claim)
	if err != nil || !checked || !current {
		t.Fatalf("credited catalog identity drifted: current=%v checked=%v err=%v", current, checked, err)
	}
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", ProducerMetadata: "producer",
		Lyricist: "作詞", Composer: "別人", Arranger: "編曲",
	}}); err != nil {
		t.Fatal(err)
	}
	current, checked, err = h.api.lyricsImportGrantCatalogCurrent(claim)
	if err != nil || !checked || current {
		t.Fatalf("composer drift was accepted: current=%v checked=%v err=%v", current, checked, err)
	}
}

func TestConsoleJSONPayloadIsBounded(t *testing.T) {
	h := setupLegacyAPI(t)
	body := `{"category":"cards","field":"prefix","key":"large","text":"` +
		strings.Repeat("x", maxJSONBodyBytes) + `","source":"human"}`
	request, err := http.NewRequest(http.MethodPut, h.server.URL+"/api/editor/v1/entry", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(loadedProducerStateHeader, loadedState(h.api.editorGate.Status()))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized payload status = %d", response.StatusCode)
	}
}
