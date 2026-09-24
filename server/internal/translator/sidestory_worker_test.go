package translator

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func TestSideStoryBackfillRoundImportsOfficialChineseAndEnglish(t *testing.T) {
	h := newSideStoryHarness(t)
	state := h.round(t)
	if state.LastRoundError != "" || state.Running || !state.Enabled || state.LastRoundAt == "" || state.CatalogRefreshedAt == "" {
		t.Fatalf("state after the first round = %+v", state)
	}
	if got := state.LastRound; got.Episodes != 3 || got.Fetched != 3 || got.Errors != 0 || got.Retrying != 0 || got.OfficialWritten != 18 || got.Requests != 18 {
		t.Fatalf("round summary = %+v", got)
	}

	for _, test := range []struct {
		kind, id, locale, episode string
		want                      map[string]string
	}{
		{store.SideStoryKindCard, testCardID, model.LocaleChinese, "1", map[string]string{
			testJPEpisodeTitle: "official:" + testCNEpisodeTitle, testJPLine1: "official:" + testCNLine1,
			testJPSpeaker: "official:" + testCNSpeaker, testJPLine2: "official:" + testCNLine2,
		}},
		{store.SideStoryKindCard, testCardID, model.LocaleEnglish, "1", map[string]string{
			testJPEpisodeTitle: "official:" + testENEpisodeTitle, testJPLine1: "official:" + testENLine1,
			testJPSpeaker: "official:" + testENSpeaker, testJPLine2: "official:" + testENLine2,
		}},
		{store.SideStoryKindArea, testAreaScenario, model.LocaleChinese, "1", map[string]string{
			testJPLine1: "official:" + testCNLine1, testJPSpeaker: "official:" + testCNSpeaker, testJPLine2: "official:" + testCNLine2,
		}},
		{store.SideStoryKindArea, testAreaScenario, model.LocaleEnglish, "1", map[string]string{
			testJPLine1: "official:" + testENLine1, testJPSpeaker: "official:" + testENSpeaker, testJPLine2: "official:" + testENLine2,
		}},
	} {
		detail := h.detail(t, test.kind, test.id, test.locale)
		if got := lineTexts(detail, test.episode); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s %s %s lines = %v, want %v", test.kind, test.id, test.locale, got, test.want)
		}
		episode := episodeDetail(t, detail, test.episode)
		if !episode.Fetched || episode.CNState != store.SideStoryStateImported || episode.ENState != store.SideStoryStateImported {
			t.Errorf("%s %s episode = %+v", test.kind, test.id, episode)
		}
	}
	area := h.detail(t, store.SideStoryKindArea, testAreaScenario, model.LocaleChinese)
	if area.Title != "テスト広場 - テスト区画" || area.AreaCategory != "event_1" || area.ActionSetID != 1300 {
		t.Fatalf("area story = %+v", area)
	}
	for _, path := range []string{testCNAreaPath, testENAreaPath, testENCardPath1, testENCardPath2} {
		if h.upstream.count(path) != 1 {
			t.Errorf("%s requested %d times, want once", path, h.upstream.count(path))
		}
	}
	if h.upstream.count("/cn-scripts/scenario/actionset/group13/") != 0 || h.upstream.count("/en-scripts/character/member/") != 0 {
		t.Fatal("round requested a CN area group from the JP id or the EN member path")
	}
	for _, missing := range [][2]string{{store.SideStoryKindCard, "1002"}, {store.SideStoryKindArea, "test_area_unlisted"}} {
		if _, err := h.store.SideStoryDetailContext(t.Context(), missing[0], missing[1], model.LocaleChinese); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s %s listed: %v", missing[0], missing[1], err)
		}
	}
	if h.changes != 1 || !slices.Contains(h.stages(), "sidestory.sync") {
		t.Fatalf("changes=%d stages=%v after a changing round", h.changes, h.stages())
	}

	before := len(h.upstream.requested())
	state = h.round(t)
	if len(h.upstream.requested()) != before || state.LastRound.Episodes != 0 || h.changes != 1 {
		t.Fatalf("idle round made %d requests, summary %+v, changes %d", len(h.upstream.requested())-before, state.LastRound, h.changes)
	}
}

