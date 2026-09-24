package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

type sideStoryTestLine struct {
	jp, role, speaker string
	position          int
}

func sideStoryLines(t *testing.T, s *Store, kind, storyID, episodeKey string) []sideStoryTestLine {
	t.Helper()
	rows, err := s.db.Query(`SELECT jp_key,role,speaker,position FROM side_story_lines
		WHERE kind=? AND story_id=? AND episode_key=? ORDER BY position`, kind, storyID, episodeKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []sideStoryTestLine
	for rows.Next() {
		var line sideStoryTestLine
		if err := rows.Scan(&line.jp, &line.role, &line.speaker, &line.position); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return lines
}

type sideStoryTestEpisodeState struct {
	sha, lastError string
	fetchedAt      int64
	attempts       int
	nextAttemptAt  int64
}

func sideStoryEpisodeState(t *testing.T, s *Store, kind, storyID, episodeKey string) sideStoryTestEpisodeState {
	t.Helper()
	var state sideStoryTestEpisodeState
	if err := s.db.QueryRow(`SELECT script_sha256,last_error,jp_fetched_at,attempts,next_attempt_at FROM side_story_episodes
		WHERE kind=? AND story_id=? AND episode_key=?`, kind, storyID, episodeKey).
		Scan(&state.sha, &state.lastError, &state.fetchedAt, &state.attempts, &state.nextAttemptAt); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestApplySideStoryScriptReplacesTheLineSetAndKeepsSurvivingTranslations(t *testing.T) {
	s := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("100", 1, "", ""))
	first := sideStoryTestScript(t, "test_card_100_01",
		sideStoryTestTalk{"テスト話者", "テスト台詞一"},
		sideStoryTestTalk{"テスト相手", "テスト台詞二"},
		sideStoryTestTalk{"テスト話者", "テスト台詞一"},
		sideStoryTestTalk{"", "テスト話者"})
	result := mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "100", EpisodeKey: "1", JP: fetchedJP(first)})
	applied := result.Episodes[0]
	if !result.Changed || !applied.Fetched || !applied.ScriptChanged || applied.CNState != "absent" || applied.Error != "" {
		t.Fatalf("first apply %+v changed=%v", applied, result.Changed)
	}
	want := []sideStoryTestLine{
		{"テスト話100-1", "title", "", -1},
		{"テスト台詞一", "talk", "テスト話者", 0},
		{"テスト話者", "speaker", "", 1},
		{"テスト台詞二", "talk", "テスト相手", 2},
		{"テスト相手", "speaker", "", 3},
	}
	if got := sideStoryLines(t, s, "card", "100", "1"); len(got) != len(want) || strings.Join(sideStoryLineKeys(got), ",") != strings.Join(sideStoryLineKeys(want), ",") {
		t.Fatalf("lines %+v want %+v", got, want)
	} else {
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("line %d %+v want %+v", index, got[index], want[index])
			}
		}
	}
	if state := sideStoryEpisodeState(t, s, "card", "100", "1"); state.sha != first.SHA256 || state.fetchedAt != sideStoryTestNow.Unix() {
		t.Fatalf("episode state %+v", state)
	}
	mustUpdateSideStory(t, s, "card", "100", "1", "zh-CN",
		SideStoryLineEdit{JP: "テスト台詞一", Text: "测试台词一"},
		SideStoryLineEdit{JP: "テスト台詞二", Text: "测试台词二"},
		SideStoryLineEdit{JP: "テスト相手", Text: "测试对象", Source: "llm"})
	mustUpdateSideStory(t, s, "card", "100", "1", "en-US",
		SideStoryLineEdit{JP: "テスト台詞二", Text: "Test line two"},
		SideStoryLineEdit{JP: "テスト話100-1", Text: "Test title"})

	second := sideStoryTestScript(t, "test_card_100_01",
		sideStoryTestTalk{"テスト相手", "テスト台詞三"},
		sideStoryTestTalk{"テスト話者", "テスト台詞一"},
		sideStoryTestTalk{"", "テスト話100-1"})
	result = mustApplySideStory(t, s, sideStoryTestNow.Add(time.Hour), SideStoryEpisodeFetch{Kind: "card", StoryID: "100", EpisodeKey: "1", JP: fetchedJP(second)})
	applied = result.Episodes[0]
	if !result.Changed || !applied.ScriptChanged || applied.DroppedHumanLines != 2 {
		t.Fatalf("second apply %+v changed=%v", applied, result.Changed)
	}
	want = []sideStoryTestLine{
		{"テスト話100-1", "title", "", -1},
		{"テスト台詞三", "talk", "テスト相手", 0},
		{"テスト相手", "speaker", "", 1},
		{"テスト台詞一", "talk", "テスト話者", 2},
		{"テスト話者", "speaker", "", 3},
	}
	got := sideStoryLines(t, s, "card", "100", "1")
	if len(got) != len(want) {
		t.Fatalf("lines after replacement %+v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("line %d %+v want %+v", index, got[index], want[index])
		}
	}
	if row, ok := sideStoryRow(t, s, "card", "100", "1", "テスト台詞一", "zh-CN"); !ok || row.text != "测试台词一" || row.revision != 1 {
		t.Fatalf("surviving translation %+v ok=%v", row, ok)
	}
	if row, ok := sideStoryRow(t, s, "card", "100", "1", "テスト相手", "zh-CN"); !ok || row.source != "llm" {
		t.Fatalf("surviving llm translation %+v ok=%v", row, ok)
	}
	if _, ok := sideStoryRow(t, s, "card", "100", "1", "テスト台詞二", "en-US"); ok {
		t.Fatal("a vanished line kept its translation")
	}
	if row, ok := sideStoryRow(t, s, "card", "100", "1", "テスト話100-1", "en-US"); !ok || row.text != "Test title" {
		t.Fatalf("title translation %+v ok=%v", row, ok)
	}
	result = mustApplySideStory(t, s, sideStoryTestNow.Add(2*time.Hour), SideStoryEpisodeFetch{Kind: "card", StoryID: "100", EpisodeKey: "1", JP: fetchedJP(second)})
	if result.Changed || result.Episodes[0].ScriptChanged || !result.Episodes[0].Fetched {
		t.Fatalf("identical re-apply %+v changed=%v", result.Episodes[0], result.Changed)
	}
}

