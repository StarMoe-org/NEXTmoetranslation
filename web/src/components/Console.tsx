"use client";

import React, { useCallback, useEffect, useRef, useState } from "react";
import { useToast } from "@/app/providers";
import { SettingsModal } from "@/components/SettingsModal";
import { AdminModal } from "@/components/AdminModal";
import { ConsoleHeader } from "@/components/console/ConsoleHeader";
import { ConsoleSidebar } from "@/components/console/ConsoleSidebar";
import { ConsoleToolbar } from "@/components/console/ConsoleToolbar";
import { EventStoryToolbar } from "@/components/console/EventStoryToolbar";
import { ProducerOperationsShell, useProducerOperations } from "@/components/console/ProducerOperationsShell";
import { TranslationEntryWorkspace } from "@/components/console/TranslationEntryWorkspace";
import { clearPersistedEventTxtDraft } from "@/components/console/console-drafts";
import { useHiddenBadges, usePref } from "@/components/console/preferences";
import type { ContentConflict } from "@/components/console/types";
import { useAppUpdateProbe } from "@/components/console/useAppUpdateProbe";
import { useConsoleEntries } from "@/components/console/useConsoleEntries";
import { useConsoleRealtime } from "@/components/console/useConsoleRealtime";
import { useEntryEditor } from "@/components/console/useEntryEditor";
import { LyricsEditor, LyricsEditorHandle } from "@/components/LyricsEditor";
import { LyricsSourceReview, LyricsSourceReviewHandle } from "@/components/LyricsSourceReview";
import { Locale, TranslationEntry, clearLoadedProducerState, clearSession, getClientID, getRole, getUsername } from "@/lib/api";
import { restoreEventStoryDraftEntries } from "@/lib/event-story-console";

interface PendingAction {
  token: number;
  contextGeneration: number;
  action: () => void;
}

