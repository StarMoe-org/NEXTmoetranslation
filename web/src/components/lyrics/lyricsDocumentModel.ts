import type { LyricsEditionCommand } from "@/components/LyricsEditionMenu";
import type {
  APIError, CatalogMusicItem, LyricsDocumentIssue, LyricsDocumentWarning, LyricsEditorLine, LyricsRenditionSide,
  RenditionLyricsDocument, SongLyrics, SongLyricsDocument,
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

export type SourceV3PublicState = "served" | "served_uncredited" | "not_served" | "withdrawn";

/** Which credits each rendition of a source-v3 document carries. */
export function lyricsRenditionCredits(document: RenditionLyricsDocument): Array<{ key: string; translation: boolean; proofreading: boolean }> {
  return document.renditions.map((rendition) => ({
    key: rendition.key,
    translation: Boolean(rendition.translationCredits?.translation?.trim()),
    proofreading: Boolean(rendition.translationCredits?.proofreading?.trim()),
  }));
}

/**
 * Public-site state of a source-v3 song, which decides what a console save claims. The editor
 * document carries none of it: the catalog item's withdrawal flag and served runtime entry do.
 * The public rebuild serves an edited source-v3 song only when some rendition carries a
 * translation or proofreading credit, so a served song without one keeps its old public page.
 * Null for legacy documents, which publish explicitly.
 */
export function sourceV3PublicStateFor(document: SongLyricsDocument | null, item: CatalogMusicItem | null): SourceV3PublicState | null {
  if (document == null || !isRenditionLyricsDocument(document)) return null;
  if (item?.lyricsWithdrawn) return "withdrawn";
  if (item != null && item.runtimeLyrics == null) return "not_served";
  const credited = lyricsRenditionCredits(document as RenditionLyricsDocument)
    .some((credits) => credits.translation || credits.proofreading);
  return credited ? "served" : "served_uncredited";
}

export type SourceV3SaveWording = {
  button: string;
  continueLabel: string;
  saved: string;
  title: string;
  detail: string;
};

const SOURCE_V3_SAVE_WORDING: Record<SourceV3PublicState, SourceV3SaveWording> = {
  served: {
    button: "保存并公开",
    continueLabel: "保存并公开后继续",
    saved: "歌词已保存并公开",
    title: "source-v3 保存即公开",
    detail: "“保存并公开”会直接更新公开歌词；此页面不调用 legacy publish/unpublish",
  },
  served_uncredited: {
    button: "保存（未署名，暂不公开）",
    continueLabel: "保存后继续",
    saved: "歌词已保存；还没有署名，暂不公开",
    title: "还没有署名，保存后不会公开",
    detail: "公开文件只收录至少一个 rendition 带翻译或校对署名的歌曲；在歌词上方的“翻译”或“校对”栏填写署名之前，保存只更新数据库中的译文，公开页面保持原样；此页面不调用 legacy publish/unpublish",
  },
  withdrawn: {
    button: "保存（已撤下）",
    continueLabel: "保存后继续",
    saved: "已保存（当前已撤下，未公开）",
    title: "当前已撤下，未公开",
    detail: "“保存（已撤下）”只更新数据库中的译文，公开歌词保持撤下；此页面不调用 legacy publish/unpublish",
  },
  // The public rebuild serves an edited source-v3 song only when a rendition carries a
  // translation or proofreading credit, so this wording cannot promise publication.
  not_served: {
    button: "保存（尚未公开）",
    continueLabel: "保存后继续",
    saved: "歌词已保存（此前未公开）",
    title: "当前未公开",
    detail: "公开镜像目前不提供这首歌，也没有撤下记录。“保存（尚未公开）”会更新数据库中的译文；至少一个 rendition 带翻译或校对署名时，重建后的公开文件才会收录它，结果以目录中的“公开镜像”为准；此页面不调用 legacy publish/unpublish",
  },
};

/** Save wording for a source-v3 song; null for legacy documents, which keep their draft wording. */
export function sourceV3SaveWording(state: SourceV3PublicState | null): SourceV3SaveWording | null {
  return state ? SOURCE_V3_SAVE_WORDING[state] : null;
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
  saveable: boolean,
) {
  const publicationTargets: Array<{ key: string; version: "full" | "game"; lines: LyricsEditorLine[] }> = renditionDocument
    ? renditionDocument.renditions.flatMap((rendition) => ([
        ...(rendition.full ? [{ key: rendition.key, version: "full" as const, lines: rendition.full.lines }] : []),
        ...(rendition.game ? [{ key: rendition.key, version: "game" as const, lines: rendition.game.lines }] : []),
      ]))
    : legacyLyrics ? [{ key: "legacy-v2", version: "full", lines: legacyLyrics.lines }] : [];
  const creditTargets: Array<{ key: string; complete: boolean }> = renditionDocument
    ? lyricsRenditionCredits(renditionDocument).map((credits) => ({ key: credits.key, complete: credits.translation }))
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
    { label: "已保存草稿", complete: lyrics.revision > 0 && !saveable },
    { label: `各 rendition 翻译署名 ${creditTargets.filter((target) => target.complete).length}/${creditTargets.length}`, complete: creditTargets.length > 0 && creditTargets.every((target) => target.complete) },
    { label: `各 side 中文翻译 ${publicationLines.filter((line) => (line["zh-CN"] || "").trim()).length}/${publicationLines.length}`, complete: publicationLines.length > 0 && publicationLines.every((line) => (line["zh-CN"] || "").trim()) },
    { label: `各 side 分段与日文一致 ${publicationLines.filter((line) => line.segments.map((segment) => segment.text).join("") === line.japanese).length}/${publicationLines.length}`, complete: publicationLines.length > 0 && publicationLines.every((line) => line.segments.map((segment) => segment.text).join("") === line.japanese) },
  ] : [];
  const publicationComplete = publicationChecks.length > 0 && publicationChecks.every((check) => check.complete);
  return { publicationProblems, publicationChecks, publicationComplete };
}