func sideStoryLineKeys(lines []sideStoryTestLine) []string {
	keys := make([]string, len(lines))
	for index, line := range lines {
		keys[index] = line.jp
	}
	return keys
}

func TestApplySideStoryJPFailuresBackOffAndSkipOfficialText(t *testing.T) {
	s := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("110", 1, "cn/110", ""))
	cn := sideStoryTestScript(t, "test_card_110_01", sideStoryTestTalk{"测试说话人", "测试台词一"})
	transient := SideStoryEpisodeFetch{Kind: "card", StoryID: "110", EpisodeKey: "1",
		JP: SideStoryFetchOutcome{Attempted: true, Err: "timeout", Transient: true}, CN: fetchedJP(cn)}
	for attempt, wantDelay := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute,
		160 * time.Minute, 320 * time.Minute, 640 * time.Minute, 1280 * time.Minute, 24 * time.Hour, 24 * time.Hour} {
		now := sideStoryTestNow.Add(time.Duration(attempt) * time.Minute)
		result := mustApplySideStory(t, s, now, transient)
		if result.Changed || result.Episodes[0].Fetched || result.Episodes[0].Error != "ja-JP: timeout" || result.Episodes[0].CNState != "pending" {
			t.Fatalf("attempt %d result %+v", attempt+1, result)
		}
		state := sideStoryEpisodeState(t, s, "card", "110", "1")
		if state.attempts != attempt+1 || state.nextAttemptAt != now.Add(wantDelay).Unix() || state.lastError != "ja-JP: timeout" {
			t.Fatalf("attempt %d state %+v want delay %s", attempt+1, state, wantDelay)
		}
	}
	if cnState, _ := sideStoryEpisodeStates(t, s, "card", "110", "1"); cnState != "pending" {
		t.Fatalf("CN processed without a JP script: %s", cnState)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM side_story_line_localizations WHERE story_id='110' AND jp_key<>'テスト話110-1'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("official rows written without a JP script: %d err=%v", rows, err)
	}
	for _, test := range []struct {
		outcome SideStoryFetchOutcome
		message string
	}{
		{SideStoryFetchOutcome{Attempted: true, Missing: true}, "ja-JP: not found"},
		{SideStoryFetchOutcome{Attempted: true, Err: "invalid JSON"}, "ja-JP: invalid JSON"},
	} {
		mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "110", EpisodeKey: "2", JP: test.outcome})
		if state := sideStoryEpisodeState(t, s, "card", "110", "2"); state.nextAttemptAt != sideStoryTestNow.Add(24*time.Hour).Unix() ||
			state.lastError != test.message {
			t.Fatalf("%s state %+v", test.message, state)
		}
	}
	progress, err := s.SideStoryProgressContext(context.Background())
	if err != nil || progress["card"].Errors != 2 {
		t.Fatalf("progress errors %+v err=%v", progress["card"], err)
	}
	result := mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "110", EpisodeKey: "2", CN: fetchedJP(cn)})
	if result.Changed || result.Episodes[0].Fetched || result.Episodes[0].CNState != "pending" {
		t.Fatalf("JP not attempted %+v", result)
	}
	jp := sideStoryTestScript(t, "test_card_110_01", sideStoryTestTalk{"テスト話者", "テスト台詞です"})
	result = mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "110", EpisodeKey: "1", JP: fetchedJP(jp)})
	if state := sideStoryEpisodeState(t, s, "card", "110", "1"); state.attempts != 0 || state.nextAttemptAt != 0 || state.lastError != "" {
		t.Fatalf("JP success did not reset the backoff: %+v", state)
	}
	result = mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "404", EpisodeKey: "1", JP: fetchedJP(jp)})
	if result.Changed || result.Episodes[0].Error != "episode not found" {
		t.Fatalf("unknown episode %+v", result)
	}
	// Real scripts carry ScenarioId labels such as `016048_rui01 のコピー`; the
	// asset path identifies the script, so the label is not compared.
	labelled := sideStoryTestScript(t, "test_card_110_01 のコピー",
		sideStoryTestTalk{"テスト話者", "テスト台詞です"}, sideStoryTestTalk{"テスト話者", "テスト二行目です"})
	result = mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "110", EpisodeKey: "1", JP: fetchedJP(labelled)})
	if !result.Episodes[0].Fetched || result.Episodes[0].Error != "" || len(sideStoryLines(t, s, "card", "110", "1")) != 4 {
		t.Fatalf("script with a ScenarioId label %+v lines=%d", result.Episodes[0], len(sideStoryLines(t, s, "card", "110", "1")))
	}
}