func TestSideStoryBackfillRetriesAMirrorServingJapaneseAndA404ADayLater(t *testing.T) {
	h := newSideStoryHarness(t)
	h.upstream.set(testENCardPath1, testJPScript(testCardScenario1))
	h.upstream.remove(testCNCardPath2)
	h.upstream.remove(testJPAreaPath)
	state := h.round(t)
	if state.LastRound.Fetched != 2 || state.LastRound.Errors != 1 || state.LastRound.Retrying != 2 {
		t.Fatalf("round summary = %+v", state.LastRound)
	}
	en := h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleEnglish)
	if episode := episodeDetail(t, en, "1"); episode.ENState != store.SideStoryStatePending || episode.CNState != store.SideStoryStateImported ||
		episode.LastError != "en-US: official script repeats the Japanese text" {
		t.Fatalf("mirrored EN episode = %+v", episode)
	}
	if got := lineTexts(en, "1"); !reflect.DeepEqual(got, map[string]string{
		testJPEpisodeTitle: "official:" + testENEpisodeTitle, testJPLine1: ":", testJPSpeaker: ":", testJPLine2: ":",
	}) {
		t.Fatalf("mirrored EN lines = %v", got)
	}
	zh := h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese)
	if episode := episodeDetail(t, zh, "2"); episode.CNState != store.SideStoryStatePending || episode.ENState != store.SideStoryStateImported ||
		episode.LastError != "zh-CN: not found" {
		t.Fatalf("CN 404 episode = %+v", episode)
	}
	area := episodeDetail(t, h.detail(t, store.SideStoryKindArea, testAreaScenario, model.LocaleChinese), "1")
	if area.Fetched || !strings.Contains(area.LastError, "not found") || area.CNState != store.SideStoryStatePending {
		t.Fatalf("JP 404 episode = %+v", area)
	}
	queued := func() []string {
		items, err := h.store.SideStoryWorkQueueContext(t.Context(), 10, h.worker.now())
		if err != nil {
			t.Fatal(err)
		}
		keys := []string{}
		for _, item := range items {
			keys = append(keys, item.Kind+"/"+item.StoryID+"/"+item.EpisodeKey)
		}
		return keys
	}
	if got := queued(); len(got) != 0 {
		t.Fatalf("queue right after the round = %v", got)
	}
	h.advance(24*time.Hour + time.Second)
	if got := queued(); !slices.Contains(got, "area/"+testAreaScenario+"/1") || !slices.Contains(got, "card/"+testCardID+"/1") ||
		!slices.Contains(got, "card/"+testCardID+"/2") || len(got) != 3 {
		t.Fatalf("queue a day later = %v", got)
	}
	h.upstream.set(testCNCardPath2, testCNScript(testCardScenario2))
	h.upstream.set(testENCardPath1, testENScript(testCardScenario1))
	cnRequests, enRequests := h.upstream.count(testCNCardPath2), h.upstream.count(testENCardPath1)
	h.round(t)
	if h.upstream.count(testCNCardPath2) != cnRequests+1 {
		t.Fatal("the CN script that 404ed was not fetched again")
	}
	if h.upstream.count(testENCardPath1) != enRequests+1 {
		t.Fatal("the EN script that repeated the Japanese text was not fetched again")
	}
	en = h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleEnglish)
	if episode := episodeDetail(t, en, "1"); episode.ENState != store.SideStoryStateImported || episode.LastError != "" {
		t.Fatalf("EN episode after the mirror synced = %+v", episode)
	}
	if got := lineTexts(en, "1"); got[testJPLine1] != "official:"+testENLine1 {
		t.Fatalf("EN lines after the mirror synced = %v", got)
	}
	zh = h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese)
	if episode := episodeDetail(t, zh, "2"); episode.CNState != store.SideStoryStateImported || episode.LastError != "" {
		t.Fatalf("CN episode after the mirror synced = %+v", episode)
	}
	if got := lineTexts(zh, "2"); got[testJPLine1] != "official:"+testCNLine1 {
		t.Fatalf("CN lines after the mirror synced = %v", got)
	}
}

