package translator

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// AITranslateAllResult summarizes a bulk event-story AI translation run.
type AITranslateAllResult struct {
	Provider        string `json:"provider"`
	TotalFields     int    `json:"totalFields"`
	TotalCandidates int    `json:"totalCandidates"`
	TotalTranslated int    `json:"totalTranslated"`
	TotalSkipped    int    `json:"totalSkipped"`
	Errors          int    `json:"errors"`
}

// automaticAIEnabled reports whether new event stories are AI-translated as soon
// as a CN sync or a refresh imports them. Off unless the
// event_story_auto_ai.enabled setting is true; the manual translate action is
// not affected.
func (t *Translator) automaticAIEnabled() bool {
	return t.cfg.GetBool(config.KeyEventStoryAutoAIOn, false)
}

// autoTranslateEventStory fills untranslated lines/titles of one event story via
// the configured provider. Returns the number of lines translated. The story
// source becomes "llm" if fully translated, else stays "jp_pending".
func (t *Translator) autoTranslateEventStory(eventID int) (int, error) {
	provider := normalizeProvider("", t.cfg.GetOr("llm.type", "openai"))
	if provider != "gemini" && provider != "openai" {
		return 0, fmt.Errorf("unsupported provider: %s", provider)
	}
	if reason, unavailable := t.automaticLLMUnavailable(); unavailable {
		return 0, fmt.Errorf("%s", reason)
	}
	return t.translateEventStoryWithMode(eventID, provider, true)
}

// translateEventStory runs LLM translation over an event story's untranslated
// targets. Every successful batch is committed immediately so a later timeout
// can be resumed without losing earlier work.
func (t *Translator) translateEventStory(eventID int, provider string) (int, error) {
	return t.translateEventStoryWithMode(eventID, provider, false)
}

func (t *Translator) translateEventStoryWithMode(eventID int, provider string, automatic bool) (int, error) {
	targets, err := t.eventStore.UntranslatedTargets(eventID)
	if err != nil {
		return 0, err
	}
	if len(targets) == 0 {
		return 0, nil
	}
	cfg := t.snapshotConfig()
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}
	total := len(targets)
	processed := 0
	translatedCount := 0
	t.emit("translate.progress", fmt.Sprintf("AI 剧情翻译准备中 0/%d", total), 0, total)
	for i := 0; i < total; i += batchSize {
		if err := t.runContext().Err(); err != nil {
			return translatedCount, err
		}
		end := i + batchSize
		if end > total {
			end = total
		}
		batchTargets := targets[i:end]
		texts := make([]string, len(batchTargets))
		for j, target := range batchTargets {
			texts[j] = target.JP
		}
		batchNo := i/batchSize + 1
		batchTotal := (total + batchSize - 1) / batchSize
		onAttempt := func(attempt, attempts int) {
			detail := fmt.Sprintf("AI 剧情翻译 %d/%d · 第 %d/%d 批 · 请求 %d/%d", processed, total, batchNo, batchTotal, attempt, attempts)
			t.emit("translate.progress", detail, processed, total)
		}
		var res []string
		if automatic {
			res, err = t.callAutomaticLLM(provider, texts, onAttempt)
		} else {
			res, err = t.callLLMWithAttempts(provider, texts, onAttempt)
		}
		if err != nil {
			return translatedCount, fmt.Errorf("event %d batch %d/%d failed after saving %d/%d: %w", eventID, batchNo, batchTotal, processed, total, err)
		}
		for j := range res {
			res[j] = strings.TrimSpace(res[j])
		}
		count, protected := 0, false
		if err := t.runContext().Err(); err != nil {
			return translatedCount, err
		}
		if automatic {
			count, protected, err = t.eventStore.ApplyEventTranslationsForSync(eventID, batchTargets, res, model.SourceLLM)
		} else {
			count, err = t.eventStore.ApplyEventTranslations(eventID, batchTargets, res, model.SourceLLM)
		}
		if err != nil {
			return translatedCount, fmt.Errorf("persist event %d batch %d/%d: %w", eventID, batchNo, batchTotal, err)
		}
		if protected {
			return translatedCount, nil
		}
		translatedCount += count
		processed = end
		if count > 0 && t.store != nil {
			t.store.NotifyChange()
		}
		t.emit("translate.progress", fmt.Sprintf("AI 剧情翻译已保存 %d/%d", processed, total), processed, total)
		if end < total {
			if err := t.wait(cfg.RateDelay); err != nil {
				return translatedCount, err
			}
		}
	}

	remaining, err := t.eventStore.UntranslatedTargets(eventID)
	if err != nil {
		return translatedCount, err
	}
	source := "jp_pending"
	if len(remaining) == 0 {
		source = model.SourceLLM
	}
	if updated, err := t.eventStore.SetStorySourceForSync(eventID, source); err != nil {
		return translatedCount, err
	} else if !updated {
		return translatedCount, nil
	}
	if t.store != nil {
		t.store.NotifyChange()
	}
	return translatedCount, nil
}

