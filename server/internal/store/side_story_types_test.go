package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/db"
)

// Every side-story test string is synthetic test data.
var sideStoryTestNow = time.Unix(1790000000, 0)

func newSideStoryTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "side-stories.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database)
}

// sideStoryTestTalk is one TalkData entry: speaker and body.
type sideStoryTestTalk [2]string

func sideStoryTestScenario(scenarioID string, talks ...sideStoryTestTalk) map[string]any {
	talkData := make([]any, len(talks))
	for index, talk := range talks {
		talkData[index] = map[string]any{"WindowDisplayName": talk[0], "Body": talk[1], "Voices": []any{}}
	}
	return map[string]any{
		"ScenarioId": scenarioID, "Snippets": []any{}, "TalkData": talkData,
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}
}

func sideStoryTestScript(t *testing.T, scenarioID string, talks ...sideStoryTestTalk) *SideStoryScript {
	t.Helper()
	script, err := ParseSideStoryScript(sideStoryTestScenario(scenarioID, talks...), scenarioID)
	if err != nil {
		t.Fatal(err)
	}
	return &script
}

// sideStoryTestCard is a card story with two episodes; cnPath/enPath "" means
// that server lacks the episode.
func sideStoryTestCard(id string, releasedAt int64, cnPath, enPath string) SideStoryCatalogStory {
	story := SideStoryCatalogStory{Kind: SideStoryKindCard, StoryID: id, Title: "テスト称号" + id, CharacterID: 1, ReleasedAt: releasedAt}
	for _, key := range []string{"1", "2"} {
		episode := SideStoryCatalogEpisode{
			Key: key, ScenarioID: "test_card_" + id + "_0" + key, TitleJP: "テスト話" + id + "-" + key, Position: len(story.Episodes) + 1,
			JPAssetPath: "character/member/test_res_" + id + "/test_card_" + id + "_0" + key,
		}
		if cnPath != "" {
			episode.CNAssetPath = cnPath + "/test_card_" + id + "_0" + key
		}
		if enPath != "" {
			episode.ENAssetPath = enPath + "/test_card_" + id + "_0" + key
		}
		story.Episodes = append(story.Episodes, episode)
	}
	return story
}

func sideStoryTestArea(scenarioID string, actionSetID int, releasedAt int64) SideStoryCatalogStory {
	return SideStoryCatalogStory{
		Kind: SideStoryKindArea, StoryID: scenarioID, Title: "テスト区域", AreaID: 3, AreaCategory: "grade1",
		ActionSetID: actionSetID, ReleasedAt: releasedAt,
		Episodes: []SideStoryCatalogEpisode{{Key: "1", ScenarioID: scenarioID, JPAssetPath: "scenario/actionset/group5/" + scenarioID}},
	}
}

