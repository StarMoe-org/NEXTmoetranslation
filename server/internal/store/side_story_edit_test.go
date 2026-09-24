package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// seedSideStoryEpisode stores card storyID with a fetched first episode whose
// lines are the title, テスト台詞一/二/三 and the speaker テスト話者.
func seedSideStoryEpisode(t *testing.T, s *Store, storyID string) {
	t.Helper()
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard(storyID, 1, "", ""))
	script := sideStoryTestScript(t, "test_card_"+storyID+"_01",
		sideStoryTestTalk{"テスト話者", "テスト台詞一"}, sideStoryTestTalk{"", "テスト台詞二"}, sideStoryTestTalk{"", "テスト台詞三"})
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: storyID, EpisodeKey: "1", JP: fetchedJP(script)})
}

func revision(value int) *int { return &value }

func TestUpdateSideStoryLinesIsAllOrNothing(t *testing.T) {
	s := newSideStoryTestStore(t)
	seedSideStoryEpisode(t, s, "200")
	ctx := context.Background()
	update := func(storyID, episodeKey string, edits ...SideStoryLineEdit) (SideStoryUpdateResult, error) {
		return s.UpdateSideStoryLinesContext(ctx, "card", storyID, episodeKey, "zh-CN", "test-editor", edits, sideStoryTestNow)
	}
	if _, err := update("201", "1", SideStoryLineEdit{JP: "テスト台詞一", Text: "测试台词一"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown story error=%v", err)
	}
	if _, err := update("200", "2", SideStoryLineEdit{JP: "テスト台詞一", Text: "测试台词一"}); !errors.Is(err, ErrSideStoryUnknownLines) {
		t.Fatalf("line of another episode error=%v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM side_story_episodes WHERE kind='card' AND story_id='200' AND episode_key='2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := update("200", "2", SideStoryLineEdit{JP: "テスト話200-2", Text: "测试标题"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown episode error=%v", err)
	}
	_, err := update("200", "1",
		SideStoryLineEdit{JP: "テスト台詞一", Text: "测试台词一"},
		SideStoryLineEdit{JP: "テスト無し一", Text: "测试"},
		SideStoryLineEdit{JP: "テスト無し二", Text: "测试"})
	var unknown *SideStoryUnknownLinesError
	if !errors.As(err, &unknown) || !errors.Is(err, ErrSideStoryUnknownLines) || strings.Join(unknown.Lines, ",") != "テスト無し一,テスト無し二" {
		t.Fatalf("unknown lines error=%v", err)
	}
	if _, ok := sideStoryRow(t, s, "card", "200", "1", "テスト台詞一", "zh-CN"); ok {
		t.Fatal("a batch with unknown lines wrote a row")
	}

	result, err := update("200", "1",
		SideStoryLineEdit{JP: "テスト台詞一", Text: "测试台词一", ExpectedRevision: revision(0)},
		SideStoryLineEdit{JP: "テスト話者", Text: "测试说话人", Source: "llm"},
		SideStoryLineEdit{JP: "テスト台詞二", Text: "测试第一行\n测试第二行"})
	if err != nil || result.Updated != 3 || result.Unchanged != 0 || len(result.Lines) != 3 {
		t.Fatalf("first update %+v err=%v", result, err)
	}
	if line := result.Lines[0]; line != (SideStoryLineState{JP: "テスト台詞一", Role: "talk", Speaker: "テスト話者", Position: 0,
		Text: "测试台词一", Source: "human", Revision: 1, UpdatedBy: "test-editor", UpdatedAt: sideStoryTestNow.Unix()}) {
		t.Fatalf("returned line %+v", line)
	}
	if result.Lines[1].Source != "llm" || result.Lines[2].Text != "测试第一行\n测试第二行" {
		t.Fatalf("returned lines %+v", result.Lines)
	}

	_, err = update("200", "1",
		SideStoryLineEdit{JP: "テスト台詞三", Text: "测试台词三", ExpectedRevision: revision(0)},
		SideStoryLineEdit{JP: "テスト台詞一", Text: "测试覆盖", ExpectedRevision: revision(0)},
		SideStoryLineEdit{JP: "テスト話者", Text: "测试覆盖", ExpectedRevision: revision(5)})
	var conflict *SideStoryRevisionConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrSideStoryRevisionConflict) || len(conflict.Conflicts) != 2 {
		t.Fatalf("conflict error=%v", err)
	}
	if conflict.Conflicts[0] != (SideStoryLineConflict{JP: "テスト台詞一", ExpectedRevision: 0, CurrentRevision: 1, CurrentText: "测试台词一", CurrentSource: "human"}) ||
		conflict.Conflicts[1] != (SideStoryLineConflict{JP: "テスト話者", ExpectedRevision: 5, CurrentRevision: 1, CurrentText: "测试说话人", CurrentSource: "llm"}) {
		t.Fatalf("conflicts %+v", conflict.Conflicts)
	}
	if _, ok := sideStoryRow(t, s, "card", "200", "1", "テスト台詞三", "zh-CN"); ok {
		t.Fatal("a conflicting batch wrote its non-conflicting edit")
	}

	result, err = update("200", "1",
		SideStoryLineEdit{JP: "テスト台詞一", Text: "测试台词一", ExpectedRevision: revision(1)},
		SideStoryLineEdit{JP: "テスト話者", Text: "测试说话人"},
		SideStoryLineEdit{JP: "テスト台詞三", Text: ""})
	if err != nil || result.Updated != 2 || result.Unchanged != 1 || result.Lines[0].Revision != 1 || result.Lines[1].Revision != 2 ||
		result.Lines[1].Source != "human" || result.Lines[2].Revision != 1 || result.Lines[2].Source != "human" {
		t.Fatalf("second update %+v err=%v", result, err)
	}
	detail, err := s.SideStoryDetailContext(ctx, "card", "200", "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	if episode := detail.Episodes[0]; episode.TranslatedCount != 3 || episode.UntranslatedCount != 2 {
		t.Fatalf("empty human text counted as translated: %+v", episode)
	}
	targets, err := s.SideStoryAITargetsContext(ctx, "card", "200", "zh-CN", "1")
	if err != nil || len(targets) != 1 || targets[0].JP != "テスト話200-1" {
		t.Fatalf("AI targets include an empty human row: %+v err=%v", targets, err)
	}
}

