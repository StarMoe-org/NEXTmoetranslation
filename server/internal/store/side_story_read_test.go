package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// seedSideStoryCard stores card id whose first episode is fetched with the
// lines テスト台詞甲 (speaker テスト話者) and テスト台詞乙; the second episode
// only has its title line.
func seedSideStoryCard(t *testing.T, s *Store, id string, releasedAt int64) {
	t.Helper()
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard(id, releasedAt, "", ""))
	script := sideStoryTestScript(t, "test_card_"+id+"_01",
		sideStoryTestTalk{"テスト話者", "テスト台詞甲"}, sideStoryTestTalk{"", "テスト台詞乙"})
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: id, EpisodeKey: "1", JP: fetchedJP(script)})
}

func setSideStorySource(t *testing.T, s *Store, storyID, locale, source string, jpKeys ...string) {
	t.Helper()
	for _, jp := range jpKeys {
		if _, err := s.db.Exec(`UPDATE side_story_line_localizations SET source=? WHERE story_id=? AND locale=? AND jp_key=?`,
			source, storyID, locale, jp); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListSideStoriesCountsLinesStatusAndPrimarySource(t *testing.T) {
	s := newSideStoryTestStore(t)
	seedSideStoryCard(t, s, "300", 3000)
	seedSideStoryCard(t, s, "301", 2000)
	seedSideStoryCard(t, s, "302", 1000)
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("303", 500, "", ""))
	edit := func(jp, text string) SideStoryLineEdit { return SideStoryLineEdit{JP: jp, Text: text} }
	mustUpdateSideStory(t, s, "card", "300", "1", "zh-CN",
		edit("テスト話300-1", "测试标题"), edit("テスト台詞甲", "测试甲"),
		edit("テスト話者", "测试说话人"), edit("テスト台詞乙", "测试乙"))
	later := sideStoryTestNow.Add(time.Hour)
	if _, err := s.UpdateSideStoryLinesContext(context.Background(), "card", "300", "2", "zh-CN", "test-editor",
		[]SideStoryLineEdit{{JP: "テスト話300-2", Text: "测试标题二", Source: "llm"}}, later); err != nil {
		t.Fatal(err)
	}
	setSideStorySource(t, s, "300", "zh-CN", "official", "テスト話300-1", "テスト台詞甲")
	mustUpdateSideStory(t, s, "card", "301", "1", "zh-CN",
		edit("テスト話301-1", "测试标题"), edit("テスト台詞甲", "测试甲"),
		edit("テスト話者", "测试说话人"), edit("テスト台詞乙", "测试乙"))
	setSideStorySource(t, s, "301", "zh-CN", "official", "テスト台詞甲", "テスト台詞乙")
	setSideStorySource(t, s, "301", "zh-CN", "llm", "テスト話301-1", "テスト話者")
	mustUpdateSideStory(t, s, "card", "302", "1", "zh-CN", edit("テスト台詞甲", ""))
	mustUpdateSideStory(t, s, "card", "302", "1", "en-US", SideStoryLineEdit{JP: "テスト台詞甲", Text: "Test line A", Source: "llm"})

	list, err := s.ListSideStoriesContext(context.Background(), "card", "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	want := []SideStorySummary{
		{Kind: "card", ID: "300", Title: "テスト称号300", CharacterID: 1, ReleasedAt: 3000, EpisodeCount: 2, FetchedEpisodeCount: 1,
			LineCount: 5, TranslatedCount: 5, SourceCounts: SideStorySourceCounts{Official: 2, LLM: 1, Human: 2},
			PrimarySource: "human", Status: "translated", UpdatedAt: later.Unix()},
		{Kind: "card", ID: "301", Title: "テスト称号301", CharacterID: 1, ReleasedAt: 2000, EpisodeCount: 2, FetchedEpisodeCount: 1,
			LineCount: 5, TranslatedCount: 4, UntranslatedCount: 1, SourceCounts: SideStorySourceCounts{Official: 2, LLM: 2},
			PrimarySource: "official", Status: "partial", UpdatedAt: sideStoryTestNow.Unix()},
		{Kind: "card", ID: "302", Title: "テスト称号302", CharacterID: 1, ReleasedAt: 1000, EpisodeCount: 2, FetchedEpisodeCount: 1,
			LineCount: 5, UntranslatedCount: 5, Status: "untranslated", UpdatedAt: sideStoryTestNow.Unix()},
		{Kind: "card", ID: "303", Title: "テスト称号303", CharacterID: 1, ReleasedAt: 500, EpisodeCount: 2,
			LineCount: 2, UntranslatedCount: 2, Status: "pending", UpdatedAt: sideStoryTestNow.Unix()},
	}
	if len(list) != len(want) {
		t.Fatalf("list %+v", list)
	}
	for index := range want {
		if list[index] != want[index] {
			t.Errorf("summary %d\n got %+v\nwant %+v", index, list[index], want[index])
		}
	}
	english, err := s.ListSideStoriesContext(context.Background(), "card", "en-US")
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range english {
		if summary.ID == "302" && (summary.Status != "partial" || summary.PrimarySource != "llm" || summary.TranslatedCount != 1) {
			t.Fatalf("en-US summary %+v", summary)
		}
		if summary.ID == "300" && summary.TranslatedCount != 0 {
			t.Fatalf("zh-CN rows counted for en-US: %+v", summary)
		}
	}
	if list, err := s.ListSideStoriesContext(context.Background(), "area", "zh-CN"); err != nil || len(list) != 0 {
		t.Fatalf("area list %+v err=%v", list, err)
	}
	if _, err := s.ListSideStoriesContext(context.Background(), "card", "ja-JP"); !errors.Is(err, ErrSideStoryInvalid) {
		t.Fatalf("ja-JP list error=%v", err)
	}
	if _, err := s.ListSideStoriesContext(context.Background(), "event", "zh-CN"); !errors.Is(err, ErrSideStoryInvalid) {
		t.Fatalf("event list error=%v", err)
	}
}

func TestSideStoryPrimarySourceBreaksTiesHumanOfficialLLM(t *testing.T) {
	for _, test := range []struct {
		counts SideStorySourceCounts
		want   string
	}{
		{SideStorySourceCounts{}, ""},
		{SideStorySourceCounts{LLM: 1}, "llm"},
		{SideStorySourceCounts{Official: 1, LLM: 1}, "official"},
		{SideStorySourceCounts{Official: 1, LLM: 1, Human: 1}, "human"},
		{SideStorySourceCounts{Official: 3, Human: 2}, "official"},
		{SideStorySourceCounts{LLM: 4, Human: 3, Official: 3}, "llm"},
	} {
		if got := sideStoryPrimarySource(test.counts); got != test.want {
			t.Errorf("primary(%+v)=%q want %q", test.counts, got, test.want)
		}
	}
}

func TestSideStoryDetailOrdersEpisodesAndLines(t *testing.T) {
	s := newSideStoryTestStore(t)
	seedSideStoryCard(t, s, "310", 1)
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "310", EpisodeKey: "2",
		JP: SideStoryFetchOutcome{Attempted: true, Missing: true}})
	mustUpdateSideStory(t, s, "card", "310", "1", "en-US", SideStoryLineEdit{JP: "テスト台詞乙", Text: "Test line B"})
	detail, err := s.SideStoryDetailContext(context.Background(), "card", "310", "en-US")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Kind != "card" || detail.ID != "310" || detail.Title != "テスト称号310" || detail.Locale != "en-US" || len(detail.Episodes) != 2 {
		t.Fatalf("detail %+v", detail)
	}
	first, second := detail.Episodes[0], detail.Episodes[1]
	if first.Key != "1" || first.ScenarioID != "test_card_310_01" || first.Title != "テスト話310-1" || !first.Fetched ||
		len(first.ScriptSHA256) != 64 || first.CNState != "absent" || first.LastError != "" ||
		first.TranslatedCount != 1 || first.UntranslatedCount != 3 {
		t.Fatalf("first episode %+v", first)
	}
	var keys []string
	for _, line := range first.Lines {
		keys = append(keys, line.Role+":"+line.JP)
	}
	if strings.Join(keys, ",") != "title:テスト話310-1,talk:テスト台詞甲,speaker:テスト話者,talk:テスト台詞乙" {
		t.Fatalf("line order %v", keys)
	}
	if line := first.Lines[3]; line.Text != "Test line B" || line.Source != "human" || line.Revision != 1 || line.Speaker != "" {
		t.Fatalf("translated line %+v", line)
	}
	if line := first.Lines[1]; line.Source != "" || line.Revision != 0 || line.Speaker != "テスト話者" {
		t.Fatalf("line without a row %+v", line)
	}
	if second.Key != "2" || second.Fetched || second.LastError != "ja-JP: not found" || len(second.Lines) != 1 || second.UntranslatedCount != 1 {
		t.Fatalf("second episode %+v", second)
	}
	if _, err := s.SideStoryDetailContext(context.Background(), "card", "311", "zh-CN"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown story error=%v", err)
	}
	if _, err := s.SideStoryDetailContext(context.Background(), "card", "310", "ja-JP"); !errors.Is(err, ErrSideStoryInvalid) {
		t.Fatalf("ja-JP detail error=%v", err)
	}
}