func TestSideStoryBackfillBacksOffTransientErrors(t *testing.T) {
	h := newSideStoryHarness(t)
	unavailable := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}
	h.upstream.handle(testJPCardPath2, unavailable)
	h.upstream.handle(testCNAreaPath, unavailable)
	state := h.round(t)
	if state.LastRound.Fetched != 2 || state.LastRound.Errors != 1 || state.LastRound.Retrying != 1 {
		t.Fatalf("round summary = %+v", state.LastRound)
	}
	card := episodeDetail(t, h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese), "2")
	if card.Fetched || !strings.Contains(card.LastError, "http 503") {
		t.Fatalf("JP 503 episode = %+v", card)
	}
	area := episodeDetail(t, h.detail(t, store.SideStoryKindArea, testAreaScenario, model.LocaleChinese), "1")
	if !area.Fetched || area.CNState != store.SideStoryStatePending || area.ENState != store.SideStoryStateImported {
		t.Fatalf("CN 503 episode = %+v", area)
	}

	requests := len(h.upstream.requested())
	h.advance(9 * time.Minute)
	if state := h.round(t); state.LastRound.Episodes != 0 || len(h.upstream.requested()) != requests {
		t.Fatalf("round inside the backoff = %+v", state.LastRound)
	}
	h.upstream.handle(testJPCardPath2, nil)
	h.upstream.handle(testCNAreaPath, nil)
	h.advance(2 * time.Minute)
	if state := h.round(t); state.LastRound.Episodes != 2 || state.LastRound.Fetched != 2 || state.LastRound.Errors != 0 {
		t.Fatalf("round after the backoff = %+v", state.LastRound)
	}
	area = episodeDetail(t, h.detail(t, store.SideStoryKindArea, testAreaScenario, model.LocaleChinese), "1")
	if area.CNState != store.SideStoryStateImported {
		t.Fatalf("CN retry left the area episode %+v", area)
	}
}

func TestSideStoryBackfillKeepsHumanRowsAndRefreshesTheCatalogOnDataVersionAndAge(t *testing.T) {
	h := newSideStoryHarness(t)
	cnEpisodes := h.upstream.files["/cn-master/cardEpisodes.json"]
	h.upstream.set("/cn-master/cardEpisodes.json", []any{})
	h.round(t)
	if got := lineTexts(h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese), "1"); got[testJPLine1] != ":" {
		t.Fatalf("zh-CN line without a CN episode = %q", got[testJPLine1])
	}
	if _, err := h.store.UpdateSideStoryLinesContext(t.Context(), store.SideStoryKindCard, testCardID, "1", model.LocaleChinese, "tester",
		[]store.SideStoryLineEdit{{JP: testJPLine1, Text: "测试人工译文"}}, h.worker.now()); err != nil {
		t.Fatal(err)
	}
	masterdata := func() int {
		return h.upstream.count("/jp-master/") + h.upstream.count("/cn-master/") + h.upstream.count("/en-master/")
	}
	if masterdata() != 9 {
		t.Fatalf("first round fetched %d masterdata files, want 9", masterdata())
	}

	h.upstream.set("/cn-master/cardEpisodes.json", cnEpisodes)
	h.advance(time.Hour)
	h.round(t)
	if masterdata() != 9 {
		t.Fatal("catalog refreshed before 6 h without a data version change")
	}
	if err := h.cfg.Set(config.KeyUpstreamLastDataVersion, "test-data-version-2"); err != nil {
		t.Fatal(err)
	}
	state := h.round(t)
	if masterdata() != 18 || state.LastRound.OfficialWritten == 0 {
		t.Fatalf("data version change: masterdata=%d summary=%+v", masterdata(), state.LastRound)
	}
	if got, want := lineTexts(h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese), "1"), map[string]string{
		testJPEpisodeTitle: "official:" + testCNEpisodeTitle, testJPLine1: "human:测试人工译文",
		testJPSpeaker: "official:" + testCNSpeaker, testJPLine2: "official:" + testCNLine2,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("zh-CN lines after the CN import = %v, want %v", got, want)
	}

	h.advance(5 * time.Hour)
	h.round(t)
	if masterdata() != 18 {
		t.Fatal("catalog refreshed before 6 h after the last refresh")
	}
	h.advance(time.Hour)
	h.round(t)
	if masterdata() != 27 {
		t.Fatalf("catalog not refreshed after 6 h: %d masterdata requests", masterdata())
	}
}

type sideStoryLogBuffer struct {
	mu  sync.Mutex
	out strings.Builder
}

func (b *sideStoryLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.Write(p)
}

func (b *sideStoryLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.String()
}

func captureSideStoryLog(t *testing.T) *sideStoryLogBuffer {
	t.Helper()
	buffer := &sideStoryLogBuffer{}
	previous := log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return buffer
}

