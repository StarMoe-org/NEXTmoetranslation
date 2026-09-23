import { useCallback, useEffect, useMemo, useRef, useState, type RefObject } from "react";
import {
  CategoryInfo, EventAssociationIndex, EventStorySummary, Locale, TranslationEntry,
  getCategories, getEntries, getEventAssociations, getEventStories, getEventStory,
} from "@/lib/api";
import type { EventStoryTxtDraft } from "@/components/EventStoryTxtImport";
import { buildEventStoryEntries, eventStoryEntryLabel } from "@/lib/labels";
import {
  eventStoryEntryHasCanonicalIdentity, eventStoryEntryType, eventStoryEpisodeNo,
  listEventStoryEpisodeNos, resolveSelectedEventStoryEpisode,
} from "@/lib/event-story-console";
import { overlayEventTxtDraft, recoverEventTxtDraft } from "@/components/console/console-drafts";
import type { ChapterTab, ContentConflict, ShowToast } from "@/components/console/types";

interface SidebarReloadResult {
  generation: number;
  locale: Locale;
  categories: PromiseSettledResult<CategoryInfo[]>;
  eventStories: PromiseSettledResult<EventStorySummary[]>;
}

export interface ConsoleEntriesOptions {
  username: string;
  locale: Locale;
  setLocale: (locale: Locale) => void;
  show: ShowToast;
  savingRef: RefObject<boolean>;
  contextGenerationRef: RefObject<number>;
  setRemoteConflict: (next: { key: string; user: string } | null) => void;
  setSidebarOpen: (open: boolean) => void;
  // Assigned by Console once the realtime hook exists; a recovered TXT draft conflict
  // has to freeze the same write fence as any other reconciliation.
  freezeRecoveredConflictRef: RefObject<(conflict: ContentConflict) => void>;
}

