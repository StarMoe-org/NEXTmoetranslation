package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var sideStoryBackupTables = []struct{ name, order string }{
	{"side_stories", "kind,story_id"},
	{"side_story_episodes", "kind,story_id,episode_key"},
	{"side_story_lines", "kind,story_id,episode_key,jp_key"},
	{"side_story_line_localizations", "kind,story_id,episode_key,jp_key,locale"},
}

// sideStoryTableDump renders every column of every side-story row with its
// driver type, so equal dumps mean byte-for-byte equal tables.
func sideStoryTableDump(t *testing.T, s *Store) []string {
	t.Helper()
	var dump []string
	for _, table := range sideStoryBackupTables {
		rows, err := s.db.Query(`SELECT * FROM ` + table.name + ` ORDER BY ` + table.order)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			parts := []string{table.name}
			for index, value := range values {
				parts = append(parts, fmt.Sprintf("%s=%T:%q", columns[index], value, fmt.Sprint(value)))
			}
			dump = append(dump, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return dump
}

func compareSideStoryDumps(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("side-story rows=%d want %d\ngot:\n%s\nwant:\n%s", len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("side-story row %d\ngot:  %s\nwant: %s", index, got[index], want[index])
		}
	}
}

// seedSideStoryBackupFixture covers every episode state, an unfetched and a
// re-fetch episode, a JP failure, official/llm/human rows, a human empty row
// and a revision above 1.
func seedSideStoryBackupFixture(t *testing.T, s *Store) {
	t.Helper()
	card := sideStoryTestCard("100", 1790000000000, "character/member/test_res_100", "character/member_scenario/test_res_100")
	card.Episodes[0].CNTitle = "测试标题一"
	plain := sideStoryTestCard("200", 1790000000001, "", "")
	area := sideStoryTestArea("test_area_001", 501, 1790000000002)
	area.Episodes[0].CNAssetPath = "scenario/actionset/group5/test_area_001"
	area.Episodes[0].ENAssetPath = "scenario/actionset/group5/test_area_001"
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, card, plain)
	mustSyncSideStoryCatalog(t, s, SideStoryKindArea, area)

	jp := sideStoryTestScript(t, "test_card_100_01",
		sideStoryTestTalk{"テスト話者", "テスト台詞一"}, sideStoryTestTalk{"テスト相手", "テスト台詞二"},
		sideStoryTestTalk{"テスト話者", "テスト台詞三"})
	cn := sideStoryTestScript(t, "test_card_100_01",
		sideStoryTestTalk{"测试说话人", "测试台词一"}, sideStoryTestTalk{"测试对象", "测试台词二"},
		sideStoryTestTalk{"测试说话人", "测试台词三"})
	en := sideStoryTestScript(t, "test_card_100_01", sideStoryTestTalk{"Test speaker", "Test line"})
	mustApplySideStory(t, s, sideStoryTestNow,
		SideStoryEpisodeFetch{Kind: "card", StoryID: "100", EpisodeKey: "1", JP: fetchedJP(jp), CN: fetchedJP(cn), EN: fetchedJP(en)},
		SideStoryEpisodeFetch{Kind: "card", StoryID: "100", EpisodeKey: "2",
			JP: SideStoryFetchOutcome{Attempted: true, Err: "测试错误", Transient: true}},
		SideStoryEpisodeFetch{Kind: "card", StoryID: "200", EpisodeKey: "1",
			JP: fetchedJP(sideStoryTestScript(t, "test_card_200_01", sideStoryTestTalk{"テスト話者", "テスト台詞四"}))},
		SideStoryEpisodeFetch{Kind: "area", StoryID: "test_area_001", EpisodeKey: "1",
			JP: fetchedJP(sideStoryTestScript(t, "test_area_001", sideStoryTestTalk{"テスト話者", "テスト台詞五"})),
			CN: SideStoryFetchOutcome{Attempted: true, Err: "测试错误"},
			EN: SideStoryFetchOutcome{Attempted: true, Missing: true}})
	plain.Episodes[0].JPAssetPath = "character/member/test_res_200_moved/test_card_200_01"
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, plain)

	later := sideStoryTestNow.Add(time.Hour)
	for _, edit := range []struct {
		locale string
		edit   SideStoryLineEdit
	}{
		{"zh-CN", SideStoryLineEdit{JP: "テスト台詞二", Text: ""}},
		{"en-US", SideStoryLineEdit{JP: "テスト台詞一", Text: "Test line one"}},
		{"en-US", SideStoryLineEdit{JP: "テスト台詞一", Text: "Test line one, revised"}},
	} {
		if _, err := s.UpdateSideStoryLinesContext(context.Background(), "card", "100", "1", edit.locale, "test-editor",
			[]SideStoryLineEdit{edit.edit}, later); err != nil {
			t.Fatal(err)
		}
	}
	targets, err := s.SideStoryAITargetsContext(context.Background(), "card", "200", "en-US", "1")
	if err != nil || len(targets) == 0 {
		t.Fatalf("ai targets=%v err=%v", targets, err)
	}
	texts := make([]string, len(targets))
	for index := range texts {
		texts[index] = fmt.Sprintf("Test AI line %d", index)
	}
	if _, err := s.ApplySideStoryAITranslationsContext(context.Background(), "card", "200", "en-US", targets, texts, later); err != nil {
		t.Fatal(err)
	}
}

