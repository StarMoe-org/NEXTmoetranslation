package filesvc

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/store"
)

var sideStoryFilesNow = time.Unix(1790000000, 0)

type sideStoryFileFixture struct {
	svc   *Service
	store *store.Store
	gen   *files.Generator
}

// setupSideStoryFiles seeds test card 800 and test area talks in JP group 30
// with zh-CN and en-US lines on the card and zh-CN lines on the area talks.
func setupSideStoryFiles(t *testing.T) sideStoryFileFixture {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "side-story-files.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := store.New(database)
	es := store.NewEventStore(database)
	ctx := context.Background()
	if _, err := s.SyncSideStoryCatalogContext(ctx, store.SideStoryKindCard, []store.SideStoryCatalogStory{{
		Kind: "card", StoryID: "800", Title: "テストカード",
		Episodes: []store.SideStoryCatalogEpisode{{Key: "1", ScenarioID: "test_card_800_01", TitleJP: "テスト前編",
			JPAssetPath: "character/member/test_card_800/test_card_800_01"}},
	}}, sideStoryFilesNow); err != nil {
		t.Fatal(err)
	}
	area := func(id string, actionSetID int) store.SideStoryCatalogStory {
		return store.SideStoryCatalogStory{Kind: "area", StoryID: id, Title: "テストエリア", ActionSetID: actionSetID, AreaCategory: "grade1",
			Episodes: []store.SideStoryCatalogEpisode{{Key: "1", ScenarioID: id, JPAssetPath: "scenario/actionset/group30/" + id}}}
	}
	if _, err := s.SyncSideStoryCatalogContext(ctx, store.SideStoryKindArea, []store.SideStoryCatalogStory{
		area("areatalk_test_x", 3001), area("areatalk_test_y", 3002),
	}, sideStoryFilesNow); err != nil {
		t.Fatal(err)
	}
	for _, story := range [][3]string{{"card", "800", "test_card_800_01"}, {"area", "areatalk_test_x", "areatalk_test_x"}, {"area", "areatalk_test_y", "areatalk_test_y"}} {
		script, err := store.ParseSideStoryScript(map[string]any{
			"ScenarioId": story[2], "Snippets": []any{}, "SpecialEffectData": []any{}, "AppearCharacters": []any{},
			"TalkData": []any{map[string]any{"WindowDisplayName": "テスト話者", "Body": "テスト台詞" + story[1]}},
		}, story[2])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplySideStoryFetchesContext(ctx, []store.SideStoryEpisodeFetch{{
			Kind: story[0], StoryID: story[1], EpisodeKey: "1", JP: store.SideStoryFetchOutcome{Attempted: true, Script: &script},
		}}, sideStoryFilesNow); err != nil {
			t.Fatal(err)
		}
	}
	fixture := sideStoryFileFixture{store: s, gen: files.NewGenerator(s, es, "")}
	fixture.edit(t, "card", "800", "zh-CN", "テスト台詞800", "测试台词八百")
	fixture.edit(t, "card", "800", "en-US", "テスト話者", "Test speaker")
	fixture.edit(t, "area", "areatalk_test_x", "zh-CN", "テスト台詞areatalk_test_x", "测试区域台词")
	fixture.svc = New(s, es, fixture.gen)
	return fixture
}

func (f sideStoryFileFixture) edit(t *testing.T, kind, id, locale, jp, text string) {
	t.Helper()
	if _, err := f.store.UpdateSideStoryLinesContext(context.Background(), kind, id, "1", locale, "test-editor",
		[]store.SideStoryLineEdit{{JP: jp, Text: text}}, sideStoryFilesNow); err != nil {
		t.Fatal(err)
	}
}

func (f sideStoryFileFixture) get(t *testing.T, path string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if len(header) == 2 {
		request.Header.Set(header[0], header[1])
	}
	response := httptest.NewRecorder()
	f.svc.Handler().ServeHTTP(response, request)
	return response
}

