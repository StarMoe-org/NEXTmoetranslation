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