func requireSideStoryBackupFixtureCoverage(t *testing.T, content SideStoryContentExport) {
	t.Helper()
	covered := map[string]bool{}
	for _, episode := range content.Episodes {
		covered["cn:"+episode.CNState] = true
		covered["en:"+episode.ENState] = true
		covered["fetched"] = covered["fetched"] || episode.ScriptSHA256 != ""
		covered["unfetched"] = covered["unfetched"] || episode.ScriptSHA256 == ""
		covered["refetch"] = covered["refetch"] || episode.JPRefetch
		covered["failure"] = covered["failure"] || (episode.Attempts > 0 && episode.LastError != "" && episode.NextAttemptAt > 0)
	}
	for _, row := range content.Localizations {
		covered["source:"+row.Source] = true
		covered["human empty"] = covered["human empty"] || (row.Source == SideStorySourceHuman && row.Text == "")
		covered["revision>1"] = covered["revision>1"] || row.Revision > 1
		covered["locale:"+row.Locale] = true
	}
	for _, line := range content.Lines {
		covered["role:"+line.Role] = true
	}
	for _, want := range []string{"cn:imported", "cn:error", "cn:pending", "en:mismatch", "en:absent", "en:pending",
		"fetched", "unfetched", "refetch", "failure", "source:official", "source:llm", "source:human", "human empty",
		"revision>1", "locale:zh-CN", "locale:en-US", "role:title", "role:talk", "role:speaker"} {
		if !covered[want] {
			t.Fatalf("fixture does not cover %q: %+v", want, content)
		}
	}
}

func restoreSideStoriesForTest(s *Store, content SideStoryContentExport) error {
	return s.RestoreBackupWithSideStoriesContext(context.Background(), restoreContractCategories(), nil, nil,
		EventContentExport{}, LyricsContentExport{}, content, true, "operator")
}

func TestSideStoryBackupRestoresEveryRowExactly(t *testing.T) {
	source := newSideStoryTestStore(t)
	seedSideStoryBackupFixture(t, source)
	want := sideStoryTableDump(t, source)
	exported, err := source.ExportSideStoryContentContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireSideStoryBackupFixtureCoverage(t, exported)
	body, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SideStoryContentExport
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}

	restored := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, restored, SideStoryKindCard, sideStoryTestCard("300", 1, "", ""))
	if err := restoreSideStoriesForTest(restored, decoded); err != nil {
		t.Fatal(err)
	}
	compareSideStoryDumps(t, sideStoryTableDump(t, restored), want)
	again, err := restored.ExportSideStoryContentContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	againBody, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(againBody) != string(body) {
		t.Fatalf("second export differs\ngot:  %s\nwant: %s", againBody, body)
	}

	imported := newSideStoryTestStore(t)
	if err := imported.ImportTranslationContentWithSideStoriesContext(context.Background(), nil, EventContentExport{},
		LyricsContentExport{}, decoded); err != nil {
		t.Fatal(err)
	}
	compareSideStoryDumps(t, sideStoryTableDump(t, imported), want)
}

func TestRestoreWithoutSideStoriesClearsTheSideStoryTables(t *testing.T) {
	for _, test := range []struct {
		name    string
		restore func(*Store) error
	}{
		{"additive restore", func(s *Store) error {
			return s.RestoreBackupContext(context.Background(), restoreContractCategories(), nil, nil,
				EventContentExport{}, LyricsContentExport{}, true, "operator")
		}},
		{"legacy restore", func(s *Store) error {
			return s.RestoreBackupContext(context.Background(), restoreContractCategories(), nil, nil,
				EventContentExport{}, LyricsContentExport{}, false, "operator")
		}},
		{"legacy restore ignores side stories", func(s *Store) error {
			exported, err := s.ExportSideStoryContentContext(context.Background())
			if err != nil {
				return err
			}
			return s.RestoreBackupWithSideStoriesContext(context.Background(), restoreContractCategories(), nil, nil,
				EventContentExport{}, LyricsContentExport{}, exported, false, "operator")
		}},
		{"content import", func(s *Store) error {
			return s.ImportTranslationContentContext(context.Background(), nil, EventContentExport{}, LyricsContentExport{})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newSideStoryTestStore(t)
			seedSideStoryBackupFixture(t, s)
			if len(sideStoryTableDump(t, s)) == 0 {
				t.Fatal("fixture wrote no side-story rows")
			}
			if err := test.restore(s); err != nil {
				t.Fatal(err)
			}
			if dump := sideStoryTableDump(t, s); len(dump) != 0 {
				t.Fatalf("restore without side stories kept rows:\n%s", strings.Join(dump, "\n"))
			}
		})
	}
}