// Whole-song document route: plain-language warnings, issues and takeover failures.

const WARNING_TEXT: Record<string, string> = {
  served_revision_differs: "公开页面显示的版本与数据库里的最新版本不同（公开文件可能还没重建，或新版本缺少署名而没有公开）；转换以公开页面为准，数据库里没有公开的译文会被替换",
  unpublished_draft_replaced: "数据库里有一份与公开页面不同的旧版草稿，转换会替换它",
  withdrawn_republished: "这首歌已撤下，按这份文档发布会重新公开它",
  credit_missing: "没有任何演唱版本带翻译或校对署名，服务器会拒绝发布；需要先补上署名",
  source_missing: "公开页面没有标注固定来源修订",
  source_unsupported: "公开页面标注的来源不能用于整曲文档",
  source_url_reencoded: "来源链接会按规范格式重新记录，指向的修订不变",
  source_differs: "各部分标注的来源不同，转换后整首歌只记录一个来源",
  rendition_inferred: "公开页面没有区分演唱版本，转换后按单一版本记录",
  game_projection_not_ordered: "Game 版没有按顺序对应 Full 版的行，转换后记为独立的 Game 歌词",
  game_projection_differs: "Game 版有行与对应的 Full 行不同，转换后记为独立的 Game 歌词",
  side_label_differs: "Game 版的名称无法保留，转换后与 Full 版同名",
  trailing_performers_dropped: "行尾演唱者无法保留",
  unknown_performer: "有演唱者不在角色目录中，转换时会去掉",
  ruby_not_served: "公开页面缺少注音，整曲文档要求每个汉字都有读音",
  ruby_not_expressible: "有注音不是“汉字 + 假名读音”的形式，无法按原样保留",
  english_dropped: "英文译文不会保留",
};

// Warnings whose published result reads the same on the public page, or which the server
// refuses outright; any other code, including one added later, may change what readers see.
const PUBLIC_PAGE_UNCHANGED_WARNINGS = new Set([
  "served_revision_differs", "unpublished_draft_replaced", "credit_missing", "source_missing", "source_unsupported",
  "source_url_reencoded", "game_projection_not_ordered", "game_projection_differs", "side_label_differs",
  "ruby_not_served", "ruby_not_expressible",
]);

/** Whether publishing the export changes what the public page shows where this warning points. */
export function lyricsDocumentWarningChangesPublicPage(warning: LyricsDocumentWarning): boolean {
  return !PUBLIC_PAGE_UNCHANGED_WARNINGS.has(warning.code);
}

function locationText(rendition?: string, side?: string, line?: number): string {
  return [
    rendition || "",
    side === "full" ? "Full" : side === "game" ? "Game" : side || "",
    line !== undefined ? `第 ${line + 1} 行` : "",
  ].filter(Boolean).join(" ");
}

