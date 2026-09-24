import type {
  Locale, SideStoryAIResult, SideStoryDetail, SideStoryEpisodeApply, SideStoryEpisodeDetail, SideStoryEpisodeSnapshot,
  SideStoryFetchState, SideStoryKind, SideStoryLineConflict, SideStoryLineEdit, SideStoryLineState, SideStoryLocale, SideStorySummary,
  TranslationEntry,
} from "./api";

// Console categories for side stories; the field is the story id (card id or area scenarioId).
export const SIDE_STORY_CATEGORY: Record<SideStoryKind, string> = { card: "cardStory", area: "areaTalk" };

export function sideStoryKindForCategory(category: string): SideStoryKind | null {
  if (category === SIDE_STORY_CATEGORY.card) return "card";
  if (category === SIDE_STORY_CATEGORY.area) return "area";
  return null;
}

// ja-JP is the read-only source view; side-story routes only accept translation locales.
export function sideStoryLocale(locale: Locale): SideStoryLocale {
  return locale === "en-US" ? "en-US" : "zh-CN";
}

// ---- Status labels ----

export type SideStoryStatusTone = "pending" | "untranslated" | "official" | "llm" | "human";

export function sideStoryStatusLabel(story: Pick<SideStorySummary, "status" | "primarySource">): { label: string; tone: SideStoryStatusTone } {
  if (story.status === "pending") return { label: "未获取", tone: "pending" };
  if (story.status !== "untranslated") {
    if (story.primarySource === "official") return { label: "官方", tone: "official" };
    if (story.primarySource === "llm") return { label: "AI", tone: "llm" };
    if (story.primarySource === "human") return { label: "人工", tone: "human" };
  }
  return { label: "未翻译", tone: "untranslated" };
}

// ---- Editor entries ----

// Stored sources map onto the console's existing source vocabulary (SOURCE_LABELS).
const ENTRY_SOURCE: Record<string, string> = { official: "cn", llm: "llm", human: "human" };

export function sideStoryEntrySource(source: string): string {
  return ENTRY_SOURCE[source] ?? "unknown";
}

// Editors may only store human or llm rows.
export const SIDE_STORY_EDIT_SOURCES = ["human", "llm"] as const;

export function sideStoryEditSource(entrySource: string): "human" | "llm" {
  return entrySource === "llm" ? "llm" : "human";
}

export function sideStoryEntryKey(episodeKey: string, jp: string): string {
  return `${episodeKey}|${jp}`;
}

function entryFromLine(episodeKey: string, line: SideStoryLineState, showJapanese: boolean): TranslationEntry {
  return {
    key: sideStoryEntryKey(episodeKey, line.jp),
    text: showJapanese ? line.jp : line.text,
    source: showJapanese ? "unknown" : sideStoryEntrySource(line.source),
    japanese: line.jp,
    episodeNo: episodeKey,
    entryType: line.role === "title" ? "title" : "talk",
    lineRole: line.role,
    revision: line.revision,
    ...(line.role === "talk" && line.speaker ? { speakerName: line.speaker } : {}),
    ...(line.updatedAt ? { updatedAt: line.updatedAt } : {}),
  };
}

/** Flattens a story into editor entries: episodes in order, lines by position (title first). */
export function buildSideStoryEntries(detail: SideStoryDetail, showJapanese = false): TranslationEntry[] {
  return detail.episodes.flatMap((episode) => [...episode.lines]
    .sort((a, b) => a.position - b.position)
    .map((line) => entryFromLine(episode.key, line, showJapanese)));
}

export function sideStoryEntryUntranslated(entry: Pick<TranslationEntry, "text">): boolean {
  return entry.text === "";
}

/** Applies authoritative line states of one episode; an older revision never replaces a newer one. */
export function applySideStoryLineStates(
  entries: readonly TranslationEntry[], episodeKey: string, lines: readonly SideStoryLineState[],
): TranslationEntry[] {
  const byJP = new Map(lines.map((line) => [line.jp, line]));
  return entries.map((entry) => {
    const line = entry.episodeNo === episodeKey && entry.japanese !== undefined ? byJP.get(entry.japanese) : undefined;
    if (!line || line.revision < (entry.revision ?? 0)) return entry;
    return {
      ...entry,
      text: line.text,
      source: sideStoryEntrySource(line.source),
      revision: line.revision,
      ...(line.updatedAt ? { updatedAt: line.updatedAt } : {}),
    };
  });
}

export function sideStoryLineEdit(entry: TranslationEntry, text: string, entrySource: string): SideStoryLineEdit {
  return {
    jp: entry.japanese ?? "",
    text,
    source: sideStoryEditSource(entrySource),
    expectedRevision: entry.revision ?? 0,
  };
}