// AITranslateAll translates every event story that still has untranslated lines.
func (t *Translator) AITranslateAll(provider string) (AITranslateAllResult, error) {
	return t.AITranslateAllContext(context.Background(), provider)
}

func (t *Translator) AITranslateAllContext(ctx context.Context, provider string) (AITranslateAllResult, error) {
	if err := t.markStart(ctx, "ai-all"); err != nil {
		return AITranslateAllResult{}, err
	}
	var runErr error
	defer func() { t.markEnd("ai-all complete", runErr) }()

	provider = normalizeProvider(provider, t.cfg.GetOr("llm.type", "openai"))
	result := AITranslateAllResult{Provider: provider}
	if provider != "gemini" && provider != "openai" {
		runErr = fmt.Errorf("unsupported provider: %s", provider)
		return result, runErr
	}

	summaries, err := t.eventStore.List()
	if err != nil {
		runErr = err
		return result, runErr
	}
	for _, sum := range summaries {
		if err := t.runContext().Err(); err != nil {
			runErr = err
			return result, runErr
		}
		// Skip stories already official/human.
		src := normalizeStorySource(sum.Source)
		if src == "official_cn" || src == model.SourceHuman || src == model.SourcePinned {
			continue
		}
		targets, err := t.eventStore.UntranslatedTargets(sum.EventID)
		if err != nil {
			result.Errors++
			continue
		}
		if len(targets) == 0 {
			continue
		}
		result.TotalFields++
		result.TotalCandidates += len(targets)
		count, err := t.translateEventStory(sum.EventID, provider)
		result.TotalTranslated += count
		if err != nil {
			result.Errors++
			result.TotalSkipped += len(targets) - count
			continue
		}
		result.TotalSkipped += len(targets) - count
	}
	return result, nil
}

// AITranslateStory translates a single event story's untranslated lines via the
// LLM. Used by the per-story "AI 补充翻译" button in the editor.
func (t *Translator) AITranslateStory(eventID int, provider string) (AITranslateAllResult, error) {
	return t.AITranslateStoryContext(context.Background(), eventID, provider)
}

func (t *Translator) AITranslateStoryContext(ctx context.Context, eventID int, provider string) (AITranslateAllResult, error) {
	if err := t.markStart(ctx, "ai-story"); err != nil {
		return AITranslateAllResult{}, err
	}
	var runErr error
	defer func() { t.markEnd(fmt.Sprintf("ai story %d complete", eventID), runErr) }()

	provider = normalizeProvider(provider, t.cfg.GetOr("llm.type", "openai"))
	result := AITranslateAllResult{Provider: provider}
	if provider != "gemini" && provider != "openai" {
		runErr = fmt.Errorf("unsupported provider: %s", provider)
		return result, runErr
	}

	targets, err := t.eventStore.UntranslatedTargets(eventID)
	if err != nil {
		runErr = err
		return result, runErr
	}
	if len(targets) == 0 {
		return result, nil
	}
	result.TotalFields = 1
	result.TotalCandidates = len(targets)
	count, err := t.translateEventStory(eventID, provider)
	result.TotalTranslated = count
	result.TotalSkipped = len(targets) - count
	if err != nil {
		runErr = err
		result.Errors++
		return result, runErr
	}
	return result, nil
}

