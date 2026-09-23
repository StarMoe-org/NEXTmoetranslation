import { APIError, type Locale, type TranslationEntry } from "@/lib/api";
import type { EventStoryTxtDraft } from "@/components/EventStoryTxtImport";
import type { ContentConflict } from "@/components/console/types";

export interface EventTxtDraftRecovery {
  draft: EventStoryTxtDraft | null;
  conflict: ContentConflict | null;
}

const EVENT_TXT_DRAFT_STORAGE_PREFIX = "moesekai-event-txt-draft-v1";
const CONTENT_CONFLICT_STORAGE_PREFIX = "moesekai-content-conflict-v1";

export function eventStoryMutationResultIsAmbiguous(reason: unknown): boolean {
  return !(reason instanceof APIError) || reason.status === 409 || reason.status >= 500;
}

function contentConflictStoragePrefix(username: string): string {
  return `${CONTENT_CONFLICT_STORAGE_PREFIX}:${encodeURIComponent(username)}`;
}

function isContentConflictStorageKey(key: string, prefix: string): boolean {
  return key === prefix || key.startsWith(`${prefix}:`);
}

function parsePersistedContentConflict(raw: string, storageKey: string): { conflict: ContentConflict; savedAt: number } | null {
  try {
    const value = JSON.parse(raw) as {
      version?: unknown;
      savedAt?: unknown;
      conflict?: Partial<ContentConflict>;
    };
    const conflict = value.conflict;
    if (value.version !== 1 || typeof value.savedAt !== "number" || !Number.isFinite(value.savedAt) || !conflict ||
        (conflict.reason !== "restore" && conflict.reason !== "gap" && conflict.reason !== "remote") ||
        typeof conflict.draft !== "string" || !conflict.draft ||
        (conflict.detail !== undefined && typeof conflict.detail !== "string")) {
      return null;
    }
    return {
      savedAt: value.savedAt,
      conflict: {
        reason: conflict.reason,
        draft: conflict.draft,
        reloadFailed: true,
        detail: conflict.detail || "已恢复上次中断时冻结的本地草稿；请重新载入权威数据后导出或舍弃。",
        storageKey,
      },
    };
  } catch {
    return null;
  }
}

export function persistContentConflict(username: string, conflict: ContentConflict): boolean {
  if (typeof window === "undefined" || !conflict.draft) return false;
  try {
    const prefix = contentConflictStoragePrefix(username);
    const storageKey = conflict.storageKey && isContentConflictStorageKey(conflict.storageKey, prefix)
      ? conflict.storageKey
      : `${prefix}:${crypto.randomUUID()}`;
    conflict.storageKey = storageKey;
    localStorage.setItem(storageKey, JSON.stringify({
      version: 1,
      savedAt: Date.now(),
      conflict,
    }));
    return true;
  } catch {
    return false;
  }
}

export function clearPersistedContentConflict(username: string, conflict: ContentConflict): boolean {
  if (typeof window === "undefined") return true;
  const prefix = contentConflictStoragePrefix(username);
  try {
    if (conflict.storageKey && isContentConflictStorageKey(conflict.storageKey, prefix)) {
      localStorage.removeItem(conflict.storageKey);
      return true;
    }
    const matchingKeys: string[] = [];
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (!key || !isContentConflictStorageKey(key, prefix)) continue;
      const raw = localStorage.getItem(key);
      const parsed = raw ? parsePersistedContentConflict(raw, key) : null;
      if (parsed?.conflict.draft === conflict.draft) matchingKeys.push(key);
    }
    matchingKeys.forEach((key) => localStorage.removeItem(key));
    return true;
  } catch {
    return false;
  }
}

export function recoverPersistedContentConflict(username: string): ContentConflict | null {
  if (typeof window === "undefined") return null;
  const prefix = contentConflictStoragePrefix(username);
  try {
    const recovered: Array<{ conflict: ContentConflict; savedAt: number }> = [];
    const invalidKeys: string[] = [];
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (!key || !isContentConflictStorageKey(key, prefix)) continue;
      const raw = localStorage.getItem(key);
      const parsed = raw ? parsePersistedContentConflict(raw, key) : null;
      if (parsed) recovered.push(parsed);
      else invalidKeys.push(key);
    }
    invalidKeys.forEach((key) => localStorage.removeItem(key));
    recovered.sort((a, b) => b.savedAt - a.savedAt);
    const latest = recovered[0]?.conflict ?? null;
    if (latest && recovered.length > 1) {
      latest.detail = `${latest.detail || "已恢复冻结的本地草稿。"} 浏览器中另有 ${recovered.length - 1} 份独立冻结草稿；它们不会被本次操作删除，并可在刷新后继续恢复。`;
    }
    return latest;
  } catch {
    return null;
  }
}

function eventTxtDraftStorageKey(username: string, eventID: number, locale: "zh-CN" | "en-US"): string {
  return `${EVENT_TXT_DRAFT_STORAGE_PREFIX}:${encodeURIComponent(username)}:${eventID}:${locale}`;
}

