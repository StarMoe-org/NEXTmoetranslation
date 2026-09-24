package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func sideStoryEpisodeStates(t *testing.T, s *Store, kind, storyID, key string) (string, string) {
	t.Helper()
	item, err := s.SideStoryEpisodeContext(context.Background(), kind, storyID, key)
	if err != nil {
		t.Fatal(err)
	}
	return item.CNState, item.ENState
}

func TestSyncSideStoryCatalogUpsertsAndKeepsStoriesMissingFromTheList(t *testing.T) {
	s := newSideStoryTestStore(t)
	first := sideStoryTestCard("10", 2000, "character/member/test_cn_10", "")
	first.Episodes[0].CNTitle = "测试标题一"
	result := mustSyncSideStoryCatalog(t, s, SideStoryKindCard, first, sideStoryTestCard("11", 1000, "", ""))
	if result != (SideStoryCatalogResult{Stories: 2, NewStories: 2, NewEpisodes: 4, OfficialTitlesWritten: 1}) {
		t.Fatalf("first sync %+v", result)
	}
	if cn, en := sideStoryEpisodeStates(t, s, "card", "10", "1"); cn != "pending" || en != "absent" {
		t.Fatalf("card 10 states cn=%s en=%s", cn, en)
	}
	if cn, en := sideStoryEpisodeStates(t, s, "card", "11", "2"); cn != "absent" || en != "absent" {
		t.Fatalf("card 11 states cn=%s en=%s", cn, en)
	}
	var role string
	var position int
	if err := s.db.QueryRow(`SELECT role,position FROM side_story_lines WHERE kind='card' AND story_id='10' AND episode_key='1' AND jp_key='テスト話10-1'`).
		Scan(&role, &position); err != nil || role != "title" || position != -1 {
		t.Fatalf("title line role=%q position=%d err=%v", role, position, err)
	}
	if row, ok := sideStoryRow(t, s, "card", "10", "1", "テスト話10-1", "zh-CN"); !ok ||
		row != (sideStoryTestRow{text: "测试标题一", source: "official", updatedBy: "sync", revision: 1}) {
		t.Fatalf("official title row %+v ok=%v", row, ok)
	}

	changed := sideStoryTestCard("10", 2000, "character/member/test_cn_10", "")
	changed.Title, changed.CharacterID = "テスト称号改", 2
	result = mustSyncSideStoryCatalog(t, s, SideStoryKindCard, changed)
	if result != (SideStoryCatalogResult{Stories: 1}) {
		t.Fatalf("second sync %+v", result)
	}
	if episodes, err := s.SideStoryEpisodesContext(context.Background(), "card", "11"); err != nil || len(episodes) != 2 ||
		episodes[0].EpisodeKey != "1" || episodes[1].EpisodeKey != "2" {
		t.Fatalf("story missing from the list was not kept: %+v err=%v", episodes, err)
	}
	var title string
	var character int
	if err := s.db.QueryRow(`SELECT title,character_id FROM side_stories WHERE kind='card' AND story_id='10'`).Scan(&title, &character); err != nil ||
		title != "テスト称号改" || character != 2 {
		t.Fatalf("story update title=%q character=%d err=%v", title, character, err)
	}
	if _, err := s.SideStoryEpisodesContext(context.Background(), "card", "12"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown story error=%v", err)
	}
	if _, err := s.SideStoryEpisodeContext(context.Background(), "card", "10", "3"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown episode error=%v", err)
	}
}