func TestFullRebuildServesSideStoriesInZhCNAndEnUSOnly(t *testing.T) {
	f := setupSideStoryFiles(t)
	f.svc.Rebuild()
	_, wantCard, ok, err := f.gen.SideStoryFileForStoryJSON(context.Background(), "card", "800", "zh-CN")
	if err != nil || !ok {
		t.Fatalf("card projection ok=%v err=%v", ok, err)
	}
	response := f.get(t, "/files/translation/cardStory/card_800.json")
	body, _ := io.ReadAll(response.Body)
	if response.Code != http.StatusOK || string(body) != string(wantCard) {
		t.Fatalf("zh-CN card = %d %s", response.Code, body)
	}
	etag := response.Header().Get("ETag")
	if got := response.Header().Get("Cache-Control"); got != "public, max-age=300, stale-while-revalidate=3600" ||
		response.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		response.Header().Get("Access-Control-Allow-Origin") != "*" || len(etag) != 34 {
		t.Fatalf("side-story headers = %v", response.Header())
	}
	if revalidated := f.get(t, "/files/translation/cardStory/card_800.json", "If-None-Match", etag); revalidated.Code != http.StatusNotModified {
		t.Fatalf("conditional side-story request = %d", revalidated.Code)
	}
	english := f.get(t, "/files/v2/en-US/translation/cardStory/card_800.json")
	if english.Code != http.StatusOK || !strings.Contains(english.Body.String(), `"テスト話者": "Test speaker"`) {
		t.Fatalf("en-US card = %d %s", english.Code, english.Body)
	}
	if area := f.get(t, "/files/translation/areaTalk/group_30.json"); area.Code != http.StatusOK ||
		!strings.Contains(area.Body.String(), `"areatalk_test_x": {`) || strings.Contains(area.Body.String(), "areatalk_test_y") {
		t.Fatalf("zh-CN area group = %d %s", area.Code, area.Body)
	}
	for _, path := range []string{
		"/files/v2/en-US/translation/areaTalk/group_30.json",
		"/files/v2/zh-CN/translation/cardStory/card_800.json",
		"/files/v2/ja-JP/translation/cardStory/card_800.json",
		"/files/translation/cardStory/card_801.json",
	} {
		if missing := f.get(t, path); missing.Code != http.StatusNotFound {
			t.Fatalf("%s = %d", path, missing.Code)
		}
	}

	f.edit(t, "area", "areatalk_test_x", "zh-CN", "テスト台詞areatalk_test_x", "")
	f.svc.Rebuild()
	if dropped := f.get(t, "/files/translation/areaTalk/group_30.json"); dropped.Code != http.StatusNotFound {
		t.Fatalf("a group without translated lines is still served: %d", dropped.Code)
	}
	if kept := f.get(t, "/files/translation/cardStory/card_800.json"); kept.Code != http.StatusOK {
		t.Fatalf("card after the second rebuild = %d", kept.Code)
	}
}

func TestRebuildSideStoryRepublishesAndWithdrawsOneFile(t *testing.T) {
	f := setupSideStoryFiles(t)
	f.svc.Rebuild()
	f.edit(t, "card", "800", "zh-CN", "テスト台詞800", "测试台词八百改")
	f.edit(t, "card", "800", "en-US", "テスト話者", "")
	if err := f.svc.RebuildSideStoryContext(context.Background(), "card", "800"); err != nil {
		t.Fatal(err)
	}
	if card := f.get(t, "/files/translation/cardStory/card_800.json"); card.Code != http.StatusOK ||
		!strings.Contains(card.Body.String(), `"测试台词八百改"`) {
		t.Fatalf("zh-CN card after the incremental rebuild = %d %s", card.Code, card.Body)
	}
	if english := f.get(t, "/files/v2/en-US/translation/cardStory/card_800.json"); english.Code != http.StatusNotFound {
		t.Fatalf("en-US card without translated lines = %d", english.Code)
	}

	f.edit(t, "area", "areatalk_test_y", "zh-CN", "テスト台詞areatalk_test_y", "测试区域台词二")
	if err := f.svc.RebuildSideStory("area", "areatalk_test_y"); err != nil {
		t.Fatal(err)
	}
	group := f.get(t, "/files/translation/areaTalk/group_30.json").Body.String()
	if !strings.Contains(group, `"测试区域台词"`) || !strings.Contains(group, `"测试区域台词二"`) ||
		strings.Index(group, "areatalk_test_x") > strings.Index(group, "areatalk_test_y") {
		t.Fatalf("area group after the incremental rebuild = %s", group)
	}
	if err := f.svc.RebuildSideStory("card", "899"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown story rebuild error = %v", err)
	}
}

