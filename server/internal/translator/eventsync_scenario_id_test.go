package translator

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func TestAdoptRequestedEventScenarioIDOnlyTakesALowerEventLabelOfTheSameEpisode(t *testing.T) {
	for _, tc := range []struct {
		own, requested string
		adopted        bool
	}{
		{"event_167_02", "event_168_02", true},
		{"event_172_08", "event_174_08", true},
		{"event_168_02", "event_168_02", false},
		{"event_169_02", "event_168_02", false},
		{"event_167_03", "event_168_02", false},
		{"wl_wonder_02_01", "event_168_01", false},
		{"event_167_01", "wl_wonder_02_01", false},
		{"", "event_168_01", false},
	} {
		original := map[string]any{"ScenarioId": tc.own, "TalkData": []any{}}
		got := adoptRequestedEventScenarioID(original, tc.requested).(map[string]any)
		want := tc.own
		if tc.adopted {
			want = tc.requested
		}
		if got["ScenarioId"] != want {
			t.Errorf("%q fetched as %q: ScenarioId = %v, want %q", tc.own, tc.requested, got["ScenarioId"], want)
		}
		if original["ScenarioId"] != tc.own {
			t.Errorf("%q fetched as %q: the fetched object was modified", tc.own, tc.requested)
		}
	}
	if got := adoptRequestedEventScenarioID([]any{}, "event_168_01"); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("non-object scenario = %#v", got)
	}
}