func mustSyncSideStoryCatalog(t *testing.T, s *Store, kind string, stories ...SideStoryCatalogStory) SideStoryCatalogResult {
	t.Helper()
	result, err := s.SyncSideStoryCatalogContext(context.Background(), kind, stories, sideStoryTestNow)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustApplySideStory(t *testing.T, s *Store, now time.Time, fetches ...SideStoryEpisodeFetch) SideStoryApplyResult {
	t.Helper()
	result, err := s.ApplySideStoryFetchesContext(context.Background(), fetches, now)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func fetchedJP(script *SideStoryScript) SideStoryFetchOutcome {
	return SideStoryFetchOutcome{Attempted: true, Script: script}
}

type sideStoryTestRow struct {
	text, source, updatedBy string
	revision                int
}

func sideStoryRow(t *testing.T, s *Store, kind, storyID, episodeKey, jp, locale string) (sideStoryTestRow, bool) {
	t.Helper()
	var row sideStoryTestRow
	err := s.db.QueryRow(`SELECT text,source,updated_by,revision FROM side_story_line_localizations
		WHERE kind=? AND story_id=? AND episode_key=? AND jp_key=? AND locale=?`, kind, storyID, episodeKey, jp, locale).
		Scan(&row.text, &row.source, &row.updatedBy, &row.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row, true
}

func mustUpdateSideStory(t *testing.T, s *Store, kind, storyID, episodeKey, locale string, edits ...SideStoryLineEdit) SideStoryUpdateResult {
	t.Helper()
	result, err := s.UpdateSideStoryLinesContext(context.Background(), kind, storyID, episodeKey, locale, "test-editor", edits, sideStoryTestNow)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSideStoryValidators(t *testing.T) {
	for _, kind := range []string{"card", "area"} {
		if !ValidSideStoryKind(kind) {
			t.Errorf("kind %q rejected", kind)
		}
	}
	for _, kind := range []string{"", "event", "Card"} {
		if ValidSideStoryKind(kind) {
			t.Errorf("kind %q accepted", kind)
		}
	}
	for _, test := range []struct {
		kind, id string
		want     bool
	}{
		{"card", "1", true}, {"card", "123", true}, {"card", "999999999", true},
		{"card", "", false}, {"card", "0", false}, {"card", "012", false}, {"card", "1234567890", false},
		{"card", "+1", false}, {"card", "-1", false}, {"card", "1a", false}, {"card", " 1", false},
		{"area", "areatalk_ev_test_001", true}, {"area", "A", true}, {"area", "a.b-c_1", true},
		{"area", strings.Repeat("a", 128), true}, {"area", strings.Repeat("a", 129), false},
		{"area", "", false}, {"area", "_area", false}, {"area", "area/talk", false}, {"area", "area talk", false},
		{"event", "1", false},
	} {
		if got := ValidSideStoryID(test.kind, test.id); got != test.want {
			t.Errorf("ValidSideStoryID(%q, %q)=%v want %v", test.kind, test.id, got, test.want)
		}
	}
	for _, test := range []struct {
		kind, key string
		want      bool
	}{{"card", "1", true}, {"card", "2", true}, {"card", "3", false}, {"area", "1", true}, {"area", "2", false}, {"event", "1", false}} {
		if got := ValidSideStoryEpisodeKey(test.kind, test.key); got != test.want {
			t.Errorf("ValidSideStoryEpisodeKey(%q, %q)=%v want %v", test.kind, test.key, got, test.want)
		}
	}
	for locale, want := range map[string]bool{"zh-CN": true, "en-US": true, "ja-JP": false, "": false, "zh-TW": false} {
		if got := ValidSideStoryLocale(locale); got != want {
			t.Errorf("ValidSideStoryLocale(%q)=%v want %v", locale, got, want)
		}
	}
	for _, test := range []struct{ locale, source, want string }{
		{"zh-CN", "official", "official_cn"}, {"en-US", "official", "official_en"},
		{"zh-CN", "llm", "llm"}, {"en-US", "human", "human"},
	} {
		if got := SideStoryPublicSourceLabel(test.locale, test.source); got != test.want {
			t.Errorf("label(%s,%s)=%q want %q", test.locale, test.source, got, test.want)
		}
	}
	if !sideStoryHasKana("テスト") || !sideStoryHasKana("てすと") || sideStoryHasKana("……") || sideStoryHasKana("测试") {
		t.Fatal("kana detection")
	}
}

func TestSideStoryBackoffDoublesFromTenMinutesUpToADay(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		1: 10 * time.Minute, 2: 20 * time.Minute, 3: 40 * time.Minute, 8: 1280 * time.Minute,
		9: 24 * time.Hour, 30: 24 * time.Hour,
	} {
		if got := sideStoryBackoff(attempts); got != want {
			t.Errorf("backoff(%d)=%s want %s", attempts, got, want)
		}
	}
}

func TestSideStoryErrorsMatchTheirSentinels(t *testing.T) {
	var conflict error = &SideStoryRevisionConflictError{Conflicts: []SideStoryLineConflict{{JP: "テスト"}}}
	var unknown error = &SideStoryUnknownLinesError{Lines: []string{"テスト"}}
	if !errors.Is(conflict, ErrSideStoryRevisionConflict) || errors.Is(conflict, ErrSideStoryUnknownLines) {
		t.Fatalf("conflict error matching: %v", conflict)
	}
	if !errors.Is(unknown, ErrSideStoryUnknownLines) || errors.Is(unknown, ErrSideStoryRevisionConflict) {
		t.Fatalf("unknown-lines error matching: %v", unknown)
	}
	var typed *SideStoryUnknownLinesError
	if !errors.As(unknown, &typed) || len(typed.Lines) != 1 {
		t.Fatal("errors.As unknown lines")
	}
	if !errors.Is(sideStoryInvalid("x"), ErrSideStoryInvalid) {
		t.Fatal("invalid sentinel")
	}
}

func TestParseSideStoryScriptMatchesTheEventCanonicalFormAndTrimsKeys(t *testing.T) {
	value := sideStoryTestScenario("test_card_1_01",
		sideStoryTestTalk{" テスト話者 ", "  テスト台詞一\n"}, sideStoryTestTalk{"", ""})
	script, err := ParseSideStoryScript(value, "test_card_1_01")
	if err != nil {
		t.Fatal(err)
	}
	canonical, digest, err := CanonicalizeEventScenario(value, "test_card_1_01")
	if err != nil {
		t.Fatal(err)
	}
	if script.CanonicalJSON != canonical || script.SHA256 != digest || script.ScenarioID != "test_card_1_01" {
		t.Fatalf("script identity %+v", script)
	}
	if len(script.Talks) != 2 || script.Talks[0] != (SideStoryTalk{Index: 0, Body: "テスト台詞一", Speaker: "テスト話者"}) ||
		script.Talks[1] != (SideStoryTalk{Index: 1}) {
		t.Fatalf("talks %+v", script.Talks)
	}
	if _, err := ParseSideStoryScript(value, "test_card_1_02"); !errors.Is(err, ErrEventScenarioConflict) {
		t.Fatalf("scenario mismatch error=%v", err)
	}
	value["TalkData"] = []any{"テスト"}
	if _, err := ParseSideStoryScript(value, "test_card_1_01"); !errors.Is(err, ErrEventScenarioInvalid) {
		t.Fatalf("non-object TalkData error=%v", err)
	}
}
