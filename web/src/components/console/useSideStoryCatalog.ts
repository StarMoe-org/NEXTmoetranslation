import { useCallback, useEffect, useRef, useState } from "react";
import {
  Locale, SideStoryKind, SideStorySummary, SideStorySyncStatus,
  getSideStories, getSideStorySyncStatus, triggerSideStorySync,
} from "@/lib/api";
import { sideStoryErrorMessage, sideStoryLocale } from "@/lib/side-story-console";
import type { ShowToast } from "@/components/console/types";

export interface SideStoryListState {
  stories: SideStorySummary[];
  loaded: boolean;
  loading: boolean;
  failed: boolean;
}

/** Sees a list a refresh reloaded; before is null when that kind had not loaded yet. */
export type SideStoryListRefreshed = (
  kind: SideStoryKind, before: readonly SideStorySummary[] | null, after: readonly SideStorySummary[],
) => void;

const EMPTY_LIST: SideStoryListState = { stories: [], loaded: false, loading: false, failed: false };
const KINDS: readonly SideStoryKind[] = ["card", "area"];
const REFRESH_DEBOUNCE_MS = 1500;

/**
 * Side-story lists are large, so each kind loads only once its sidebar group (or one of
 * its stories) is opened; later refreshes are debounced and skip kinds nobody asked for.
 */
export function useSideStoryCatalog({ locale, show }: { locale: Locale; show: ShowToast }) {
  const listLocale = sideStoryLocale(locale);
  const [lists, setLists] = useState<Record<SideStoryKind, SideStoryListState>>({ card: EMPTY_LIST, area: EMPTY_LIST });
  const [syncStatus, setSyncStatus] = useState<SideStorySyncStatus | null>(null);
  const [syncBusy, setSyncBusy] = useState(false);
  const wantedRef = useRef(new Set<SideStoryKind>());
  const syncWantedRef = useRef(false);
  const requestRef = useRef<Record<SideStoryKind, number>>({ card: 0, area: 0 });
  const syncRequestRef = useRef(0);
  const pendingRef = useRef(new Set<SideStoryKind>());
  const pendingCallbacksRef = useRef(new Set<SideStoryListRefreshed>());
  const loadedRef = useRef<Record<SideStoryKind, SideStorySummary[] | null>>({ card: null, area: null });
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const showRef = useRef(show);
  showRef.current = show;

  const loadList = useCallback(async (kind: SideStoryKind, onLoaded: readonly SideStoryListRefreshed[] = []) => {
    const request = ++requestRef.current[kind];
    setLists((prev) => ({ ...prev, [kind]: { ...prev[kind], loading: true } }));
    try {
      const list = await getSideStories(kind, listLocale);
      if (requestRef.current[kind] !== request) return;
      const stories = list.stories ?? [];
      const before = loadedRef.current[kind];
      loadedRef.current[kind] = stories;
      setLists((prev) => ({ ...prev, [kind]: { stories, loaded: true, loading: false, failed: false } }));
      onLoaded.forEach((callback) => callback(kind, before, stories));
    } catch (error) {
      if (requestRef.current[kind] !== request) return;
      setLists((prev) => ({ ...prev, [kind]: { ...prev[kind], loading: false, failed: true } }));
      showRef.current(sideStoryErrorMessage(error, "剧情列表载入失败"), "err");
    }
  }, [listLocale]);

  // A refresh scheduled before a locale switch must load the current locale.
  const loadListRef = useRef(loadList);
  loadListRef.current = loadList;

  const loadSyncStatus = useCallback(async () => {
    const request = ++syncRequestRef.current;
    try {
      const status = await getSideStorySyncStatus();
      if (syncRequestRef.current === request) setSyncStatus(status);
    } catch (error) {
      if (syncRequestRef.current === request) showRef.current(sideStoryErrorMessage(error, "回填进度载入失败"), "err");
    }
  }, []);

  // A locale switch invalidates every list; reload the kinds already in use.
  useEffect(() => {
    KINDS.forEach((kind) => { requestRef.current[kind]++; });
    loadedRef.current = { card: null, area: null };
    setLists({ card: EMPTY_LIST, area: EMPTY_LIST });
    wantedRef.current.forEach((kind) => { void loadList(kind); });
  }, [loadList]);

  useEffect(() => () => {
    if (timerRef.current) clearTimeout(timerRef.current);
  }, []);

  const ensureList = useCallback((kind: SideStoryKind) => {
    if (wantedRef.current.has(kind)) return;
    wantedRef.current.add(kind);
    void loadList(kind);
  }, [loadList]);

  const watchSyncStatus = useCallback((watching: boolean) => {
    syncWantedRef.current = watching;
    if (watching) void loadSyncStatus();
  }, [loadSyncStatus]);

  const refreshLists = useCallback((kind?: SideStoryKind, onRefreshed?: SideStoryListRefreshed) => {
    (kind ? [kind] : KINDS).forEach((candidate) => {
      if (wantedRef.current.has(candidate)) pendingRef.current.add(candidate);
    });
    if (onRefreshed) pendingCallbacksRef.current.add(onRefreshed);
    if (timerRef.current) clearTimeout(timerRef.current);
    timerRef.current = setTimeout(() => {
      timerRef.current = null;
      const kinds = [...pendingRef.current];
      const callbacks = [...pendingCallbacksRef.current];
      pendingRef.current.clear();
      pendingCallbacksRef.current.clear();
      kinds.forEach((candidate) => { void loadListRef.current(candidate, callbacks); });
      if (syncWantedRef.current) void loadSyncStatus();
    }, REFRESH_DEBOUNCE_MS);
  }, [loadSyncStatus]);

  const reloadList = useCallback((kind: SideStoryKind) => {
    wantedRef.current.add(kind);
    void loadList(kind);
  }, [loadList]);

  const runSync = useCallback(async (refreshCatalog: boolean) => {
    if (syncBusy) return;
    setSyncBusy(true);
    try {
      const result = await triggerSideStorySync(refreshCatalog);
      setSyncStatus((prev) => (prev ? { ...prev, state: result.state } : prev));
      // A disabled backfill is a 409; while a round runs, the requested round follows it.
      showRef.current(result.state.running
        ? refreshCatalog ? "回填正在运行，本轮结束后将刷新目录并续跑一轮" : "回填正在运行，本轮结束后将续跑一轮"
        : refreshCatalog ? "已开始刷新目录并续跑一轮回填" : "已开始续跑一轮回填", "ok");
      void loadSyncStatus();
    } catch (error) {
      showRef.current(sideStoryErrorMessage(error, "续跑请求失败"), "err");
    } finally {
      setSyncBusy(false);
    }
  }, [loadSyncStatus, syncBusy]);

  return {
    sideStoryLists: lists, ensureList, reloadList, refreshLists,
    syncStatus, syncBusy, watchSyncStatus, loadSyncStatus, runSync,
  };
}