func TestSideStoryBackfillRebuildsOnCatalogTitleChangesAndLogsDroppedHumanTitles(t *testing.T) {
	h := newSideStoryHarness(t)
	h.round(t)
	changes := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.changes
	}
	refresh := func(version string) store.SideStoryBackfillState {
		t.Helper()
		if err := h.cfg.Set(config.KeyUpstreamLastDataVersion, version); err != nil {
			t.Fatal(err)
		}
		return h.round(t)
	}
	cardEpisodes := func(bundle, seq1Title, seq2Title string) []any {
		return []any{
			map[string]any{"id": 1, "cardId": 1001, "seq": 2, "title": seq2Title, "scenarioId": testCardScenario2, "assetbundleName": bundle},
			map[string]any{"id": 2, "cardId": 1001, "seq": 1, "title": seq1Title, "scenarioId": testCardScenario1, "assetbundleName": bundle},
		}
	}

	before := changes()
	h.upstream.set("/cn-master/cardEpisodes.json", cardEpisodes("res_test001", "测试上篇改", "测试下篇"))
	if state := refresh("test-data-version-2"); state.LastRound.Episodes != 0 || changes() != before+1 {
		t.Fatalf("official title change: summary %+v, changes %d -> %d", state.LastRound, before, changes())
	}
	if got := lineTexts(h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese), "1"); got[testJPEpisodeTitle] != "official:测试上篇改" {
		t.Fatalf("zh-CN lines after the title change = %v", got)
	}

	if _, err := h.store.UpdateSideStoryLinesContext(t.Context(), store.SideStoryKindCard, testCardID, "2", model.LocaleChinese, "tester",
		[]store.SideStoryLineEdit{{JP: "テスト後編", Text: "测试人工标题"}}, h.worker.now()); err != nil {
		t.Fatal(err)
	}
	jpEpisodes := append(cardEpisodes("res_test001", testJPEpisodeTitle, "テスト後編改"),
		map[string]any{"id": 3, "cardId": 1002, "seq": 1, "title": testJPEpisodeTitle, "scenarioId": "test_card_1002_01", "assetbundleName": "res_test002"})
	h.upstream.set("/jp-master/cardEpisodes.json", jpEpisodes)
	h.upstream.set("/cn-master/cardEpisodes.json", cardEpisodes("res_test001", "测试上篇改", ""))
	h.upstream.set("/en-master/cardEpisodes.json", cardEpisodes("res_test001_en", testENEpisodeTitle, ""))
	logs := captureSideStoryLog(t)
	before = changes()
	if state := refresh("test-data-version-3"); state.LastRound.Episodes != 0 || changes() != before+1 {
		t.Fatalf("dropped human title: summary %+v, changes %d -> %d", state.LastRound, before, changes())
	}
	const line = "[side-story] card catalog: changed JP episode titles deleted 1 human title translation(s)"
	if got := logs.String(); strings.Count(got, line) != 1 || strings.Count(got, "human title translation") != 1 {
		t.Fatalf("log after a JP title change dropped a human title = %q", got)
	}
	if got := lineTexts(h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese), "2"); got["テスト後編改"] != ":" || got["テスト後編"] != "" {
		t.Fatalf("zh-CN lines after the JP title change = %v", got)
	}

	jpEpisodes = append(cardEpisodes("res_test001", "テスト前編改", "テスト後編改"), jpEpisodes[2])
	h.upstream.set("/jp-master/cardEpisodes.json", jpEpisodes)
	h.upstream.set("/cn-master/cardEpisodes.json", cardEpisodes("res_test001", "", ""))
	h.upstream.set("/en-master/cardEpisodes.json", cardEpisodes("res_test001_en", "", ""))
	before = changes()
	if state := refresh("test-data-version-4"); state.LastRound.Episodes != 0 || changes() != before+1 {
		t.Fatalf("JP title change that only deleted official titles: summary %+v, changes %d -> %d", state.LastRound, before, changes())
	}
	if got := lineTexts(h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese), "1"); got["テスト前編改"] != ":" || got[testJPEpisodeTitle] != "" {
		t.Fatalf("zh-CN lines after the second JP title change = %v", got)
	}
}