func TestUpdateSideStoryLinesStoresWhitespaceOnlyTextAsAnEmptyLine(t *testing.T) {
	s := newSideStoryTestStore(t)
	seedSideStoryEpisode(t, s, "230")
	ctx := context.Background()
	mustUpdateSideStory(t, s, "card", "230", "1", "zh-CN",
		SideStoryLineEdit{JP: "テスト台詞一", Text: "测试台词一"}, SideStoryLineEdit{JP: "テスト台詞二", Text: "测试台词二", Source: "llm"})
	result := mustUpdateSideStory(t, s, "card", "230", "1", "zh-CN",
		SideStoryLineEdit{JP: "テスト台詞一", Text: " \n"}, SideStoryLineEdit{JP: "テスト台詞二", Text: "\u3000"},
		SideStoryLineEdit{JP: "テスト台詞三", Text: " 测试台词三 "})
	if result.Updated != 3 || result.Lines[0].Text != "" || result.Lines[1].Text != "" || result.Lines[2].Text != " 测试台词三 " {
		t.Fatalf("whitespace update %+v", result)
	}
	for jp, want := range map[string]sideStoryTestRow{
		"テスト台詞一": {text: "", source: "human", updatedBy: "test-editor", revision: 2},
		"テスト台詞二": {text: "", source: "human", updatedBy: "test-editor", revision: 2},
		"テスト台詞三": {text: " 测试台词三 ", source: "human", updatedBy: "test-editor", revision: 1},
	} {
		if row, _ := sideStoryRow(t, s, "card", "230", "1", jp, "zh-CN"); row != want {
			t.Errorf("%s row %+v want %+v", jp, row, want)
		}
	}
	if again := mustUpdateSideStory(t, s, "card", "230", "1", "zh-CN", SideStoryLineEdit{JP: "テスト台詞一", Text: "\t"}); again.Unchanged != 1 {
		t.Fatalf("whitespace over an empty human line %+v", again)
	}
	detail, err := s.SideStoryDetailContext(ctx, "card", "230", "zh-CN")
	if err != nil || detail.Episodes[0].TranslatedCount != 1 || detail.Episodes[0].UntranslatedCount != 4 {
		t.Fatalf("whitespace counted as translated: %+v err=%v", detail.Episodes[0], err)
	}
	_, file, ok, err := s.SideStoryPublicFileForStoryContext(ctx, "card", "230", "zh-CN")
	if err != nil || !ok || len(file.Episodes[0].Talk) != 1 || file.Episodes[0].Talk[0].JP != "テスト台詞三" {
		t.Fatalf("public file with whitespace lines ok=%v err=%v %+v", ok, err, file)
	}
}