func TestApplySideStoryOfficialImportPairsByTalkDataIndex(t *testing.T) {
	s := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("120", 1, "cn/120", "en/120"))
	jp := sideStoryTestScript(t, "test_card_120_01",
		sideStoryTestTalk{"テスト話者", "テスト台詞です"},
		sideStoryTestTalk{"テスト相手", "……"},
		sideStoryTestTalk{"テスト話者", "テスト返事だよ"},
		sideStoryTestTalk{"テスト相手", "テスト空欄ね"},
		sideStoryTestTalk{"テスト話者", "テスト同文さ"},
		sideStoryTestTalk{"テスト話者", "テスト台詞です"})
	cn := sideStoryTestScript(t, "test_card_120_01",
		sideStoryTestTalk{"测试说话人", "测试台词一"},
		sideStoryTestTalk{"测试对象", "……"},
		sideStoryTestTalk{"测试说话人", "测试回答"},
		sideStoryTestTalk{"测试对象", ""},
		sideStoryTestTalk{"测试说话人", "テスト同文さ"},
		sideStoryTestTalk{"测试说话人", "测试台词重复"})
	result := mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "120", EpisodeKey: "1",
		JP: fetchedJP(jp), CN: fetchedJP(cn)})
	applied := result.Episodes[0]
	if applied.CNState != "imported" || applied.ENState != "pending" || applied.OfficialWritten != 5 || !result.Changed {
		t.Fatalf("CN import %+v", applied)
	}
	for jpKey, want := range map[string]string{
		"テスト台詞です": "测试台词一", "……": "……", "テスト返事だよ": "测试回答", "テスト話者": "测试说话人", "テスト相手": "测试对象",
	} {
		if row, ok := sideStoryRow(t, s, "card", "120", "1", jpKey, "zh-CN"); !ok ||
			row != (sideStoryTestRow{text: want, source: "official", updatedBy: "sync", revision: 1}) {
			t.Errorf("%s row %+v ok=%v", jpKey, row, ok)
		}
	}
	for _, skipped := range []string{"テスト空欄ね", "テスト同文さ"} {
		if _, ok := sideStoryRow(t, s, "card", "120", "1", skipped, "zh-CN"); ok {
			t.Errorf("%s was written", skipped)
		}
	}

	mustUpdateSideStory(t, s, "card", "120", "1", "zh-CN",
		SideStoryLineEdit{JP: "テスト台詞です", Text: "测试人工台词"},
		SideStoryLineEdit{JP: "テスト空欄ね", Text: ""},
		SideStoryLineEdit{JP: "テスト返事だよ", Text: "测试模型台词", Source: "llm"})
	revised := sideStoryTestScript(t, "test_card_120_01",
		sideStoryTestTalk{"测试说话人", "测试台词一改"},
		sideStoryTestTalk{"测试对象", "……"},
		sideStoryTestTalk{"测试说话人", "测试回答改"},
		sideStoryTestTalk{"测试对象", "测试空栏"},
		sideStoryTestTalk{"测试说话人", "测试同文"},
		sideStoryTestTalk{"测试说话人", "测试台词重复"})
	result = mustApplySideStory(t, s, sideStoryTestNow.Add(time.Hour), SideStoryEpisodeFetch{Kind: "card", StoryID: "120", EpisodeKey: "1",
		JP: fetchedJP(jp), CN: fetchedJP(revised), EN: SideStoryFetchOutcome{Attempted: true, Missing: true}})
	if applied := result.Episodes[0]; applied.CNState != "imported" || applied.ENState != "pending" || applied.OfficialWritten != 2 {
		t.Fatalf("second CN import %+v", applied)
	}
	for jpKey, want := range map[string]sideStoryTestRow{
		"テスト台詞です": {text: "测试人工台词", source: "human", updatedBy: "test-editor", revision: 2},
		"テスト空欄ね":  {text: "", source: "human", updatedBy: "test-editor", revision: 1},
		"テスト返事だよ": {text: "测试回答改", source: "official", updatedBy: "sync", revision: 3},
		"テスト同文さ":  {text: "测试同文", source: "official", updatedBy: "sync", revision: 1},
		"……":      {text: "……", source: "official", updatedBy: "sync", revision: 1},
		"テスト話者":   {text: "测试说话人", source: "official", updatedBy: "sync", revision: 1},
	} {
		if row, _ := sideStoryRow(t, s, "card", "120", "1", jpKey, "zh-CN"); row != want {
			t.Errorf("%s row %+v want %+v", jpKey, row, want)
		}
	}
	result = mustApplySideStory(t, s, sideStoryTestNow.Add(2*time.Hour), SideStoryEpisodeFetch{Kind: "card", StoryID: "120", EpisodeKey: "1",
		JP: fetchedJP(jp), CN: fetchedJP(revised)})
	if result.Changed || result.Episodes[0].OfficialWritten != 0 {
		t.Fatalf("identical re-import wrote rows: %+v", result.Episodes[0])
	}
	en := sideStoryTestScript(t, "test_card_120_02", sideStoryTestTalk{"Tester", "Test line one"})
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "120", EpisodeKey: "2",
		JP: fetchedJP(sideStoryTestScript(t, "test_card_120_02", sideStoryTestTalk{"テスト話者", "テスト台詞です"})), EN: fetchedJP(en)})
	if row, ok := sideStoryRow(t, s, "card", "120", "2", "テスト台詞です", "en-US"); !ok || row.text != "Test line one" || row.source != "official" {
		t.Fatalf("EN import row %+v ok=%v", row, ok)
	}
}