/** Why the loaded episode differs from a TXT-import snapshot; "" when every snapshot line matches. */
export function sideStorySnapshotMismatch(entries: readonly TranslationEntry[], snapshot: SideStoryEpisodeSnapshot): string {
  const loaded = new Map(entries.filter((entry) => entry.episodeNo === snapshot.episode).map((entry) => [entry.japanese ?? "", entry]));
  for (const segment of snapshot.segments) {
    const entry = loaded.get(segment.id);
    if (!entry || (entry.revision ?? 0) !== (segment.revision ?? 0) || entry.text !== segment.text) {
      return `当前话的「${segment.id}」已不等于服务器快照，请重新载入后再导入`;
    }
  }
  return "";
}

// ---- Mutation errors ----

interface ContractErrorLike { status?: unknown; code?: unknown; current?: unknown; message?: unknown; details?: unknown }

export function sideStoryConflictsFromError(error: unknown): SideStoryLineConflict[] | null {
  const e = error as ContractErrorLike | null;
  if (!e || e.status !== 409 || e.code !== "revision_conflict") return null;
  const conflicts = (e.current as { conflicts?: unknown } | undefined)?.conflicts;
  if (!Array.isArray(conflicts)) return null;
  return conflicts.flatMap((value) => {
    const c = value as Partial<SideStoryLineConflict> | null;
    if (!c || typeof c.jp !== "string" || !Number.isSafeInteger(c.currentRevision) || typeof c.currentText !== "string") return [];
    return [{
      jp: c.jp,
      expectedRevision: Number(c.expectedRevision ?? 0),
      currentRevision: c.currentRevision as number,
      currentText: c.currentText,
      currentSource: c.currentSource === "official" || c.currentSource === "llm" || c.currentSource === "human" ? c.currentSource : "",
    }];
  });
}

export function sideStoryUnknownLinesFromError(error: unknown): string[] | null {
  const e = error as ContractErrorLike | null;
  if (!e || e.status !== 422 || e.code !== "unknown_lines") return null;
  const lines = (e.current as { lines?: unknown } | undefined)?.lines;
  return Array.isArray(lines) ? lines.filter((line): line is string => typeof line === "string") : [];
}

/** Only a transport failure or a 5xx leaves it unknown whether the batch was written. */
export function sideStoryMutationResultIsAmbiguous(error: unknown): boolean {
  const status = (error as ContractErrorLike | null)?.status;
  return typeof status !== "number" || status >= 500;
}

const ERROR_MESSAGES: Record<string, string> = {
  side_story_unavailable: "剧情服务当前不可用",
  backfill_disabled: "后台回填未启用：管理设置中的“卡牌剧情/区域对话后台回填”为 false，或设置了 SIDE_STORY_BACKFILL_ENABLED=false",
  upstream_unavailable: "上游剧本暂时无法获取，请稍后重试",
  script_not_fetched: "该话剧本尚未获取，请先由管理员重新获取剧本",
  script_changed: "上游剧本已变化，请先由管理员重新获取剧本后再导入",
  not_found: "剧情不存在",
  revision_conflict: "保存被拒绝：服务器上的译文已更新",
  unknown_lines: "剧本已变化，提交的部分行已不存在，请重新载入",
  invalid_request: "请求参数无效",
  already_running: "另一个任务正在运行（同步、AI 翻译或备份恢复），请稍后再试",
  draining: "服务正在关闭或重启，请稍后再试",
  internal_error: "服务器内部错误",
  producer_state_changed: "保存被拒绝",
};

// Their details only restate the Chinese message in English.
const SELF_EXPLANATORY_CODES = new Set(["backfill_disabled", "script_not_fetched", "script_changed"]);

/** Contract errors carry only the code in `error`; the reason is in `details`. */
export function sideStoryErrorMessage(error: unknown, fallback: string): string {
  const e = error as ContractErrorLike | null;
  const mapped = e && typeof e.code === "string" ? ERROR_MESSAGES[e.code] : undefined;
  const message = mapped || (e && typeof e.message === "string" && e.message ? e.message : fallback);
  const details = Array.isArray(e?.details) && !(mapped && SELF_EXPLANATORY_CODES.has(e?.code as string))
    ? e.details.filter((detail): detail is string => typeof detail === "string" && detail !== "") : [];
  return details.length > 0 ? `${message}：${details.join("；")}` : message;
}

// ---- Admin action results ----

export const SIDE_STORY_FETCH_STATE_LABELS: Record<SideStoryFetchState, string> = {
  pending: "待导入", imported: "已导入", absent: "未发布", mismatch: "剧本结构不一致", error: "导入失败",
};

// Parts are "<locale>: <message>" joined with "; "; a message may itself contain "; "
// (joined source failures), so only a following locale prefix starts a new part.
const ERROR_PART_SEPARATOR = /; (?=(?:ja-JP|zh-CN|en-US): )/;

/** Splits an episode's error into parts; locale "" is the JP script or the episode itself. */
function sideStoryErrorParts(error: string): { locale: SideStoryLocale | ""; text: string }[] {
  if (!error) return [];
  return error.split(ERROR_PART_SEPARATOR).map((text) => {
    const locale = text.startsWith("zh-CN: ") ? "zh-CN" : text.startsWith("en-US: ") ? "en-US" : "";
    return { locale, text };
  });
}

