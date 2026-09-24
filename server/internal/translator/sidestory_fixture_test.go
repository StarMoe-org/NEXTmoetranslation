package translator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/store"
)

// Synthetic test data: every name, line and path below is invented.
const (
	testCardID         = "1001"
	testCardScenario1  = "test_card_1001_01"
	testCardScenario2  = "test_card_1001_02"
	testAreaScenario   = "test_area_talk_01"
	testJPCardPath1    = "/jp-scripts/character/member/res_test001/test_card_1001_01.json"
	testJPCardPath2    = "/jp-scripts/character/member/res_test001/test_card_1001_02.json"
	testCNCardPath1    = "/cn-scripts/character/member/res_test001/test_card_1001_01.json"
	testCNCardPath2    = "/cn-scripts/character/member/res_test001/test_card_1001_02.json"
	testENCardPath1    = "/en-scripts/character/member_scenario/res_test001_en/test_card_1001_01.json"
	testENCardPath2    = "/en-scripts/character/member_scenario/res_test001_en/test_card_1001_02.json"
	testJPAreaPath     = "/jp-scripts/scenario/actionset/group13/test_area_talk_01.json"
	testCNAreaPath     = "/cn-scripts/scenario/actionset/group12/test_area_talk_01.json"
	testENAreaPath     = "/en-scripts/scenario/actionset/group14/test_area_talk_01.json"
	testJPLine1        = "テスト台詞一です"
	testJPLine2        = "テスト台詞二です"
	testJPSpeaker      = "テスト話者"
	testCNLine1        = "测试台词一"
	testCNLine2        = "测试台词二"
	testCNSpeaker      = "测试说话人"
	testENLine1        = "Test line one"
	testENLine2        = "Test line two"
	testENSpeaker      = "Test Speaker"
	testJPEpisodeTitle = "テスト前編"
	testCNEpisodeTitle = "测试上篇"
	testENEpisodeTitle = "Test part one"
)

type sideStoryRequest struct {
	path string
	at   time.Time
}

// sideStoryUpstream serves JP/CN/EN masterdata and scripts from one test
// server; handlers override single paths and unknown paths answer 404.
type sideStoryUpstream struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	files    map[string]any
	handlers map[string]http.HandlerFunc
	requests []sideStoryRequest
}

func newSideStoryUpstream(t *testing.T) *sideStoryUpstream {
	t.Helper()
	upstream := &sideStoryUpstream{t: t, files: map[string]any{}, handlers: map[string]http.HandlerFunc{}}
	upstream.server = httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(upstream.server.Close)
	upstream.setDefaultFixture()
	return upstream
}

func (u *sideStoryUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.requests = append(u.requests, sideStoryRequest{path: r.URL.Path, at: time.Now()})
	handler := u.handlers[r.URL.Path]
	body, ok := u.files[r.URL.Path]
	u.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (u *sideStoryUpstream) set(path string, body any) {
	u.mu.Lock()
	u.files[path] = body
	u.mu.Unlock()
}

func (u *sideStoryUpstream) remove(path string) {
	u.mu.Lock()
	delete(u.files, path)
	u.mu.Unlock()
}

func (u *sideStoryUpstream) handle(path string, handler http.HandlerFunc) {
	u.mu.Lock()
	u.handlers[path] = handler
	u.mu.Unlock()
}

func (u *sideStoryUpstream) requested() []sideStoryRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]sideStoryRequest(nil), u.requests...)
}

func (u *sideStoryUpstream) count(prefix string) int {
	n := 0
	for _, request := range u.requested() {
		if strings.HasPrefix(request.path, prefix) {
			n++
		}
	}
	return n
}

func testScript(scenarioID string, talks ...[2]string) map[string]any {
	talkData := make([]any, 0, len(talks))
	for _, talk := range talks {
		talkData = append(talkData, map[string]any{"WindowDisplayName": talk[0], "Body": talk[1]})
	}
	return map[string]any{
		"ScenarioId": scenarioID, "Snippets": []any{}, "TalkData": talkData,
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}
}

