package translator

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/store"
)

func TestSideStorySourcesDefaultToTheStartappMirrorsAndFollowOverrides(t *testing.T) {
	tr, _, cfg := openTestTranslator(t)
	for _, test := range []struct {
		name string
		got  []string
		want []string
	}{
		{"jp card", tr.sideStoryScriptBases("jp", store.SideStoryKindCard), []string{defaultJPScriptsURL, defaultJPScriptsFallbackURL}},
		{"jp area", tr.sideStoryScriptBases("jp", store.SideStoryKindArea), []string{defaultJPScriptsURL}},
		{"cn", tr.sideStoryScriptBases("cn", store.SideStoryKindArea), []string{defaultCNScriptsURL}},
		{"en", tr.sideStoryScriptBases("en", store.SideStoryKindCard), []string{defaultENScriptsURL}},
		{"en masterdata", tr.masterdataBases("en"), []string{defaultENMasterdataURL, defaultENMasterdataFallbackURL}},
	} {
		if !reflect.DeepEqual(test.got, test.want) {
			t.Errorf("%s bases = %q, want %q", test.name, test.got, test.want)
		}
	}
	if defaultJPScriptsURL != "https://storage.exmeaning.com/sekai-jp-assets" ||
		defaultJPScriptsFallbackURL != "https://assets.unipjsk.com/startapp" ||
		defaultCNScriptsURL != "https://sekai-assets-bdf29c81.seiunx.net/cn-assets/startapp" ||
		defaultENScriptsURL != "https://storage.exmeaning.com/sekai-en-assets" ||
		defaultENMasterdataURL != "https://metadata.pjsk.moe/en/master" ||
		defaultENMasterdataFallbackURL != "https://raw.githubusercontent.com/Team-Haruki/haruki-sekai-en-master/main/master" {
		t.Fatal("side-story source defaults changed")
	}

	if _, err := cfg.SetMany(map[string]string{
		config.KeyUpstreamJPScriptsURL: "https://jp.example/assets/", config.KeyUpstreamJPScriptsFallbackURL: "https://jp-fallback.example/assets",
		config.KeyUpstreamCNScriptsURL: "https://cn.example/assets", config.KeyUpstreamENScriptsURL: "https://en.example/assets",
		config.KeyUpstreamENMasterdataURL: "https://en.example/master", config.KeyUpstreamENMasterdataFallbackURL: "https://en-fallback.example/master",
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		got  []string
		want []string
	}{
		{"jp card", tr.sideStoryScriptBases("jp", store.SideStoryKindCard), []string{"https://jp.example/assets", "https://jp-fallback.example/assets"}},
		{"jp area", tr.sideStoryScriptBases("jp", store.SideStoryKindArea), []string{"https://jp.example/assets"}},
		{"cn", tr.sideStoryScriptBases("cn", store.SideStoryKindCard), []string{"https://cn.example/assets"}},
		{"en", tr.sideStoryScriptBases("en", store.SideStoryKindArea), []string{"https://en.example/assets"}},
		{"en masterdata", tr.masterdataBases("en"), []string{"https://en.example/master", "https://en-fallback.example/master"}},
	} {
		if !reflect.DeepEqual(test.got, test.want) {
			t.Errorf("overridden %s bases = %q, want %q", test.name, test.got, test.want)
		}
	}
}

func TestFetchSideStoryScriptClassifiesFailuresAndTriesTheNextBase(t *testing.T) {
	upstream := newSideStoryUpstream(t)
	tr, _, cfg := openTestTranslator(t)
	configureSideStorySources(t, cfg, upstream.server.URL)
	if err := cfg.Set(config.KeyUpstreamJPScriptsFallbackURL, upstream.server.URL+"/jp-fallback"); err != nil {
		t.Fatal(err)
	}
	const card = "character/member/res_test001/test_card_fetch"
	fetch := func(server, kind, path string) store.SideStoryFetchOutcome {
		t.Helper()
		outcome, err := tr.fetchSideStoryScriptContext(t.Context(), &sideStoryPacer{}, server, kind, path)
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Attempted {
			t.Fatalf("%s %s outcome not attempted", server, path)
		}
		return outcome
	}

	if outcome := fetch("jp", store.SideStoryKindCard, card); !outcome.Missing || outcome.Err != "" || outcome.Script != nil {
		t.Fatalf("404 on every base = %+v, want Missing", outcome)
	}
	if upstream.count("/jp-fallback/"+card+".json") != 1 {
		t.Fatal("card fetch did not try the JP fallback after a 404")
	}

	upstream.set("/jp-fallback/"+card+".json", testJPScript("test_card_fetch"))
	upstream.set("/jp-scripts/"+card+".json", map[string]any{"ScenarioId": "test_card_fetch", "TalkData": []any{}})
	outcome := fetch("jp", store.SideStoryKindCard, card)
	if outcome.Script == nil || outcome.Script.ScenarioID != "test_card_fetch" || len(outcome.Script.Talks) != 2 ||
		outcome.Script.Talks[0].Body != testJPLine1 || outcome.Script.SHA256 == "" {
		t.Fatalf("a primary without TalkData did not fall through to the fallback: %+v", outcome)
	}

	upstream.handle("/jp-scripts/"+card+".json", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	upstream.remove("/jp-fallback/" + card + ".json")
	outcome = fetch("jp", store.SideStoryKindCard, card)
	if outcome.Missing || !outcome.Transient || !strings.Contains(outcome.Err, "http 503") {
		t.Fatalf("503 then 404 = %+v, want a transient error", outcome)
	}

	upstream.set("/cn-scripts/"+card+".json", map[string]any{"ScenarioId": "test_card_fetch"})
	outcome = fetch("cn", store.SideStoryKindCard, card)
	if outcome.Missing || outcome.Transient || !strings.Contains(outcome.Err, "missing TalkData") {
		t.Fatalf("script without TalkData = %+v, want a permanent error", outcome)
	}

	const area = "scenario/actionset/group1/test_area_fetch"
	upstream.set("/jp-fallback/"+area+".json", testJPScript("test_area_fetch"))
	if outcome := fetch("jp", store.SideStoryKindArea, area); !outcome.Missing {
		t.Fatalf("area fetch = %+v, want Missing from the primary only", outcome)
	}
	if upstream.count("/jp-fallback/"+area+".json") != 0 {
		t.Fatal("area fetch used the card-only JP fallback")
	}
}

func TestSideStoryMasterdataRetriesATransientFailureOnce(t *testing.T) {
	h := newSideStoryHarness(t)
	cards := h.upstream.files["/jp-master/cards.json"]
	var mu sync.Mutex
	calls := 0
	h.upstream.handle("/jp-master/cards.json", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			http.Error(w, "origin handshake failed", 525)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cards)
	})
	h.upstream.remove("/en-master/actionSets.json")
	state := h.round(t)
	if strings.Contains(state.LastRoundError, "card catalog") || !strings.Contains(state.LastRoundError, "area catalog") {
		t.Fatalf("round error after a transient card masterdata failure = %q", state.LastRoundError)
	}
	var attempts []sideStoryRequest
	for _, request := range h.upstream.requested() {
		if request.path == "/jp-master/cards.json" {
			attempts = append(attempts, request)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("cards.json requested %d times, want 2", len(attempts))
	}
	if gap := attempts[1].at.Sub(attempts[0].at); gap < 450*time.Millisecond {
		t.Fatalf("retry followed the failure after %s, want about 500ms", gap)
	}
	if got := h.upstream.count("/en-master/actionSets.json"); got != 1 {
		t.Fatalf("a 404 masterdata file was requested %d times, want once", got)
	}
}