func TestSyncSideStoryCatalogRequeuesChangedOfficialPathsAndMarksRemovedOnesAbsent(t *testing.T) {
	s := newSideStoryTestStore(t)
	story := sideStoryTestCard("20", 1, "cn/a", "")
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story)
	if _, err := s.db.Exec(`UPDATE side_story_episodes SET cn_state='imported' WHERE kind='card' AND story_id='20'`); err != nil {
		t.Fatal(err)
	}
	if result := mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story); result.OfficialRequeued != 0 {
		t.Fatalf("unchanged paths requeued %+v", result)
	}
	if cn, _ := sideStoryEpisodeStates(t, s, "card", "20", "1"); cn != "imported" {
		t.Fatalf("unchanged path state=%s", cn)
	}
	moved := sideStoryTestCard("20", 1, "cn/b", "en/a")
	if result := mustSyncSideStoryCatalog(t, s, SideStoryKindCard, moved); result.OfficialRequeued != 4 {
		t.Fatalf("changed CN and new EN paths requeued %+v", result)
	}
	if cn, en := sideStoryEpisodeStates(t, s, "card", "20", "2"); cn != "pending" || en != "pending" {
		t.Fatalf("requeued states cn=%s en=%s", cn, en)
	}
	removed := sideStoryTestCard("20", 1, "", "en/a")
	if result := mustSyncSideStoryCatalog(t, s, SideStoryKindCard, removed); result.OfficialRequeued != 0 {
		t.Fatalf("removed CN path requeued %+v", result)
	}
	if cn, en := sideStoryEpisodeStates(t, s, "card", "20", "1"); cn != "absent" || en != "pending" {
		t.Fatalf("removed path states cn=%s en=%s", cn, en)
	}
	if result := mustSyncSideStoryCatalog(t, s, SideStoryKindCard, moved); result.OfficialRequeued != 2 {
		t.Fatalf("restored CN path requeued %+v", result)
	}
}

func TestSyncSideStoryCatalogMarksAChangedJPPathForRefetch(t *testing.T) {
	s := newSideStoryTestStore(t)
	story := sideStoryTestCard("30", 1, "", "")
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story)
	script := sideStoryTestScript(t, "test_card_30_01", sideStoryTestTalk{"テスト話者", "テスト台詞一"})
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "30", EpisodeKey: "1", JP: fetchedJP(script)})
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "30", EpisodeKey: "2",
		JP: SideStoryFetchOutcome{Attempted: true, Err: "timeout", Transient: true}})
	queued := func() []string {
		items, err := s.SideStoryWorkQueueContext(context.Background(), 10, sideStoryTestNow)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, item := range items {
			keys = append(keys, item.StoryID+"/"+item.EpisodeKey)
		}
		return keys
	}
	if got := strings.Join(queued(), ","); got != "" {
		t.Fatalf("fetched and backed-off episodes queued: %s", got)
	}
	story.Episodes[0].JPAssetPath += "_v2"
	story.Episodes[1].JPAssetPath += "_v2"
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story)
	if got := strings.Join(queued(), ","); got != "30/1,30/2" {
		t.Fatalf("changed JP paths queued %s", got)
	}
	progress, err := s.SideStoryProgressContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if card := progress["card"]; card.PendingFetch != 2 || card.Fetched != 1 || card.Errors != 0 {
		t.Fatalf("progress after JP path change %+v", card)
	}
	var refetch, attempts int
	if err := s.db.QueryRow(`SELECT jp_refetch,attempts FROM side_story_episodes WHERE kind='card' AND story_id='30' AND episode_key='2'`).
		Scan(&refetch, &attempts); err != nil || refetch != 0 || attempts != 0 {
		t.Fatalf("never-fetched episode refetch=%d attempts=%d err=%v", refetch, attempts, err)
	}
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "30", EpisodeKey: "1", JP: fetchedJP(script)})
	if got := strings.Join(queued(), ","); got != "30/2" {
		t.Fatalf("queue after re-fetch %s", got)
	}
}