// RetryEventStorySync re-fetches one event story from remote, preferring official
// CN and falling back to JP-pending + auto LLM. Overwrites local non-edited data.
func (t *Translator) RetryEventStorySync(eventID int) (map[string]any, error) {
	return t.RetryEventStorySyncContext(context.Background(), eventID)
}

func (t *Translator) RetryEventStorySyncContext(ctx context.Context, eventID int) (map[string]any, error) {
	if err := t.markStart(ctx, "retry-event-story"); err != nil {
		return nil, err
	}
	var runErr error
	defer func() { t.markEnd(fmt.Sprintf("retry event %d", eventID), runErr) }()

	jpStories, err := t.fetchMasterdata("eventStories.json", "jp")
	if err != nil {
		runErr = err
		return nil, fmt.Errorf("fetch JP eventStories: %w", err)
	}
	cnStories, err := t.fetchMasterdata("eventStories.json", "cn")
	if err != nil {
		runErr = err
		return nil, fmt.Errorf("fetch CN eventStories: %w", err)
	}
	cnEvents, err := t.fetchMasterdata("events.json", "cn")
	if err != nil {
		runErr = err
		return nil, fmt.Errorf("fetch CN events: %w", err)
	}

	var jpStory map[string]any
	for _, s := range jpStories {
		if getInt(s, "eventId") == eventID {
			jpStory = s
			break
		}
	}
	if jpStory == nil {
		runErr = fmt.Errorf("event %d not found in JP eventStories", eventID)
		return nil, runErr
	}
	cnStoryByEvent := byIntID(cnStories, "eventId")
	cnEventSet := map[int]bool{}
	for _, e := range cnEvents {
		cnEventSet[getInt(e, "id")] = true
	}

	result := map[string]any{"eventId": eventID}

	if cnEventSet[eventID] && cnStoryByEvent[eventID] != nil {
		episodes, hasTalk, _, episodeErrors := t.buildOfficialCNEpisodes(jpStory, cnStoryByEvent[eventID])
		if len(episodeErrors) > 0 {
			runErr = fmt.Errorf("event %d: incomplete official episode fetch: %w", eventID, summarizeErrors(episodeErrors))
			return nil, runErr
		}
		if hasTalk {
			runCtx := t.runContext()
			if err := runCtx.Err(); err != nil {
				runErr = err
				return nil, runErr
			}
			meta := model.EventStoryMeta{Source: "official_cn", Version: "1.0", LastUpdated: time.Now().Unix()}
			imported, err := t.eventStore.ImportOrderedForSyncContext(runCtx, eventID, meta, toOrderedEpisodes(episodes, "cn"))
			if err != nil {
				runErr = err
				return nil, err
			}
			if !imported {
				result["skipped"] = "protected"
				return result, nil
			}
			t.store.NotifyChange()
			result["source"] = "official_cn"
			result["episodes"] = len(episodes)
			return result, nil
		}
	}

	episodes, episodeErrors := t.buildJPPendingEpisodes(jpStory)
	if len(episodeErrors) > 0 {
		runErr = fmt.Errorf("event %d: incomplete JP episode fetch: %w", eventID, summarizeErrors(episodeErrors))
		return nil, runErr
	}
	if len(episodes) == 0 {
		runErr = fmt.Errorf("event %d: no episodes fetched", eventID)
		return nil, runErr
	}
	meta := model.EventStoryMeta{Source: "jp_pending", Version: "1.0", LastUpdated: time.Now().Unix()}
	runCtx := t.runContext()
	if err := runCtx.Err(); err != nil {
		runErr = err
		return nil, runErr
	}
	imported, err := t.eventStore.ImportOrderedForSyncContext(runCtx, eventID, meta, toOrderedEpisodes(episodes, "unknown"))
	if err != nil {
		runErr = err
		return nil, err
	}
	if !imported {
		result["skipped"] = "protected"
		return result, nil
	}
	t.store.NotifyChange()
	result["source"] = "jp_pending"
	result["episodes"] = len(episodes)

	if t.automaticAIEnabled() {
		translated, autoErr := t.autoTranslateEventStory(eventID)
		if translated > 0 {
			result["translated"] = translated
		}
		if autoErr != nil {
			result["translateError"] = autoErr.Error()
		}
	}
	detail, err := t.eventStore.Detail(eventID)
	if err != nil {
		runErr = fmt.Errorf("read persisted event %d source: %w", eventID, err)
		return nil, runErr
	}
	result["source"] = detail.Meta.Source
	return result, nil
}

