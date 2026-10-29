package store

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
)

func TestStreamingBackupJSONParityWithExportStructs(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "stream-parity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	st := New(database)
	es := NewEventStore(database)

	assertParity := func(label string) {
		t.Helper()

		exportedLocs, err := st.ExportEntryLocalizationsContext(ctx)
		if err != nil {
			t.Fatalf("%s ExportEntryLocalizationsContext: %v", label, err)
		}
		wantLocs, err := json.MarshalIndent(exportedLocs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		var gotLocs bytes.Buffer
		nLocs, err := st.StreamEntryLocalizationsJSONContext(ctx, &gotLocs)
		if err != nil {
			t.Fatalf("%s StreamEntryLocalizationsJSONContext: %v", label, err)
		}
		if nLocs != len(exportedLocs) || !bytes.Equal(gotLocs.Bytes(), wantLocs) {
			t.Fatalf("%s entry localizations JSON mismatch (n=%d want=%d):\ngot:\n%s\nwant:\n%s", label, nLocs, len(exportedLocs), gotLocs.String(), string(wantLocs))
		}

		exportedEvents, err := st.ExportEventContentContext(ctx)
		if err != nil {
			t.Fatalf("%s ExportEventContentContext: %v", label, err)
		}
		wantEvents, err := json.MarshalIndent(exportedEvents, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		var gotEvents bytes.Buffer
		nEventRecords, nScenarios, err := st.StreamEventContentJSONContext(ctx, &gotEvents)
		if err != nil {
			t.Fatalf("%s StreamEventContentJSONContext: %v", label, err)
		}
		wantEventRecordCount := len(exportedEvents.Segments) + len(exportedEvents.Localizations) + len(exportedEvents.LocaleMeta)
		if nEventRecords != wantEventRecordCount || nScenarios != len(exportedEvents.Scenarios) || !bytes.Equal(gotEvents.Bytes(), wantEvents) {
			t.Fatalf("%s event content JSON mismatch (records=%d/%d scenarios=%d/%d):\ngot:\n%s\nwant:\n%s",
				label, nEventRecords, wantEventRecordCount, nScenarios, len(exportedEvents.Scenarios), gotEvents.String(), string(wantEvents))
		}

		exportedSide, err := st.ExportSideStoryContentContext(ctx)
		if err != nil {
			t.Fatalf("%s ExportSideStoryContentContext: %v", label, err)
		}
		wantSide, err := json.MarshalIndent(exportedSide, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		var gotSide bytes.Buffer
		nSide, err := st.StreamSideStoryContentJSONContext(ctx, &gotSide)
		if err != nil {
			t.Fatalf("%s StreamSideStoryContentJSONContext: %v", label, err)
		}
		wantSideCount := len(exportedSide.Stories) + len(exportedSide.Episodes) + len(exportedSide.Lines) + len(exportedSide.Localizations)
		if nSide != wantSideCount || !bytes.Equal(gotSide.Bytes(), wantSide) {
			t.Fatalf("%s side story content JSON mismatch (n=%d want=%d):\ngot:\n%s\nwant:\n%s", label, nSide, wantSideCount, gotSide.String(), string(wantSide))
		}
	}

	// 1. Empty database parity.
	assertParity("empty")

	// 2. Populated database parity (including special HTML characters <, >, &).
	if _, err := database.Exec(`INSERT INTO entry_localizations(category, field, jp_key, locale, text, source, updated_at, updated_by, revision) VALUES
		('cards', 'prefix', 'ミク <1> & リン', 'zh-CN', '初音 <1> & 铃', 'cn', 1700000010, 'tester', 1),
		('cards', 'prefix', 'ミク <1> & リン', 'en-US', 'Miku <1> & Rin', 'official', 1700000020, 'tester', 2)`); err != nil {
		t.Fatal(err)
	}

	rawScenario := map[string]any{
		"ScenarioId":        "event_01_01",
		"Snippets":          []any{},
		"AppearCharacters":  []any{},
		"SpecialEffectData": []any{},
		"TalkData": []any{
			map[string]any{
				"Body":              "こんにちは <世界> & 歌",
				"WindowDisplayName": "ミク",
				"TalkCharacters":    []any{map[string]any{"Character2dId": 21}},
				"Voices":            []any{map[string]any{"VoiceId": "v1"}},
			},
		},
	}
	canon, sha, err := CanonicalizeEventScenario(rawScenario, "event_01_01")
	if err != nil {
		t.Fatal(err)
	}
	if err := es.ImportOrdered(1, model.EventStoryMeta{Source: model.SourceLLM, Version: "1.0", LastUpdated: 1700000123}, []OrderedEpisode{{
		EpisodeNo:             "1",
		ScenarioID:            "event_01_01",
		Title:                 "第1話 <開幕>",
		TitleSource:           model.SourceCN,
		TalkData:              map[string]string{"こんにちは <世界> & 歌": "你好 <世界> & 歌"},
		TalkSources:           map[string]string{"こんにちは <世界> & 歌": model.SourceLLM},
		SpeakerNames:          map[string]string{"こんにちは <世界> & 歌": "ミク"},
		ScenarioCanonicalJSON: canon,
		ScenarioSHA256:        sha,
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := database.Exec(`INSERT INTO side_stories(kind, story_id, title, character_id, area_id, area_category, action_set_id, released_at, updated_at) VALUES
		('card', '101', 'カード101', 21, 0, '', 0, 1700000000, 1700000010),
		('area', '5001', 'エリア会話', 0, 3, 'sekai', 5001, 1700000000, 1700000020)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO side_story_episodes(kind, story_id, episode_key, scenario_id, title_jp, position, jp_asset_path, cn_asset_path, en_asset_path, script_sha256, jp_fetched_at, jp_refetch, cn_state, en_state, attempts, next_attempt_at, last_error, updated_at) VALUES
		('card', '101', '1', 'sc_card_101_1', '前編', 0, 'a', 'b', 'c', ?, 1700000005, 0, 'imported', 'pending', 0, 0, '', 1700000010),
		('area', '5001', '1', 'sc_area_5001', '会話', 0, 'a', 'b', 'c', ?, 1700000005, 0, 'imported', 'pending', 0, 0, '', 1700000020)`, sha, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO side_story_lines(kind, story_id, episode_key, jp_key, role, speaker, position) VALUES
		('card', '101', '1', '台詞A & B', 'talk', 'ミク', 0),
		('area', '5001', '1', 'エリア台詞', 'talk', 'リン', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO side_story_line_localizations(kind, story_id, episode_key, jp_key, locale, text, source, revision, updated_by, updated_at) VALUES
		('card', '101', '1', '台詞A & B', 'zh-CN', '台词A & B', 'official', 1, 'seed', 1700000010),
		('area', '5001', '1', 'エリア台詞', 'zh-CN', '区域台词', 'llm', 1, 'seed', 1700000020)`); err != nil {
		t.Fatal(err)
	}

	assertParity("populated")

	for _, locale := range []string{model.LocaleChinese, model.LocaleEnglish} {
		wantMap, err := st.SideStoryPublicFilesContext(ctx, locale)
		if err != nil {
			t.Fatal(err)
		}
		gotMap := map[string]SideStoryPublicFile{}
		if err := st.ForEachSideStoryPublicFileContext(ctx, locale, func(key string, file SideStoryPublicFile) error {
			gotMap[key] = file
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotMap, wantMap) {
			t.Fatalf("ForEachSideStoryPublicFileContext(%s) mismatch: got=%#v want=%#v", locale, gotMap, wantMap)
		}
	}
}