func testJPScript(scenarioID string) map[string]any {
	return testScript(scenarioID, [2]string{testJPSpeaker, testJPLine1}, [2]string{testJPSpeaker, testJPLine2})
}

func testCNScript(scenarioID string) map[string]any {
	return testScript(scenarioID, [2]string{testCNSpeaker, testCNLine1}, [2]string{testCNSpeaker, testCNLine2})
}

func testENScript(scenarioID string) map[string]any {
	return testScript(scenarioID, [2]string{testENSpeaker, testENLine1}, [2]string{testENSpeaker, testENLine2})
}

func (u *sideStoryUpstream) setDefaultFixture() {
	u.set("/jp-master/cards.json", []any{
		map[string]any{"id": 1001, "characterId": 5, "prefix": "テスト用カード", "assetbundleName": "res_test001", "releaseAt": 1700000000000, "cardSkillName": "unused"},
		map[string]any{"id": 1002, "characterId": 6, "prefix": "片方だけのカード", "assetbundleName": "res_test002", "releaseAt": 1700000001000},
	})
	u.set("/jp-master/cardEpisodes.json", []any{
		map[string]any{"id": 1, "cardId": 1001, "seq": 2, "title": "テスト後編", "scenarioId": testCardScenario2, "assetbundleName": "res_test001"},
		map[string]any{"id": 2, "cardId": 1001, "seq": 1, "title": testJPEpisodeTitle, "scenarioId": testCardScenario1, "assetbundleName": "res_test001"},
		map[string]any{"id": 3, "cardId": 1002, "seq": 1, "title": testJPEpisodeTitle, "scenarioId": "test_card_1002_01", "assetbundleName": "res_test002"},
	})
	u.set("/jp-master/actionSets.json", []any{
		map[string]any{"id": 1300, "areaId": 5, "scenarioId": testAreaScenario, "releaseConditionId": 100001, "actionSetType": "normal", "archivePublishedAt": 1700000002000},
		map[string]any{"id": 1301, "areaId": 5, "scenarioId": "test_area_unlisted", "releaseConditionId": 5, "actionSetType": "normal"},
	})
	u.set("/jp-master/areas.json", []any{map[string]any{"id": 5, "name": "テスト広場", "subName": "テスト区画"}})
	u.set("/cn-master/cards.json", []any{map[string]any{"id": 1001, "assetbundleName": "res_test001"}})
	u.set("/cn-master/cardEpisodes.json", []any{
		map[string]any{"id": 1, "cardId": 1001, "seq": 2, "title": "测试下篇", "scenarioId": testCardScenario2},
		map[string]any{"id": 2, "cardId": 1001, "seq": 1, "title": testCNEpisodeTitle, "scenarioId": testCardScenario1},
	})
	u.set("/cn-master/actionSets.json", []any{map[string]any{"id": 1299, "areaId": 5, "scenarioId": testAreaScenario, "releaseConditionId": 100001}})
	u.set("/en-master/cardEpisodes.json", []any{
		map[string]any{"id": 1, "cardId": 1001, "seq": 1, "title": testENEpisodeTitle, "scenarioId": testCardScenario1, "assetbundleName": "res_test001_en"},
		map[string]any{"id": 2, "cardId": 1001, "seq": 2, "title": "Test part two", "scenarioId": testCardScenario2, "assetbundleName": "res_test001_en"},
	})
	u.set("/en-master/actionSets.json", []any{map[string]any{"id": 1400, "areaId": 5, "scenarioId": testAreaScenario, "releaseConditionId": 100001}})
	u.set(testJPCardPath1, testJPScript(testCardScenario1))
	u.set(testJPCardPath2, testJPScript(testCardScenario2))
	u.set(testCNCardPath1, testCNScript(testCardScenario1))
	u.set(testCNCardPath2, testCNScript(testCardScenario2))
	u.set(testENCardPath1, testENScript(testCardScenario1))
	u.set(testENCardPath2, testENScript(testCardScenario2))
	u.set(testJPAreaPath, testJPScript(testAreaScenario))
	u.set(testCNAreaPath, testCNScript(testAreaScenario))
	u.set(testENAreaPath, testENScript(testAreaScenario))
}

