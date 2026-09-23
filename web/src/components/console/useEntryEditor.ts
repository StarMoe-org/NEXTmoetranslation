import { useCallback, type Dispatch, type RefObject, type SetStateAction } from "react";
import { Locale, TranslationEntry, updateEntry, updateEventStoryLine } from "@/lib/api";
import type { EventStoryTxtDraft } from "@/components/EventStoryTxtImport";
import { EVENT_STORY_TITLE_MARKER, SOURCE_LABELS, parseEventStoryEntryKey } from "@/lib/labels";
import { eventStoryEntryHasCanonicalIdentity, restoreEventStoryDraftEntries } from "@/lib/event-story-console";
import {
  clearPersistedEventTxtDraft, eventStoryMutationResultIsAmbiguous, persistEventTxtDraft,
} from "@/components/console/console-drafts";
import type { ReconciliationReason, ShowToast } from "@/components/console/types";

export interface EntryEditorOptions {
  username: string;
  show: ShowToast;
  category: string;
  field: string;
  locale: Locale;
  isEventStory: boolean;
  isReadOnly: boolean;
  entries: TranslationEntry[];
  entriesRef: RefObject<TranslationEntry[]>;
  setEntries: Dispatch<SetStateAction<TranslationEntry[]>>;
  filtered: TranslationEntry[];
  selectedKey: string | null;
  setSelectedKey: (key: string | null) => void;
  selectedEntry: TranslationEntry | null;
  selectedEpisode: string;
  editValue: string;
  setEditValue: (value: string) => void;
  entryDirty: boolean;
  eventTxtDraft: EventStoryTxtDraft | null;
  setEventTxtDraft: (draft: EventStoryTxtDraft | null) => void;
  eventTxtDraftDirty: boolean;
  keepTranslationEntryVisible: (key: string) => void;
  reloadSidebar: () => Promise<boolean>;
  reconcileContentRef: RefObject<(reason: ReconciliationReason, draft?: string | null, detail?: string) => Promise<boolean>>;
  writeFenceRef: RefObject<boolean>;
  savingRef: RefObject<boolean>;
  setSaving: (saving: boolean) => void;
  remoteConflictRef: RefObject<{ key: string; user: string } | null>;
  contextGenerationRef: RefObject<number>;
}