/** A document-route warning in plain language, located by rendition, side and line. */
export function lyricsDocumentWarningText(warning: LyricsDocumentWarning): string {
  const location = locationText(warning.rendition, warning.side, warning.line);
  const text = WARNING_TEXT[warning.code] || warning.message;
  return location ? `${location}：${text}` : text;
}

const ISSUE_FIELDS: Record<string, string> = {
  ja: "日文", zh: "简中", en: "英文", segments: "分段", performerIds: "演唱者",
  translationCredit: "署名", translationCredits: "署名", proofreadingCredit: "校对署名",
  source: "来源", "source.url": "来源链接", game: "Game 版", gameLines: "Game 行", lines: "歌词行",
  translationEditions: "译本列表", zhEditions: "其他译本的简中", editionCredits: "其他译本的署名",
  "editionCredits.translation": "其他译本的翻译署名", "editionCredits.proofreading": "其他译本的校对署名",
};

/** A 422 issue, located in plain language; the server's message stays verbatim. */
export function lyricsDocumentIssueText(issue: LyricsDocumentIssue): string {
  const location = [
    locationText(issue.rendition, issue.side, issue.line),
    issue.edition ? `译本 ${issue.edition}` : "",
    issue.field ? ISSUE_FIELDS[issue.field] || issue.field : "",
  ].filter(Boolean).join(" · ");
  return location ? `${location}：${issue.message}` : issue.message;
}

/**
 * Why a recovery-ledger song cannot be converted yet and what to do first: takeover publishes what
 * the site serves, so a song the site does not serve has nothing to convert. Null when it is served.
 */
export function lyricsRecoveryTakeoverBlocked(state: SourceV3PublicState | null): { reason: string; next: string } | null {
  if (state === "withdrawn") {
    return {
      reason: "这首歌已撤下，公开站点上没有可转换的内容。",
      next: "请先由管理员恢复公开（本页没有这个操作，需调用发布接口 POST /api/editor/v1/lyrics/publish），之后就可以转换。",
    };
  }
  if (state === "not_served") {
    return {
      reason: "这首歌还没有公开，公开站点上没有可转换的内容。",
      next: "请先在本页填好简中译文和署名并保存；公开页面收录这首歌后就可以转换。",
    };
  }
  return null;
}

// apiFetch's refusal before sending a write while no producer proof is loaded; the console
// reloads the proof and lifts its write lock once the proof is valid again.
const PRODUCER_PROOF_NOT_LOADED = "内容版本尚未完成校对，请重试";

function conflictRevision(error: APIError): number | null {
  const revision = (error.current as { revision?: unknown } | undefined)?.revision;
  return Number.isSafeInteger(revision) ? revision as number : null;
}

/** What went wrong with a takeover and what the admin can do about it. */
export function lyricsRecoveryTakeoverFailure(error: APIError): { summary: string; lines: string[]; reload: boolean; retry: boolean } {
  const lines = [...error.issues.map(lyricsDocumentIssueText), ...error.details];
  if (error.code === "revision_conflict" || error.code === "expected_revision_required") {
    const current = conflictRevision(error);
    return {
      summary: `这首歌在打开对话框后有了新的修改${current != null ? `（当前 revision ${current}）` : ""}，没有转换。请重新载入后再试。`,
      lines: [], reload: true, retry: false,
    };
  }
  if (error.status === 404) return { summary: "公开站点目前不提供这首歌，没有可转换的公开内容。", lines: [], reload: false, retry: false };
  if (error.status === 403) return { summary: "只有管理员可以转换歌词文档。", lines: [], reload: false, retry: false };
  if (error.status === 409 && error.code === PRODUCER_PROOF_NOT_LOADED) {
    return { summary: "内容版本还没有完成校对，这次没有转换，也没有改动任何数据。校对完成后可以重试。", lines: [], reload: false, retry: true };
  }
  if (error.code === "producer_state_changed") {
    return { summary: "内容版本已变化，需要重新校对后再转换。", lines: error.details, reload: false, retry: false };
  }
  if (error.status === 422) {
    return { summary: "服务器拒绝了这次转换，公开页面和数据库都没有改变：", lines, reload: false, retry: false };
  }
  return { summary: `转换没有完成：${sourceLabel(error)}`, lines, reload: false, retry: error.status >= 500 };
}
