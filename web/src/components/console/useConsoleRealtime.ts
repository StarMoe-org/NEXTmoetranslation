import { useCallback, useEffect, useRef, useState, type Dispatch, type RefObject, type SetStateAction } from "react";
import {
  EditorGateStatus, Locale, SideStoryKind, SideStorySummary, TranslationEntry,
  acceptLoadedProducerState, clearLoadedProducerState, getEditorGateStatus,
  subscribeProducerProofInvalidated,
} from "@/lib/api";
import type { EventStoryTxtDraft } from "@/components/EventStoryTxtImport";
import type { LyricsEditorHandle } from "@/components/LyricsEditor";
import type { LyricsSourceReviewHandle } from "@/components/LyricsSourceReview";
import {
  eventStoryEntryHasCanonicalIdentity, eventStoryEpisodeNo, eventStoryUpdateAffectsLocale,
  findEventStoryUpdateTarget,
} from "@/lib/event-story-console";
import {
  lyricsUpdateMatchesEditorTarget, lyricsUpdateTargetLabel, normalizeLyricsUpdateEvent,
} from "@/lib/lyrics-collaboration.mjs";
import {
  applySideStoryLineStates, normalizeSideStoryUpdateEvent, sideStoryEntryKey, sideStoryLocale,
  sideStoryUpdateEffect, sideStoryUpdateRefreshesList,
} from "@/lib/side-story-console";
import { useSSE } from "@/lib/sse";
import {
  clearPersistedContentConflict, clearPersistedEventTxtDraft, clearPersistedEventTxtDraftFromConflict,
  persistContentConflict, recoverPersistedContentConflict,
} from "@/components/console/console-drafts";
import type { ContentConflict, ReconciliationReason, RemoteConflict, ShowToast } from "@/components/console/types";
import type { SideStoryListRefreshed } from "@/components/console/useSideStoryCatalog";

interface Progress { label: string; current: number; total: number }

// Saves share the debounced list load with a sync, so only what a save cannot change counts.
function sideStoryBackfillChanged(before: SideStorySummary, after: SideStorySummary): boolean {
  return before.title !== after.title || before.episodeCount !== after.episodeCount ||
    before.fetchedEpisodeCount !== after.fetchedEpisodeCount || before.lineCount !== after.lineCount ||
    after.sourceCounts.official > before.sourceCounts.official;
}

export interface ConsoleRealtimeOptions {
  username: string;
  clientID: string;
  locale: Locale;
  show: ShowToast;
  category: string;
  field: string;
  isEventStory: boolean;
  sideStoryKind: SideStoryKind | null;
  isLyrics: boolean;
  isLyricsSourceReview: boolean;
  entries: TranslationEntry[];
  setEntries: Dispatch<SetStateAction<TranslationEntry[]>>;
  selectedKey: string | null;
  // Reselects the line after a backfill sync reloaded the open story.
  setSelectedKey: (key: string) => void;
  selectedEntry: TranslationEntry | null;
  entryDirty: boolean;
  editValue: string;
  setEditValue: (value: string) => void;
  eventTxtDraft: EventStoryTxtDraft | null;
  setEventTxtDraft: (draft: EventStoryTxtDraft | null) => void;
  eventTxtDraftDirty: boolean;
  lyricsDirty: boolean;
  lyricsEditorRef: RefObject<LyricsEditorHandle | null>;
  lyricsSourceReviewRef: RefObject<LyricsSourceReviewHandle | null>;
  setRemoteConflict: (next: RemoteConflict | null) => void;
  contextGenerationRef: RefObject<number>;
  invalidatePendingAction: () => void;
  loadEntries: () => Promise<boolean>;
  reloadSidebar: () => Promise<boolean>;
  // Debounced refresh of the loaded side-story lists (all kinds when omitted).
  refreshSideStoryLists: (kind?: SideStoryKind, onRefreshed?: SideStoryListRefreshed) => void;
}