export function useEntryEditor({
  username,
  show,
  category,
  field,
  locale,
  isEventStory,
  isReadOnly,
  entries,
  entriesRef,
  setEntries,
  filtered,
  selectedKey,
  setSelectedKey,
  selectedEntry,
  selectedEpisode,
  editValue,
  setEditValue,
  entryDirty,
  eventTxtDraft,
  setEventTxtDraft,
  eventTxtDraftDirty,
  keepTranslationEntryVisible,
  reloadSidebar,
  reconcileContentRef,
  writeFenceRef,
  savingRef,
  setSaving,
  remoteConflictRef,
  contextGenerationRef,
}: EntryEditorOptions) {
  const applyEventTxtDraft = (draft: EventStoryTxtDraft) => {
    if (!isEventStory || Number(field) !== draft.eventId || locale !== draft.locale || writeFenceRef.current || savingRef.current) return;
    const bySegment = new Map(entries.flatMap((entry) => entry.segmentId ? [[entry.segmentId, entry] as const] : []));
    for (const translation of draft.translations) {
      const entry = bySegment.get(translation.segmentId);
      if (!entry || entry.episodeNo !== draft.episodeNo || entry.sourceHash !== translation.sourceHash ||
          (entry.revision ?? 0) !== translation.revision || entry.text !== translation.authoritativeText) {
        show(`条目 ${translation.segmentId} 已不等于导入预览，请重新加载后再导入`, "err");
        return;
      }
    }
    const selectedTranslation = selectedEntry?.segmentId
      ? draft.translations.find((translation) => translation.segmentId === selectedEntry.segmentId)
      : undefined;
    if (!persistEventTxtDraft(username, draft)) {
      show("无法写入 TXT 本地草稿，导入已取消", "err");
      return;
    }
    setEntries((current) => current.map((entry) => {
      const translation = entry.segmentId ? draft.translations.find((candidate) => candidate.segmentId === entry.segmentId) : undefined;
      return translation ? { ...entry, text: translation.text } : entry;
    }));
    if (selectedTranslation) setEditValue(selectedTranslation.text);
    setEventTxtDraft(draft);
  };

  const undoEventTxtDraft = () => {
    const draft = eventTxtDraft;
    if (!draft?.undoAvailable || entryDirty || savingRef.current) return;
    if (!clearPersistedEventTxtDraft(username, draft.eventId, draft.locale)) {
      show("无法清理 TXT 本地草稿，撤销已取消", "err");
      return;
    }
    const selectedTranslation = selectedEntry?.segmentId
      ? draft.translations.find((translation) => translation.segmentId === selectedEntry.segmentId)
      : undefined;
    setEntries((current) => restoreEventStoryDraftEntries(current, draft.translations));
    if (selectedTranslation) setEditValue(selectedTranslation.authoritativeText);
    setEventTxtDraft(null);
    show("已一步撤销本次 TXT 导入，本地译文恢复到导入前的权威内容", "ok");
  };

  const save = useCallback(async (overrideSource?: string, advance = true) => {
    if (writeFenceRef.current || savingRef.current || !selectedKey || !category || !field || isReadOnly) return false;
    if (isEventStory && (!selectedEntry || !eventStoryEntryHasCanonicalIdentity(selectedEntry))) {
      show("当前剧情行缺少权威来源身份，请重新获取剧情后再编辑", "err");
      return false;
    }
    if (remoteConflictRef.current?.key === selectedKey) {
      show("协作者已更新当前行；请先选择采用远端版本或明确保留本地草稿", "err");
      return false;
    }
    savingRef.current = true;
    setSaving(true);
    const src = overrideSource || "human";
    const generation = contextGenerationRef.current;
    const saveCategory = category;
    const saveField = field;
    const saveLocale = locale;
    const saveKey = selectedKey;
    const saveValue = editValue;
    const saveEntry = selectedEntry;
    try {
      if (isEventStory) {
        const p = parseEventStoryEntryKey(saveKey);
        const episodeNo = saveEntry?.episodeNo || p.episodeNo;
        const entryType = saveEntry?.entryType || p.entryType;
        const japanese = saveEntry?.japanese || p.originalText;
        const result = await updateEventStoryLine(Number(saveField), episodeNo, entryType === "title" ? "" : japanese,
          saveValue, src, entryType, saveLocale, saveEntry?.segmentId || "", saveEntry?.sourceHash || "", saveEntry?.revision ?? 0);
        if (contextGenerationRef.current !== generation) return true;
        const currentEntry = entriesRef.current.find((e) => e.key === saveKey);
        if (currentEntry && typeof currentEntry.revision === "number" && currentEntry.revision > (saveEntry?.revision ?? -1)) {
          void reconcileContentRef.current(
            "remote",
            null,
            "保存期间协作者提交了同一行的更高 revision；本地保存响应未应用，正在重新载入权威 revision。",
          );
          return true;
        }
        setEntries((prev) => prev.map((e) =>
          e.key === saveKey
            ? { ...e, key: entryType === "title" && !e.segmentId ? `${episodeNo}|${EVENT_STORY_TITLE_MARKER}|${saveValue}` : e.key,
                text: saveValue, source: src, revision: result.revision }
            : e));
        if (saveEntry?.segmentId && eventTxtDraft) {
          const translations = eventTxtDraft.translations.filter((translation) => translation.segmentId !== saveEntry.segmentId);
          const nextDraft = translations.length > 0 ? { ...eventTxtDraft, undoAvailable: false, translations } : null;
          const persisted = nextDraft
            ? persistEventTxtDraft(username, nextDraft)
            : clearPersistedEventTxtDraft(username, eventTxtDraft.eventId, eventTxtDraft.locale);
          if (!persisted) {
            show("远端保存成功，但 TXT 本地草稿状态无法更新；请勿离开页面并手动导出当前草稿", "err");
          }
          setEventTxtDraft(nextDraft);
        }
        if (entryType === "title" && !saveEntry?.segmentId) setSelectedKey(`${episodeNo}|${EVENT_STORY_TITLE_MARKER}|${saveValue}`);
      } else {
        await updateEntry(saveCategory, saveField, saveKey, saveValue, src, saveLocale);
        if (contextGenerationRef.current !== generation) return true;
        setEntries((prev) => prev.map((e) => (e.key === saveKey ? { ...e, text: saveValue, source: src } : e)));
      }
      if (isEventStory) void reloadSidebar();
      // Advance to next.
      if (advance) {
        const idx = filtered.findIndex((e) => e.key === saveKey);
        if (idx >= 0 && idx < filtered.length - 1) {
          const next = filtered[idx + 1];
          setSelectedKey(next.key); setEditValue(next.text);
          setTimeout(() => keepTranslationEntryVisible(next.key), 40);
        } else {
          show(isEventStory && selectedEpisode !== "all" ? "已到本章最后一条" : "已到最后一条", "ok");
        }
      }
      return true;
    } catch (e) {
      const ambiguousEventStoryFailure = isEventStory && eventStoryMutationResultIsAmbiguous(e);
      if (ambiguousEventStoryFailure) {
        const preservedDraft = JSON.stringify({
          exportedAt: new Date().toISOString(),
          kind: "translation",
          category: saveCategory,
          field: saveField,
          locale: saveLocale,
          key: saveKey,
          staleText: saveValue,
          previouslyLoadedText: saveEntry?.text ?? "",
          eventTxtDraft,
        }, null, 2);
        void reconcileContentRef.current(
          "remote",
          preservedDraft,
          "剧情保存结果无法确认；本地草稿已冻结，并正在重新载入权威 revision。",
        );
      }
      show(e instanceof Error ? e.message : "保存失败", "err");
      return false;
    } finally {
      savingRef.current = false;
      setSaving(false);
    }
  }, [selectedKey, selectedEntry, category, eventTxtDraft, field, editValue, filtered, isEventStory, isReadOnly, locale, selectedEpisode, show, username, keepTranslationEntryVisible, reloadSidebar, contextGenerationRef, entriesRef, reconcileContentRef, remoteConflictRef, savingRef, setEditValue, setEntries, setEventTxtDraft, setSaving, setSelectedKey, writeFenceRef]);

  // ---- Change source for a single entry ----
  const handleSourceChange = useCallback(async (key: string, newSource: string) => {
    if (writeFenceRef.current || savingRef.current || eventTxtDraftDirty || !category || !field) return;
    if (remoteConflictRef.current?.key === key) {
      show("协作者已更新当前行；请先明确处理冲突，再修改来源", "err");
      return;
    }
    const entry = entries.find((e) => e.key === key);
    if (!entry) return;
    if (isEventStory && !eventStoryEntryHasCanonicalIdentity(entry)) {
      show("当前剧情行缺少权威来源身份，请重新获取剧情后再编辑", "err");
      return;
    }
    const generation = contextGenerationRef.current;
    let nextRevision = entry.revision;
    const saveStartRevision = entry.revision;
    savingRef.current = true;
    setSaving(true);
    try {
      if (isEventStory) {
        const parsed = parseEventStoryEntryKey(key);
        const episodeNo = entry.episodeNo || parsed.episodeNo;
        const entryType = entry.entryType || parsed.entryType;
        const result = await updateEventStoryLine(
          Number(field), episodeNo,
          entryType === "title" ? "" : (entry.japanese || parsed.originalText),
          entry.text, newSource, entryType, locale, entry.segmentId || "", entry.sourceHash || "", entry.revision ?? 0,
        );
        nextRevision = result.revision;
      } else {
        await updateEntry(category, field, key, entry.text, newSource, locale);
      }
      if (contextGenerationRef.current !== generation) return;
      const currentEntry = entriesRef.current.find((e) => e.key === key);
      if (isEventStory && currentEntry && typeof currentEntry.revision === "number" && typeof saveStartRevision === "number" && currentEntry.revision > saveStartRevision) {
        void reconcileContentRef.current(
          "remote",
          null,
          "来源修改期间协作者提交了同一行的更高 revision；本地来源修改响应未应用，正在重新载入权威 revision。",
        );
        return;
      }
      setEntries((prev) => prev.map((e) => (e.key === key ? { ...e, source: newSource, ...(nextRevision !== undefined ? { revision: nextRevision } : {}) } : e)));
      if (isEventStory) void reloadSidebar();
      show(`来源已改为「${SOURCE_LABELS[newSource] || newSource}」`, "ok");
    } catch (err) {
      if (isEventStory && eventStoryMutationResultIsAmbiguous(err)) {
        const preservedDraft = JSON.stringify({
          exportedAt: new Date().toISOString(),
          kind: "event-story-source",
          category,
          field,
          locale,
          key,
          text: entry.text,
          previouslyLoadedSource: entry.source,
          attemptedSource: newSource,
          segmentId: entry.segmentId,
          sourceHash: entry.sourceHash,
          revision: entry.revision,
        }, null, 2);
        void reconcileContentRef.current(
          "remote",
          preservedDraft,
          "剧情来源修改结果无法确认；修改意图已冻结，并正在重新载入权威 revision。",
        );
      }
      show(err instanceof Error ? err.message : "修改失败", "err");
    } finally {
      savingRef.current = false;
      setSaving(false);
    }
  }, [category, eventTxtDraftDirty, field, entries, isEventStory, locale, show, reloadSidebar, contextGenerationRef, entriesRef, reconcileContentRef, remoteConflictRef, savingRef, setEntries, setSaving, writeFenceRef]);

  return { save, handleSourceChange, applyEventTxtDraft, undoEventTxtDraft };
}