// configureSideStorySources points every masterdata and script source at the
// test server. Each fallback repeats its primary so the chain collapses to
// the local source instead of re-enabling a public default.
func configureSideStorySources(t *testing.T, cfg *config.Config, base string) {
	t.Helper()
	if _, err := cfg.SetMany(map[string]string{
		config.KeyUpstreamJPMasterdataURL: base + "/jp-master", config.KeyUpstreamJPMasterdataFallbackURL: base + "/jp-master",
		config.KeyUpstreamCNMasterdataURL: base + "/cn-master", config.KeyUpstreamCNMasterdataFallbackURL: base + "/cn-master",
		config.KeyUpstreamENMasterdataURL: base + "/en-master", config.KeyUpstreamENMasterdataFallbackURL: base + "/en-master",
		config.KeyUpstreamJPScriptsURL: base + "/jp-scripts", config.KeyUpstreamJPScriptsFallbackURL: base + "/jp-scripts",
		config.KeyUpstreamCNScriptsURL: base + "/cn-scripts", config.KeyUpstreamENScriptsURL: base + "/en-scripts",
	}); err != nil {
		t.Fatal(err)
	}
}

type sideStoryHarness struct {
	upstream *sideStoryUpstream
	store    *store.Store
	cfg      *config.Config
	gate     *editorgate.Gate
	tr       *Translator
	worker   *SideStoryBackfill
	clock    time.Time
	changes  int
	events   []string
	mu       sync.Mutex
}

func newSideStoryHarness(t *testing.T) *sideStoryHarness {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/side-story.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	cfg, err := config.New(database, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := &sideStoryHarness{upstream: newSideStoryUpstream(t), store: store.New(database), cfg: cfg, gate: editorgate.MustNew()}
	configureSideStorySources(t, cfg, h.upstream.server.URL)
	h.tr = New(h.store, nil, cfg, h.gate)
	h.store.OnChange(func() {
		h.mu.Lock()
		h.changes++
		h.mu.Unlock()
	})
	h.tr.SetProgress(func(stage, _ string, _, _ int) {
		h.mu.Lock()
		h.events = append(h.events, stage)
		h.mu.Unlock()
	})
	h.worker = NewSideStoryBackfill(h.tr, SideStoryBackfillOptions{Enabled: true, Interval: time.Hour, Batch: 30})
	h.clock = time.Unix(1_800_000_000, 0)
	h.worker.now = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.clock
	}
	return h
}

func (h *sideStoryHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
}

func (h *sideStoryHarness) round(t *testing.T) store.SideStoryBackfillState {
	t.Helper()
	h.worker.runRound(t.Context())
	return h.worker.SideStoryBackfillState()
}

func (h *sideStoryHarness) stages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func (h *sideStoryHarness) detail(t *testing.T, kind, id, locale string) store.SideStoryDetail {
	t.Helper()
	detail, err := h.store.SideStoryDetailContext(t.Context(), kind, id, locale)
	if err != nil {
		t.Fatal(err)
	}
	return detail
}

// lineTexts maps each JP key of an episode to "source:text" in the detail's locale.
func lineTexts(detail store.SideStoryDetail, episodeKey string) map[string]string {
	out := map[string]string{}
	for _, episode := range detail.Episodes {
		if episode.Key != episodeKey {
			continue
		}
		for _, line := range episode.Lines {
			out[line.JP] = line.Source + ":" + line.Text
		}
	}
	return out
}

func episodeDetail(t *testing.T, detail store.SideStoryDetail, key string) store.SideStoryEpisodeDetail {
	t.Helper()
	for _, episode := range detail.Episodes {
		if episode.Key == key {
			return episode
		}
	}
	t.Fatalf("episode %s missing from %s/%s", key, detail.Kind, detail.ID)
	return store.SideStoryEpisodeDetail{}
}