// sideStoryPassPause is the context of a full rebuild that stops when
// addSideStoryAssets checks it before the last locale pass: the zh-CN files
// have been read and nothing is swapped yet. The rebuild has no hook there;
// matching the caller keeps the database driver's context checks out of it.
type sideStoryPassPause struct {
	context.Context
	checks  atomic.Int32
	reached chan struct{}
	resume  chan struct{}
}

func (p *sideStoryPassPause) Err() error {
	pc := make([]uintptr, 1)
	if runtime.Callers(2, pc) == 1 {
		caller, _ := runtime.CallersFrames(pc).Next()
		if strings.HasSuffix(caller.Function, ".(*Service).addSideStoryAssets") && p.checks.Add(1) == int32(len(sideStoryRoots)) {
			close(p.reached)
			<-p.resume
		}
	}
	return p.Context.Err()
}

// pauseFullRebuild starts a full rebuild and returns once it has read the zh-CN
// side-story files; finish lets it swap and returns its error.
func pauseFullRebuild(t *testing.T, svc *Service) (finish func() error) {
	t.Helper()
	pause := &sideStoryPassPause{Context: context.Background(), reached: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- svc.rebuildAssetsContext(pause) }()
	var once sync.Once
	var err error
	finish = func() error {
		once.Do(func() {
			close(pause.resume)
			err = <-done
		})
		return err
	}
	t.Cleanup(func() { finish() })
	select {
	case <-pause.reached:
	case early := <-done:
		done <- early
		t.Fatalf("the full rebuild finished without reaching the side-story pass: %v", early)
	case <-time.After(10 * time.Second):
		t.Fatal("the full rebuild did not reach the side-story pass")
	}
	return finish
}

func TestFullRebuildKeepsASideStoryFileFirstPublishedDuringIt(t *testing.T) {
	f := setupSideStoryFiles(t)
	f.edit(t, "area", "areatalk_test_x", "zh-CN", "テスト台詞areatalk_test_x", "")
	f.svc.Rebuild()
	const group = "/files/translation/areaTalk/group_30.json"
	if before := f.get(t, group); before.Code != http.StatusNotFound {
		t.Fatalf("group before its first translated line = %d", before.Code)
	}
	f.edit(t, "card", "800", "zh-CN", "テスト台詞800", "测试台词八百改")

	finish := pauseFullRebuild(t, f.svc)
	f.edit(t, "area", "areatalk_test_x", "zh-CN", "テスト台詞areatalk_test_x", "测试区域台词")
	if err := f.svc.RebuildSideStoryContext(context.Background(), "area", "areatalk_test_x"); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if published := f.get(t, group); published.Code != http.StatusOK || !strings.Contains(published.Body.String(), `"测试区域台词"`) {
		t.Fatalf("group first published during the full rebuild = %d %s", published.Code, published.Body)
	}
	if card := f.get(t, "/files/translation/cardStory/card_800.json"); !strings.Contains(card.Body.String(), `"测试台词八百改"`) {
		t.Fatalf("the full rebuild was not swapped in: %d %s", card.Code, card.Body)
	}
}

func TestFullRebuildDoesNotRestoreASideStoryFileWithdrawnDuringIt(t *testing.T) {
	f := setupSideStoryFiles(t)
	f.svc.Rebuild()
	const group = "/files/translation/areaTalk/group_30.json"
	if before := f.get(t, group); before.Code != http.StatusOK {
		t.Fatalf("group before its withdrawal = %d", before.Code)
	}
	f.edit(t, "card", "800", "zh-CN", "テスト台詞800", "测试台词八百改")

	finish := pauseFullRebuild(t, f.svc)
	f.edit(t, "area", "areatalk_test_x", "zh-CN", "テスト台詞areatalk_test_x", "")
	if err := f.svc.RebuildSideStoryContext(context.Background(), "area", "areatalk_test_x"); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if restored := f.get(t, group); restored.Code != http.StatusNotFound {
		t.Fatalf("group withdrawn during the full rebuild = %d %s", restored.Code, restored.Body)
	}
	if card := f.get(t, "/files/translation/cardStory/card_800.json"); !strings.Contains(card.Body.String(), `"测试台词八百改"`) {
		t.Fatalf("the full rebuild was not swapped in: %d %s", card.Code, card.Body)
	}
}