func cloneSideStoryContentExport(content SideStoryContentExport) SideStoryContentExport {
	return SideStoryContentExport{
		Stories:       append([]SideStoryBackupRecord(nil), content.Stories...),
		Episodes:      append([]SideStoryEpisodeBackupRecord(nil), content.Episodes...),
		Lines:         append([]SideStoryLineBackupRecord(nil), content.Lines...),
		Localizations: append([]SideStoryLocalizationBackupRecord(nil), content.Localizations...),
	}
}

func TestSideStoryRestoreRejectsTamperedRowsAndLeavesTheDatabaseUnchanged(t *testing.T) {
	source := newSideStoryTestStore(t)
	seedSideStoryBackupFixture(t, source)
	exported, err := source.ExportSideStoryContentContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	destination := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, destination, SideStoryKindCard, sideStoryTestCard("300", 1, "", ""))
	mustApplySideStory(t, destination, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "300", EpisodeKey: "1",
		JP: fetchedJP(sideStoryTestScript(t, "test_card_300_01", sideStoryTestTalk{"テスト話者", "テスト台詞六"}))})
	mustUpdateSideStory(t, destination, "card", "300", "1", "zh-CN", SideStoryLineEdit{JP: "テスト台詞六", Text: "测试台词六"})
	before := sideStoryTableDump(t, destination)

	firstEpisode := func(content SideStoryContentExport, kind string) int {
		for index, episode := range content.Episodes {
			if episode.Kind == kind {
				return index
			}
		}
		t.Fatalf("fixture has no %s episode", kind)
		return -1
	}
	card := -1
	for index, story := range exported.Stories {
		if story.Kind == SideStoryKindCard && story.StoryID == "100" {
			card = index
		}
	}
	if card < 0 {
		t.Fatal("fixture has no card 100")
	}
	for _, test := range []struct {
		name, want string
		mutate     func(*SideStoryContentExport)
	}{
		{"unknown kind", "invalid kind or id", func(c *SideStoryContentExport) { c.Stories[0].Kind = "event" }},
		{"non-canonical card id", "invalid kind or id", func(c *SideStoryContentExport) { c.Stories[card].StoryID = "0100" }},
		{"repeated story", "story card/100 is repeated", func(c *SideStoryContentExport) { c.Stories = append(c.Stories, c.Stories[card]) }},
		{"orphan episode", "has no story", func(c *SideStoryContentExport) { c.Episodes[0].StoryID = "999" }},
		{"card episode key", "invalid episode key", func(c *SideStoryContentExport) {
			c.Episodes[firstEpisode(*c, "card")].EpisodeKey = "3"
		}},
		{"area scenario id", "invalid scenario id", func(c *SideStoryContentExport) {
			c.Episodes[firstEpisode(*c, "area")].ScenarioID = "test_area_002"
		}},
		{"episode state", "invalid state", func(c *SideStoryContentExport) { c.Episodes[0].CNState = "done" }},
		{"script sha256", "invalid script sha256", func(c *SideStoryContentExport) {
			c.Episodes[0].ScriptSHA256 = strings.Repeat("A", 64)
		}},
		{"orphan line", "has no episode", func(c *SideStoryContentExport) { c.Lines[0].EpisodeKey = "2"; c.Lines[0].StoryID = "999" }},
		{"untrimmed jp key", "invalid jp key", func(c *SideStoryContentExport) { c.Lines[0].JPKey += "\u3000" }},
		{"line role", "invalid role", func(c *SideStoryContentExport) { c.Lines[0].Role = "narration" }},
		{"title position", "invalid role", func(c *SideStoryContentExport) {
			for index := range c.Lines {
				if c.Lines[index].Role == SideStoryRoleTitle {
					c.Lines[index].Position = 0
					return
				}
			}
		}},
		{"bad locale", "invalid locale", func(c *SideStoryContentExport) { c.Localizations[0].Locale = "ja-JP" }},
		{"orphan localization", "has no line", func(c *SideStoryContentExport) { c.Localizations[0].JPKey = "テスト孤立行" }},
		{"revision 0", "invalid revision 0", func(c *SideStoryContentExport) { c.Localizations[0].Revision = 0 }},
		{"source", "invalid source", func(c *SideStoryContentExport) { c.Localizations[0].Source = "official_cn" }},
		{"repeated localization", "is repeated", func(c *SideStoryContentExport) {
			c.Localizations = append(c.Localizations, c.Localizations[0])
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tampered := cloneSideStoryContentExport(exported)
			test.mutate(&tampered)
			err := restoreSideStoriesForTest(destination, tampered)
			if err == nil || !strings.Contains(err.Error(), "side story backup: ") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("restore error=%v, want %q", err, test.want)
			}
			compareSideStoryDumps(t, sideStoryTableDump(t, destination), before)
		})
	}
	if err := restoreSideStoriesForTest(destination, exported); err != nil {
		t.Fatalf("the untampered backup must restore: %v", err)
	}
}