export function useConsoleRealtime({
  username,
  clientID,
  locale,
  show,
  category,
  field,
  isEventStory,
  sideStoryKind,
  isLyrics,
  isLyricsSourceReview,
  entries,
  setEntries,
  selectedKey,
  setSelectedKey,
  selectedEntry,
  entryDirty,
  editValue,
  setEditValue,
  eventTxtDraft,
  setEventTxtDraft,
  eventTxtDraftDirty,
  lyricsDirty,
  lyricsEditorRef,
  lyricsSourceReviewRef,
  setRemoteConflict,
  contextGenerationRef,
  invalidatePendingAction,
  loadEntries,
  reloadSidebar,
  refreshSideStoryLists,
}: ConsoleRealtimeOptions) {
  const [progress, setProgress] = useState<Progress | null>(null);
  const [realtimeState, setRealtimeState] = useState<"connecting" | "connected" | "reconnecting" | "offline">("connecting");
  const [onlineUsers, setOnlineUsers] = useState<string[]>([]);
  const [remoteHighlights, setRemoteHighlights] = useState<Record<string, { user: string; until: number }>>({});
  const remoteHighlightTimersRef = useRef<Record<string, ReturnType<typeof setTimeout>>>({});
  const [writesLocked, setWritesLocked] = useState(true);
  const [contentConflict, setContentConflict] = useState<ContentConflict | null>(null);
  const writeFenceRef = useRef(true);
  const reconciliationGenerationRef = useRef(0);
  const contentEventGenerationRef = useRef(0);
  const sseConnectedRef = useRef(false);
  const preservedConflictDraftRef = useRef<string | null>(null);
  const preservedConflictStorageKeyRef = useRef<string | null>(null);
  const reconcileContentRef = useRef<(reason: ReconciliationReason, draft?: string | null, detail?: string) => Promise<boolean>>(async () => false);
  const reconcileRetryRef = useRef<{ reason: ReconciliationReason; draft: string | null; detail?: string; attempt: number } | null>(null);
  const reconcileRetryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const setWriteFence = (locked: boolean) => {
    writeFenceRef.current = locked;
    setWritesLocked(locked);
  };

  const freezeRecoveredConflict = useCallback((conflict: ContentConflict) => {
    preservedConflictDraftRef.current = conflict.draft;
    preservedConflictStorageKeyRef.current = conflict.storageKey ?? null;
    writeFenceRef.current = true;
    setWritesLocked(true);
    setContentConflict(conflict);
  }, []);

  useEffect(() => {
    const recovered = recoverPersistedContentConflict(username);
    if (!recovered) return;
    freezeRecoveredConflict(recovered);
  }, [freezeRecoveredConflict, username]);

  const captureUnsavedDraft = (): string | null => {
    const lyricsSnapshot = isLyrics ? lyricsEditorRef.current?.snapshot() ?? null : null;
    const dirtyNow = isLyrics ? lyricsSnapshot?.dirty ?? lyricsDirty : entryDirty || eventTxtDraftDirty;
    if (!dirtyNow) return null;
    const payload = isLyrics
      ? { kind: "lyrics", editionKey: lyricsSnapshot?.editionKey || "", document: lyricsSnapshot?.document ?? null }
      : {
          kind: "translation", category, field, locale, key: selectedKey,
          staleText: editValue, previouslyLoadedText: selectedEntry?.text ?? "",
          eventTxtDraft,
        };
    return JSON.stringify({ exportedAt: new Date().toISOString(), ...payload }, null, 2);
  };

  const reconcileContent = async (
    reason: ReconciliationReason,
    preservedDraft?: string | null,
    detail?: string,
  ): Promise<boolean> => {
    const reconciliation = ++reconciliationGenerationRef.current;
    if (reconcileRetryTimerRef.current) {
      clearTimeout(reconcileRetryTimerRef.current);
      reconcileRetryTimerRef.current = null;
    }
    const contentEventGeneration = contentEventGenerationRef.current;
    setWriteFence(true);
    clearLoadedProducerState();
    invalidatePendingAction();
    const draft = preservedDraft === undefined
      ? (contentConflict?.draft ?? preservedConflictDraftRef.current ?? captureUnsavedDraft())
      : preservedDraft;
    const conflictDetail = detail ?? contentConflict?.detail;
    if (draft) {
      const sameContentConflict = contentConflict?.draft === draft ? contentConflict : null;
      const conflictStorageKey = sameContentConflict?.storageKey ?? preservedConflictStorageKeyRef.current;
      const frozenConflict: ContentConflict = {
        reason,
        draft,
        reloadFailed: true,
        ...(conflictDetail ? { detail: conflictDetail } : {}),
        ...(conflictStorageKey ? { storageKey: conflictStorageKey } : {}),
      };
      preservedConflictDraftRef.current = draft;
      const persisted = persistContentConflict(username, frozenConflict);
      preservedConflictStorageKeyRef.current = frozenConflict.storageKey ?? null;
      if (!persisted) {
        show("本地草稿已冻结在当前页面，但浏览器持久化失败；请立即导出且不要关闭页面", "err");
      }
    }
    contextGenerationRef.current++;
    const failReconcile = (message: string) => {
      if (draft) {
        setContentConflict({
          reason,
          draft,
          reloadFailed: true,
          ...(conflictDetail ? { detail: conflictDetail } : {}),
          ...(preservedConflictStorageKeyRef.current ? { storageKey: preservedConflictStorageKeyRef.current } : {}),
        });
        show(message, "err");
      }
    };
    const retryReconcileLater = () => {
      const previous = reconcileRetryRef.current;
      const attempt = previous?.reason === reason && previous.draft === draft && previous.detail === conflictDetail ? previous.attempt + 1 : 0;
      const delay = Math.min(15_000, 1_000 * (2 ** Math.min(attempt, 4)));
      reconcileRetryRef.current = { reason, draft, ...(conflictDetail ? { detail: conflictDetail } : {}), attempt };
      reconcileRetryTimerRef.current = setTimeout(() => {
        reconcileRetryTimerRef.current = null;
        if (sseConnectedRef.current) void reconcileContent(reason, draft, conflictDetail);
      }, delay);
    };
    if (!sseConnectedRef.current) {
      failReconcile("实时连接尚未恢复，写入仍已锁定");
      retryReconcileLater();
      return false;
    }
    let producerBefore: EditorGateStatus;
    try {
      producerBefore = await getEditorGateStatus();
    } catch {
      failReconcile("实时校对暂时无法读取服务器状态，正在自动重试");
      retryReconcileLater();
      return false;
    }
    if (producerBefore.running) {
      failReconcile("服务器内容任务仍在运行，写入仍已锁定");
      retryReconcileLater();
      return false;
    }
    const lyricsReload = isLyrics
      ? lyricsEditorRef.current?.reloadAuthoritative() ?? Promise.resolve(false)
      : Promise.resolve(true);
    const reviewReload = isLyricsSourceReview
      ? lyricsSourceReviewRef.current?.reloadAuthoritative() ?? Promise.resolve(false)
      : Promise.resolve(true);
    const [sidebarLoaded, entriesLoaded, lyricsLoaded, reviewLoaded] = await Promise.all([
      reloadSidebar(), loadEntries(), lyricsReload, reviewReload,
    ]);
    if (reconciliationGenerationRef.current !== reconciliation) return false;
    if (contentEventGenerationRef.current !== contentEventGeneration) {
      return reconcileContent(reason, draft, conflictDetail);
    }
    if (!sidebarLoaded || !entriesLoaded || !lyricsLoaded || !reviewLoaded) {
      failReconcile("权威数据暂时未完整载入，正在自动重试");
      retryReconcileLater();
      return false;
    }
    let producerAfter: EditorGateStatus;
    try {
      producerAfter = await getEditorGateStatus();
    } catch {
      failReconcile("实时校对暂时无法确认服务器状态，正在自动重试");
      retryReconcileLater();
      return false;
    }
    if (reconciliationGenerationRef.current !== reconciliation) return false;
    if (contentEventGenerationRef.current !== contentEventGeneration) {
      return reconcileContent(reason, draft, conflictDetail);
    }
    if (producerAfter.running) {
      failReconcile("校对期间服务器内容任务已启动，写入仍已锁定");
      retryReconcileLater();
      return false;
    }
    if (producerBefore.instanceId !== producerAfter.instanceId ||
        producerBefore.revision !== producerAfter.revision ||
        producerBefore.completedGeneration !== producerAfter.completedGeneration) {
      return reconcileContent(reason, draft, conflictDetail);
    }
    if (!sseConnectedRef.current) {
      failReconcile("校对期间实时连接已断开，写入仍已锁定");
      retryReconcileLater();
      return false;
    }
    if (!acceptLoadedProducerState(producerAfter)) {
      failReconcile("内容代次无效，写入仍已锁定");
      retryReconcileLater();
      return false;
    }
    reconcileRetryRef.current = null;
    if (draft) {
      setContentConflict({
        reason,
        draft,
        reloadFailed: false,
        ...(conflictDetail ? { detail: conflictDetail } : {}),
        ...(preservedConflictStorageKeyRef.current ? { storageKey: preservedConflictStorageKeyRef.current } : {}),
      });
      return true;
    }
    preservedConflictDraftRef.current = null;
    preservedConflictStorageKeyRef.current = null;
    setContentConflict(null);
    setWriteFence(false);
    return true;
  };
  reconcileContentRef.current = reconcileContent;

  useEffect(() => subscribeProducerProofInvalidated(() => {
    reconciliationGenerationRef.current++;
    setWriteFence(true);
    void reconcileContentRef.current("gap");
  }), []);

  const resolveContentConflict = (conflict: ContentConflict) => {
    if (eventTxtDraft && !clearPersistedEventTxtDraft(username, eventTxtDraft.eventId, eventTxtDraft.locale)) {
      show("无法清理 TXT 本地草稿，旧缓冲区仍被保留", "err");
      return;
    }
    if (!clearPersistedEventTxtDraftFromConflict(username, conflict)) {
      show("无法清理被冻结的 TXT 原始草稿，旧缓冲区仍被保留", "err");
      return;
    }
    if (!clearPersistedContentConflict(username, conflict)) {
      show("无法清理浏览器中的冻结草稿；为避免下次载入误判，当前操作已取消", "err");
      return;
    }
    setEventTxtDraft(null);
    preservedConflictDraftRef.current = null;
    preservedConflictStorageKeyRef.current = null;
    setContentConflict({ ...conflict, draft: null, reloadFailed: true });
    void reconcileContent(conflict.reason, null, conflict.detail);
  };

  const exportConflictDraft = (conflict: ContentConflict) => {
    if (!conflict.draft) return;
    const blob = new Blob([conflict.draft], { type: "application/json;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `moesekai-stale-draft-${Date.now()}.json`;
    link.click();
    URL.revokeObjectURL(url);
  };

  // Discarding unsaved work in the dirty guard must also drop the frozen copy.
  const discardFrozenConflict = (conflict: ContentConflict): boolean => {
    if (!clearPersistedEventTxtDraftFromConflict(username, conflict) ||
        !clearPersistedContentConflict(username, conflict)) {
      show("无法清理浏览器中的冻结草稿，放弃操作已取消", "err");
      return false;
    }
    preservedConflictDraftRef.current = null;
    preservedConflictStorageKeyRef.current = null;
    setContentConflict(null);
    return true;
  };

  const highlightRemoteRow = useCallback((key: string, user: string) => {
    const currentTimer = remoteHighlightTimersRef.current[key];
    if (currentTimer) clearTimeout(currentTimer);
    setRemoteHighlights((prev) => ({ ...prev, [key]: { user, until: Date.now() + 10_000 } }));
    remoteHighlightTimersRef.current[key] = setTimeout(() => {
      setRemoteHighlights((prev) => {
        const next = { ...prev };
        delete next[key];
        return next;
      });
      delete remoteHighlightTimersRef.current[key];
    }, 10_000);
  }, []);

  useEffect(() => () => {
    Object.values(remoteHighlightTimersRef.current).forEach(clearTimeout);
  }, []);

  // ---- Backfill sync of the open side story ----
  // sidestory.sync names no story, so the open story's list summary decides. A reload waits
  // until an unsaved draft is saved or discarded, and reselects the line once loaded.
  const syncReloadPendingRef = useRef<{ kind: SideStoryKind; id: string; locale: Locale } | null>(null);
  const syncReselectRef = useRef<{ kind: SideStoryKind; id: string; locale: Locale; key: string } | null>(null);

  const reloadSyncedSideStory = () => {
    syncReloadPendingRef.current = null;
    if (!sideStoryKind) return;
    syncReselectRef.current = selectedKey ? { kind: sideStoryKind, id: field, locale, key: selectedKey } : null;
    void loadEntries().then((loaded) => {
      if (!loaded) syncReselectRef.current = null;
    });
  };
  const reloadSyncedSideStoryRef = useRef(reloadSyncedSideStory);
  reloadSyncedSideStoryRef.current = reloadSyncedSideStory;

  const handleSideStoryListSynced: SideStoryListRefreshed = (kind, before, after) => {
    if (kind !== sideStoryKind || !before) return;
    const previous = before.find((story) => story.id === field);
    const next = after.find((story) => story.id === field);
    if (!previous || !next || !sideStoryBackfillChanged(previous, next)) return;
    if (!entryDirty) {
      reloadSyncedSideStory();
      show("后台回填更新了当前剧情，已重新载入", "ok");
    } else {
      syncReloadPendingRef.current = { kind, id: field, locale };
      if (selectedKey) setRemoteConflict({ key: selectedKey, user: "后台回填" });
      show("后台回填更新了当前剧情；保存或放弃本地草稿后将自动重新载入", "ok");
    }
  };
  const handleSideStoryListSyncedRef = useRef(handleSideStoryListSynced);
  handleSideStoryListSyncedRef.current = handleSideStoryListSynced;
  const onSideStoryListSynced = useCallback<SideStoryListRefreshed>(
    (kind, before, after) => handleSideStoryListSyncedRef.current(kind, before, after), [],
  );

  useEffect(() => {
    const pending = syncReloadPendingRef.current;
    if (!pending || entryDirty) return;
    syncReloadPendingRef.current = null;
    if (pending.kind === sideStoryKind && pending.id === field && pending.locale === locale) reloadSyncedSideStoryRef.current();
  }, [entryDirty, field, locale, sideStoryKind]);

  useEffect(() => {
    const reselect = syncReselectRef.current;
    if (!reselect || entries.length === 0) return;
    syncReselectRef.current = null;
    if (reselect.kind !== sideStoryKind || reselect.id !== field || reselect.locale !== locale) return;
    const entry = entries.find((candidate) => candidate.key === reselect.key);
    if (entry) {
      setSelectedKey(entry.key);
      setEditValue(entry.text);
    }
  }, [entries, field, locale, setEditValue, setSelectedKey, sideStoryKind]);

  // ---- Realtime SSE ----
  useSSE((event, data) => {
    const d = data as Record<string, unknown>;
    if (event === "entry.updated" || event === "entry.locale.updated" ||
        event === "eventstory.updated" || event === "eventstory.locale.updated" || event === "sidestory.updated" ||
        event === "lyrics.updated" || event === "content.restored") {
      contentEventGenerationRef.current++;
    }
    if (event === "presence.snapshot" || event === "presence.joined" || event === "presence.left") {
      const users = Array.isArray(d.users)
        ? d.users.map((value) => String(value)).filter(Boolean)
        : [];
      if (users.length > 0 || event === "presence.snapshot") {
        setOnlineUsers(Array.from(new Set(users)).sort((a, b) => a.localeCompare(b)));
      } else if (event === "presence.joined") {
        const remoteUser = String(d.user || "");
        if (remoteUser) setOnlineUsers((prev) => Array.from(new Set([...prev, remoteUser])).sort((a, b) => a.localeCompare(b)));
      } else {
        const remoteUser = String(d.user || "");
        if (remoteUser) setOnlineUsers((prev) => prev.filter((candidate) => candidate !== remoteUser));
      }
      return;
    }
    if (event === "gate.status" && d && typeof d === "object") {
      sseConnectedRef.current = true;
      const status = d as unknown as EditorGateStatus;
      if (status.instanceId && !status.running) {
        if (acceptLoadedProducerState(status)) {
          if (!contentConflict && !preservedConflictDraftRef.current) {
            setWriteFence(false);
          }
        }
      } else if (status.running) {
        setWriteFence(true);
      }
    }
    if (event === "sse.disconnected") {
      reconciliationGenerationRef.current++;
      sseConnectedRef.current = false;
      setRealtimeState("reconnecting");
      setOnlineUsers([]);
      clearLoadedProducerState();
      setWriteFence(true);
      if (!contentConflict) void reconcileContentRef.current("gap");
    } else if (event === "sse.reconnected") {
      sseConnectedRef.current = true;
      setRealtimeState("connected");
    } else if (event === "sse.missed-events") {
      sseConnectedRef.current = true;
      setRealtimeState("connected");
      refreshSideStoryLists();
      void reconcileContent("gap").then((reconciled) => {
        if (reconciled) show(d.initial === true ? "实时连接已建立" : "实时连接已恢复", "ok");
      });
    } else if (event === "sync.progress" || event === "translate.progress") {
      setProgress({ label: String(d.detail ?? ""), current: Number(d.current ?? 0), total: Number(d.total ?? 0) });
      if (Number(d.current) >= Number(d.total)) setTimeout(() => setProgress(null), 1500);
    } else if (event === "entry.updated" || event === "entry.locale.updated") {
      const updateLocale = String(d.locale || "zh-CN");
      if (updateLocale === locale && d.category === category && d.field === field && d.clientId !== clientID) {
        const nextText = String(d.text);
        const remoteUser = String(d.user || "协作者");
        highlightRemoteRow(String(d.key), remoteUser);
        if (d.key === selectedKey && selectedEntry && entryDirty) {
          setRemoteConflict({ key: String(d.key), user: remoteUser });
        } else if (d.key === selectedKey) {
          setEditValue(nextText);
          setRemoteConflict(null);
        }
        setEntries((prev) => prev.map((e) => (e.key === d.key ? { ...e, text: nextText, source: String(d.source) } : e)));
        show(`${remoteUser} 修改了一条翻译`, "ok");
      }
    } else if (event === "eventstory.updated" || event === "eventstory.locale.updated") {
      const action = String(d.action || "");
      const updateLocale = d.locale == null ? "" : String(d.locale);
      const isBulkAction = action === "ai-translate" || action === "retry" || action === "reorder";
      if (eventStoryUpdateAffectsLocale(locale, updateLocale, action) && isEventStory &&
          Number(d.eventId) === Number(field) && d.clientId !== clientID) {
        const remoteUser = String(d.user || "协作者");
        if (d.promote === "human" || isBulkAction) {
          const preservedDraft = captureUnsavedDraft();
          const actionLabel = d.promote === "human" ? "整篇标记人工" : "批量更新活动剧情";
          void reconcileContent("remote", preservedDraft, `${remoteUser} 执行了${actionLabel}；已重新载入权威 revision。`).then((reconciled) => {
            if (reconciled && !preservedDraft) show(`${remoteUser} 已${actionLabel}`, "ok");
          });
        } else {
          const update = {
            segmentId: d.segmentId ? String(d.segmentId) : "",
            episodeNo: d.episodeNo != null ? String(d.episodeNo) : "",
            jpKey: d.jpKey != null ? String(d.jpKey) : "",
            entryType: d.entryType ? String(d.entryType) : "",
          };
          const targetEntry = findEventStoryUpdateTarget(entries, update);
          if (!targetEntry) {
            const preservedDraft = captureUnsavedDraft();
            void reconcileContent("remote", preservedDraft, `${remoteUser} 更新了当前活动剧情；本地未能安全定位目标行。`);
            return;
          }
          if (targetEntry.segmentId && eventTxtDraft?.translations.some((translation) => translation.segmentId === targetEntry.segmentId)) {
            const preservedDraft = captureUnsavedDraft();
            void reconcileContent("remote", preservedDraft, `${remoteUser} 更新了 TXT 本地草稿中的同一行；草稿已冻结，禁止静默覆盖。`);
            return;
          }

          const targetKey = targetEntry.key;
          const nextText = String(d.cnText ?? d.text ?? "");
          const nextSource = String(d.source || "human");
          const nextRevision = typeof d.revision === "number" ? d.revision : undefined;
          if (!update.segmentId || !eventStoryEntryHasCanonicalIdentity(targetEntry) || nextRevision === undefined) {
            const preservedDraft = captureUnsavedDraft();
            void reconcileContent(
              "remote",
              preservedDraft,
              `${remoteUser} 更新了当前活动剧情，但事件未携带可继续编辑的权威 revision。`,
            );
            return;
          }
          setEntries((prev) => prev.map((entry) => entry.key === targetKey ? {
            ...entry,
            text: nextText,
            source: nextSource,
            revision: nextRevision,
          } : entry));
          highlightRemoteRow(targetKey, remoteUser);
          if (targetKey === selectedKey && selectedEntry && entryDirty) {
            setRemoteConflict({ key: targetKey, user: remoteUser });
          } else if (targetKey === selectedKey) {
            setEditValue(nextText);
            setRemoteConflict(null);
          }
          void reloadSidebar();
          show(`${remoteUser} 修改了第 ${update.episodeNo || eventStoryEpisodeNo(targetEntry)} 话的一条剧情翻译`, "ok");
        }
      }
    } else if (event === "sidestory.updated") {
      const update = normalizeSideStoryUpdateEvent(d);
      if (!update) return;
      if (sideStoryUpdateRefreshesList(update, sideStoryLocale(locale))) refreshSideStoryLists(update.kind);
      const effect = sideStoryUpdateEffect(update, { kind: sideStoryKind, id: field, locale }, clientID);
      if (effect === "reload") {
        const preservedDraft = captureUnsavedDraft();
        const actionLabel = update.action === "ai" ? "AI 补充翻译" : update.action === "refresh" ? "重新获取剧本" : "批量修改";
        void reconcileContent("remote", preservedDraft, `${update.user} 对当前剧情执行了${actionLabel}；已重新载入权威 revision。`).then((reconciled) => {
          if (reconciled && !preservedDraft) show(`${update.user} 已对当前剧情执行${actionLabel}`, "ok");
        });
      } else if (effect === "apply-lines") {
        const keys = new Map(update.lines.map((line) => [sideStoryEntryKey(update.episode, line.jp), line]));
        setEntries((prev) => applySideStoryLineStates(prev, update.episode, update.lines));
        keys.forEach((_, key) => highlightRemoteRow(key, update.user));
        const selectedLine = selectedKey ? keys.get(selectedKey) : undefined;
        if (selectedKey && selectedLine && selectedLine.revision >= (selectedEntry?.revision ?? 0)) {
          if (selectedEntry && entryDirty) {
            setRemoteConflict({ key: selectedKey, user: update.user });
          } else {
            setEditValue(selectedLine.text);
            setRemoteConflict(null);
          }
        }
        show(`${update.user} 修改了第 ${update.episode} 话的 ${update.lines.length} 行剧情翻译`, "ok");
      }
    } else if (event === "sidestory.sync") {
      refreshSideStoryLists(undefined, onSideStoryListSynced);
    } else if (event === "lyrics.updated") {
      const update = normalizeLyricsUpdateEvent(d);
      if (isLyrics && update && update.clientId !== clientID) {
        const activeTarget = lyricsEditorRef.current?.activeTarget() ?? null;
        if (activeTarget?.musicId === update.musicId) {
          const targetMatches = lyricsUpdateMatchesEditorTarget(update, activeTarget);
          const targetLabel = lyricsUpdateTargetLabel(update);
          const remoteUser = String(d.user || "协作者");
          const lyricsSnapshot = lyricsEditorRef.current?.snapshot() ?? null;
          if (lyricsSnapshot?.dirty ?? lyricsDirty) {
            const detail = targetMatches
              ? `${remoteUser} 更新了你当前查看的${targetLabel ? ` ${targetLabel}` : "歌词目标"}；本地未保存草稿已冻结。`
              : `${remoteUser} 更新了同一歌曲的${targetLabel ? ` ${targetLabel}` : "其他歌词目标"}；共享 revision 已变化，本地未保存草稿已冻结。`;
            const draft = lyricsSnapshot?.document
              ? JSON.stringify({ exportedAt: new Date().toISOString(), kind: "lyrics", editionKey: lyricsSnapshot.editionKey, document: lyricsSnapshot.document }, null, 2)
              : captureUnsavedDraft();
            void reconcileContent("remote", draft, detail);
          } else {
            lyricsEditorRef.current?.reloadAuthoritative();
            show(`${remoteUser} 更新了${targetLabel ? ` ${targetLabel}` : "当前歌曲歌词"}，已载入服务器版本`, "ok");
          }
        } else {
          lyricsEditorRef.current?.reloadCatalog();
        }
      }
    } else if (event === "content.restored") {
      refreshSideStoryLists();
      void reconcileContent("restore");
    }
  }, true);

  useEffect(() => {
    if (realtimeState === "connecting") {
      const timer = setTimeout(() => setRealtimeState("offline"), 5000);
      return () => clearTimeout(timer);
    }
    return undefined;
  }, [realtimeState]);

  return {
    writesLocked, writeFenceRef, setWriteFence,
    contentConflict, preservedConflictDraftRef,
    reconcileContent, reconcileContentRef,
    freezeRecoveredConflict, resolveContentConflict, exportConflictDraft, discardFrozenConflict,
    realtimeState, onlineUsers, remoteHighlights, progress,
  };
}