export function useConsoleEntries({
  username,
  locale,
  setLocale,
  show,
  savingRef,
  contextGenerationRef,
  setRemoteConflict,
  setSidebarOpen,
  freezeRecoveredConflictRef,
}: ConsoleEntriesOptions) {
  const [categories, setCategories] = useState<CategoryInfo[]>([]);
  const [eventStories, setEventStories] = useState<EventStorySummary[]>([]);
  const [eventAssociations, setEventAssociations] = useState<EventAssociationIndex>({ categories: {} });
  const [category, setCategory] = useState("");
  const [field, setField] = useState("");
  const [entries, setEntries] = useState<TranslationEntry[]>([]);
  const entriesRef = useRef(entries);
  entriesRef.current = entries;
  const [selectedEpisode, setSelectedEpisode] = useState<string>("1");
  const selectedEpisodeRef = useRef(selectedEpisode);
  selectedEpisodeRef.current = selectedEpisode;
  const [loading, setLoading] = useState(false);
  const [query, setQuery] = useState("");
  const [sortMode, setSortMode] = useState<"kana" | "id-desc" | "time-desc">("time-desc");
  const [eventNameQuery, setEventNameQuery] = useState("");
  const [relatedEventQuery, setRelatedEventQuery] = useState("");
  const [selectedKey, setSelectedKey] = useState<string | null>(null);
  const [editValue, setEditValue] = useState("");
  const [eventTxtDraft, setEventTxtDraft] = useState<EventStoryTxtDraft | null>(null);
  const translationEntryListRef = useRef<HTMLDivElement>(null);
  const loadGenerationRef = useRef(0);
  const sidebarReloadGenerationRef = useRef(0);
  const sidebarAppliedGenerationRef = useRef(0);
  const sidebarReloadRef = useRef<{ generation: number; promise: Promise<SidebarReloadResult> } | null>(null);
  const sidebarSnapshotRef = useRef<Map<Locale, { categories: CategoryInfo[]; eventStories: EventStorySummary[] }>>(new Map());

  const isEventStory = category === "eventStory";
  const isLyrics = category === "lyrics";
  const isLyricsSourceReview = category === "lyricsSourceReview";

  // ---- Load categories + event stories ----
  const reloadSidebar = useCallback(async (): Promise<boolean> => {
    const generation = ++sidebarReloadGenerationRef.current;
    const requestLocale = locale;
    const promise = Promise.allSettled([
      getCategories(requestLocale),
      getEventStories(requestLocale),
    ]).then(([categories, eventStories]): SidebarReloadResult => ({ generation, locale: requestLocale, categories, eventStories }));
    sidebarReloadRef.current = { generation, promise };

    while (true) {
      const latest: { generation: number; promise: Promise<SidebarReloadResult> } | null = sidebarReloadRef.current;
      if (!latest) return false;
      const result: SidebarReloadResult = await latest.promise;
      if (sidebarReloadRef.current?.generation !== result.generation) continue;
      if (result.locale !== locale) continue;

      const loaded = result.categories.status === "fulfilled" && result.eventStories.status === "fulfilled";
      if (sidebarAppliedGenerationRef.current < result.generation) {
        sidebarAppliedGenerationRef.current = result.generation;
        if (result.categories.status === "fulfilled") {
          setCategories(result.categories.value);
          if (result.eventStories.status === "fulfilled") {
            sidebarSnapshotRef.current.set(result.locale, {
              categories: result.categories.value,
              eventStories: result.eventStories.value,
            });
          }
        } else {
          const snapshot = sidebarSnapshotRef.current.get(result.locale);
          if (snapshot) {
            setCategories(snapshot.categories);
          }
          show(result.categories.reason instanceof Error ? result.categories.reason.message : "侧栏分类载入失败", "err");
        }
        if (result.eventStories.status === "fulfilled") {
          setEventStories(result.eventStories.value);
        } else {
          const snapshot = sidebarSnapshotRef.current.get(result.locale);
          if (snapshot) {
            setEventStories(snapshot.eventStories);
          }
        }
      }
      return loaded;
    }
  }, [locale, show]);

  useEffect(() => { void reloadSidebar(); }, [reloadSidebar]);

  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const refresh = async () => {
      let delay = 60 * 60 * 1000;
      try {
        const index = await getEventAssociations();
        if (!stopped) setEventAssociations(index);
      } catch {
        // Preserve the last successful client snapshot and retry transient
        // upstream failures without requiring a full page refresh.
        delay = 30_000;
      } finally {
        if (!stopped) timer = setTimeout(refresh, delay);
      }
    };
    void refresh();
    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
    };
  }, []);

  // ---- Load entries on selection change ----
  const loadEntries = useCallback(async (): Promise<boolean> => {
    const generation = ++loadGenerationRef.current;
    if (!category || !field) {
      setEntries([]);
      setSelectedKey(null);
      setEditValue("");
      setEventTxtDraft(null);
      setRemoteConflict(null);
      return true;
    }
    setEntries([]);
    setSelectedKey(null);
    setEditValue("");
    setEventTxtDraft(null);
    setRemoteConflict(null);
    if (isLyrics || isLyricsSourceReview) {
      setLoading(false);
      return true;
    }
    setLoading(true);
    try {
      if (isEventStory) {
        const detail = await getEventStory(Number(field), locale);
        if (loadGenerationRef.current !== generation) return false;
        const list = buildEventStoryEntries(detail);
        const recovery = recoverEventTxtDraft(username, Number(field), locale, list);
        const visible = recovery.draft ? overlayEventTxtDraft(list, recovery.draft) : list;
        setEventTxtDraft(recovery.draft);
        if (recovery.conflict) {
          freezeRecoveredConflictRef.current(recovery.conflict);
        }
        setEntries(visible);
        const availableEpisodes = listEventStoryEpisodeNos(visible);
        const initialEp = resolveSelectedEventStoryEpisode(selectedEpisodeRef.current, availableEpisodes);
        selectedEpisodeRef.current = initialEp;
        setSelectedEpisode(initialEp);
        const epEntries = initialEp === "all" ? visible : visible.filter((entry) => eventStoryEpisodeNo(entry) === initialEp);
        const first = epEntries.length > 0 ? epEntries[0] : visible[0];
        if (first) { setSelectedKey(first.key); setEditValue(first.text); }
        if (recovery.draft) {
          show(`已恢复 ${recovery.draft.fileName} 的 ${recovery.draft.translations.length} 条 TXT 本地草稿`, "ok");
        } else if (recovery.conflict) {
          show("检测到 revision 已变化的 TXT 本地草稿；已冻结并等待导出或舍弃", "err");
        }
        return true;
      }
      const data = await getEntries(category, field, undefined, locale);
      if (loadGenerationRef.current !== generation) return false;
      setEntries(data);
      const first = [...data].sort((a, b) => {
        const kana = a.key.localeCompare(b.key, "ja", { usage: "sort", sensitivity: "base" });
        return kana !== 0 ? kana : a.key.localeCompare(b.key, undefined, { numeric: true });
      })[0];
      if (first) { setSelectedKey(first.key); setEditValue(first.text); }
      return true;
    } catch (e) {
      if (loadGenerationRef.current === generation) show(e instanceof Error ? e.message : "加载失败", "err");
      return false;
    } finally {
      if (loadGenerationRef.current === generation) setLoading(false);
    }
  }, [category, field, freezeRecoveredConflictRef, isEventStory, isLyrics, isLyricsSourceReview, locale, setRemoteConflict, show, username]);

  useEffect(() => { void loadEntries(); }, [loadEntries]);

  // ---- Derived ----
  const sortedEntries = useMemo(() => {
    const next = [...entries];
    next.sort((a, b) => {
      if (sortMode === "time-desc") {
        const time = (b.updatedAt ?? 0) - (a.updatedAt ?? 0);
        if (time !== 0) return time;
      } else if (sortMode === "id-desc") {
        const aID = Number(a.ids?.[0] ?? Number.NaN);
        const bID = Number(b.ids?.[0] ?? Number.NaN);
        if (Number.isFinite(aID) && Number.isFinite(bID) && aID !== bID) return bID - aID;
        if (Number.isFinite(aID) !== Number.isFinite(bID)) return Number.isFinite(bID) ? 1 : -1;
      }
      const kana = a.key.localeCompare(b.key, "ja", { usage: "sort", sensitivity: "base" });
      return kana !== 0 ? kana : a.key.localeCompare(b.key, undefined, { numeric: true });
    });
    return next;
  }, [entries, sortMode]);

  const visibleEventStories = useMemo(
    () => eventStories.filter((story) => !story.allOfficialTagged),
    [eventStories],
  );

  const filteredEventStories = useMemo(() => {
    const q = eventNameQuery.trim().toLowerCase();
    if (!q) return visibleEventStories;
    return visibleEventStories.filter((story) =>
      `${story.eventName || ""}\n${story.eventNameJapanese || ""}\n${story.eventId}`.toLowerCase().includes(q),
    );
  }, [eventNameQuery, visibleEventStories]);

  const categoryEventAssociations = useMemo(
    () => eventAssociations.categories[category] || {},
    [category, eventAssociations.categories],
  );
  const relatedEventFilterAvailable = category === "events" || Object.keys(categoryEventAssociations).length > 0;
  const relatedEventEntityIDs = useMemo(() => {
    const q = relatedEventQuery.trim().toLowerCase();
    if (!q || !relatedEventFilterAvailable) return null;
    const matchingEventIDs = new Set(eventStories
      .filter((story) => `${story.eventName || ""}\n${story.eventNameJapanese || ""}\n${story.eventId}`.toLowerCase().includes(q))
      .map((story) => story.eventId));
    if (category === "events") return new Set([...matchingEventIDs].map(String));
    const entityIDs = new Set<string>();
    for (const [entityID, eventIDs] of Object.entries(categoryEventAssociations)) {
      if (eventIDs.some((eventID) => matchingEventIDs.has(eventID))) entityIDs.add(entityID);
    }
    return entityIDs;
  }, [category, categoryEventAssociations, eventStories, relatedEventFilterAvailable, relatedEventQuery]);

  const chapters = useMemo<ChapterTab[]>(() => {
    if (!isEventStory || entries.length === 0) return [];
    const map = new Map<string, { title: string; total: number; untranslated: number }>();
    for (const entry of entries) {
      const epNo = eventStoryEpisodeNo(entry);
      let ep = map.get(epNo);
      if (!ep) {
        ep = { title: "", total: 0, untranslated: 0 };
        map.set(epNo, ep);
      }
      ep.total++;
      const isUntranslated = !entry.text || entry.source === "unknown" || entry.source === "llm";
      if (isUntranslated) ep.untranslated++;
      if (eventStoryEntryType(entry) === "title" && !ep.title) {
        ep.title = entry.text || entry.japanese || "";
      }
    }
    const result: ChapterTab[] = [];
    for (const [episodeNo, data] of map.entries()) {
      result.push({
        episodeNo,
        title: data.title,
        total: data.total,
        untranslated: data.untranslated,
      });
    }
    result.sort((a, b) => a.episodeNo.localeCompare(b.episodeNo, undefined, { numeric: true }));
    return result;
  }, [entries, isEventStory]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    let source = isEventStory ? entries : sortedEntries;
    if (isEventStory && selectedEpisode !== "all") {
      source = source.filter((entry) => eventStoryEpisodeNo(entry) === selectedEpisode);
    }
    return source.filter((e) => {
      if (relatedEventEntityIDs && !(e.ids || []).some((id) => relatedEventEntityIDs.has(String(id)))) return false;
      if (!q) return true;
      return isEventStory
        ? `${e.japanese || eventStoryEntryLabel(e.key)}\n${e.text}`.toLowerCase().includes(q)
        : e.key.toLowerCase().includes(q) || e.text.toLowerCase().includes(q);
    });
  }, [entries, isEventStory, query, relatedEventEntityIDs, selectedEpisode, sortedEntries]);

  const selectedIndex = useMemo(
    () => (selectedKey ? filtered.findIndex((e) => e.key === selectedKey) : -1),
    [selectedKey, filtered],
  );
  const selectedEntry = selectedKey ? entries.find((entry) => entry.key === selectedKey) ?? null : null;
  const selectedEventStoryIdentityMissing = Boolean(
    isEventStory && selectedEntry && !eventStoryEntryHasCanonicalIdentity(selectedEntry),
  );
  const entryDirty = selectedEntry != null && editValue !== selectedEntry.text;
  const eventTxtDraftDirty = isEventStory && (eventTxtDraft?.translations.length ?? 0) > 0;
  const selectionStateRef = useRef({ selectedKey, entryDirty, eventTxtDraftDirty });
  selectionStateRef.current = { selectedKey, entryDirty, eventTxtDraftDirty };

  // ---- Actions ----
  const performSelectField = (cat: string, f: string) => {
    contextGenerationRef.current++;
    loadGenerationRef.current++;
    setEventTxtDraft(null);
    selectedEpisodeRef.current = "1";
    setSelectedEpisode("1");
    setCategory(cat); setField(f); setQuery(""); setSelectedKey(null);
    if (typeof window !== "undefined" && window.matchMedia("(max-width: 768px)").matches) {
      setSidebarOpen(false);
    }
  };

  const applyLocale = (next: Locale) => {
    contextGenerationRef.current++;
    loadGenerationRef.current++;
    sidebarReloadRef.current = null;
    setEventTxtDraft(null);
    setLocale(next);
    setCategories([]);
    setEventStories([]);
    setEntries([]);
    setSelectedKey(null);
    setEditValue("");
  };

  const keepTranslationEntryVisible = useCallback((key: string) => {
    const container = translationEntryListRef.current;
    const row = container?.querySelector<HTMLElement>(`[data-key="${CSS.escape(key)}"]`);
    if (!container || !row) return;
    const containerRect = container.getBoundingClientRect();
    const rowRect = row.getBoundingClientRect();
    const nextTop = rowRect.top < containerRect.top
      ? container.scrollTop + rowRect.top - containerRect.top - 12
      : rowRect.bottom > containerRect.bottom
        ? container.scrollTop + rowRect.bottom - containerRect.bottom + 12
        : container.scrollTop;
    if (nextTop !== container.scrollTop) container.scrollTo({ top: nextTop, behavior: "smooth" });
  }, []);

  const performNavigate = useCallback((dir: 1 | -1) => {
    if (savingRef.current || selectedIndex < 0) return;
    const idx = selectedIndex + dir;
    if (idx < 0 || idx >= filtered.length) return;
    const nextKey = filtered[idx].key;
    const next = entriesRef.current.find((entry) => entry.key === nextKey) ?? filtered[idx];
    setSelectedKey(next.key);
    setEditValue(next.text);
    requestAnimationFrame(() => keepTranslationEntryVisible(next.key));
  }, [savingRef, selectedIndex, filtered, keepTranslationEntryVisible]);

  const performSelectChapter = (epNo: string) => {
    selectedEpisodeRef.current = epNo;
    setSelectedEpisode(epNo);
    const currentEntries = entriesRef.current;
    const targetEntries = epNo === "all" ? currentEntries : currentEntries.filter((entry) => eventStoryEpisodeNo(entry) === epNo);
    if (targetEntries.length > 0) {
      const alreadySelected = targetEntries.some((entry) => entry.key === selectedKey);
      if (!alreadySelected) {
        setSelectedKey(targetEntries[0].key);
        setEditValue(targetEntries[0].text);
      }
    }
  };

  return {
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
  };
}