func TestSyncSideStoryCatalogKeepsTheReasonOfALocaleLeftInMismatch(t *testing.T) {
	s := newSideStoryTestStore(t)
	story := sideStoryTestCard("40", 1, "", "en/40")
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story)
	jp := sideStoryTestScript(t, "test_card_40_01", sideStoryTestTalk{"テスト話者", "テスト台詞一"})
	en := sideStoryTestScript(t, "test_card_40_01", sideStoryTestTalk{"Tester", "Test line one"}, sideStoryTestTalk{"Tester", "Test line two"})
	cn := sideStoryTestScript(t, "test_card_40_01", sideStoryTestTalk{"测试说话人", "测试台词一"})
	reason := "en-US: TalkData length mismatch (1 != 2)"
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "40", EpisodeKey: "1", JP: fetchedJP(jp), EN: fetchedJP(en)})
	check := func(step string, applied *SideStoryEpisodeApply) {
		t.Helper()
		if state := sideStoryEpisodeState(t, s, "card", "40", "1"); state.lastError != reason || state.attempts != 0 || state.nextAttemptAt != 0 {
			t.Fatalf("%s stored %+v", step, state)
		}
		if applied != nil && (applied.Error != reason || applied.ENState != "mismatch") {
			t.Fatalf("%s result %+v", step, applied)
		}
	}
	check("EN mismatch", nil)

	// A later CN path requeues CN only; the backfill then fetches JP and CN.
	story = sideStoryTestCard("40", 1, "cn/40", "en/40")
	if result := mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story); result.OfficialRequeued != 2 {
		t.Fatalf("new CN paths %+v", result)
	}
	check("new CN path", nil)
	applied := mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "40", EpisodeKey: "1",
		JP: fetchedJP(jp), CN: fetchedJP(cn)}).Episodes[0]
	if applied.CNState != "imported" {
		t.Fatalf("CN import %+v", applied)
	}
	check("CN import", &applied)

	story.Episodes[0].JPAssetPath += "_v2"
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story)
	check("new JP path", nil)
	// The kept EN reason is no JP fetch error.
	if progress, err := s.SideStoryProgressContext(context.Background()); err != nil || progress["card"].Errors != 0 {
		t.Fatalf("progress after the new JP path %+v err=%v", progress["card"], err)
	}
	applied = mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "40", EpisodeKey: "1", JP: fetchedJP(jp)}).Episodes[0]
	check("unchanged JP refetch", &applied)

	// A new EN path requeues EN, whose own reason goes with its old path.
	story.Episodes[0].ENAssetPath += "_v2"
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story)
	if state := sideStoryEpisodeState(t, s, "card", "40", "1"); state.lastError != "" || state.attempts != 0 || state.nextAttemptAt != 0 {
		t.Fatalf("new EN path stored %+v", state)
	}
	if _, enState := sideStoryEpisodeStates(t, s, "card", "40", "1"); enState != "pending" {
		t.Fatalf("new EN path left EN %s", enState)
	}
}

