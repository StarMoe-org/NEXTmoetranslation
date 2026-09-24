// Package importer loads a translations directory (legacy layout: category JSON
// files plus an eventStory/ subdir) into the SQLite-backed stores. It is shared
// by backup restore; the migration command has its own copy with verification.
package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// Result summarizes an import.
type Result struct {
	Categories   int
	Entries      int
	EventStories int
	Warnings     []string
}

type Payload struct {
	Categories map[string]model.Category
	Events     []store.LegacyEventRestore
}

// ReadDir parses a complete restore candidate without mutating the database.
// Callers that need all-or-nothing restore can validate additive content and
// commit this payload with it in one transaction.
func ReadDir(src string) (Payload, Result, error) {
	return ReadDirContext(context.Background(), src)
}

func ReadDirContext(ctx context.Context, src string) (Payload, Result, error) {
	return readDirContext(ctx, src, "restore")
}

func readDirContext(ctx context.Context, src, validationLabel string) (Payload, Result, error) {
	payload := Payload{Categories: map[string]model.Category{}}
	var res Result
	for _, cat := range model.SupportedCategories {
		if err := ctx.Err(); err != nil {
			return payload, res, err
		}
		category, warnings, err := loadCategory(src, cat)
		if err != nil {
			return payload, res, err
		}
		payload.Categories[cat] = category
		res.Warnings = append(res.Warnings, warnings...)
		res.Categories++
		for _, entries := range category {
			res.Entries += len(entries)
		}
	}
	eventDir := filepath.Join(src, "eventStory")
	info, err := os.Stat(eventDir)
	if err != nil || !info.IsDir() {
		return payload, res, fmt.Errorf("missing eventStory directory")
	}
	storyFiles, err := filepath.Glob(filepath.Join(eventDir, "event_*.json"))
	if err != nil {
		return payload, res, err
	}
	for _, file := range storyFiles {
		if err := ctx.Err(); err != nil {
			return payload, res, err
		}
		eventID := eventIDFromPath(file)
		if eventID <= 0 {
			return payload, res, fmt.Errorf("invalid event story filename %s", filepath.Base(file))
		}
		story, err := legacy.LoadEventStory(file)
		if err != nil {
			return payload, res, fmt.Errorf("event %d: %w", eventID, err)
		}
		episodes := make([]store.OrderedEpisode, 0, len(story.EpisodeKeys))
		for _, no := range story.EpisodeKeys {
			episode := story.Episodes[no]
			episodes = append(episodes, store.OrderedEpisode{
				EpisodeNo: no, ScenarioID: episode.ScenarioID, Title: episode.Title,
				TitleSource: episode.TitleSource, TalkKeys: episode.TalkKeys,
				TalkData: episode.TalkData, TalkSources: episode.TalkSources,
				SpeakerNames: episode.SpeakerNames,
			})
		}
		payload.Events = append(payload.Events, store.LegacyEventRestore{EventID: eventID, Meta: story.Meta, Episodes: episodes})
		res.EventStories++
	}
	if validationLabel != "" {
		if err := validateCompletePayload(payload, res, validationLabel); err != nil {
			return payload, res, err
		}
	}
	return payload, res, nil
}

// ReadSeedDir applies the stricter first-boot contract. A seed is a complete,
// non-empty production snapshot, not an optional partial restore: every
// category except restore-optional ones must contain at least one entry and at
// least one valid event story with complete metadata and episode
// representation must be present.
func ReadSeedDir(ctx context.Context, src string) (Payload, Result, error) {
	payload, result, err := readDirContext(ctx, src, "")
	if err != nil {
		return payload, result, err
	}
	if err := validateCompletePayload(payload, result, "seed"); err != nil {
		return payload, result, err
	}
	return payload, result, nil
}

func validateCompletePayload(payload Payload, result Result, label string) error {
	if result.Categories != len(model.SupportedCategories) {
		return fmt.Errorf("%s contains %d of %d expected categories", label, result.Categories, len(model.SupportedCategories))
	}
	for _, categoryName := range model.SupportedCategories {
		category := payload.Categories[categoryName]
		entries := 0
		for field, values := range category {
			if strings.TrimSpace(field) == "" {
				return fmt.Errorf("%s category %s contains an empty field name", label, categoryName)
			}
			for key, entry := range values {
				entries++
				if key == "" {
					return fmt.Errorf("%s category %s/%s contains an empty source key", label, categoryName, field)
				}
				if entry.Source != "" && !model.IsValidSource(entry.Source) {
					return fmt.Errorf("%s category %s/%s key %q has invalid source %q", label, categoryName, field, key, entry.Source)
				}
			}
		}
		if entries == 0 && !model.IsRestoreOptionalCategory(categoryName) {
			return fmt.Errorf("%s category %s is empty", label, categoryName)
		}
	}
	if len(payload.Events) == 0 {
		return fmt.Errorf("%s contains no event stories", label)
	}
	seenEvents := make(map[int]bool, len(payload.Events))
	for _, event := range payload.Events {
		if event.EventID <= 0 || seenEvents[event.EventID] {
			return fmt.Errorf("%s contains invalid or duplicate event id %d", label, event.EventID)
		}
		seenEvents[event.EventID] = true
		if strings.TrimSpace(event.Meta.Source) == "" || strings.TrimSpace(event.Meta.Version) == "" || event.Meta.LastUpdated <= 0 {
			return fmt.Errorf("%s event %d has incomplete metadata", label, event.EventID)
		}
		if len(event.Episodes) == 0 {
			return fmt.Errorf("%s event %d contains no episodes", label, event.EventID)
		}
		seenEpisodes := make(map[string]bool, len(event.Episodes))
		for _, episode := range event.Episodes {
			if strings.TrimSpace(episode.EpisodeNo) == "" || seenEpisodes[episode.EpisodeNo] {
				return fmt.Errorf("%s event %d contains an invalid or duplicate episode", label, event.EventID)
			}
			seenEpisodes[episode.EpisodeNo] = true
			if strings.TrimSpace(episode.ScenarioID) == "" || episode.TalkData == nil {
				return fmt.Errorf("%s event %d episode %s is incomplete", label, event.EventID, episode.EpisodeNo)
			}
			if len(episode.TalkKeys) != len(episode.TalkData) {
				return fmt.Errorf("%s event %d episode %s has incomplete talk order", label, event.EventID, episode.EpisodeNo)
			}
			for _, key := range episode.TalkKeys {
				if _, ok := episode.TalkData[key]; !ok {
					return fmt.Errorf("%s event %d episode %s talk order references missing key %q", label, event.EventID, episode.EpisodeNo, key)
				}
			}
			for key := range episode.TalkSources {
				if _, ok := episode.TalkData[key]; !ok {
					return fmt.Errorf("%s event %d episode %s source references missing key %q", label, event.EventID, episode.EpisodeNo, key)
				}
			}
			for key := range episode.SpeakerNames {
				if _, ok := episode.TalkData[key]; !ok {
					return fmt.Errorf("%s event %d episode %s speaker references missing key %q", label, event.EventID, episode.EpisodeNo, key)
				}
			}
		}
	}
	return nil
}

