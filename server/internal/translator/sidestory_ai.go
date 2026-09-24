package translator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/store"
)

// AITranslateSideStoryContext fills the untranslated lines of one card or
// area story in locale via the LLM, persisting every batch. episodeKey ""
// covers all episodes. It runs as the single translate job "ai-side-story"
// and is never started by sync or the backfill.
func (t *Translator) AITranslateSideStoryContext(ctx context.Context, kind, storyID, locale, episodeKey, provider string) (store.SideStoryAIResult, error) {
	if !store.ValidSideStoryKind(kind) || !store.ValidSideStoryID(kind, storyID) || !store.ValidSideStoryLocale(locale) ||
		(episodeKey != "" && !store.ValidSideStoryEpisodeKey(kind, episodeKey)) {
		return store.SideStoryAIResult{}, fmt.Errorf("%w: %s %q locale %q episode %q", store.ErrSideStoryInvalid, kind, storyID, locale, episodeKey)
	}
	provider = normalizeProvider(provider, t.cfg.GetOr(config.KeyLLMType, "openai"))
	if provider != "gemini" && provider != "openai" {
		return store.SideStoryAIResult{}, fmt.Errorf("unsupported provider: %s", provider)
	}
	if err := t.markStart(ctx, "ai-side-story"); err != nil {
		return store.SideStoryAIResult{}, err
	}
	var runErr error
	defer func() { t.markEnd(fmt.Sprintf("ai side story %s/%s %s complete", kind, storyID, locale), runErr) }()

	targets, err := t.store.SideStoryAITargetsContext(t.runContext(), kind, storyID, locale, episodeKey)
	if err != nil {
		runErr = err
		return store.SideStoryAIResult{}, runErr
	}
	result := store.SideStoryAIResult{}
	result.Translated, runErr = t.translateSideStoryTargets(kind, storyID, locale, provider, targets)
	result.Remaining = len(targets) - result.Translated
	if remaining, err := t.store.SideStoryAITargetsContext(t.runContext(), kind, storyID, locale, episodeKey); err == nil {
		result.Remaining = len(remaining)
	} else if runErr == nil {
		runErr = err
	}
	return result, runErr
}

// translateSideStoryTargets batches, retries, reports progress and waits
// between batches like translateEventStoryWithMode(automatic=false).
func (t *Translator) translateSideStoryTargets(kind, storyID, locale, provider string, targets []store.SideStoryAITarget) (int, error) {
	total := len(targets)
	if total == 0 {
		return 0, nil
	}
	cfg := t.snapshotConfig()
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}
	instructions := llmPromptForLocale(locale)
	processed, translated := 0, 0
	t.emit("translate.progress", fmt.Sprintf("AI 剧情翻译准备中 0/%d", total), 0, total)
	for i := 0; i < total; i += batchSize {
		if err := t.runContext().Err(); err != nil {
			return translated, err
		}
		end := min(i+batchSize, total)
		batch := targets[i:end]
		texts := make([]string, len(batch))
		for j, target := range batch {
			texts[j] = target.JP
		}
		batchNo := i/batchSize + 1
		batchTotal := (total + batchSize - 1) / batchSize
		onAttempt := func(attempt, attempts int) {
			detail := fmt.Sprintf("AI 剧情翻译 %d/%d · 第 %d/%d 批 · 请求 %d/%d", processed, total, batchNo, batchTotal, attempt, attempts)
			t.emit("translate.progress", detail, processed, total)
		}
		res, err := t.callLLMUsingPrompt(provider, instructions, texts, t.snapshotConfig(), onAttempt)
		if err != nil {
			return translated, fmt.Errorf("%s %s batch %d/%d failed after saving %d/%d: %w", kind, storyID, batchNo, batchTotal, processed, total, err)
		}
		for j := range res {
			res[j] = strings.TrimSpace(res[j])
		}
		runCtx := t.runContext()
		if err := runCtx.Err(); err != nil {
			return translated, err
		}
		count, err := t.store.ApplySideStoryAITranslationsContext(runCtx, kind, storyID, locale, batch, res, time.Now())
		if err != nil {
			return translated, fmt.Errorf("persist %s %s batch %d/%d: %w", kind, storyID, batchNo, batchTotal, err)
		}
		translated += count
		processed = end
		if count > 0 {
			t.store.NotifyChange()
		}
		t.emit("translate.progress", fmt.Sprintf("AI 剧情翻译已保存 %d/%d", processed, total), processed, total)
		if end < total {
			if err := t.wait(cfg.RateDelay); err != nil {
				return translated, err
			}
		}
	}
	return translated, nil
}
