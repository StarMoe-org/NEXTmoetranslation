import type { LyricsEditionCommand } from "@/components/LyricsEditionMenu";
import type {
  APIError, CatalogMusicItem, LyricsEditorLine, LyricsRenditionSide, RenditionLyricsDocument,
  SongLyrics, SongLyricsDocument,
} from "@/lib/api";
import { editableLyricSegments } from "@/lib/lyrics-segmentation.mjs";
import { isRenditionLyricsDocument, lyricsHasPerformerSegmentation } from "@/lib/lyrics-versioning.mjs";

export type LyricsProjectionKind = "full_only" | "game_only" | "exact_projection" | "independent_game" | "invalid";

export function databaseAvailabilityDescription(state: NonNullable<CatalogMusicItem["lyricsAvailabilityState"]>): string {
  if (state === "satisfied_no_lyrics") return "目录已审核为无需歌词，因此没有可编辑正文。";
  if (state === "incomplete") return "来源结果尚未形成可编辑正文，系统保持 fail-closed。";
  if (state === "ambiguous") return "来源仍有歧义，系统不会自动选择正文。";
  if (state === "missing") return "尚未找到可验证来源，系统不会生成空白正文冒充导入结果。";
  return "来源处理失败，系统保留数据库状态但不生成可编辑正文。";
}

export function isLegacyLyricsDocument(document: SongLyricsDocument | null | undefined): document is SongLyrics {
  return Boolean(document) && !isRenditionLyricsDocument(document);
}

export function preserveReadOnlyLyricsSourceFacts(saved: SongLyricsDocument, attempted: SongLyricsDocument): SongLyricsDocument {
  if (isRenditionLyricsDocument(saved)) return saved;
  if (isRenditionLyricsDocument(attempted)) return saved;
  const legacySaved = saved as SongLyrics;
  const legacyAttempted = attempted as SongLyrics;
  return {
    ...legacySaved,
    ...(legacySaved.availableVersions === undefined && legacyAttempted.availableVersions !== undefined ? { availableVersions: legacyAttempted.availableVersions } : {}),
    ...(legacySaved.gameProjection === undefined && legacyAttempted.gameProjection !== undefined ? { gameProjection: legacyAttempted.gameProjection } : {}),
    ...(legacySaved.reasonCode === undefined && legacyAttempted.reasonCode !== undefined ? { reasonCode: legacyAttempted.reasonCode } : {}),
    ...(legacySaved.fixedIdentities === undefined && legacyAttempted.fixedIdentities !== undefined ? { fixedIdentities: legacyAttempted.fixedIdentities } : {}),
    ...(legacySaved.provenance === undefined && legacyAttempted.provenance !== undefined ? { provenance: legacyAttempted.provenance } : {}),
  };
}

function editableRenditionSide(side: LyricsRenditionSide | undefined): LyricsRenditionSide | undefined {
  if (!side) return undefined;
  return {
    ...side,
    lines: side.lines.map((line) => ({
      ...line,
      trailingPerformerIds: [...line.trailingPerformerIds],
      segments: editableLyricSegments(line.japanese, line.segments) as LyricsRenditionSide["lines"][number]["segments"],
    })),
  };
}

export function editableLyricsDocument(loaded: SongLyricsDocument): SongLyricsDocument {
  if (isLegacyLyricsDocument(loaded)) {
    return {
      ...loaded,
      lines: loaded.lines.map((line) => ({
        ...line,
        segments: editableLyricSegments(line.japanese, line.segments),
      })),
    };
  }
  return {
    ...loaded,
    translationEditions: loaded.translationEditions.map((edition) => ({ ...edition })),
    renditions: loaded.renditions.map((rendition) => ({
      ...rendition,
      performers: rendition.performers.map((performer) => ({ ...performer })),
      ...(rendition.full ? { full: editableRenditionSide(rendition.full) } : {}),
      ...(rendition.game ? { game: editableRenditionSide(rendition.game) } : {}),
      relation: {
        ...rendition.relation,
        ...(rendition.relation.lineIds ? { lineIds: [...rendition.relation.lineIds] } : {}),
      },
      ...(rendition.translationCredits ? { translationCredits: { ...rendition.translationCredits } } : {}),
    })),
  };
}