// loadCategory treats a restore-optional category whose flat and full files are
// both absent as empty; a single missing file is still a broken projection.
func loadCategory(src, category string) (model.Category, []string, error) {
	if model.IsRestoreOptionalCategory(category) {
		flatMissing, err := fileMissing(filepath.Join(src, category+".json"))
		if err != nil {
			return nil, nil, err
		}
		fullMissing, err := fileMissing(filepath.Join(src, category+".full.json"))
		if err != nil {
			return nil, nil, err
		}
		if flatMissing && fullMissing {
			return model.Category{}, []string{fmt.Sprintf("%s: absent from source, imported as empty", category)}, nil
		}
	}
	return loadCompleteCategory(src, category)
}

func fileMissing(path string) (bool, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, err
}

func loadCompleteCategory(src, category string) (model.Category, []string, error) {
	flatPath := filepath.Join(src, category+".json")
	fullPath := filepath.Join(src, category+".full.json")
	flatBody, err := os.ReadFile(flatPath)
	if err != nil {
		return nil, nil, fmt.Errorf("missing %s.json: %w", category, err)
	}
	if _, err := os.Stat(fullPath); err != nil {
		return nil, nil, fmt.Errorf("missing %s.full.json: %w", category, err)
	}
	var flat map[string]map[string]string
	if err := legacy.UnmarshalUnique(flatBody, &flat); err != nil {
		return nil, nil, fmt.Errorf("%s.json: %w", category, err)
	}
	loaded, warnings, err := legacy.LoadCategory(src, category)
	if err != nil {
		return nil, warnings, err
	}
	flatOnly, fullOnly := 0, 0
	for field, entries := range flat {
		if loaded[field] == nil {
			loaded[field] = make(map[string]model.Entry, len(entries))
		}
		for key, text := range entries {
			entry, exists := loaded[field][key]
			if !exists {
				loaded[field][key] = model.Entry{Text: text, Source: model.SourceUnknown}
				flatOnly++
				continue
			}
			if entry.Text != text {
				return nil, warnings, fmt.Errorf("%s flat/full conflict at field %s key %q", category, field, key)
			}
		}
	}
	for field, entries := range loaded {
		for key := range entries {
			if _, exists := flat[field][key]; !exists {
				fullOnly++
			}
		}
	}
	if category != "gacha" && (flatOnly > 0 || fullOnly > 0) {
		return nil, warnings, fmt.Errorf("%s flat/full projections do not match", category)
	}
	if flatOnly > 0 || fullOnly > 0 {
		warnings = append(warnings, fmt.Sprintf("%s: reconciled flat/full union (%d flat-only, %d full-only)", category, flatOnly, fullOnly))
	}
	return loaded, warnings, nil
}

// ImportDir loads every category and event story under src into the stores,
// then fires a single change notification so public files regenerate. src must
// directly contain X.json/X.full.json and an eventStory/ subdir.
func ImportDir(src string, s *store.Store, es *store.EventStore) (Result, error) {
	return ImportDirContext(context.Background(), src, s, es)
}

func ImportDirContext(ctx context.Context, src string, s *store.Store, es *store.EventStore) (Result, error) {
	_ = es
	payload, res, err := ReadDirContext(ctx, src)
	if err != nil {
		return res, err
	}
	if err := s.RestoreBackupContext(ctx, payload.Categories, payload.Events, nil, store.EventContentExport{}, store.LyricsContentExport{}, false, "import"); err != nil {
		return res, err
	}
	return res, nil
}

func eventIDFromPath(path string) int {
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	base = strings.TrimPrefix(base, "event_")
	n, err := strconv.Atoi(base)
	if err != nil {
		return -1
	}
	return n
}

// ValidateDir parses the complete legacy projection before a destructive
// restore. Every generated category pair and the event directory are required,
// except restore-optional categories that are absent altogether.
func ValidateDir(src string) error {
	_, _, err := ReadDir(src)
	return err
}