func TestApplySideStoryOfficialImportRejectsMismatchedScriptsAndRetriesAMirroredOne(t *testing.T) {
	s := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("130", 1, "cn/130", "en/130"))
	jpTalks := []sideStoryTestTalk{
		{"テスト話者", "テスト台詞です"}, {"テスト相手", "テスト返事だよ"}, {"テスト話者", "テストもう一つ"},
		{"テスト相手", "テスト最後ね"}, {"テスト話者", "……"},
	}
	jp := sideStoryTestScript(t, "test_card_130_01", jpTalks...)
	apply := func(cn *SideStoryScript) SideStoryEpisodeApply {
		t.Helper()
		return mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "130", EpisodeKey: "1",
			JP: fetchedJP(jp), CN: fetchedJP(cn)}).Episodes[0]
	}
	official := func() int {
		var rows int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM side_story_line_localizations WHERE story_id='130' AND source='official'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	short := sideStoryTestScript(t, "test_card_130_01", sideStoryTestTalk{"测试说话人", "测试台词一"})
	if applied := apply(short); applied.CNState != "mismatch" || applied.OfficialWritten != 0 || !strings.Contains(applied.Error, "zh-CN: TalkData length mismatch (5 != 1)") {
		t.Fatalf("length mismatch %+v", applied)
	}
	mirrored := sideStoryTestScript(t, "test_card_130_01",
		sideStoryTestTalk{"测试说话人", "テスト台詞です"}, sideStoryTestTalk{"测试对象", "テスト返事だよ"},
		sideStoryTestTalk{"测试说话人", "テストもう一つ"}, sideStoryTestTalk{"测试对象", "测试最后"}, sideStoryTestTalk{"测试说话人", "……"})
	if applied := apply(mirrored); applied.CNState != "pending" || applied.OfficialWritten != 0 || official() != 0 ||
		applied.Error != "zh-CN: official script repeats the Japanese text" {
		t.Fatalf("mirrored Japanese %+v official rows=%d", applied, official())
	}
	if state := sideStoryEpisodeState(t, s, "card", "130", "1"); state.attempts != 0 ||
		state.nextAttemptAt != sideStoryTestNow.Add(24*time.Hour).Unix() {
		t.Fatalf("mirrored Japanese retry state %+v", state)
	}
	// The official ScenarioId is only a label, like the JP one.
	fewIdentical := sideStoryTestScript(t, "test_card_130_01 のコピー",
		sideStoryTestTalk{"测试说话人", "测试台词一"}, sideStoryTestTalk{"测试对象", "テスト返事だよ"},
		sideStoryTestTalk{"测试说话人", "测试又一句"}, sideStoryTestTalk{"测试对象", "测试最后"}, sideStoryTestTalk{"测试说话人", "……"})
	if applied := apply(fewIdentical); applied.CNState != "imported" || applied.OfficialWritten != 6 {
		t.Fatalf("translation with a few identical lines %+v", applied)
	}
	for _, test := range []struct {
		outcome SideStoryFetchOutcome
		state   string
	}{
		{SideStoryFetchOutcome{Attempted: true, Missing: true}, "pending"},
		{SideStoryFetchOutcome{Attempted: true, Err: "forbidden"}, "error"},
		{SideStoryFetchOutcome{Attempted: true, Err: "timeout", Transient: true}, "pending"},
	} {
		result := mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "130", EpisodeKey: "1",
			JP: fetchedJP(jp), EN: test.outcome})
		if result.Episodes[0].ENState != test.state || result.Episodes[0].CNState != "imported" {
			t.Fatalf("EN %+v gave %+v", test.outcome, result.Episodes[0])
		}
	}
	if state := sideStoryEpisodeState(t, s, "card", "130", "1"); state.attempts != 1 ||
		state.nextAttemptAt != sideStoryTestNow.Add(10*time.Minute).Unix() || state.lastError != "en-US: timeout" {
		t.Fatalf("transient EN failure state %+v", state)
	}
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "130", EpisodeKey: "1",
		JP: fetchedJP(jp), EN: SideStoryFetchOutcome{Attempted: true, Err: "timeout", Transient: true}})
	if state := sideStoryEpisodeState(t, s, "card", "130", "1"); state.attempts != 2 || state.nextAttemptAt != sideStoryTestNow.Add(20*time.Minute).Unix() {
		t.Fatalf("second transient EN failure state %+v", state)
	}

	changed := sideStoryTestScript(t, "test_card_130_01", append(jpTalks, sideStoryTestTalk{"テスト話者", "テスト追加だ"})...)
	result := mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "130", EpisodeKey: "1", JP: fetchedJP(changed)})
	if applied := result.Episodes[0]; !applied.ScriptChanged || applied.CNState != "pending" || applied.ENState != "pending" {
		t.Fatalf("changed JP script did not requeue the official import: %+v", applied)
	}
}

