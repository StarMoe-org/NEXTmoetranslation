package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestSideStoryPublicFilesProjectTranslatedTalkBySource(t *testing.T) {
	s := newSideStoryTestStore(t)
	ctx := context.Background()
	seedSideStoryCard(t, s, "400", 1)
	seedSideStoryCard(t, s, "401", 1)
	seedSideStoryCard(t, s, "402", 1)
	script := sideStoryTestScript(t, "test_card_402_02", sideStoryTestTalk{"", "テスト台詞丙"})
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "402", EpisodeKey: "2", JP: fetchedJP(script)})

	mustUpdateSideStory(t, s, "card", "400", "1", "zh-CN",
		SideStoryLineEdit{JP: "テスト台詞乙", Text: "测试乙"}, SideStoryLineEdit{JP: "テスト話者", Text: "测试说话人"},
		SideStoryLineEdit{JP: "テスト話400-1", Text: "测试标题一"}, SideStoryLineEdit{JP: "テスト台詞甲", Text: ""})
	mustUpdateSideStory(t, s, "card", "400", "2", "zh-CN", SideStoryLineEdit{JP: "テスト話400-2", Text: "测试标题二"})
	setSideStorySource(t, s, "400", "zh-CN", "official", "テスト台詞乙", "テスト話者", "テスト話400-1", "テスト話400-2")
	mustUpdateSideStory(t, s, "card", "400", "1", "en-US", SideStoryLineEdit{JP: "テスト台詞甲", Text: "Test line A"})
	setSideStorySource(t, s, "400", "en-US", "official", "テスト台詞甲")
	mustUpdateSideStory(t, s, "card", "401", "1", "zh-CN", SideStoryLineEdit{JP: "テスト話401-1", Text: "测试标题"})
	mustUpdateSideStory(t, s, "card", "401", "2", "zh-CN", SideStoryLineEdit{JP: "テスト話401-2", Text: "测试标题"})
	mustUpdateSideStory(t, s, "card", "402", "1", "zh-CN",
		SideStoryLineEdit{JP: "テスト台詞甲", Text: "测试甲"}, SideStoryLineEdit{JP: "テスト台詞乙", Text: "测试乙", Source: "llm"})
	setSideStorySource(t, s, "402", "zh-CN", "official", "テスト台詞甲")
	if _, err := s.UpdateSideStoryLinesContext(ctx, "card", "402", "2", "zh-CN", "test-editor",
		[]SideStoryLineEdit{{JP: "テスト台詞丙", Text: "测试丙"}}, sideStoryTestNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	files, err := s.SideStoryPublicFilesContext(ctx, "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]SideStoryPublicFile{
		"cardStory/card_400.json": {Source: "official_cn", LastUpdated: sideStoryTestNow.Unix(), Episodes: []SideStoryPublicEpisode{{
			Key: "1", ScenarioID: "test_card_400_01", Title: "测试标题一", Source: "official_cn",
			Talk: []SideStoryPublicTalk{{JP: "テスト話者", Text: "测试说话人"}, {JP: "テスト台詞乙", Text: "测试乙"}},
		}}},
		"cardStory/card_402.json": {Source: "human", LastUpdated: sideStoryTestNow.Add(time.Hour).Unix(), Episodes: []SideStoryPublicEpisode{
			{Key: "1", ScenarioID: "test_card_402_01", Source: "llm",
				Talk: []SideStoryPublicTalk{{JP: "テスト台詞甲", Text: "测试甲"}, {JP: "テスト台詞乙", Text: "测试乙"}}},
			{Key: "2", ScenarioID: "test_card_402_02", Source: "human", Talk: []SideStoryPublicTalk{{JP: "テスト台詞丙", Text: "测试丙"}}},
		}},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("zh-CN files\n got %+v\nwant %+v", files, want)
	}
	english, err := s.SideStoryPublicFilesContext(ctx, "en-US")
	if err != nil {
		t.Fatal(err)
	}
	if file := english["cardStory/card_400.json"]; len(english) != 1 || file.Source != "official_en" || file.Episodes[0].Source != "official_en" ||
		file.Episodes[0].Title != "" || !reflect.DeepEqual(file.Episodes[0].Talk, []SideStoryPublicTalk{{JP: "テスト台詞甲", Text: "Test line A"}}) {
		t.Fatalf("en-US files %+v", english)
	}

	for _, id := range []string{"400", "402"} {
		key, file, ok, err := s.SideStoryPublicFileForStoryContext(ctx, "card", id, "zh-CN")
		if err != nil || !ok || key != "cardStory/card_"+id+".json" || !reflect.DeepEqual(file, want[key]) {
			t.Fatalf("card %s file key=%q ok=%v err=%v file=%+v", id, key, ok, err, file)
		}
	}
	if key, _, ok, err := s.SideStoryPublicFileForStoryContext(ctx, "card", "401", "zh-CN"); err != nil || ok || key != "cardStory/card_401.json" {
		t.Fatalf("title-only card key=%q ok=%v err=%v", key, ok, err)
	}
	if _, _, _, err := s.SideStoryPublicFileForStoryContext(ctx, "card", "499", "zh-CN"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown card error=%v", err)
	}
	if _, err := s.SideStoryPublicFilesContext(ctx, "ja-JP"); !errors.Is(err, ErrSideStoryInvalid) {
		t.Fatalf("ja-JP files error=%v", err)
	}
}