func TestSideStoryBackfillRetriesAFailedCatalogAfterADelay(t *testing.T) {
	h := newSideStoryHarness(t)
	h.upstream.remove("/en-master/actionSets.json")
	state := h.round(t)
	if !strings.Contains(state.LastRoundError, "area catalog") || state.CatalogRefreshedAt != "" || state.LastRound.Episodes != 2 {
		t.Fatalf("state after a failed area catalog = %+v", state)
	}
	requests := h.upstream.count("/jp-master/")
	h.advance(time.Minute)
	h.round(t)
	if h.upstream.count("/jp-master/") != requests {
		t.Fatal("failed catalog was retried within the retry delay")
	}
	h.upstream.set("/en-master/actionSets.json", []any{})
	if !h.worker.TriggerSideStoryBackfill(true) {
		t.Fatal("trigger rejected while enabled")
	}
	<-h.worker.wake
	state = h.round(t)
	if state.LastRoundError != "" || state.CatalogRefreshedAt == "" || h.upstream.count("/jp-master/") == requests {
		t.Fatalf("requested catalog refresh = %+v", state)
	}
}

func TestSideStoryBackfillHonoursTheRequestDelay(t *testing.T) {
	h := newSideStoryHarness(t)
	const delay = 60 * time.Millisecond
	h.worker.opts.RequestDelay = delay
	h.round(t)
	requests := h.upstream.requested()
	if len(requests) != 18 {
		t.Fatalf("%d requests, want 18", len(requests))
	}
	for i := 1; i < len(requests); i++ {
		if gap := requests[i].at.Sub(requests[i-1].at); gap < delay*9/10 {
			t.Fatalf("request %d (%s) followed the previous one after %s, want at least %s", i, requests[i].path, gap, delay)
		}
	}
}

