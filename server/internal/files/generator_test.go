package files

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// TestCategoryRoundTrip verifies an imported category reads back from the DB
// with identical text, source, and ids, and that the generated flat JSON
// matches the input text.
func TestCategoryRoundTrip(t *testing.T) {
	database, err := db.Open(t.TempDir() + "/roundtrip.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	s := store.New(database)
	es := store.NewEventStore(database)

	cat := model.Category{
		"prefix": {
			"こんにちは":     {Text: "你好", Source: model.SourceCN, Ids: []string{"1", "2"}},
			"A & B < C": {Text: "甲 & 乙 < 丙", Source: model.SourceHuman},
		},
	}
	if _, err := s.ImportCategory("cards", cat); err != nil {
		t.Fatal(err)
	}

	got, err := s.CategoryData("cards")
	if err != nil {
		t.Fatal(err)
	}
	for field, entries := range cat {
		for k, want := range entries {
			g := got[field][k]
			if g.Text != want.Text || g.Source != want.Source {
				t.Errorf("%s/%s: got %+v want %+v", field, k, g, want)
			}
			if len(g.Ids) != len(want.Ids) {
				t.Errorf("%s/%s ids: got %v want %v", field, k, g.Ids, want.Ids)
			}
		}
	}

	// Flat JSON keeps HTML-escaping ON (legacy category-file convention).
	g := NewGenerator(s, es, "")
	flat, err := g.CategoryFlatJSON("cards")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]map[string]string
	if err := json.Unmarshal(flat, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["prefix"]["A & B < C"] != "甲 & 乙 < 丙" {
		t.Errorf("flat text mismatch: %q", parsed["prefix"]["A & B < C"])
	}
}

// TestEventStoryOrderPreserved verifies that talk-line order survives the DB
// round-trip and that the public event JSON keeps lines in story order with
// literal &, <, > (event-file convention).
func TestEventStoryOrderPreserved(t *testing.T) {
	database, err := db.Open(t.TempDir() + "/eventorder.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	es := store.NewEventStore(database)
	meta := model.EventStoryMeta{Source: "official_cn", Version: "1.0", LastUpdated: 100}
	// Keys deliberately NOT alphabetical, to catch map-sorting regressions.
	keys := []string{"zebra", "apple", "mango & lime"}
	ep := store.OrderedEpisode{
		EpisodeNo:  "1",
		ScenarioID: "s1",
		Title:      "T",
		TalkKeys:   keys,
		TalkData:   map[string]string{"zebra": "z", "apple": "a", "mango & lime": "m"},
	}
	if err := es.ImportOrdered(7, meta, []store.OrderedEpisode{ep}); err != nil {
		t.Fatal(err)
	}

	od, err := es.OrderedDetail(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(od.Episodes) != 1 {
		t.Fatalf("episodes: got %d want 1", len(od.Episodes))
	}
	got := od.Episodes[0].TalkKeys
	for i, k := range keys {
		if got[i] != k {
			t.Errorf("talk order at %d: got %q want %q", i, got[i], k)
		}
	}

	g := NewGenerator(store.New(database), es, "")
	b, err := g.EventStoryJSON(7)
	if err != nil {
		t.Fatal(err)
	}
	// Literal ampersand, not &.
	if !containsLiteral(b, "mango & lime") {
		t.Errorf("event JSON should keep literal &: %s", b)
	}
}

func TestWriteAllContextKeepsLegacyProjectionWithoutPublishedLyrics(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "legacy-projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	s := store.New(database)
	es := store.NewEventStore(database)
	for _, category := range model.SupportedCategories {
		if _, err := s.ImportCategory(category, model.Category{"name": map[string]model.Entry{
			category + "-jp": {Text: category + "-zh", Source: model.SourceHuman},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", ChineseTitle: "新歌", EnglishTitle: "New Song", IsNewlyWrittenMusic: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "初音ミク"}}); err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveLyrics(model.SongLyrics{
		MusicID: 10, Attribution: "MoeSeka translation team",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "歌う", Chinese: "歌唱", English: "Sings",
			Segments: []model.LyricSegment{{Text: "歌う", PerformerIDs: []int{1}}},
		}},
	}, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(10, saved.Revision); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	written, err := NewGenerator(s, es, root).WriteAllContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantWritten := len(model.SupportedCategories) * 2
	if written != wantWritten {
		t.Fatalf("WriteAllContext wrote %d files, want %d legacy category files", written, wantWritten)
	}
	if _, err := os.Stat(filepath.Join(root, "translation", "lyrics")); !os.IsNotExist(err) {
		t.Fatalf("WriteAllContext unexpectedly materialized published lyrics: %v", err)
	}
}

func containsLiteral(haystack []byte, needle string) bool {
	h, n := string(haystack), needle
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

var sideStoryGeneratorNow = time.Unix(1790000000, 0)

func sideStoryGeneratorScript(t *testing.T, scenarioID string, talks ...[2]string) store.SideStoryScript {
	t.Helper()
	talkData := []any{}
	for _, talk := range talks {
		talkData = append(talkData, map[string]any{"WindowDisplayName": talk[0], "Body": talk[1]})
	}
	script, err := store.ParseSideStoryScript(map[string]any{
		"ScenarioId": scenarioID, "Snippets": []any{}, "TalkData": talkData,
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}, scenarioID)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func editSideStoryGenerator(t *testing.T, s *store.Store, kind, id, episode, locale, source, jp, text string, now time.Time) {
	t.Helper()
	if _, err := s.UpdateSideStoryLinesContext(context.Background(), kind, id, episode, locale, "test-editor",
		[]store.SideStoryLineEdit{{JP: jp, Text: text, Source: source}}, now); err != nil {
		t.Fatal(err)
	}
}

// seedSideStoryGenerator seeds test card 700 (episode 1 imported as official
// zh-CN, episode 2 edited by a human) and three test area talks: two in JP
// group 12 and one in group 13 with only a title-free empty edit.
func seedSideStoryGenerator(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.SyncSideStoryCatalogContext(ctx, store.SideStoryKindCard, []store.SideStoryCatalogStory{{
		Kind: "card", StoryID: "700", Title: "テストカード", CharacterID: 1,
		Episodes: []store.SideStoryCatalogEpisode{
			{Key: "1", ScenarioID: "test_card_700_01", TitleJP: "テスト前編", Position: 1, CNTitle: "测试前篇",
				JPAssetPath: "character/member/test_card_700/test_card_700_01", CNAssetPath: "character/member/test_card_700/test_card_700_01"},
			{Key: "2", ScenarioID: "test_card_700_02", TitleJP: "テスト後編", Position: 2,
				JPAssetPath: "character/member/test_card_700/test_card_700_02"},
		},
	}}, sideStoryGeneratorNow); err != nil {
		t.Fatal(err)
	}
	area := func(id string, actionSetID int) store.SideStoryCatalogStory {
		return store.SideStoryCatalogStory{Kind: "area", StoryID: id, Title: "テストエリア", AreaID: 5, AreaCategory: "grade1", ActionSetID: actionSetID,
			Episodes: []store.SideStoryCatalogEpisode{{Key: "1", ScenarioID: id, JPAssetPath: "scenario/actionset/group12/" + id}}}
	}
	if _, err := s.SyncSideStoryCatalogContext(ctx, store.SideStoryKindArea, []store.SideStoryCatalogStory{
		area("areatalk_test_b", 1250), area("areatalk_test_a", 1201), area("areatalk_test_c", 1300),
	}, sideStoryGeneratorNow); err != nil {
		t.Fatal(err)
	}
	jp := sideStoryGeneratorScript(t, "test_card_700_01", [2]string{"テスト話者", "テスト台詞一 & <"}, [2]string{"テスト話者", "テスト台詞二"})
	cn := sideStoryGeneratorScript(t, "test_card_700_01", [2]string{"测试说话人", "测试台词一 & <"}, [2]string{"测试说话人", "测试台词二"})
	second := sideStoryGeneratorScript(t, "test_card_700_02", [2]string{"", "テスト台詞三"})
	fetches := []store.SideStoryEpisodeFetch{
		{Kind: "card", StoryID: "700", EpisodeKey: "1", JP: store.SideStoryFetchOutcome{Attempted: true, Script: &jp},
			CN: store.SideStoryFetchOutcome{Attempted: true, Script: &cn}},
		{Kind: "card", StoryID: "700", EpisodeKey: "2", JP: store.SideStoryFetchOutcome{Attempted: true, Script: &second}},
	}
	for _, id := range []string{"areatalk_test_a", "areatalk_test_b", "areatalk_test_c"} {
		script := sideStoryGeneratorScript(t, id, [2]string{"テスト話者", "テスト区域台詞" + id})
		fetches = append(fetches, store.SideStoryEpisodeFetch{Kind: "area", StoryID: id, EpisodeKey: "1", JP: store.SideStoryFetchOutcome{Attempted: true, Script: &script}})
	}
	if _, err := s.ApplySideStoryFetchesContext(ctx, fetches, sideStoryGeneratorNow); err != nil {
		t.Fatal(err)
	}
	later := sideStoryGeneratorNow.Add(time.Hour)
	editSideStoryGenerator(t, s, "card", "700", "2", "zh-CN", "", "テスト台詞三", "测试台词三", later)
	editSideStoryGenerator(t, s, "area", "areatalk_test_b", "1", "zh-CN", "llm", "テスト区域台詞areatalk_test_b", "测试区域台词乙", later)
	editSideStoryGenerator(t, s, "area", "areatalk_test_a", "1", "zh-CN", "llm", "テスト区域台詞areatalk_test_a", "测试区域台词甲", sideStoryGeneratorNow)
	editSideStoryGenerator(t, s, "area", "areatalk_test_c", "1", "zh-CN", "", "テスト区域台詞areatalk_test_c", "", later)
}

func TestSideStoryFilesJSONBytes(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "side-story-files.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	seedSideStoryGenerator(t, s)
	generator := NewGenerator(s, store.NewEventStore(database), "")

	filesByKey, err := generator.SideStoryFilesJSON(context.Background(), "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range filesByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "areaTalk/group_12.json" || keys[1] != "cardStory/card_700.json" {
		t.Fatalf("zh-CN side-story files = %v", keys)
	}
	wantCard := `{
  "meta": {
    "source": "human",
    "version": "1",
    "last_updated": 1790003600
  },
  "episodes": {
    "1": {
      "scenarioId": "test_card_700_01",
      "title": "测试前篇",
      "source": "official_cn",
      "talkData": {
        "テスト台詞一 & <": "测试台词一 & <",
        "テスト話者": "测试说话人",
        "テスト台詞二": "测试台词二"
      }
    },
    "2": {
      "scenarioId": "test_card_700_02",
      "title": "",
      "source": "human",
      "talkData": {
        "テスト台詞三": "测试台词三"
      }
    }
  }
}`
	if got := string(filesByKey["cardStory/card_700.json"]); got != wantCard {
		t.Fatalf("card file\n got:\n%s\nwant:\n%s", got, wantCard)
	}
	wantArea := `{
  "meta": {
    "source": "llm",
    "version": "1",
    "last_updated": 1790003600
  },
  "episodes": {
    "areatalk_test_a": {
      "scenarioId": "areatalk_test_a",
      "title": "",
      "source": "llm",
      "talkData": {
        "テスト区域台詞areatalk_test_a": "测试区域台词甲"
      }
    },
    "areatalk_test_b": {
      "scenarioId": "areatalk_test_b",
      "title": "",
      "source": "llm",
      "talkData": {
        "テスト区域台詞areatalk_test_b": "测试区域台词乙"
      }
    }
  }
}`
	if got := string(filesByKey["areaTalk/group_12.json"]); got != wantArea {
		t.Fatalf("area file\n got:\n%s\nwant:\n%s", got, wantArea)
	}

	key, body, ok, err := generator.SideStoryFileForStoryJSON(context.Background(), "area", "areatalk_test_a", "zh-CN")
	if err != nil || !ok || key != "areaTalk/group_12.json" || string(body) != wantArea {
		t.Fatalf("area story file key=%q ok=%v err=%v", key, ok, err)
	}
	key, body, ok, err = generator.SideStoryFileForStoryJSON(context.Background(), "area", "areatalk_test_c", "zh-CN")
	if err != nil || ok || key != "areaTalk/group_13.json" || body != nil {
		t.Fatalf("untranslated area story file key=%q ok=%v err=%v body=%s", key, ok, err, body)
	}
	english, err := generator.SideStoryFilesJSON(context.Background(), "en-US")
	if err != nil || len(english) != 0 {
		t.Fatalf("en-US side-story files = %v err=%v", english, err)
	}
}

// The legacy backup layout carries no side-story files.
func TestWriteAllContextLeavesSideStoriesOut(t *testing.T) {
	gen, _ := openLegacyGenerator(t)
	seedSideStoryGenerator(t, gen.store)
	if published, err := gen.SideStoryFilesJSON(context.Background(), "zh-CN"); err != nil || len(published) != 2 {
		t.Fatalf("seeded side-story files = %d err=%v", len(published), err)
	}
	root := t.TempDir()
	if _, err := gen.WithOutDir(root).WriteAllContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var written []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, path)
		written = append(written, filepath.ToSlash(relative))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, category := range model.SupportedCategories {
		want = append(want, "translation/"+category+".json", "translation/"+category+".full.json")
	}
	want = append(want, "translation/eventStory/event_42.json")
	sort.Strings(want)
	sort.Strings(written)
	if len(written) != len(want) {
		t.Fatalf("WriteAllContext wrote %v, want %v", written, want)
	}
	for index := range want {
		if written[index] != want[index] {
			t.Fatalf("WriteAllContext wrote %v, want %v", written, want)
		}
	}
}