export function Console({ onLogout }: { onLogout: () => void }) {
  const { show } = useToast();

  const [username] = useState(getUsername());
  const [role] = useState(getRole());
  const [clientID] = useState(getClientID());

  // Modal states
  const [showSettings, setShowSettings] = useState(false);
  const [showAdmin, setShowAdmin] = useState(false);
  const [locale, setLocale] = useState<Locale>("zh-CN");
  const [lyricsDirty, setLyricsDirty] = useState(false);
  const [pendingActionLabel, setPendingActionLabel] = useState("");
  const [pendingActionBusy, setPendingActionBusy] = useState(false);
  const [saving, setSaving] = useState(false);
  const pendingActionBusyRef = useRef(false);
  const pendingActionTokenRef = useRef(0);
  const pendingActionRef = useRef<PendingAction | null>(null);
  const lyricsEditorRef = useRef<LyricsEditorHandle>(null);
  const lyricsSourceReviewRef = useRef<LyricsSourceReviewHandle>(null);
  const editRef = useRef<HTMLTextAreaElement>(null);
  const savingRef = useRef(false);
  const contextGenerationRef = useRef(0);
  // Filled in from the realtime hook below, which needs the entry loaders it fences.
  const freezeRecoveredConflictRef = useRef<(conflict: ContentConflict) => void>(() => {});

  const [remoteConflict, setRemoteConflictState] = useState<{ key: string; user: string } | null>(null);
  const remoteConflictRef = useRef(remoteConflict);
  const setRemoteConflict = useCallback((next: { key: string; user: string } | null) => {
    remoteConflictRef.current = next;
    setRemoteConflictState(next);
  }, []);

  // ---- UI prefs ----
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const [enterSaves, setEnterSaves] = usePref("ui.saveShortcut", false);
  const [eventStoriesExpanded, setEventStoriesExpanded] = usePref("ui.eventStoriesExpanded", true);
  const hiddenBadges = useHiddenBadges();
  const updateAvailable = useAppUpdateProbe();

  // On first mount, collapse the sidebar by default on narrow screens.
  useEffect(() => {
    if (typeof window !== "undefined" && window.matchMedia("(max-width: 768px)").matches) {
      setSidebarOpen(false);
    }
  }, []);

  const isReadOnly = locale === "ja-JP";

  const invalidatePendingAction = () => {
    pendingActionTokenRef.current++;
    pendingActionRef.current = null;
    pendingActionBusyRef.current = false;
    setPendingActionBusy(false);
    setPendingActionLabel("");
  };

  const {
    categories, eventStories, visibleEventStories, filteredEventStories,
    eventNameQuery, setEventNameQuery, relatedEventQuery, setRelatedEventQuery, relatedEventFilterAvailable,
    query, setQuery, sortMode, setSortMode,
    category, field, isEventStory, isLyrics, isLyricsSourceReview,
    entries, entriesRef, setEntries, loading,
    selectedEpisode, chapters, filtered, selectedIndex, selectedKey, setSelectedKey, selectedEntry,
    selectedEventStoryIdentityMissing, selectionStateRef, entryDirty, eventTxtDraftDirty,
    editValue, setEditValue, eventTxtDraft, setEventTxtDraft,
    translationEntryListRef, keepTranslationEntryVisible,
    reloadSidebar, loadEntries,
    performSelectField, performSelectChapter, performNavigate, applyLocale,
  } = useConsoleEntries({
    username, locale, setLocale, show, savingRef, contextGenerationRef,
    setRemoteConflict, setSidebarOpen, freezeRecoveredConflictRef,
  });

  const {
    writesLocked, writeFenceRef, setWriteFence,
    contentConflict, preservedConflictDraftRef,
    reconcileContent, reconcileContentRef,
    freezeRecoveredConflict, resolveContentConflict, exportConflictDraft, discardFrozenConflict,
    realtimeState, onlineUsers, remoteHighlights, progress,
  } = useConsoleRealtime({
    username, clientID, locale, show, category, field,
    isEventStory, isLyrics, isLyricsSourceReview,
    entries, setEntries, selectedKey, selectedEntry, entryDirty,
    editValue, setEditValue, eventTxtDraft, setEventTxtDraft, eventTxtDraftDirty,
    lyricsDirty, lyricsEditorRef, lyricsSourceReviewRef,
    setRemoteConflict, contextGenerationRef, invalidatePendingAction,
    loadEntries, reloadSidebar,
  });
  freezeRecoveredConflictRef.current = freezeRecoveredConflict;

  const frozenConflictDraftDirty = Boolean(contentConflict?.draft || preservedConflictDraftRef.current);
  const hasUnsavedChanges = (isLyrics ? lyricsDirty : entryDirty || eventTxtDraftDirty) || frozenConflictDraftDirty;
  const currentHasUnsavedChanges = () => (isLyrics
    ? lyricsEditorRef.current?.isDirty() ?? lyricsDirty
    : entryDirty || eventTxtDraftDirty) || Boolean(contentConflict?.draft || preservedConflictDraftRef.current);

  useEffect(() => {
    if (!hasUnsavedChanges) return;
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [hasUnsavedChanges]);

  const runOrGuard = (label: string, action: () => void) => {
    if (pendingActionBusyRef.current) return;
    if (savingRef.current) {
      show("当前保存尚未完成，请等待结果确认后再继续", "err");
      return;
    }
    if (!currentHasUnsavedChanges()) {
      action();
      return;
    }
    const token = ++pendingActionTokenRef.current;
    pendingActionRef.current = { token, contextGeneration: contextGenerationRef.current, action };
    setPendingActionLabel(label);
  };
  const runOrGuardRef = useRef(runOrGuard);
  runOrGuardRef.current = runOrGuard;

  const guardProducerMutation = (label: string, action: () => Promise<void>) => {
    if (writeFenceRef.current) {
      show("实时连接校对完成前禁止写入", "err");
      return;
    }
    runOrGuard(label, () => {
      if (writeFenceRef.current) {
        show("保存等待期间实时校对已锁定，上游操作未执行", "err");
        return;
      }
      setWriteFence(true);
      clearLoadedProducerState();
      void Promise.resolve().then(action).finally(() => reconcileContentRef.current("gap"));
    });
  };

  const { save, handleSourceChange, applyEventTxtDraft, undoEventTxtDraft } = useEntryEditor({
    username, show, category, field, locale, isEventStory, isReadOnly,
    entries, entriesRef, setEntries, filtered,
    selectedKey, setSelectedKey, selectedEntry, selectedEpisode,
    editValue, setEditValue, entryDirty,
    eventTxtDraft, setEventTxtDraft, eventTxtDraftDirty,
    keepTranslationEntryVisible, reloadSidebar, reconcileContentRef,
    writeFenceRef, savingRef, setSaving, remoteConflictRef, contextGenerationRef,
  });

  const { busy, publishing, doPublish, doAIStory, promoteStory, retryStory, reorderStory } = useProducerOperations({
    show, category, field, locale, writesLocked, writeFenceRef, contextGenerationRef, loadEntries, reloadSidebar,
  });

  // ---- Guarded transitions ----
  const selectField = (cat: string, f: string) => {
    if (cat === category && f === field) return;
    runOrGuard("切换内容", () => performSelectField(cat, f));
  };

  const requestLocaleChange = (next: Locale) => {
    if (next === locale) return;
    runOrGuard("切换编辑语言", () => applyLocale(next));
  };

  const navigate = (dir: 1 | -1) => {
    if (eventTxtDraftDirty && !entryDirty) performNavigate(dir);
    else runOrGuard("切换条目", () => performNavigate(dir));
  };

  const selectChapter = (epNo: string) => {
    if (epNo === selectedEpisode) return;
    runOrGuard("切换章节", () => performSelectChapter(epNo));
  };

  const selectEntry = useCallback((entry: TranslationEntry) => {
    if (savingRef.current) return;
    const current = selectionStateRef.current;
    if (entry.key === current.selectedKey) return;
    const action = () => {
      const latest = entriesRef.current.find((candidate) => candidate.key === entry.key) ?? entry;
      setSelectedKey(latest.key);
      setEditValue(latest.text);
    };
    if (current.eventTxtDraftDirty && !current.entryDirty) action();
    else runOrGuardRef.current("切换条目", action);
  }, [entriesRef, selectionStateRef, setEditValue, setSelectedKey]);

  const closePendingAction = () => {
    if (pendingActionBusyRef.current || savingRef.current) return;
    invalidatePendingAction();
  };

  const continuePendingAction = async (saveFirst: boolean) => {
    if (pendingActionBusyRef.current || savingRef.current || (saveFirst && writeFenceRef.current)) return;
    const pending = pendingActionRef.current;
    if (!pending) return;
    pendingActionBusyRef.current = true;
    setPendingActionBusy(true);
    const pendingIsCurrent = () => pendingActionRef.current?.token === pending.token &&
      pendingActionTokenRef.current === pending.token &&
      contextGenerationRef.current === pending.contextGeneration;
    try {
      if (saveFirst) {
        const importedCountBeforeSave = eventTxtDraft?.translations.length ?? 0;
        const selectedImportedBeforeSave = Boolean(selectedEntry?.segmentId && eventTxtDraft?.translations.some((translation) => translation.segmentId === selectedEntry.segmentId));
        if (isEventStory && eventTxtDraftDirty && !entryDirty && !selectedImportedBeforeSave) {
          invalidatePendingAction();
          show("TXT 草稿仍有未保存条目；请先选择一条草稿内容保存，再重试当前操作", "ok");
          return;
        }
        const saved = isLyrics ? await lyricsEditorRef.current?.save() : await save(undefined, false);
        if (!saved || !pendingIsCurrent()) return;
        if (isEventStory && importedCountBeforeSave - (selectedImportedBeforeSave ? 1 : 0) > 0) {
          invalidatePendingAction();
          show("当前条目已保存；TXT 草稿仍有剩余条目，请继续逐条保存，未保存部分不会被丢弃", "ok");
          return;
        }
      } else {
        if (isLyrics) {
          if (!lyricsEditorRef.current?.discard()) return;
        } else {
          if (eventTxtDraft && !clearPersistedEventTxtDraft(username, eventTxtDraft.eventId, eventTxtDraft.locale)) {
            show("无法清理 TXT 本地草稿，放弃操作已取消", "err");
            return;
          }
          if (eventTxtDraft) {
            const restoredEntries = restoreEventStoryDraftEntries(entriesRef.current, eventTxtDraft.translations);
            entriesRef.current = restoredEntries;
            setEntries(restoredEntries);
            const restoredSelectedEntry = selectedEntry
              ? restoredEntries.find((entry) => entry.key === selectedEntry.key) ?? selectedEntry
              : null;
            if (restoredSelectedEntry) setEditValue(restoredSelectedEntry.text);
          } else if (selectedEntry) {
            setEditValue(selectedEntry.text);
          }
          setEventTxtDraft(null);
          setRemoteConflict(null);
        }
        if (contentConflict?.draft && !discardFrozenConflict(contentConflict)) return;
      }
      if (!pendingIsCurrent()) return;
      invalidatePendingAction();
      pending.action();
    } finally {
      if (pendingIsCurrent()) {
        pendingActionBusyRef.current = false;
        setPendingActionBusy(false);
      }
    }
  };

  const onTextareaKey = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (enterSaves) {
      // Enter = save (Shift+Enter = newline)
      if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); save(); }
    } else {
      // Shift+Enter = save (Enter = newline, default)
      if (e.key === "Enter" && e.shiftKey) { e.preventDefault(); save(); }
    }
    if (e.key === "Escape") { runOrGuard("关闭当前条目", () => setSelectedKey(null)); }
    else if ((e.ctrlKey || e.metaKey) && e.key === "ArrowUp") { e.preventDefault(); navigate(-1); }
    else if ((e.ctrlKey || e.metaKey) && e.key === "ArrowDown") { e.preventDefault(); navigate(1); }
  };

  useEffect(() => {
    if (selectedKey && editRef.current) {
      editRef.current.focus();
      editRef.current.select();
    }
  }, [selectedKey]);

  const currentField = categories.find((c) => c.name === category)?.fields?.find((f) => f.name === field);
  const currentStory = isEventStory ? eventStories.find((s) => String(s.eventId) === field) : undefined;
  const visibleEventStoryCount = visibleEventStories.length;

  const appClass = `app${sidebarOpen ? "" : " sidebar-collapsed"}`;

  return (
    <div className={appClass}>
      {/* Floating button to reopen the sidebar when collapsed/hidden. */}
      {!sidebarOpen && (
        <button className="sidebar-open-btn" onClick={() => setSidebarOpen(true)} aria-label="显示侧边栏" title="显示侧边栏">☰</button>
      )}
      {/* Mobile drawer backdrop. */}
      <div className="sidebar-backdrop" onClick={() => setSidebarOpen(false)} />

      <ConsoleSidebar
        username={username}
        role={role}
        locale={locale}
        isLyrics={isLyrics}
        isLyricsSourceReview={isLyricsSourceReview}
        publishing={publishing}
        writesLocked={writesLocked}
        categories={categories}
        category={category}
        field={field}
        hiddenBadges={hiddenBadges}
        visibleEventStoryCount={visibleEventStoryCount}
        filteredEventStories={filteredEventStories}
        eventNameQuery={eventNameQuery}
        setEventNameQuery={setEventNameQuery}
        eventStoriesExpanded={eventStoriesExpanded}
        setEventStoriesExpanded={setEventStoriesExpanded}
        doPublish={doPublish}
        onOpenSettings={() => setShowSettings(true)}
        onOpenAdmin={() => setShowAdmin(true)}
        onRequestLogout={() => runOrGuard("退出登录", () => {
          void clearSession().then((cleared) => { if (cleared) onLogout(); }).catch((error) => show(error.message, "err"));
        })}
        onCollapse={() => setSidebarOpen(false)}
        requestLocaleChange={requestLocaleChange}
        selectField={selectField}
      />

      <main className="main">
        {updateAvailable && (
          <div className="update-available-banner" role="status" aria-live="polite">
            页面已有新版本，刷新后会自动加载最新 assets；当前未保存内容不会自动替换。
            <button type="button" className="btn btn-primary btn-sm" onClick={() => {
              window.sessionStorage.removeItem("nexttrans-html-etag");
              window.location.reload();
            }}>立即刷新</button>
          </div>
        )}
        {writesLocked && (
          <div className="connection-fence" role="status" aria-live="assertive">
            实时事件可能有缺口，正在载入服务器权威数据。校对完成前所有内容写入均已锁定。
          </div>
        )}
        {progress && (
          <div className="progress-line" role="status" aria-live="polite">
            <span>{progress.label}</span>
            <div className="progress-track">
              <div className="progress-fill" style={{ width: `${progress.total ? (progress.current / progress.total) * 100 : 0}%` }} />
            </div>
          </div>
        )}

        {isLyrics ? (
          <LyricsEditor ref={lyricsEditorRef} role={role} writeLocked={writesLocked} onDirtyChange={setLyricsDirty} />
        ) : isLyricsSourceReview && role === "admin" ? (
          <LyricsSourceReview ref={lyricsSourceReviewRef} writeLocked={writesLocked} />
        ) : !category || !field ? (
          <div className="center-state">
            <p>从左侧选择一个翻译类别</p>
          </div>
        ) : (
          <>
            <ConsoleHeader
              category={category}
              field={field}
              currentStory={currentStory}
              currentField={currentField}
              realtimeState={realtimeState}
              onlineUsers={onlineUsers}
              selectedIndex={selectedIndex}
              filteredCount={filtered.length}
            />

            {/* Per-story toolbar with integrated Chapter Navigation */}
            {isEventStory && (
              <EventStoryToolbar
                role={role}
                locale={locale}
                field={field}
                entries={entries}
                selectedEntry={selectedEntry}
                currentStory={currentStory}
                chapters={chapters}
                selectedEpisode={selectedEpisode}
                selectChapter={selectChapter}
                busy={busy}
                saving={saving}
                writesLocked={writesLocked}
                entryDirty={entryDirty}
                eventTxtDraftDirty={eventTxtDraftDirty}
                eventTxtDraft={eventTxtDraft}
                applyEventTxtDraft={applyEventTxtDraft}
                undoEventTxtDraft={undoEventTxtDraft}
                onAIStory={() => guardProducerMutation("运行 AI 剧情翻译", doAIStory)}
                onPromoteStory={() => guardProducerMutation("整篇标记人工", promoteStory)}
                onRetryStory={() => guardProducerMutation("重新获取剧情", retryStory)}
                onReorderStory={() => guardProducerMutation("重排序对话", reorderStory)}
              />
            )}

            <ConsoleToolbar
              isEventStory={isEventStory}
              locale={locale}
              relatedEventFilterAvailable={relatedEventFilterAvailable}
              relatedEventQuery={relatedEventQuery}
              onRelatedEventQueryChange={setRelatedEventQuery}
              query={query}
              onQueryChange={setQuery}
              sortMode={sortMode}
              onSortModeChange={setSortMode}
            />

            <TranslationEntryWorkspace
              category={category}
              field={field}
              isEventStory={isEventStory}
              isReadOnly={isReadOnly}
              loading={loading}
              saving={saving}
              writesLocked={writesLocked}
              filtered={filtered}
              selectedKey={selectedKey}
              selectedEntry={selectedEntry}
              selectedIndex={selectedIndex}
              selectedEventStoryIdentityMissing={selectedEventStoryIdentityMissing}
              remoteHighlights={remoteHighlights}
              remoteConflict={remoteConflict}
              setRemoteConflict={setRemoteConflict}
              eventTxtDraftDirty={eventTxtDraftDirty}
              editValue={editValue}
              setEditValue={setEditValue}
              editRef={editRef}
              translationEntryListRef={translationEntryListRef}
              enterSaves={enterSaves}
              setEnterSaves={setEnterSaves}
              onTextareaKey={onTextareaKey}
              navigate={navigate}
              save={save}
              selectEntry={selectEntry}
              handleSourceChange={handleSourceChange}
            />
          </>
        )}
      </main>

      {/* Settings & Admin modals */}
      <SettingsModal open={showSettings} onClose={() => setShowSettings(false)} locale={locale} guardProducerMutation={guardProducerMutation} />
      {role === "admin" && <AdminModal open={showAdmin} onClose={() => setShowAdmin(false)} guardProducerMutation={guardProducerMutation} />}
      <ProducerOperationsShell
        pendingActionLabel={pendingActionLabel}
        pendingActionBusy={pendingActionBusy}
        saving={saving}
        writesLocked={writesLocked}
        closePendingAction={closePendingAction}
        continuePendingAction={continuePendingAction}
        contentConflict={contentConflict}
        exportConflictDraft={exportConflictDraft}
        resolveContentConflict={resolveContentConflict}
        reconcileContent={reconcileContent}
      />
    </div>
  );
}