export function persistEventTxtDraft(username: string, draft: EventStoryTxtDraft): boolean {
  if (typeof window === "undefined") return false;
  try {
    localStorage.setItem(eventTxtDraftStorageKey(username, draft.eventId, draft.locale), JSON.stringify(draft));
    return true;
  } catch {
    return false;
  }
}

export function clearPersistedEventTxtDraft(username: string, eventID: number, locale: Locale): boolean {
  if (typeof window === "undefined" || locale === "ja-JP") return true;
  try {
    localStorage.removeItem(eventTxtDraftStorageKey(username, eventID, locale));
    return true;
  } catch {
    return false;
  }
}

function eventTxtDraftIdentityFromConflict(conflict: ContentConflict): { eventId: number; locale: "zh-CN" | "en-US" } | null {
  if (!conflict.draft) return null;
  try {
    const payload = JSON.parse(conflict.draft) as { eventTxtDraft?: Partial<EventStoryTxtDraft> };
    const draft = payload.eventTxtDraft;
    if (!draft || !Number.isSafeInteger(draft.eventId) || (draft.eventId ?? 0) <= 0 ||
        (draft.locale !== "zh-CN" && draft.locale !== "en-US")) {
      return null;
    }
    return { eventId: draft.eventId as number, locale: draft.locale };
  } catch {
    return null;
  }
}

export function clearPersistedEventTxtDraftFromConflict(username: string, conflict: ContentConflict): boolean {
  const identity = eventTxtDraftIdentityFromConflict(conflict);
  return !identity || clearPersistedEventTxtDraft(username, identity.eventId, identity.locale);
}

export function recoverEventTxtDraft(username: string, eventID: number, locale: Locale, entries: readonly TranslationEntry[]): EventTxtDraftRecovery {
  if (typeof window === "undefined" || locale === "ja-JP") return { draft: null, conflict: null };
  const key = eventTxtDraftStorageKey(username, eventID, locale);
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return { draft: null, conflict: null };
    const value = JSON.parse(raw) as Partial<EventStoryTxtDraft>;
    if (value.eventId !== eventID || value.locale !== locale || typeof value.episodeNo !== "string" || !value.episodeNo ||
        typeof value.snapshotRevision !== "string" || !value.snapshotRevision || typeof value.fileName !== "string" ||
        !Array.isArray(value.translations) || value.translations.length === 0 || value.translations.length > 4000) {
      throw new Error("invalid event TXT draft");
    }
    const seen = new Set<string>();
    for (const candidate of value.translations) {
      if (!candidate || typeof candidate.segmentId !== "string" || !candidate.segmentId || seen.has(candidate.segmentId) ||
          typeof candidate.sourceHash !== "string" || !candidate.sourceHash || !Number.isSafeInteger(candidate.revision) || candidate.revision < 0 ||
          typeof candidate.authoritativeText !== "string" || typeof candidate.text !== "string") {
        throw new Error("invalid event TXT draft row");
      }
      seen.add(candidate.segmentId);
    }

    const draft = { ...value, undoAvailable: value.undoAvailable === true } as EventStoryTxtDraft;
    const bySegment = new Map(entries.flatMap((entry) => entry.segmentId ? [[entry.segmentId, entry] as const] : []));
    const stale = draft.translations.some((candidate) => {
      const entry = bySegment.get(candidate.segmentId);
      return !entry || entry.episodeNo !== draft.episodeNo || entry.sourceHash !== candidate.sourceHash ||
        (entry.revision ?? 0) !== candidate.revision || entry.text !== candidate.authoritativeText;
    });
    if (!stale) return { draft, conflict: null };

    const conflict: ContentConflict = {
      reason: "remote",
      draft: JSON.stringify({
        exportedAt: new Date().toISOString(),
        kind: "translation",
        category: "eventStory",
        field: String(eventID),
        locale,
        key: null,
        staleText: "",
        previouslyLoadedText: "",
        eventTxtDraft: draft,
      }, null, 2),
      reloadFailed: false,
      detail: "服务器中的活动剧情 revision 已变化；原 TXT 本地草稿已冻结，未覆盖也未删除。",
    };
    if (!persistContentConflict(username, conflict)) {
      conflict.detail += " 浏览器冲突副本持久化失败，请立即导出且不要关闭页面。";
    }
    return { draft: null, conflict };
  } catch {
    try { localStorage.removeItem(key); } catch { /* best effort */ }
    return { draft: null, conflict: null };
  }
}

export function overlayEventTxtDraft(entries: readonly TranslationEntry[], draft: EventStoryTxtDraft): TranslationEntry[] {
  const imported = new Map(draft.translations.map((translation) => [translation.segmentId, translation.text]));
  return entries.map((entry) => entry.segmentId && imported.has(entry.segmentId)
    ? { ...entry, text: imported.get(entry.segmentId) as string }
    : entry);
}