func TestApplySideStoryRequeueAndANewAssetPathOutrankAnother404(t *testing.T) {
	s := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("150", 1, "cn/150", "en/150"))
	jp := sideStoryTestScript(t, "test_card_150_01", sideStoryTestTalk{"テスト話者", "テスト台詞です"})
	revised := sideStoryTestScript(t, "test_card_150_01", sideStoryTestTalk{"テスト話者", "テスト台詞改です"})
	cn := sideStoryTestScript(t, "test_card_150_01", sideStoryTestTalk{"测试说话人", "测试台词一"})
	missing := SideStoryFetchOutcome{Attempted: true, Missing: true}
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "150", EpisodeKey: "1",
		JP: fetchedJP(jp), CN: fetchedJP(cn), EN: missing})

	later := sideStoryTestNow.Add(24 * time.Hour)
	result := mustApplySideStory(t, s, later, SideStoryEpisodeFetch{Kind: "card", StoryID: "150", EpisodeKey: "1",
		JP: fetchedJP(revised), EN: missing})
	if applied := result.Episodes[0]; applied.CNState != "pending" || applied.ENState != "pending" || applied.Error != "en-US: not found" {
		t.Fatalf("JP change with an EN 404 %+v", applied)
	}
	if state := sideStoryEpisodeState(t, s, "card", "150", "1"); state.attempts != 0 || state.nextAttemptAt != 0 {
		t.Fatalf("requeued CN waits for the EN 404 retry: %+v", state)
	}

	mustApplySideStory(t, s, later, SideStoryEpisodeFetch{Kind: "card", StoryID: "150", EpisodeKey: "1",
		JP: fetchedJP(revised), CN: fetchedJP(cn), EN: missing})
	if state := sideStoryEpisodeState(t, s, "card", "150", "1"); state.nextAttemptAt != later.Add(24*time.Hour).Unix() {
		t.Fatalf("EN 404 state %+v", state)
	}
	if synced := mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("150", 1, "cn/150", "en/150-moved")); synced.OfficialRequeued != 2 {
		t.Fatalf("catalog with new EN paths %+v", synced)
	}
	if state := sideStoryEpisodeState(t, s, "card", "150", "1"); state.attempts != 0 || state.nextAttemptAt != 0 || state.lastError != "" {
		t.Fatalf("new EN path waits for the old path's 404 retry: %+v", state)
	}
}

