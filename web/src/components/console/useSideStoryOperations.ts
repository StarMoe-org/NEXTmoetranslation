import { useState, type RefObject } from "react";
import { Locale, SideStoryKind, aiTranslateSideStory, refreshSideStory } from "@/lib/api";
import {
  describeSideStoryAIResult, describeSideStoryRefresh, sideStoryErrorMessage, sideStoryLocale,
} from "@/lib/side-story-console";
import type { ShowToast } from "@/components/console/types";

export interface SideStoryOperationsOptions {
  show: ShowToast;
  sideStoryKind: SideStoryKind | null;
  field: string;
  locale: Locale;
  selectedEpisode: string;
  contextGenerationRef: RefObject<number>;
  refreshSideStoryLists: (kind?: SideStoryKind) => void;
}

// Both actions run inside guardProducerMutation, which owns the write fence and reconciles afterwards.
export function useSideStoryOperations({
  show, sideStoryKind, field, locale, selectedEpisode, contextGenerationRef, refreshSideStoryLists,
}: SideStoryOperationsOptions) {
  const [sideStoryBusy, setSideStoryBusy] = useState(false);

  const run = async (action: (kind: SideStoryKind, isCurrent: () => boolean) => Promise<void>) => {
    if (!sideStoryKind || sideStoryBusy) return;
    const generation = contextGenerationRef.current;
    setSideStoryBusy(true);
    try {
      await action(sideStoryKind, () => contextGenerationRef.current === generation);
    } finally {
      setSideStoryBusy(false);
    }
  };

  const doSideStoryAI = () => run(async (kind, isCurrent) => {
    if (locale === "ja-JP") return;
    const episode = selectedEpisode === "all" ? "" : selectedEpisode;
    try {
      const result = await aiTranslateSideStory(kind, field, sideStoryLocale(locale), episode, "openai");
      refreshSideStoryLists(kind);
      if (isCurrent()) show(describeSideStoryAIResult(result, episode), result.translated === 0 && result.remaining > 0 ? "err" : "ok");
    } catch (reason) {
      if (isCurrent()) show(sideStoryErrorMessage(reason, "AI 补充翻译失败"), "err");
    }
  });

  const doSideStoryRefresh = () => run(async (kind, isCurrent) => {
    try {
      const result = await refreshSideStory(kind, field);
      refreshSideStoryLists(kind);
      const summary = describeSideStoryRefresh(result.episodes ?? []);
      if (isCurrent()) show(summary.message, summary.ok ? "ok" : "err");
    } catch (reason) {
      if (isCurrent()) show(sideStoryErrorMessage(reason, "重新获取剧本失败"), "err");
    }
  });

  return { sideStoryBusy, doSideStoryAI, doSideStoryRefresh };
}
