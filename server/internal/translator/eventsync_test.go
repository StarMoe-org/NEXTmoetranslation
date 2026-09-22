package translator

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"moesekai/server/internal/config"
	"moesekai/server/internal/model"
)

func TestFailedEventScenarioIsRetriedAfterLaterEventImports(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	jpMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventStories.json") {
			fmt.Fprint(w, `[{"eventId":50,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"fifty","title":"五十"}]},
				{"eventId":51,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"fiftyone","title":"五十一"}]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer jpMaster.Close()
	cnMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/eventStories.json"):
			fmt.Fprint(w, `[{"eventId":50,"eventStoryEpisodes":[{"episodeNo":1,"title":"五十中"}]},
				{"eventId":51,"eventStoryEpisodes":[{"episodeNo":1,"title":"五十一中"}]}]`)
		case strings.HasSuffix(r.URL.Path, "/events.json"):
			fmt.Fprint(w, `[{"id":50},{"id":51}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cnMaster.Close()
	var fiftyFetches atomic.Int32
	jpAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fifty.json") {
			if fiftyFetches.Add(1) == 1 {
				http.Error(w, "missing", http.StatusNotFound)
				return
			}
			fmt.Fprint(w, `{"ScenarioId":"fifty","Snippets":[],"TalkData":[{"Body":"五十原文","WindowDisplayName":"角色"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
			return
		}
		fmt.Fprint(w, `{"ScenarioId":"fiftyone","Snippets":[],"TalkData":[{"Body":"五十一原文","WindowDisplayName":"角色"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer jpAssets.Close()
	cnAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fifty.json") {
			fmt.Fprint(w, `{"TalkData":[{"Body":"五十译文","WindowDisplayName":"角色中"}]}`)
			return
		}
		fmt.Fprint(w, `{"TalkData":[{"Body":"五十一译文","WindowDisplayName":"角色中"}]}`)
	}))
	defer cnAssets.Close()
	for key, value := range map[string]string{
		config.KeyUpstreamJPMasterdataURL: jpMaster.URL, config.KeyUpstreamJPMasterdataFallbackURL: "",
		config.KeyUpstreamCNMasterdataURL: cnMaster.URL, config.KeyUpstreamCNMasterdataFallbackURL: "",
		config.KeyUpstreamJPAssetsURL: jpAssets.URL, config.KeyUpstreamJPAssetsFallbackURL: "",
		config.KeyUpstreamCNAssetsURL: cnAssets.URL, config.KeyUpstreamCNAssetsFallbackURL: "",
	} {
		if err := cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}

	first, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Processed != 1 || len(first.PartialErrors) == 0 {
		t.Fatalf("first run outcome = %+v", first)
	}
	if _, err := events.Detail(50); err != sql.ErrNoRows {
		t.Fatalf("failed event 50 was imported: %v", err)
	}

	second, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fiftyFetches.Load() < 2 {
		t.Fatalf("event 50 scenario was never retried: fetches=%d", fiftyFetches.Load())
	}
	if second.Processed != 1 {
		t.Fatalf("second run outcome = %+v", second)
	}
	detail, err := events.Detail(50)
	if err != nil {
		t.Fatalf("retried event 50 missing: %v", err)
	}
	if detail.Meta.Source != "official_cn" {
		t.Fatalf("retried event 50 source = %q", detail.Meta.Source)
	}
}

func TestTitleOnlyOfficialEventsDoNotEndTheOfficialScan(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/jp-master/eventStories.json":
			var stories []string
			for id := 60; id <= 63; id++ {
				stories = append(stories, fmt.Sprintf(
					`{"eventId":%d,"assetbundleName":"asset","eventStoryEpisodes":[{"episodeNo":1,"scenarioId":"s%d","title":"原題%d"}]}`, id, id, id))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(stories, ","))
		case r.URL.Path == "/cn-master/eventStories.json":
			var stories []string
			for id := 60; id <= 63; id++ {
				stories = append(stories, fmt.Sprintf(`{"eventId":%d,"eventStoryEpisodes":[{"episodeNo":1,"title":"中文标题%d"}]}`, id, id))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(stories, ","))
		case r.URL.Path == "/cn-master/events.json":
			fmt.Fprint(w, `[{"id":60},{"id":61},{"id":62},{"id":63}]`)
		case strings.HasPrefix(r.URL.Path, "/jp-assets/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jp-assets/event_story/asset/scenario/"), ".json")
			fmt.Fprintf(w, `{"ScenarioId":%q,"Snippets":[],"TalkData":[{"Body":"原文%s","WindowDisplayName":"角色"}],"SpecialEffectData":[],"AppearCharacters":[]}`, id, id)
		case strings.HasPrefix(r.URL.Path, "/cn-assets/"):
			// only the newest event has official talk text; the rest are title-only
			if strings.HasSuffix(r.URL.Path, "/s63.json") {
				fmt.Fprint(w, `{"TalkData":[{"Body":"译文s63","WindowDisplayName":"角色中"}]}`)
				return
			}
			fmt.Fprint(w, `{"TalkData":[{"Body":"","WindowDisplayName":""}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	configureRetryTestSources(t, cfg, upstream.URL)

	if _, err := tr.syncEventStoriesCNOnly(0, 1); err != nil {
		t.Fatal(err)
	}
	detail, err := events.Detail(63)
	if err != nil {
		t.Fatalf("event 63 missing: %v", err)
	}
	if detail.Meta.Source != "official_cn" {
		t.Fatalf("title-only events ended the official scan: event 63 source = %q", detail.Meta.Source)
	}
}

func TestPartiallyTranslatedOfficialEpisodeKeepsUntranslatedLines(t *testing.T) {
	_, events, cfg := openTestTranslator(t)
	jpAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ScenarioId":"scenario","Snippets":[],"TalkData":[
			{"Body":"一","WindowDisplayName":"角色"},
			{"Body":"二","WindowDisplayName":"角色"},
			{"Body":"三","WindowDisplayName":"角色"}],"SpecialEffectData":[],"AppearCharacters":[]}`)
	}))
	defer jpAssets.Close()
	cnAssets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"TalkData":[
			{"Body":"一译","WindowDisplayName":"角色中"},
			{"Body":"","WindowDisplayName":"角色中"},
			{"Body":"","WindowDisplayName":"角色中"}]}`)
	}))
	defer cnAssets.Close()
	if err := cfg.Set(config.KeyUpstreamJPAssetsURL, jpAssets.URL); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(config.KeyUpstreamCNAssetsURL, cnAssets.URL); err != nil {
		t.Fatal(err)
	}
	local := New(nil, events, cfg)
	jpStory := map[string]any{
		"assetbundleName":    "asset",
		"eventStoryEpisodes": []any{map[string]any{"episodeNo": float64(1), "scenarioId": "scenario", "title": "原題"}},
	}
	cnStory := map[string]any{
		"eventStoryEpisodes": []any{map[string]any{"episodeNo": float64(1), "title": "中文标题"}},
	}
	episodes, hasTalk, _, errs := local.buildOfficialCNEpisodes(jpStory, cnStory)
	if len(errs) != 0 || !hasTalk {
		t.Fatalf("official episode errors=%v hasTalk=%v", errs, hasTalk)
	}
	if err := events.ImportOrdered(77, model.EventStoryMeta{Source: "official_cn"},
		toOrderedEpisodes(episodes, "cn")); err != nil {
		t.Fatal(err)
	}
	ordered, err := events.OrderedDetail(77)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered.Episodes) != 1 {
		t.Fatalf("episodes = %+v", ordered.Episodes)
	}
	bodies := 0
	for _, key := range ordered.Episodes[0].TalkKeys {
		if key == "一" || key == "二" || key == "三" {
			bodies++
		}
	}
	if bodies != 3 {
		t.Fatalf("legacy lines dropped untranslated bodies: keys=%v", ordered.Episodes[0].TalkKeys)
	}
	targets, err := events.UntranslatedTargets(77)
	if err != nil {
		t.Fatal(err)
	}
	var targetKeys []string
	for _, target := range targets {
		targetKeys = append(targetKeys, target.JP)
	}
	if len(targetKeys) != 2 || targetKeys[0] != "二" || targetKeys[1] != "三" {
		t.Fatalf("AI gap-fill targets = %v, want [二 三]", targetKeys)
	}
}