func TestApplySideStoryOfficial404StaysPendingAndRetriesADayLater(t *testing.T) {
	s := newSideStoryTestStore(t)
	mustSyncSideStoryCatalog(t, s, SideStoryKindCard, sideStoryTestCard("140", 1, "cn/140", "en/140"))
	jp := sideStoryTestScript(t, "test_card_140_01", sideStoryTestTalk{"テスト話者", "テスト台詞です"})
	cn := sideStoryTestScript(t, "test_card_140_01", sideStoryTestTalk{"测试说话人", "测试台词一"})
	en := sideStoryTestScript(t, "test_card_140_01", sideStoryTestTalk{"Test speaker", "Test line one"})
	missing := SideStoryFetchOutcome{Attempted: true, Missing: true}
	queued := func(now time.Time) bool {
		t.Helper()
		items, err := s.SideStoryWorkQueueContext(context.Background(), 10, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.StoryID == "140" && item.EpisodeKey == "1" {
				return true
			}
		}
		return false
	}
	mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "140", EpisodeKey: "1",
		JP: fetchedJP(jp), CN: SideStoryFetchOutcome{Attempted: true, Err: "timeout", Transient: true}, EN: fetchedJP(en)})
	result := mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "140", EpisodeKey: "1",
		JP: fetchedJP(jp), CN: missing})
	if applied := result.Episodes[0]; applied.CNState != "pending" || applied.ENState != "imported" || applied.Error != "zh-CN: not found" {
		t.Fatalf("CN 404 %+v", applied)
	}
	if state := sideStoryEpisodeState(t, s, "card", "140", "1"); state.attempts != 0 ||
		state.nextAttemptAt != sideStoryTestNow.Add(24*time.Hour).Unix() || state.lastError != "zh-CN: not found" {
		t.Fatalf("CN 404 state %+v", state)
	}
	if queued(sideStoryTestNow.Add(24*time.Hour - time.Second)) {
		t.Fatal("a 404 episode was queued before its day of backoff")
	}
	if !queued(sideStoryTestNow.Add(24 * time.Hour)) {
		t.Fatal("a 404 episode was not queued after its day of backoff")
	}

	later := sideStoryTestNow.Add(24 * time.Hour)
	result = mustApplySideStory(t, s, later, SideStoryEpisodeFetch{Kind: "card", StoryID: "140", EpisodeKey: "1", JP: fetchedJP(jp), CN: fetchedJP(cn)})
	if applied := result.Episodes[0]; applied.CNState != "imported" || applied.OfficialWritten != 2 || applied.Error != "" {
		t.Fatalf("CN fetch after the 404 %+v", applied)
	}
	if row, ok := sideStoryRow(t, s, "card", "140", "1", "テスト台詞です", "zh-CN"); !ok || row.text != "测试台词一" || row.source != "official" {
		t.Fatalf("CN row after the 404 %+v ok=%v", row, ok)
	}
	if state := sideStoryEpisodeState(t, s, "card", "140", "1"); state.nextAttemptAt != 0 || state.lastError != "" || queued(later) {
		t.Fatalf("imported episode state %+v", state)
	}

	result = mustApplySideStory(t, s, sideStoryTestNow, SideStoryEpisodeFetch{Kind: "card", StoryID: "140", EpisodeKey: "2",
		JP: fetchedJP(sideStoryTestScript(t, "test_card_140_02")), CN: missing, EN: SideStoryFetchOutcome{Attempted: true, Err: "timeout", Transient: true}})
	if applied := result.Episodes[0]; applied.CNState != "pending" || applied.ENState != "pending" || applied.Error != "zh-CN: not found; en-US: timeout" {
		t.Fatalf("CN 404 with an EN timeout %+v", applied)
	}
	if state := sideStoryEpisodeState(t, s, "card", "140", "2"); state.attempts != 1 || state.nextAttemptAt != sideStoryTestNow.Add(10*time.Minute).Unix() {
		t.Fatalf("a transient EN failure did not keep its shorter backoff: %+v", state)
	}
}