func TestSyncSideStoryCatalogWritesOfficialTitlesUnderTheOfficialWriteRule(t *testing.T) {
	s := newSideStoryTestStore(t)
	story := sideStoryTestCard("40", 1, "", "")
	story.Episodes[0].CNTitle, story.Episodes[0].ENTitle = "测试标题一", "Test Title One"
	story.Episodes[1].CNTitle, story.Episodes[1].ENTitle = "测试标题二", story.Episodes[1].TitleJP
	titleCounts := func(want SideStoryCatalogResult) {
		t.Helper()
		result := mustSyncSideStoryCatalog(t, s, SideStoryKindCard, story)
		if result.OfficialTitlesWritten != want.OfficialTitlesWritten || result.DroppedHumanTitles != want.DroppedHumanTitles ||
			result.TitlesReplaced != want.TitlesReplaced {
			t.Fatalf("title sync %+v want %d official titles written, %d human titles dropped, %d titles replaced",
				result, want.OfficialTitlesWritten, want.DroppedHumanTitles, want.TitlesReplaced)
		}
	}
	titleCounts(SideStoryCatalogResult{OfficialTitlesWritten: 3})
	if _, ok := sideStoryRow(t, s, "card", "40", "2", "テスト話40-2", "en-US"); ok {
		t.Fatal("an EN title identical to the kana JP title was written")
	}
	titleCounts(SideStoryCatalogResult{})
	if row, _ := sideStoryRow(t, s, "card", "40", "1", "テスト話40-1", "zh-CN"); row.revision != 1 {
		t.Fatalf("unchanged official title bumped to %+v", row)
	}
	if _, err := s.db.Exec(`UPDATE side_story_line_localizations SET source='llm' WHERE locale='en-US'`); err != nil {
		t.Fatal(err)
	}
	mustUpdateSideStory(t, s, "card", "40", "2", "zh-CN", SideStoryLineEdit{JP: "テスト話40-2", Text: "测试人工标题"})
	story.Episodes[0].CNTitle, story.Episodes[0].ENTitle = "测试标题一改", "Test Title One Revised"
	story.Episodes[1].CNTitle = "测试标题二改"
	titleCounts(SideStoryCatalogResult{OfficialTitlesWritten: 2})
	for _, test := range []struct {
		key, jp, locale string
		want            sideStoryTestRow
	}{
		{"1", "テスト話40-1", "zh-CN", sideStoryTestRow{text: "测试标题一改", source: "official", updatedBy: "sync", revision: 2}},
		{"1", "テスト話40-1", "en-US", sideStoryTestRow{text: "Test Title One Revised", source: "official", updatedBy: "sync", revision: 2}},
		{"2", "テスト話40-2", "zh-CN", sideStoryTestRow{text: "测试人工标题", source: "human", updatedBy: "test-editor", revision: 2}},
	} {
		if row, _ := sideStoryRow(t, s, "card", "40", test.key, test.jp, test.locale); row != test.want {
			t.Errorf("%s %s row %+v want %+v", test.key, test.locale, row, test.want)
		}
	}

	story.Episodes[0].TitleJP = "テスト新題"
	titleCounts(SideStoryCatalogResult{OfficialTitlesWritten: 2, TitlesReplaced: 1})
	var titles []string
	rows, err := s.db.Query(`SELECT jp_key FROM side_story_lines WHERE kind='card' AND story_id='40' AND episode_key='1' AND role='title'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		titles = append(titles, key)
	}
	rows.Close()
	if strings.Join(titles, ",") != "テスト新題" {
		t.Fatalf("title lines after a JP title change %v", titles)
	}
	if _, ok := sideStoryRow(t, s, "card", "40", "1", "テスト話40-1", "zh-CN"); ok {
		t.Fatal("the old title's translation survived")
	}
	if row, _ := sideStoryRow(t, s, "card", "40", "1", "テスト新題", "zh-CN"); row.text != "测试标题一改" || row.revision != 1 {
		t.Fatalf("new title row %+v", row)
	}

	mustUpdateSideStory(t, s, "card", "40", "2", "en-US", SideStoryLineEdit{JP: "テスト話40-2", Text: "Test human title"})
	story.Episodes[1].TitleJP, story.Episodes[1].ENTitle = "テスト新題二", ""
	titleCounts(SideStoryCatalogResult{OfficialTitlesWritten: 1, DroppedHumanTitles: 2, TitlesReplaced: 1})
	if row, _ := sideStoryRow(t, s, "card", "40", "2", "テスト新題二", "zh-CN"); row.text != "测试标题二改" || row.source != "official" {
		t.Fatalf("title row after the human titles were dropped %+v", row)
	}
}

func TestSyncSideStoryCatalogRejectsInvalidInputWithoutWriting(t *testing.T) {
	s := newSideStoryTestStore(t)
	valid := sideStoryTestCard("50", 1, "", "")
	for name, test := range map[string]struct {
		kind    string
		stories func() []SideStoryCatalogStory
	}{
		"unknown kind": {"event", func() []SideStoryCatalogStory { return []SideStoryCatalogStory{valid} }},
		"kind differs": {"area", func() []SideStoryCatalogStory { return []SideStoryCatalogStory{valid} }},
		"invalid id": {"card", func() []SideStoryCatalogStory {
			story := sideStoryTestCard("050", 1, "", "")
			return []SideStoryCatalogStory{valid, story}
		}},
		"repeated story": {"card", func() []SideStoryCatalogStory { return []SideStoryCatalogStory{valid, valid} }},
		"card with an action set": {"card", func() []SideStoryCatalogStory {
			story := sideStoryTestCard("51", 1, "", "")
			story.ActionSetID = 5
			return []SideStoryCatalogStory{story}
		}},
		"area without an action set": {"area", func() []SideStoryCatalogStory {
			return []SideStoryCatalogStory{sideStoryTestArea("areatalk_test_1", 0, 1)}
		}},
		"area episode of another scenario": {"area", func() []SideStoryCatalogStory {
			story := sideStoryTestArea("areatalk_test_1", 501, 1)
			story.Episodes[0].ScenarioID = "areatalk_test_2"
			return []SideStoryCatalogStory{story}
		}},
		"third card episode": {"card", func() []SideStoryCatalogStory {
			story := sideStoryTestCard("52", 1, "", "")
			story.Episodes[1].Key = "3"
			return []SideStoryCatalogStory{story}
		}},
		"repeated episode": {"card", func() []SideStoryCatalogStory {
			story := sideStoryTestCard("53", 1, "", "")
			story.Episodes[1].Key = "1"
			return []SideStoryCatalogStory{story}
		}},
		"no JP path": {"card", func() []SideStoryCatalogStory {
			story := sideStoryTestCard("54", 1, "", "")
			story.Episodes[0].JPAssetPath = ""
			return []SideStoryCatalogStory{story}
		}},
		"no episodes": {"card", func() []SideStoryCatalogStory {
			story := sideStoryTestCard("55", 1, "", "")
			story.Episodes = nil
			return []SideStoryCatalogStory{story}
		}},
	} {
		if _, err := s.SyncSideStoryCatalogContext(context.Background(), test.kind, test.stories(), sideStoryTestNow); !errors.Is(err, ErrSideStoryInvalid) {
			t.Errorf("%s: error=%v", name, err)
		}
	}
	var stories int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM side_stories`).Scan(&stories); err != nil || stories != 0 {
		t.Fatalf("rejected catalogs wrote %d stories err=%v", stories, err)
	}
}