func TestSideStoryPublicAreaFilesGroupByActionSetHundreds(t *testing.T) {
	s := newSideStoryTestStore(t)
	ctx := context.Background()
	mustSyncSideStoryCatalog(t, s, SideStoryKindArea,
		sideStoryTestArea("areatalk_test_b", 550, 1), sideStoryTestArea("areatalk_test_a", 599, 1),
		sideStoryTestArea("areatalk_test_c", 501, 1), sideStoryTestArea("areatalk_test_d", 600, 1),
		sideStoryTestArea("areatalk_test_e", 520, 1))
	for _, id := range []string{"areatalk_test_a", "areatalk_test_b", "areatalk_test_c", "areatalk_test_d", "areatalk_test_e"} {
		script := sideStoryTestScript(t, id, sideStoryTestTalk{"テスト話者", "テスト台詞" + id})
		mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "area", StoryID: id, EpisodeKey: "1", JP: fetchedJP(script)})
		if id == "areatalk_test_e" {
			continue
		}
		mustUpdateSideStory(t, s, "area", id, "1", "zh-CN", SideStoryLineEdit{JP: "テスト台詞" + id, Text: "测试" + id})
	}
	files, err := s.SideStoryPublicFilesContext(ctx, "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("area files %+v", files)
	}
	group := files["areaTalk/group_5.json"]
	var keys []string
	for _, episode := range group.Episodes {
		keys = append(keys, episode.Key+"="+episode.ScenarioID)
		if episode.Source != "human" || len(episode.Talk) != 1 || episode.Talk[0].Text != "测试"+episode.Key {
			t.Fatalf("area episode %+v", episode)
		}
	}
	if !reflect.DeepEqual(keys, []string{"areatalk_test_c=areatalk_test_c", "areatalk_test_b=areatalk_test_b", "areatalk_test_a=areatalk_test_a"}) ||
		group.Source != "human" {
		t.Fatalf("group 5 %v source=%q", keys, group.Source)
	}
	if other := files["areaTalk/group_6.json"]; len(other.Episodes) != 1 || other.Episodes[0].Key != "areatalk_test_d" {
		t.Fatalf("group 6 %+v", other)
	}
	for _, id := range []string{"areatalk_test_b", "areatalk_test_e"} {
		key, file, ok, err := s.SideStoryPublicFileForStoryContext(ctx, "area", id, "zh-CN")
		if err != nil || !ok || key != "areaTalk/group_5.json" || !reflect.DeepEqual(file, group) {
			t.Fatalf("%s file key=%q ok=%v err=%v file=%+v", id, key, ok, err, file)
		}
	}
}
