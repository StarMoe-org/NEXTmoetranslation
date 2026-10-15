package translator

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"moesekai/server/internal/config"
	"moesekai/server/internal/store"
)

// First lines of files the asset mirror served as JP: event 168/1 in Japanese
// and in the Taiwan server's Traditional Chinese, card 1116/1 in Korean.
const (
	languageTestJP = "——皆さん、お待たせしました！\n受付が終わったので、中に入りましょう"
	languageTestTW = "──讓大家久等了！報到手續完成了，我們進去吧。"
	languageTestKR = "……그럼 컬래버레이션 스테이지에서 할 쇼 대본도\n완성됐으니, 당장 연습을 시작해 볼까? 아오야기 군."
)

func languageTestScenario(bodies ...string) map[string]any {
	talks := make([]any, 0, len(bodies))
	for _, body := range bodies {
		talks = append(talks, map[string]any{"Body": body, "WindowDisplayName": "遥"})
	}
	return map[string]any{"ScenarioId": "event_167_01", "TalkData": talks}
}

func TestJapaneseScenarioCheckRejectsOtherServersText(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
		ok    bool
	}{
		{"japanese", languageTestScenario(languageTestJP), true},
		{"traditional chinese", languageTestScenario(languageTestTW), false},
		{"korean", languageTestScenario(languageTestKR), false},
		{"english", languageTestScenario("What a nice breeze..."), false},
		{"symbols only", languageTestScenario("……！", "♪——————", ""), true},
		{"no talk data", map[string]any{"ScenarioId": "x"}, true},
		{"not an object", []any{}, true},
		// More than a quarter as much kana as kanji.
		{"kana above a quarter", languageTestScenario("あ漢字字"), true},
		{"kana at a quarter", languageTestScenario("あ漢字字字"), false},
		// Speaker names are not counted.
		{"japanese name, chinese line", map[string]any{"TalkData": []any{map[string]any{"WindowDisplayName": "みのり", "Body": languageTestTW}}}, false},
	} {
		err := errNotJapaneseScenario(test.value)
		if (err == nil) != test.ok {
			t.Errorf("%s: err = %v, want ok=%v", test.name, err, test.ok)
		}
	}
	err := errNotJapaneseScenario(languageTestScenario("ア가ㄱ漢㐀字字Ab1！"))
	if err == nil || err.Error() != "not Japanese text (kana 1, hangul 2, kanji 4, latin 2)" {
		t.Fatalf("counts = %v", err)
	}
}