func TestSideStoryWorkQueueOrdersImportableThenNewerAndHonoursDueTimeAndLimit(t *testing.T) {
	s := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard,
		sideStoryTestCard("60", 3000, "", ""), sideStoryTestCard("61", 1000, "cn/61", ""), sideStoryTestCard("62", 2000, "", "en/62"))
	mustSyncSideStoryCatalog(t, s, SideStoryKindArea, sideStoryTestArea("areatalk_test_q", 501, 4000))
	queue := func(limit int, now time.Time) string {
		items, err := s.SideStoryWorkQueueContext(context.Background(), limit, now)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, item := range items {
			keys = append(keys, item.Kind+":"+item.StoryID+"/"+item.EpisodeKey)
		}
		return strings.Join(keys, ",")
	}
	want := "card:62/1,card:62/2,card:61/1,card:61/2,area:areatalk_test_q/1,card:60/1,card:60/2"
	if got := queue(10, sideStoryTestNow); got != want {
		t.Fatalf("queue %s want %s", got, want)
	}
	if got := queue(3, sideStoryTestNow); got != "card:62/1,card:62/2,card:61/1" {
		t.Fatalf("limited queue %s", got)
	}
	if got := queue(1000, sideStoryTestNow); got != want {
		t.Fatalf("queue above the cap %s", got)
	}
	if _, err := s.SideStoryWorkQueueContext(context.Background(), 0, sideStoryTestNow); !errors.Is(err, ErrSideStoryInvalid) {
		t.Fatalf("zero limit error=%v", err)
	}
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "62", EpisodeKey: "1",
		JP: SideStoryFetchOutcome{Attempted: true, Err: "timeout", Transient: true}})
	if got := queue(2, sideStoryTestNow); got != "card:62/2,card:61/1" {
		t.Fatalf("queue during backoff %s", got)
	}
	if got := queue(1, sideStoryTestNow.Add(10*time.Minute)); got != "card:62/1" {
		t.Fatalf("queue after backoff %s", got)
	}
	item, err := s.SideStoryEpisodeContext(context.Background(), "card", "62", "1")
	if err != nil || item.Attempts != 1 || item.ENAssetPath != "en/62/test_card_62_01" || item.ENState != "pending" ||
		item.ScenarioID != "test_card_62_01" || item.ScriptSHA256 != "" {
		t.Fatalf("work item %+v err=%v", item, err)
	}
	script := sideStoryTestScript(t, "test_card_61_01", sideStoryTestTalk{"テスト話者", "テスト台詞一"})
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "61", EpisodeKey: "1",
		JP: fetchedJP(script), CN: fetchedJP(sideStoryTestScript(t, "test_card_61_01", sideStoryTestTalk{"测试说话人", "测试台词一"}))})
	if got := queue(10, sideStoryTestNow); strings.Contains(got, "card:61/1") {
		t.Fatalf("fetched episode with nothing to import is queued: %s", got)
	}
}