export function sourceLabel(error: APIError): string {
  const labels: Record<string, string> = {
    revision_conflict: "其他编辑者已保存新版本",
    segment_mismatch: "分段文字与日文原文不一致",
    invalid_performer: "包含无效的演唱者",
    incomplete_publication: "发布前必须补齐翻译署名、中文翻译及适用的角色分词",
    admin_required: "仅管理员可以导入外部歌词来源",
    not_found: "服务器上找不到这首曲目或歌词",
    internal_error: "服务器处理失败，请稍后重试",
    load_failed: "歌词加载失败",
    save_failed: "歌词草稿保存失败",
    publication_failed: "歌词发布状态更新失败",
    producer_state_changed: "内容版本已变化，需要重新校对后再操作",
    source_drift: "歌词来源或日文原文已变化",
    source_restricted: "来源页面禁止转载",
    source_unsupported: "无法安全解析来源页面",
    source_identity_mismatch: "来源页面与曲目资料不匹配",
    source_identity_incomplete: "曲目缺少用于核对来源的作者资料",
    source_performer_mapping_failed: "来源中的演唱者证据无法安全映射",
    invalid_source_preview: "来源预览结构无效",
    invalid_lyrics_response: "服务器返回了无法验证的歌词结果",
    invalid_translation_edition: "歌词译本合同无效",
    translation_edition_not_found: "服务器上找不到该歌词译本",
    translation_edition_exists: "该歌词译本 key 已存在，请重试",
    translation_edition_limit: "这首歌已达到 16 个译本上限",
    translation_edition_conflict: "歌词译本已被其他编辑者更新",
    invalid_game_projection: "Game 投影引用无效，当前修改不能保存",
    source_unavailable: "歌词来源暂时不可用",
  };
  return labels[error.code] || error.message;
}

const TERMINAL_SOURCE_IMPORT_CODES = new Set([
  "admin_required", "source_drift", "source_identity_mismatch", "source_import_expired",
  "source_import_consumed", "source_import_identity_mismatch", "source_import_producer_mismatch",
]);

export function sourceImportFailureIsTerminal(error: APIError): boolean {
  // Network/5xx failures, busy claims, missing producer proof, and correctable
  // draft validation retain the exact verified preview for a direct retry.
  if (error.status >= 500) return false;
  if (error.status === 401 || error.status === 403) return true;
  if (error.code === "source_import_in_flight" || error.status === 428 ||
      error.code === "segment_mismatch" || error.code === "invalid_performer") return false;
  if (TERMINAL_SOURCE_IMPORT_CODES.has(error.code)) return true;
  const signal = [error.code, error.message, ...error.details].join(" ").toLowerCase();
  return /(?:token|grant|授权).*(?:expir|consum|过期|已消费)|(?:identity|producer).*(?:mismatch|changed|不匹配|变化)|source[_ -]?drift|来源(?:已|发生)?变化/.test(signal);
}

export function detailLabel(detail: string): string {
  const line = detail.match(/^lines\[(\d+)]/);
  const lineLabel = line ? `第 ${Number(line[1]) + 1} 行` : "歌词草稿";
  const segment = detail.match(/\.segments\[(\d+)]/);
  const segmentLabel = segment ? `第 ${Number(segment[1]) + 1} 分段` : "";
  if (detail.includes("translation credit is required") || detail.includes("attribution is required")) return "请填写翻译署名";
  if (detail.includes("requires japanese and zh-CN") || detail.includes("requires japanese, zh-CN")) return `${lineLabel}缺少日文或简中内容`;
  if (detail.includes("requires at least one performerId")) return `${lineLabel}${segmentLabel}未指定演唱者`;
  if (detail.includes("invalid performerId")) return `${lineLabel}${segmentLabel}包含无效演唱者`;
  if (detail.includes("duplicate performerId")) return `${lineLabel}${segmentLabel}包含重复演唱者`;
  if (detail.includes("japanese must equal concatenated segment text")) return `${lineLabel}的分段文字与日文原文不一致`;
  if (detail.includes("japanese must not be empty")) return `${lineLabel}的日文原文不能为空`;
  if (detail.includes("lyrics document exceeds") || detail.includes("exceeds the safe")) return "歌词内容超过安全大小限制";
  if (detail.includes("lines must contain")) return "歌词必须包含 1 至 5000 行";
  if (detail.includes("new source provenance requires")) return "外部来源必须通过固定 revision 预览导入";
  if (detail.includes("sourceUrl must be")) return "来源链接必须是无账号凭据的完整 HTTP(S) 地址";
  if (detail.includes("verified source preview expired")) return "固定 revision 预览已失效，请重新预览后导入";
  if (detail.includes("Game 投影") || detail.includes("availableVersions") || detail.includes("untagged_uncut_identity")) return detail;
  return "服务器拒绝了当前歌词内容，请检查对应字段";
}

export type PendingTransition =
  | { kind: "choose"; item: CatalogMusicItem }
  | { kind: "publish"; nextPublished: boolean }
  | { kind: "edition-switch"; editionKey: string }
  | { kind: "edition-command"; command: LyricsEditionCommand };