// laggedScenarioUpstream serves event 168 like the official assets: every file
// is fetched by event_168_<ep> but labels itself with ownLabel(ep).
func laggedScenarioUpstream(t *testing.T, ownLabel func(ep string) string, withOfficialCN bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/jp-master/eventStories.json":
			fmt.Fprint(w, `[{"eventId":168,"assetbundleName":"event_cheerheart_2025","eventStoryEpisodes":[
				{"episodeNo":1,"scenarioId":"event_168_01","title":"集うアイドル達"},
				{"episodeNo":2,"scenarioId":"event_168_02","title":"真のアイドル"}]}]`)
		case r.URL.Path == "/cn-master/eventStories.json":
			if !withOfficialCN {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprint(w, `[{"eventId":168,"eventStoryEpisodes":[{"episodeNo":1,"title":"齐聚的偶像们"},{"episodeNo":2,"title":"真正的偶像"}]}]`)
		case r.URL.Path == "/cn-master/events.json":
			if !withOfficialCN {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprint(w, `[{"id":168}]`)
		case strings.HasPrefix(r.URL.Path, "/jp-assets/event_story/event_cheerheart_2025/scenario/event_168_"):
			ep := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jp-assets/event_story/event_cheerheart_2025/scenario/event_168_"), ".json")
			fmt.Fprintf(w, `{"ScenarioId":%q,"Snippets":[{"Action":1,"ReferenceIndex":0}],"TalkData":[{"Body":"げんぶん%s","WindowDisplayName":"遥","Voices":[]}],"SpecialEffectData":[],"AppearCharacters":[]}`,
				ownLabel(ep), ep)
		case strings.HasPrefix(r.URL.Path, "/cn-assets/event_story/event_cheerheart_2025/scenario/event_168_"):
			ep := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/cn-assets/event_story/event_cheerheart_2025/scenario/event_168_"), ".json")
			fmt.Fprintf(w, `{"ScenarioId":%q,"TalkData":[{"Body":"译文%s","WindowDisplayName":"遥"}]}`, ownLabel(ep), ep)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestOfficialCNImportStoresAnEventFileLabelledOneEventLowerUnderItsMasterScenarioID(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	upstream := laggedScenarioUpstream(t, func(ep string) string { return "event_167_" + ep }, true)
	configureLocalSources(t, cfg, upstream.URL)

	outcome, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Processed != 1 || len(outcome.PartialErrors) != 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
	detail, err := events.Detail(168)
	if err != nil || detail.Meta.Source != "official_cn" {
		t.Fatalf("event 168 detail=%+v err=%v", detail.Meta, err)
	}
	for _, ep := range []string{"1", "2"} {
		snapshot, err := events.EpisodeSnapshot(168, ep, model.LocaleChinese)
		want := "event_168_0" + ep
		if err != nil || snapshot.Scenario.ScenarioID != want || !strings.Contains(snapshot.Scenario.RawJSON, `"ScenarioId":"`+want+`"`) {
			t.Fatalf("episode %s snapshot=%+v err=%v", ep, snapshot.Scenario, err)
		}
	}
}

func TestScenarioBackfillAdoptsTheMasterIDOfAnEventFileLabelledOneEventLower(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(168, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{
		{EpisodeNo: "1", ScenarioID: "event_168_01", Title: "齐聚的偶像们", TitleSource: model.SourceHuman,
			TalkKeys: []string{"げんぶん01"}, TalkData: map[string]string{"げんぶん01": "人工译文一"}, TalkSources: map[string]string{"げんぶん01": model.SourceHuman}},
		{EpisodeNo: "2", ScenarioID: "event_168_02", Title: "真正的偶像", TitleSource: model.SourceHuman,
			TalkKeys: []string{"げんぶん02"}, TalkData: map[string]string{"げんぶん02": "人工译文二"}, TalkSources: map[string]string{"げんぶん02": model.SourceHuman}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := events.Detail(168)
	if err != nil {
		t.Fatal(err)
	}
	upstream := laggedScenarioUpstream(t, func(ep string) string { return "event_167_" + ep }, false)
	configureLocalSources(t, cfg, upstream.URL)

	outcome, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.PartialErrors) != 0 {
		t.Fatalf("backfill errors = %v", outcome.PartialErrors)
	}
	if missing, err := events.MissingScenarioEpisodes(168); err != nil || len(missing) != 0 {
		t.Fatalf("missing scenarios=%+v err=%v", missing, err)
	}
	after, err := events.Detail(168)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("backfill changed translations: after=%+v before=%+v err=%v", after, before, err)
	}
	snapshot, err := events.EpisodeSnapshot(168, "2", model.LocaleChinese)
	if err != nil || snapshot.Scenario.ScenarioID != "event_168_02" || snapshot.Segments[1].Text != "人工译文二" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestScenarioBackfillStillRejectsAFileLabelledWithAnotherEpisode(t *testing.T) {
	tr, events, cfg := openTestTranslator(t)
	if err := events.ImportOrdered(168, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{
		{EpisodeNo: "1", ScenarioID: "event_168_01", Title: "齐聚的偶像们", TitleSource: model.SourceHuman,
			TalkKeys: []string{"げんぶん01"}, TalkData: map[string]string{"げんぶん01": "人工译文一"}, TalkSources: map[string]string{"げんぶん01": model.SourceHuman}},
		{EpisodeNo: "2", ScenarioID: "event_168_02", Title: "真正的偶像", TitleSource: model.SourceHuman,
			TalkKeys: []string{"げんぶん02"}, TalkData: map[string]string{"げんぶん02": "人工译文二"}, TalkSources: map[string]string{"げんぶん02": model.SourceHuman}},
	}); err != nil {
		t.Fatal(err)
	}
	upstream := laggedScenarioUpstream(t, func(string) string { return "event_167_05" }, false)
	configureLocalSources(t, cfg, upstream.URL)

	outcome, err := tr.syncEventStoriesCNOnly(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.PartialErrors) == 0 || !strings.Contains(fmt.Sprint(outcome.PartialErrors), "ScenarioId mismatch") {
		t.Fatalf("backfill errors = %v", outcome.PartialErrors)
	}
	if missing, err := events.MissingScenarioEpisodes(168); err != nil || len(missing) != 2 {
		t.Fatalf("missing scenarios=%+v err=%v", missing, err)
	}
}