func TestSideStoryProgressCountsStatesPerKind(t *testing.T) {
	s := newSideStoryTestStore(t)
	progress, err := s.SideStoryProgressContext(context.Background())
	if err != nil || len(progress) != 2 || progress["card"] != (SideStoryKindProgress{}) || progress["area"] != (SideStoryKindProgress{}) {
		t.Fatalf("empty progress %+v err=%v", progress, err)
	}
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("70", 1, "cn/70", "en/70"), sideStoryTestCard("71", 1, "", ""))
	mustSyncSideStoryCatalog(t, s, SideStoryKindArea, sideStoryTestArea("areatalk_test_p", 501, 1))
	script := sideStoryTestScript(t, "test_card_70_01", sideStoryTestTalk{"テスト話者", "テスト台詞です"})
	cn := sideStoryTestScript(t, "test_card_70_01", sideStoryTestTalk{"测试说话人", "测试台词一"})
	mustApplySideStory(t, s, sideStoryTestNow,
		SideStoryEpisodeFetch{Kind: "card", StoryID: "70", EpisodeKey: "1", JP: fetchedJP(script),
			CN: fetchedJP(cn), EN: SideStoryFetchOutcome{Attempted: true, Err: "forbidden"}},
		SideStoryEpisodeFetch{Kind: "card", StoryID: "70", EpisodeKey: "2", JP: fetchedJP(sideStoryTestScript(t, "test_card_70_02")),
			CN: fetchedJP(sideStoryTestScript(t, "test_card_70_02", sideStoryTestTalk{"", "测试"})), EN: SideStoryFetchOutcome{Attempted: true, Missing: true}},
		SideStoryEpisodeFetch{Kind: "card", StoryID: "71", EpisodeKey: "1", JP: SideStoryFetchOutcome{Attempted: true, Missing: true}})
	progress, err = s.SideStoryProgressContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantCard := SideStoryKindProgress{
		Stories: 2, Episodes: 4, Fetched: 2, PendingFetch: 2,
		CNImported: 1, CNMismatch: 1, CNAbsent: 2,
		ENError: 1, ENPending: 1, ENAbsent: 2, Errors: 1,
	}
	if progress["card"] != wantCard {
		t.Fatalf("card progress %+v want %+v", progress["card"], wantCard)
	}
	if want := (SideStoryKindProgress{Stories: 1, Episodes: 1, PendingFetch: 1, CNAbsent: 1, ENAbsent: 1}); progress["area"] != want {
		t.Fatalf("area progress %+v want %+v", progress["area"], want)
	}
}