// ReorderEventStory re-fetches remote scenarios to obtain the original dialogue
// order and updates stored line positions, without touching translations.
func (t *Translator) ReorderEventStory(eventID int) (map[string]any, error) {
	return t.ReorderEventStoryContext(context.Background(), eventID)
}

func (t *Translator) ReorderEventStoryContext(ctx context.Context, eventID int) (map[string]any, error) {
	if err := t.markStart(ctx, "reorder-event-story"); err != nil {
		return nil, err
	}
	var runErr error
	defer func() { t.markEnd(fmt.Sprintf("reorder event %d", eventID), runErr) }()

	if ok, err := t.eventStore.Exists(eventID); err != nil {
		runErr = err
		return nil, err
	} else if !ok {
		runErr = sql.ErrNoRows
		return nil, fmt.Errorf("event %d not found", eventID)
	}

	jpStories, err := t.fetchMasterdata("eventStories.json", "jp")
	if err != nil {
		runErr = err
		return nil, fmt.Errorf("fetch JP eventStories: %w", err)
	}
	var jpStory map[string]any
	for _, s := range jpStories {
		if getInt(s, "eventId") == eventID {
			jpStory = s
			break
		}
	}
	if jpStory == nil {
		runErr = fmt.Errorf("event %d not found in JP eventStories", eventID)
		return nil, runErr
	}

	asset := getString(jpStory, "assetbundleName")
	reordered, fetchErrors := 0, 0
	for _, ep := range toMapSlice(jpStory["eventStoryEpisodes"]) {
		if err := t.runContext().Err(); err != nil {
			runErr = err
			return nil, runErr
		}
		epNo := strconv.Itoa(getInt(ep, "episodeNo"))
		scenarioID := getString(ep, "scenarioId")
		if scenarioID == "" {
			continue
		}
		localKeys, err := t.eventStore.EpisodeTalkKeys(eventID, epNo)
		if err != nil || len(localKeys) == 0 {
			continue
		}
		scenarioPath := fmt.Sprintf("event_story/%s/scenario/%s", asset, scenarioID)
		jpScenario, fetchErr := t.fetchJPScenarioJSON(scenarioPath)
		if fetchErr != nil {
			fetchErrors++
			continue
		}
		var order []string
		seen := map[string]bool{}
		for _, talk := range toMapSlice(asMap(jpScenario)["TalkData"]) {
			jpBody := strings.TrimSpace(getString(talk, "Body"))
			if jpBody != "" && localKeys[jpBody] && !seen[jpBody] {
				order = append(order, jpBody)
				seen[jpBody] = true
			}
			jpName := strings.TrimSpace(getString(talk, "WindowDisplayName"))
			if jpName != "" && localKeys[jpName] && !seen[jpName] {
				order = append(order, jpName)
				seen[jpName] = true
			}
		}
		if err := t.eventStore.ReorderEpisodeLines(eventID, epNo, order); err != nil {
			runErr = err
			return nil, err
		}
		reordered++
	}
	t.store.NotifyChange()
	return map[string]any{"status": "ok", "episodes": reordered, "fetchErrors": fetchErrors}, nil
}

func normalizeStorySource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "official_cn", "official_cn_legacy", "cn":
		return "official_cn"
	case "llm":
		return model.SourceLLM
	case "human":
		return model.SourceHuman
	case "pinned":
		return model.SourcePinned
	default:
		return "jp_pending"
	}
}

var _ = store.EventTranslateTarget{}