function sideStoryLocaleState(episode: Pick<SideStoryEpisodeApply, "cnState" | "enState">, locale: SideStoryLocale): SideStoryFetchState {
  return locale === "en-US" ? episode.enState : episode.cnState;
}

const sideStoryStateFailed = (state: SideStoryFetchState) => state === "error" || state === "mismatch";

/** A pending locale's note is a scheduled retry; other locales' notes are ignored. ja-JP follows zh-CN like the state label. */
export function sideStoryEpisodeNotice(
  episode: Pick<SideStoryEpisodeDetail, "fetched" | "cnState" | "enState" | "lastError">, locale: Locale,
): "error" | "retry" | null {
  const parts = sideStoryErrorParts(episode.lastError ?? "");
  if (parts.some((part) => part.locale === "")) return "error";
  if (!episode.fetched) return null;
  const viewed = sideStoryLocale(locale);
  const state = sideStoryLocaleState(episode, viewed);
  if (sideStoryStateFailed(state)) return "error";
  return state === "pending" && parts.some((part) => part.locale === viewed) ? "retry" : null;
}

export function describeSideStoryAIResult(result: SideStoryAIResult, episode: string): string {
  return `AI 补充翻译完成（${episode ? `第 ${episode} 话` : "整篇"}）：已翻译 ${result.translated} 行，仍有 ${result.remaining} 行未翻译`;
}

export function describeSideStoryRefresh(episodes: readonly SideStoryEpisodeApply[]): { message: string; ok: boolean } {
  const fetched = episodes.filter((episode) => episode.fetched).length;
  const changed = episodes.filter((episode) => episode.scriptChanged).length;
  const official = episodes.reduce((sum, episode) => sum + episode.officialWritten, 0);
  const dropped = episodes.reduce((sum, episode) => sum + episode.droppedHumanLines, 0);
  const failures: string[] = [];
  const retries: string[] = [];
  for (const episode of episodes) {
    for (const part of sideStoryErrorParts(episode.error ?? "")) {
      const retry = part.locale !== "" && sideStoryLocaleState(episode, part.locale) === "pending";
      (retry ? retries : failures).push(`第 ${episode.key} 话 ${part.text}`);
    }
  }
  const ok = episodes.every((episode) => episode.fetched && !sideStoryStateFailed(episode.cnState) && !sideStoryStateFailed(episode.enState));
  const parts = [`已重新获取 ${fetched}/${episodes.length} 话剧本`];
  if (changed > 0) parts.push(`${changed} 话剧本有变化`);
  if (official > 0) parts.push(`写入官方译文 ${official} 行`);
  if (dropped > 0) parts.push(`${dropped} 行人工译文因原文已删除而移除`);
  if (failures.length > 0) parts.push(`失败：${failures.join("；")}`);
  if (retries.length > 0) parts.push(`待重试：${retries.join("；")}`);
  return { message: parts.join("，"), ok };
}

// ---- Realtime ----

export interface SideStoryUpdateEvent {
  kind: SideStoryKind;
  id: string;
  episode: string;
  locale: string;
  action: "update" | "ai" | "refresh";
  lines: SideStoryLineState[];
  user: string;
  clientId: string;
}

export function normalizeSideStoryUpdateEvent(data: unknown): SideStoryUpdateEvent | null {
  const d = data as Record<string, unknown> | null;
  if (!d || typeof d !== "object" || (d.kind !== "card" && d.kind !== "area") || typeof d.id !== "string" || !d.id) return null;
  if (d.action !== "update" && d.action !== "ai" && d.action !== "refresh") return null;
  const lines = Array.isArray(d.lines)
    ? d.lines.filter((line): line is SideStoryLineState => Boolean(line) && typeof line.jp === "string" &&
        typeof line.text === "string" && Number.isSafeInteger(line.revision))
    : [];
  return {
    kind: d.kind,
    id: d.id,
    episode: typeof d.episode === "string" ? d.episode : "",
    locale: typeof d.locale === "string" ? d.locale : "",
    action: d.action,
    lines,
    user: typeof d.user === "string" && d.user ? d.user : "协作者",
    clientId: typeof d.clientId === "string" ? d.clientId : "",
  };
}

export type SideStoryUpdateEffect = "ignore" | "apply-lines" | "reload";

/** What an update event means for the open story: a refresh replaces lines in every locale. */
export function sideStoryUpdateEffect(
  update: SideStoryUpdateEvent, view: { kind: SideStoryKind | null; id: string; locale: Locale }, clientID: string,
): SideStoryUpdateEffect {
  if (view.kind !== update.kind || view.id !== update.id || (update.clientId !== "" && update.clientId === clientID)) return "ignore";
  if (update.action === "refresh") return "reload";
  if (view.locale === "ja-JP" || update.locale !== view.locale) return "ignore";
  return update.action === "update" && update.episode !== "" && update.lines.length > 0 ? "apply-lines" : "reload";
}

/** Whether an update changes the counts of a list loaded in listLocale. */
export function sideStoryUpdateRefreshesList(update: SideStoryUpdateEvent, listLocale: SideStoryLocale): boolean {
  return update.action === "refresh" || update.locale === listLocale;
}