func TestUpdateSideStoryLinesValidatesInput(t *testing.T) {
	s := newSideStoryTestStore(t)
	seedSideStoryEpisode(t, s, "210")
	ctx := context.Background()
	edit := SideStoryLineEdit{JP: "テスト台詞一", Text: "测试台词一"}
	tooMany := make([]SideStoryLineEdit, 2001)
	for index := range tooMany {
		tooMany[index] = SideStoryLineEdit{JP: strings.Repeat("テ", index+1), Text: "测试"}
	}
	for name, test := range map[string]struct {
		kind, id, episode, locale string
		edits                     []SideStoryLineEdit
	}{
		"no edits":         {"card", "210", "1", "zh-CN", nil},
		"2001 edits":       {"card", "210", "1", "zh-CN", tooMany},
		"repeated line":    {"card", "210", "1", "zh-CN", []SideStoryLineEdit{edit, edit}},
		"invalid UTF-8":    {"card", "210", "1", "zh-CN", []SideStoryLineEdit{{JP: edit.JP, Text: "\xff"}}},
		"NUL":              {"card", "210", "1", "zh-CN", []SideStoryLineEdit{{JP: edit.JP, Text: "测试\x00"}}},
		"16385 bytes":      {"card", "210", "1", "zh-CN", []SideStoryLineEdit{{JP: edit.JP, Text: strings.Repeat("a", 16385)}}},
		"official source":  {"card", "210", "1", "zh-CN", []SideStoryLineEdit{{JP: edit.JP, Text: "测试", Source: "official"}}},
		"ja-JP locale":     {"card", "210", "1", "ja-JP", []SideStoryLineEdit{edit}},
		"unknown kind":     {"event", "210", "1", "zh-CN", []SideStoryLineEdit{edit}},
		"invalid card id":  {"card", "0210", "1", "zh-CN", []SideStoryLineEdit{edit}},
		"invalid episode":  {"card", "210", "3", "zh-CN", []SideStoryLineEdit{edit}},
		"area episode two": {"area", "areatalk_test_1", "2", "zh-CN", []SideStoryLineEdit{edit}},
	} {
		if _, err := s.UpdateSideStoryLinesContext(ctx, test.kind, test.id, test.episode, test.locale, "test-editor", test.edits, sideStoryTestNow); !errors.Is(err, ErrSideStoryInvalid) {
			t.Errorf("%s: error=%v", name, err)
		}
	}
	if _, err := s.UpdateSideStoryLinesContext(ctx, "card", "210", "1", "en-US", "test-editor",
		[]SideStoryLineEdit{{JP: edit.JP, Text: strings.Repeat("a", 16384)}}, sideStoryTestNow); err != nil {
		t.Fatalf("16384-byte text: %v", err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM side_story_line_localizations WHERE locale='zh-CN'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("invalid edits wrote %d rows err=%v", rows, err)
	}
}

func TestSideStoryAITranslationsOnlyFillLinesUnchangedSinceTheirTargets(t *testing.T) {
	s := newSideStoryTestStore(t)
	seedSideStoryEpisode(t, s, "220")
	ctx := context.Background()
	mustUpdateSideStory(t, s, "card", "220", "1", "zh-CN",
		SideStoryLineEdit{JP: "テスト台詞一", Text: "测试人工台词"},
		SideStoryLineEdit{JP: "テスト台詞二", Text: "", Source: "llm"})
	if _, err := s.SideStoryAITargetsContext(ctx, "card", "221", "zh-CN", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown story error=%v", err)
	}
	if _, err := s.SideStoryAITargetsContext(ctx, "card", "220", "ja-JP", ""); !errors.Is(err, ErrSideStoryInvalid) {
		t.Fatalf("ja-JP targets error=%v", err)
	}
	all, err := s.SideStoryAITargetsContext(ctx, "card", "220", "zh-CN", "")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, target := range all {
		keys = append(keys, target.EpisodeKey+":"+target.JP)
	}
	if strings.Join(keys, ",") != "1:テスト話220-1,1:テスト話者,1:テスト台詞二,1:テスト台詞三,2:テスト話220-2" {
		t.Fatalf("targets %v", keys)
	}
	targets, err := s.SideStoryAITargetsContext(ctx, "card", "220", "zh-CN", "1")
	if err != nil || len(targets) != 4 || targets[2] != (SideStoryAITarget{EpisodeKey: "1", JP: "テスト台詞二", Revision: 1}) {
		t.Fatalf("episode targets %+v err=%v", targets, err)
	}
	// Between targets and apply an editor translates the title and the speaker
	// row gets an empty llm row.
	mustUpdateSideStory(t, s, "card", "220", "1", "zh-CN",
		SideStoryLineEdit{JP: "テスト話220-1", Text: "测试人工标题"},
		SideStoryLineEdit{JP: "テスト話者", Text: "", Source: "llm"})
	if _, err := s.ApplySideStoryAITranslationsContext(ctx, "card", "220", "zh-CN", targets, []string{"x"}, sideStoryTestNow); !errors.Is(err, ErrSideStoryInvalid) {
		t.Fatalf("length mismatch error=%v", err)
	}
	written, err := s.ApplySideStoryAITranslationsContext(ctx, "card", "220", "zh-CN", targets,
		[]string{"测试模型标题", "测试模型说话人", "测试模型台词二", ""}, sideStoryTestNow)
	if err != nil || written != 1 {
		t.Fatalf("AI apply written=%d err=%v", written, err)
	}
	for jp, want := range map[string]sideStoryTestRow{
		"テスト話220-1": {text: "测试人工标题", source: "human", updatedBy: "test-editor", revision: 1},
		"テスト話者":     {text: "", source: "llm", updatedBy: "test-editor", revision: 1},
		"テスト台詞二":    {text: "测试模型台词二", source: "llm", updatedBy: "ai", revision: 2},
	} {
		if row, _ := sideStoryRow(t, s, "card", "220", "1", jp, "zh-CN"); row != want {
			t.Errorf("%s row %+v want %+v", jp, row, want)
		}
	}
	if _, ok := sideStoryRow(t, s, "card", "220", "1", "テスト台詞三", "zh-CN"); ok {
		t.Fatal("an empty AI text was written")
	}
	fresh, err := s.SideStoryAITargetsContext(ctx, "card", "220", "zh-CN", "1")
	if err != nil {
		t.Fatal(err)
	}
	written, err = s.ApplySideStoryAITranslationsContext(ctx, "card", "220", "zh-CN", fresh, []string{"测试模型说话人", "测试模型台词三"}, sideStoryTestNow)
	if err != nil || written != 2 {
		t.Fatalf("second AI apply written=%d err=%v targets=%+v", written, err, fresh)
	}
	if row, _ := sideStoryRow(t, s, "card", "220", "1", "テスト台詞三", "zh-CN"); row != (sideStoryTestRow{text: "测试模型台词三", source: "llm", updatedBy: "ai", revision: 1}) {
		t.Fatalf("AI insert row %+v", row)
	}
}