func TestSideStoryBackfillDefersWritesWhileAProducerRuns(t *testing.T) {
	h := newSideStoryHarness(t)
	release, err := h.gate.BeginProducerContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state := h.round(t)
	if !strings.Contains(state.LastRoundError, "producer") || len(h.upstream.requested()) != 0 {
		t.Fatalf("round under a held producer: state=%+v requests=%d", state, len(h.upstream.requested()))
	}
	release()

	var once sync.Once
	var releaseMidRound func()
	h.upstream.handle(testJPCardPath1, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			var err error
			if releaseMidRound, err = h.gate.BeginProducerContext(r.Context()); err != nil {
				t.Error(err)
			}
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ScenarioId":"` + testCardScenario1 + `","Snippets":[],"TalkData":[{"WindowDisplayName":"` +
			testJPSpeaker + `","Body":"` + testJPLine1 + `"}],"SpecialEffectData":[],"AppearCharacters":[]}`))
	})
	state = h.round(t)
	if !strings.Contains(state.LastRoundError, "producer") || state.LastRound.Fetched != 0 {
		t.Fatalf("round with a producer started mid-round = %+v", state)
	}
	if episode := episodeDetail(t, h.detail(t, store.SideStoryKindCard, testCardID, model.LocaleChinese), "1"); episode.Fetched {
		t.Fatal("apply ran while a producer held the gate")
	}
	releaseMidRound()
	if state = h.round(t); state.LastRoundError != "" || state.LastRound.Fetched != 3 {
		t.Fatalf("round after the producer finished = %+v", state)
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startTestBackfill(t *testing.T, tr *Translator, opts SideStoryBackfillOptions) *SideStoryBackfill {
	t.Helper()
	worker := NewSideStoryBackfill(tr, opts)
	worker.Start()
	t.Cleanup(func() {
		worker.Stop()
		worker.Wait()
	})
	return worker
}

func TestSideStoryBackfillRunsOnlyWhileTheSchedulerAndEnvAllowIt(t *testing.T) {
	h := newSideStoryHarness(t)
	if err := h.cfg.Set(config.KeySchedulerOn, "false"); err != nil {
		t.Fatal(err)
	}
	worker := startTestBackfill(t, h.tr, SideStoryBackfillOptions{Enabled: true, Interval: 10 * time.Millisecond, Batch: 30})
	disabled := startTestBackfill(t, h.tr, SideStoryBackfillOptions{Enabled: false, Interval: 10 * time.Millisecond, Batch: 30})
	time.Sleep(100 * time.Millisecond)
	if n := len(h.upstream.requested()); n != 0 || worker.TriggerSideStoryBackfill(true) || worker.SideStoryBackfillState().Enabled {
		t.Fatalf("scheduler off: %d requests, state %+v", n, worker.SideStoryBackfillState())
	}
	if err := h.cfg.Set(config.KeySchedulerOn, "true"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a round after the scheduler was enabled", func() bool { return h.upstream.count("/jp-master/") > 0 })
	worker.Stop()
	worker.Wait()
	// A request sent just before Stop can reach the test server after Wait returns.
	time.Sleep(20 * time.Millisecond)
	requests := len(h.upstream.requested())
	time.Sleep(50 * time.Millisecond)
	if n := len(h.upstream.requested()); n != requests || disabled.TriggerSideStoryBackfill(false) || disabled.SideStoryBackfillState().Enabled {
		t.Fatalf("SIDE_STORY_BACKFILL_ENABLED=false: %d requests, state %+v", n-requests, disabled.SideStoryBackfillState())
	}
}

func TestSideStoryBackfillStopsPromptlyDuringARound(t *testing.T) {
	h := newSideStoryHarness(t)
	entered := make(chan struct{})
	var once sync.Once
	h.upstream.handle("/jp-master/cards.json", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	worker := startTestBackfill(t, h.tr, SideStoryBackfillOptions{Enabled: true, Interval: time.Hour, Batch: 30, RequestDelay: time.Hour})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the enabled worker did not start a round")
	}
	waitFor(t, "a running round", func() bool { return worker.SideStoryBackfillState().Running })
	if !worker.TriggerSideStoryBackfill(false) {
		t.Fatal("trigger rejected while enabled")
	}
	started := time.Now()
	worker.Stop()
	worker.Wait()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("stop took %s while the round waited on the request delay", elapsed)
	}
	if worker.TriggerSideStoryBackfill(false) || worker.SideStoryBackfillState().Enabled {
		t.Fatal("stopped worker still reports enabled")
	}
}

func TestRefreshSideStoryFetchesEveryLocaleNow(t *testing.T) {
	h := newSideStoryHarness(t)
	h.round(t)
	cnRequests := h.upstream.count(testCNCardPath1)
	applied, err := h.tr.RefreshSideStoryContext(t.Context(), store.SideStoryKindCard, testCardID)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || !applied[0].Fetched || applied[0].CNState != store.SideStoryStateImported || applied[1].ENState != store.SideStoryStateImported {
		t.Fatalf("refresh = %+v", applied)
	}
	if h.upstream.count(testCNCardPath1) != cnRequests+1 {
		t.Fatal("refresh did not re-fetch an imported CN script")
	}
	if _, err := h.tr.RefreshSideStoryContext(t.Context(), store.SideStoryKindCard, "999"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown story refresh err = %v", err)
	}
	unavailable := func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusBadGateway) }
	h.upstream.handle(testJPCardPath1, unavailable)
	h.upstream.handle(testJPCardPath2, unavailable)
	if _, err := h.tr.RefreshSideStoryContext(t.Context(), store.SideStoryKindCard, testCardID); !errors.Is(err, ErrSideStoryUpstreamUnavailable) {
		t.Fatalf("refresh with the JP source down err = %v", err)
	}
	release, err := h.gate.BeginProducerContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := h.tr.RefreshSideStoryContext(t.Context(), store.SideStoryKindArea, testAreaScenario); !IsAlreadyRunning(err) {
		t.Fatalf("refresh under a producer err = %v", err)
	}
}

func TestFetchSideStoryJPScriptMatchesTheStoredDigest(t *testing.T) {
	h := newSideStoryHarness(t)
	h.round(t)
	canonical, digest, err := h.tr.FetchSideStoryJPScriptContext(t.Context(), store.SideStoryKindArea, testAreaScenario, "1")
	if err != nil {
		t.Fatal(err)
	}
	episode := episodeDetail(t, h.detail(t, store.SideStoryKindArea, testAreaScenario, model.LocaleChinese), "1")
	if digest != episode.ScriptSHA256 || !strings.Contains(canonical, `"ScenarioId":"`+testAreaScenario+`"`) {
		t.Fatalf("snapshot digest %s, stored %s, canonical %.80s", digest, episode.ScriptSHA256, canonical)
	}
	if _, _, err := h.tr.FetchSideStoryJPScriptContext(t.Context(), store.SideStoryKindCard, testCardID, "3"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown episode err = %v", err)
	}
	h.upstream.remove(testJPAreaPath)
	if _, _, err := h.tr.FetchSideStoryJPScriptContext(t.Context(), store.SideStoryKindArea, testAreaScenario, "1"); !errors.Is(err, ErrSideStoryUpstreamUnavailable) {
		t.Fatalf("missing JP script err = %v", err)
	}
}