func TestFetchJPScenarioSkipsASourceServingAnotherLanguage(t *testing.T) {
	serve := func(calls *atomic.Int32, body string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"ScenarioId":"event_167_01","TalkData":[{"Body":%q}]}`, body)
		}))
		t.Cleanup(server.Close)
		return server
	}
	var primaryCalls, fallbackCalls atomic.Int32
	primary := serve(&primaryCalls, languageTestTW)
	fallback := serve(&fallbackCalls, languageTestJP)

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPAssetsURL, primary.URL)
	cfg.Set(config.KeyUpstreamJPAssetsFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)
	result, err := tr.fetchJPScenarioJSON("event_story/event_cheerheart_2025/scenario/event_168_01")
	if err != nil || errNotJapaneseScenario(result) != nil {
		t.Fatalf("result=%v err=%v, want the fallback's Japanese", result, err)
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("calls: primary=%d fallback=%d", primaryCalls.Load(), fallbackCalls.Load())
	}

	// With no source in Japanese the fetch fails, without retrying, and keeps
	// the reason.
	cfg.Set(config.KeyUpstreamJPAssetsFallbackURL, primary.URL)
	primaryCalls.Store(0)
	result, err = tr.fetchJPScenarioJSON("event_story/event_cheerheart_2025/scenario/event_168_01")
	if result != nil || err == nil || !strings.Contains(err.Error(), "not Japanese text (kana 0, hangul 0, kanji 18, latin 0)") {
		t.Fatalf("result=%v err=%v, want a rejection", result, err)
	}
	if primaryCalls.Load() != 1 {
		t.Fatalf("a source serving another language was fetched %d times", primaryCalls.Load())
	}
}

func TestSideStoryJPScriptInAnotherLanguageMovesOnToTheNextBase(t *testing.T) {
	upstream := newSideStoryUpstream(t)
	tr, _, cfg := openTestTranslator(t)
	configureSideStorySources(t, cfg, upstream.server.URL)
	if err := cfg.Set(config.KeyUpstreamJPScriptsFallbackURL, upstream.server.URL+"/jp-fallback"); err != nil {
		t.Fatal(err)
	}
	fetch := func(server, kind, path string) store.SideStoryFetchOutcome {
		t.Helper()
		outcome, err := tr.fetchSideStoryScriptContext(t.Context(), &sideStoryPacer{}, server, kind, path)
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}

	const card = "character/member/res_test001/test_card_language"
	upstream.set("/jp-scripts/"+card+".json", testScript("test_card_language", [2]string{"토우야", languageTestKR}))
	upstream.set("/jp-fallback/"+card+".json", testScript("test_card_language", [2]string{"冬弥", languageTestJP}))
	outcome := fetch("jp", store.SideStoryKindCard, card)
	if outcome.Script == nil || len(outcome.Script.Talks) != 1 || outcome.Script.Talks[0].Body != languageTestJP {
		t.Fatalf("a Korean primary did not fall through to the Japanese fallback: %+v", outcome)
	}

	// An area talk has one JP base: the Korean script is an error, neither
	// missing nor transient, so the stored script stays.
	const area = "scenario/actionset/group1/test_area_language"
	upstream.set("/jp-scripts/"+area+".json", testScript("test_area_language", [2]string{"토우야", languageTestKR}))
	outcome = fetch("jp", store.SideStoryKindArea, area)
	if outcome.Script != nil || outcome.Missing || outcome.Transient || !strings.Contains(outcome.Err, "not Japanese text") {
		t.Fatalf("Korean area script = %+v, want a permanent error", outcome)
	}

	// Official CN and EN scripts are in their own language.
	upstream.set("/cn-scripts/"+area+".json", testScript("test_area_language", [2]string{"冬弥", languageTestTW}))
	if outcome := fetch("cn", store.SideStoryKindArea, area); outcome.Script == nil {
		t.Fatalf("CN script = %+v, want it accepted", outcome)
	}
}

// The CN mirror serves the Japanese text of an episode CN has listed but not
// translated yet, here an older revision with one line worded differently.
// That line must not become the official translation, and the event must not
// be imported as official CN.
func TestOfficialCNImportIgnoresACNScenarioInJapanese(t *testing.T) {
	const (
		jpLine1  = "——うむ！　全員そろったな！"
		jpLine2  = "いえ。これは僕が永峯さんにお会いして日が浅い分、\n客観視できたから閃いたにすぎないと思います"
		oldLine2 = "いえ。これは僕が今日初めて永峯さんにお会いした分、\n客観視できたから閃いたにすぎないと思います"
	)
	talks := func(lines ...string) string {
		parts := make([]string, 0, len(lines))
		for _, line := range lines {
			parts = append(parts, fmt.Sprintf(`{"Body":%q,"WindowDisplayName":"司","Voices":[]}`, line))
		}
		return strings.Join(parts, ",")
	}
	cnBody := talks(jpLine1, oldLine2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jp-master/eventStories.json":
			fmt.Fprint(w, `[{"eventId":200,"assetbundleName":"event_direction_2026","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"event_200_01","title":"道を決める前に"}]}]`)
		case "/cn-master/eventStories.json":
			fmt.Fprint(w, `[{"eventId":200,"eventStoryEpisodes":[{"episodeNo":1,"title":"在确定道路之前"}]}]`)
		case "/cn-master/events.json":
			fmt.Fprint(w, `[{"id":200}]`)
		case "/jp-assets/event_story/event_direction_2026/scenario/event_200_01.json":
			fmt.Fprintf(w, `{"ScenarioId":"event_200_01","Snippets":[],"TalkData":[%s],"SpecialEffectData":[],"AppearCharacters":[]}`, talks(jpLine1, jpLine2))
		case "/cn-assets/event_story/event_direction_2026/scenario/event_200_01.json":
			fmt.Fprintf(w, `{"ScenarioId":"event_200_01","TalkData":[%s]}`, cnBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	tr, events, cfg := openTestTranslator(t)
	configureLocalSources(t, cfg, upstream.URL)
	jpStories, err := tr.fetchMasterdata("eventStories.json", "jp")
	if err != nil {
		t.Fatal(err)
	}
	cnStories, err := tr.fetchMasterdata("eventStories.json", "cn")
	if err != nil {
		t.Fatal(err)
	}
	episodes, hasTalk, hasTitleOnly, errs := tr.buildOfficialCNEpisodes(jpStories[0], cnStories[0])
	if len(errs) != 0 || hasTalk || !hasTitleOnly {
		t.Fatalf("hasTalk=%v hasTitleOnly=%v errs=%v, want title only", hasTalk, hasTitleOnly, errs)
	}
	if text := episodes["1"].talkData[jpLine2]; text != "" {
		t.Fatalf("the Japanese CN line became a translation: %q", text)
	}

	outcome, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Processed != 0 || len(outcome.PartialErrors) != 0 {
		t.Fatalf("outcome = %+v, want nothing imported and no error", outcome)
	}
	if detail, err := events.Detail(200); err == nil && detail.Meta.Source == "official_cn" {
		t.Fatalf("event 200 was imported as official CN: %+v", detail.Meta)
	}

	// Once CN publishes Chinese, the episode is imported as before.
	cnBody = `{"Body":"——嗯！全员到齐了！","WindowDisplayName":"司"},{"Body":"不。这只是因为我和永峰先生认识不久，\n才能客观地看待问题。","WindowDisplayName":"司"}`
	outcome, err = tr.syncEventStoriesCNOnly(0, 1)
	if err != nil || outcome.Processed != 1 {
		t.Fatalf("outcome=%+v err=%v, want the Chinese import", outcome, err)
	}
	detail, err := events.Detail(200)
	if err != nil || detail.Meta.Source != "official_cn" || detail.Episodes["1"].TalkData[jpLine1] != "——嗯！全员到齐了！" {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
}