export type EditionWorkflow = {
  command: LyricsEditionCommand;
  editionKey: string;
  label: string;
};

export type PendingAnnotationOperation =
  | { kind: "segment-text"; lineIndex: number; segmentIndex: number; text: string }
  | { kind: "segment-split"; lineIndex: number; segmentIndex: number; splitOffset: number }
  | { kind: "ruby-edit"; lineIndex: number; segmentIndex: number; rubyIndex: number; patch: { text?: string; reading?: string } }
  | { kind: "ruby-split"; lineIndex: number; segmentIndex: number; rubyIndex: number; splitOffset: number }
  | { kind: "ruby-merge"; lineIndex: number; segmentIndex: number; rubyIndex: number };

const COLLABORATION_COLORS = ["#1677FF", "#16A085", "#C0392B", "#8E44AD", "#D97706", "#397D54"] as const;

export function collaborationColor(identity: string): string {
  let hash = 0;
  for (let index = 0; index < identity.length; index++) hash = ((hash * 31) + identity.charCodeAt(index)) >>> 0;
  return COLLABORATION_COLORS[hash % COLLABORATION_COLORS.length];
}

export function lyricsPublicationReadiness(
  lyrics: SongLyricsDocument | null,
  renditionDocument: RenditionLyricsDocument | null,
  legacyLyrics: SongLyrics | null,
  dirty: boolean,
) {
  const publicationTargets: Array<{ key: string; version: "full" | "game"; lines: LyricsEditorLine[] }> = renditionDocument
    ? renditionDocument.renditions.flatMap((rendition) => ([
        ...(rendition.full ? [{ key: rendition.key, version: "full" as const, lines: rendition.full.lines }] : []),
        ...(rendition.game ? [{ key: rendition.key, version: "game" as const, lines: rendition.game.lines }] : []),
      ]))
    : legacyLyrics ? [{ key: "legacy-v2", version: "full", lines: legacyLyrics.lines }] : [];
  const creditTargets: Array<{ key: string; complete: boolean }> = renditionDocument
    ? renditionDocument.renditions.map((rendition) => ({
        key: rendition.key,
        complete: Boolean(rendition.translationCredits?.translation?.trim()),
      }))
    : legacyLyrics ? [{ key: "legacy-v2", complete: Boolean(legacyLyrics.translationCredit?.trim() || legacyLyrics.attribution?.trim()) }] : [];
  const publicationProblems = lyrics ? [
    ...(publicationTargets.length > 0 ? [] : ["至少一个稳定 rendition side"]),
    ...creditTargets.filter((target) => !target.complete).map((target) => `${target.key} 的翻译署名`),
    ...publicationTargets.flatMap((target) => target.lines.flatMap((line, index) => {
      const missing: string[] = [];
      const prefix = `${target.key} ${target.version === "full" ? "Full" : "Game"} 第 ${index + 1} 行`;
      if (!line.japanese.trim()) missing.push(`${prefix}的日文原文`);
      if (!(line["zh-CN"] || "").trim()) missing.push(`${prefix}的中文翻译`);
      if (line.segments.map((segment) => segment.text).join("") !== line.japanese) missing.push(`${prefix}的分段文字未完整拼接为日文原文`);
      if (lyricsHasPerformerSegmentation(lyrics, target.key === "legacy-v2" ? undefined : target.key, target.version) &&
          line.segments.some((segment) => segment.performerIds.length === 0)) missing.push(`${prefix}的演唱者`);
      return missing;
    })),
  ] : [];
  const publicationLines = publicationTargets.flatMap((target) => target.lines);
  const publicationChecks = lyrics ? [
    { label: "已保存草稿", complete: lyrics.revision > 0 && !dirty },
    { label: `各 rendition 翻译署名 ${creditTargets.filter((target) => target.complete).length}/${creditTargets.length}`, complete: creditTargets.length > 0 && creditTargets.every((target) => target.complete) },
    { label: `各 side 中文翻译 ${publicationLines.filter((line) => (line["zh-CN"] || "").trim()).length}/${publicationLines.length}`, complete: publicationLines.length > 0 && publicationLines.every((line) => (line["zh-CN"] || "").trim()) },
    { label: `各 side 分段与日文一致 ${publicationLines.filter((line) => line.segments.map((segment) => segment.text).join("") === line.japanese).length}/${publicationLines.length}`, complete: publicationLines.length > 0 && publicationLines.every((line) => line.segments.map((segment) => segment.text).join("") === line.japanese) },
  ] : [];
  const publicationComplete = publicationChecks.length > 0 && publicationChecks.every((check) => check.complete);
  return { publicationProblems, publicationChecks, publicationComplete };
}
