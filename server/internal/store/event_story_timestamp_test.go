package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
)

// aiTranslatedEventStory imports one event story and backdates both the legacy
// row and the zh-CN locale metadata, as an AI run leaves them.
func aiTranslatedEventStory(t *testing.T, database *db.DB, events *EventStore, eventID int, stamp int64) {
	t.Helper()
	scenarioID := fmt.Sprintf("scenario-%d", eventID)
	canonical, digest, err := CanonicalizeEventScenario(map[string]any{
		"ScenarioId":        scenarioID,
		"Snippets":          []any{},
		"TalkData":          []any{map[string]any{"WindowDisplayName": "角色", "Body": "台词", "Voices": []any{}}},
		"SpecialEffectData": []any{},
		"AppearCharacters":  []any{},
	}, scenarioID)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.ImportOrdered(eventID, model.EventStoryMeta{Source: model.SourceLLM, LastUpdated: stamp}, []OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: scenarioID, ScenarioCanonicalJSON: canonical, ScenarioSHA256: digest,
		Title: "标题", TitleSource: model.SourceLLM,
		Lines: []OrderedLine{
			{JPKey: "台词", Text: "机翻台词", Source: model.SourceLLM, ScenarioPosition: 0, Field: "body"},
			{JPKey: "角色", Text: "角色", Source: model.SourceLLM, ScenarioPosition: 1, Field: "speaker"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO event_story_locale_meta(event_id, locale, last_updated) VALUES (?, ?, ?)
		ON CONFLICT(event_id, locale) DO UPDATE SET last_updated=excluded.last_updated`,
		eventID, model.LocaleChinese, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestHumanEventEditsAdvanceTheSummaryTimestamp(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "event-timestamp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	events := NewEventStore(database)
	aiStamp := time.Now().Unix() - 3600
	aiTranslatedEventStory(t, database, events, 301, aiStamp)
	aiTranslatedEventStory(t, database, events, 302, aiStamp)
	aiTranslatedEventStory(t, database, events, 303, aiStamp)

	if err := events.UpdateLine(301, "1", "台词", "人工台词", model.SourceHuman, "talk"); err != nil {
		t.Fatal(err)
	}
	if err := events.PromoteHuman(302); err != nil {
		t.Fatal(err)
	}
	detail, err := events.DetailLocale(303, model.LocaleChinese)
	if err != nil {
		t.Fatal(err)
	}
	var target model.EventStorySegment
	for _, segment := range detail.Episodes["1"].Segments {
		if segment.Kind == "talk" && segment.Japanese == "台词" {
			target = segment
		}
	}
	if target.ID == "" {
		t.Fatalf("no talk segment in %+v", detail.Episodes["1"].Segments)
	}
	if err := events.UpdateLineLocaleRevision(303, "1", "台词", target.ID, target.SourceHash,
		"分段台词", model.SourceHuman, "talk", model.LocaleChinese, "editor", nil); err != nil {
		t.Fatal(err)
	}
	summaries, err := events.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 3 {
		t.Fatalf("summaries = %+v", summaries)
	}
	for _, summary := range summaries {
		if summary.LastUpdated <= aiStamp {
			t.Fatalf("event %d summary timestamp stuck at the AI run: %d, want > %d",
				summary.EventID, summary.LastUpdated, aiStamp)
		}
	}
}
