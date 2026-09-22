"use client";

import { forwardRef, useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState } from "react";
import { useToast } from "@/app/providers";
import { Modal } from "@/components/Modal";
import { LyricsEditionMenu, type LyricsEditionCommand } from "@/components/LyricsEditionMenu";
import { LyricsCatalogSidebar, runtimeLyricsStateLabel, runtimeLyricsVersionsLabel } from "@/components/lyrics/LyricsCatalogSidebar";
import { LyricsCollaborationBanner } from "@/components/lyrics/LyricsCollaborationBanner";
import { LyricsLineEditor } from "@/components/lyrics/LyricsLineEditor";
import { LyricsMetadataCard } from "@/components/lyrics/LyricsMetadataCard";
import { LyricsProjectionStatusCard } from "@/components/lyrics/LyricsProjectionStatusCard";
import { LyricsVocalCard } from "@/components/lyrics/VocalPlayer";
import { sameImportedLyricsFrozenIdentity } from "@/lib/lyrics-recovery.mjs";
import { buildLyricsLinesFromSourcePreview } from "@/lib/lyrics-source-import.mjs";
import { performerRepresentativeColor } from "@/lib/performer-colors.mjs";
import {
  isTranslationEditionLabel, selectTranslationEditionKey, translationEditionURLHint,
} from "@/lib/lyrics-editions.mjs";
import {
  isRenditionLyricsDocument, lyricsHasPerformerSegmentation, lyricsRenditionByKey, lyricsRenditionKeys,
  lyricsVersionSaveProblems, normalizedLyricsVersions, projectGameLyricsLines, referencedGameFullLineIds,
  renditionProjectionStatus, resolvedLyricsComponentProvenance, retainedLyricsTranslationTarget,
} from "@/lib/lyrics-versioning.mjs";
import {
  editableLyricSegments, lyricGraphemeMidpoint,
  mergeAdjacentLyricRubySpans, mergeAdjacentLyricSegments, replaceLyricRubySpan,
  replaceLyricSegmentText, splitLyricRubySpanAt, splitLyricSegmentAt,
} from "@/lib/lyrics-segmentation.mjs";
import {
  APIError, CatalogMusicItem, CatalogPerformerItem, LyricLine, LyricRubySpan, LyricsEditorLine, LyricsEditorSegment,
  LyricsPerformerID, LyricsRendition, LyricsRenditionPerformer, LyricsRenditionSide, LyricsSourceCandidate,
  LyricsSourcePreview, ProjectionStatus, RenditionLyricsDocument, SongLyrics, SongLyricsDocument,
  checkpointLyrics, getCatalogMusic, getCatalogPerformers, getClientID, getUsername,
  getLyrics, getProjectionStatus, issueLyricsCollabTicket, mutateLyricsTranslationEdition, previewLyricsSource, publishLyrics, saveLyrics,
  searchLyricsSource, subscribeSessionChanged, unpublishLyrics,
} from "@/lib/api";
import {
  LyricsCollaboration,
  type LyricsCollaborationPeer,
  type LyricsCollaborationStatus,
  canonicalLyricsJSON,
} from "@/lib/yjs-lyrics";

function databaseAvailabilityDescription(state: NonNullable<CatalogMusicItem["lyricsAvailabilityState"]>): string {
  if (state === "satisfied_no_lyrics") return "目录已审核为无需歌词，因此没有可编辑正文。";
  if (state === "incomplete") return "来源结果尚未形成可编辑正文，系统保持 fail-closed。";
  if (state === "ambiguous") return "来源仍有歧义，系统不会自动选择正文。";
  if (state === "missing") return "尚未找到可验证来源，系统不会生成空白正文冒充导入结果。";
  return "来源处理失败，系统保留数据库状态但不生成可编辑正文。";
}

function isLegacyLyricsDocument(document: SongLyricsDocument | null | undefined): document is SongLyrics {
  return Boolean(document) && !isRenditionLyricsDocument(document);
}

function preserveReadOnlyLyricsSourceFacts(saved: SongLyricsDocument, attempted: SongLyricsDocument): SongLyricsDocument {
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

function editableLyricsDocument(loaded: SongLyricsDocument): SongLyricsDocument {
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

function renderRubySpans(spans: LyricRubySpan[]) {
  return spans.map((span, index) => span.reading
    ? <ruby key={`${index}:${span.text}:${span.reading}`}>{span.text}<rt>{span.reading}</rt></ruby>
    : <span key={`${index}:${span.text}`}>{span.text}</span>);
}

function sourceLabel(error: APIError): string {
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

function sourceImportFailureIsTerminal(error: APIError): boolean {
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

function detailLabel(detail: string): string {
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

interface ResolvedLyricsComponentProvenanceRow {
  component: string;
  label: string;
  renditionKey: string;
  identity: {
    provider: string;
    revisionId: number;
    canonicalUrl: string;
    section: string;
  } | null;
}

export interface LyricsEditorHandle {
  save: () => Promise<boolean>;
  discard: () => boolean;
  isDirty: () => boolean;
  snapshot: () => { dirty: boolean; document: SongLyricsDocument | null; generation: number; editionKey: string };
  isEditing: (musicID: number) => boolean;
  activeTarget: () => {
    musicId: number;
    editionKey: string;
    renditionKey: string;
    side: "full" | "game";
    locale: "zh-CN";
    projectionKind: "full_only" | "game_only" | "exact_projection" | "independent_game" | "invalid";
  } | null;
  reloadCatalog: () => void;
  exportDraft: () => SongLyricsDocument | null;
  reloadAuthoritative: () => Promise<boolean>;
}

interface LyricsEditorProps {
  role: "admin" | "editor" | "";
  reloadGeneration: number;
  writeLocked?: boolean;
  onDirtyChange?: (dirty: boolean) => void;
}

type PendingTransition =
  | { kind: "choose"; item: CatalogMusicItem }
  | { kind: "publish"; nextPublished: boolean }
  | { kind: "edition-switch"; editionKey: string }
  | { kind: "edition-command"; command: LyricsEditionCommand };

type EditionWorkflow = {
  command: LyricsEditionCommand;
  editionKey: string;
  label: string;
};

type PendingAnnotationOperation =
  | { kind: "segment-text"; lineIndex: number; segmentIndex: number; text: string }
  | { kind: "segment-split"; lineIndex: number; segmentIndex: number; splitOffset: number }
  | { kind: "ruby-edit"; lineIndex: number; segmentIndex: number; rubyIndex: number; patch: { text?: string; reading?: string } }
  | { kind: "ruby-split"; lineIndex: number; segmentIndex: number; rubyIndex: number; splitOffset: number }
  | { kind: "ruby-merge"; lineIndex: number; segmentIndex: number; rubyIndex: number };

const COLLABORATION_COLORS = ["#1677FF", "#16A085", "#C0392B", "#8E44AD", "#D97706", "#397D54"] as const;

function collaborationColor(identity: string): string {
  let hash = 0;
  for (let index = 0; index < identity.length; index++) hash = ((hash * 31) + identity.charCodeAt(index)) >>> 0;
  return COLLABORATION_COLORS[hash % COLLABORATION_COLORS.length];
}

export const LyricsEditor = forwardRef<LyricsEditorHandle, LyricsEditorProps>(function LyricsEditor({ role, reloadGeneration, writeLocked: producerWriteLocked = false, onDirtyChange }, ref) {
  const { show } = useToast();
  const [query, setQuery] = useState("");
  const [catalog, setCatalog] = useState<CatalogMusicItem[]>([]);
  const [performers, setPerformers] = useState<CatalogPerformerItem[]>([]);
  const [selectedMusic, setSelectedMusic] = useState<CatalogMusicItem | null>(null);
  const [lyrics, setLyrics] = useState<SongLyricsDocument | null>(null);
  const [runtimeOnlyMissingDatabaseSource, setRuntimeOnlyMissingDatabaseSource] = useState(false);
  const [databaseAvailabilityOnly, setDatabaseAvailabilityOnly] = useState(false);
  const [baseline, setBaseline] = useState("");
  const [loading, setLoading] = useState(false);
  const [catalogLoading, setCatalogLoading] = useState(false);
  const [catalogError, setCatalogError] = useState(false);
  const [performerError, setPerformerError] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<APIError | null>(null);
  const [candidates, setCandidates] = useState<LyricsSourceCandidate[]>([]);
  const [sourceSearchCompleted, setSourceSearchCompleted] = useState(false);
  const [sourceActivity, setSourceActivity] = useState<"" | "searching" | "previewing">("");
  const [sourcePreviewCandidate, setSourcePreviewCandidate] = useState<LyricsSourceCandidate | null>(null);
  const [sourceRetry, setSourceRetry] = useState<{ kind: "search" } | { kind: "preview"; candidate: LyricsSourceCandidate } | null>(null);
  const [sourcePreview, setSourcePreview] = useState<LyricsSourcePreview | null>(null);
  // Keep the one-time import grant outside SongLyrics, baseline JSON, dirty checks,
  // draft exports, and any recoverable document state.
  const sourceImportTokenRef = useRef("");
  const collaborationRef = useRef<LyricsCollaboration | null>(null);
  const collaborationGenerationRef = useRef(0);
  const collaborationSyncedRef = useRef(false);
  const collaborationInitialBaselineRef = useRef<number | null>(null);
  const collaborationDocumentJSONRef = useRef("");
  const collaborationAuthoritativeRef = useRef<SongLyricsDocument | null>(null);
  const collaborationStructuralConflictRef = useRef(false);
  const [collaborationStatus, setCollaborationStatus] = useState<LyricsCollaborationStatus>("offline");
  const [collaborationPeers, setCollaborationPeers] = useState<LyricsCollaborationPeer[]>([]);
  const [collaborationError, setCollaborationError] = useState("");
  const [collaborationStructuralConflict, setCollaborationStructuralConflict] = useState(false);
  const [confirmSourceImport, setConfirmSourceImport] = useState(false);
  const [confirmImportRecovery, setConfirmImportRecovery] = useState<SongLyricsDocument | null>(null);
  const [confirmConflictReload, setConfirmConflictReload] = useState(false);
  const [activeTranslationEditionKey, setActiveTranslationEditionKey] = useState("");
  const [editionWorkflow, setEditionWorkflow] = useState<EditionWorkflow | null>(null);
  const [activeRenditionKey, setActiveRenditionKey] = useState("");
  const [activeVersion, setActiveVersion] = useState<"full" | "game">("full");
  const [previewLocale, setPreviewLocale] = useState<"ja-JP" | "zh-CN" | "en-US">("zh-CN");
  const requestSequence = useRef(0);
  const performerSequence = useRef(0);
  const lyricsLoadSequence = useRef(0);
  const selectedMusicIDRef = useRef<number | null>(null);
  const busyRef = useRef(false);
  const localSourceImportDraft = sourceImportTokenRef.current !== "";
  const collaborationRequired = selectedMusic != null && lyrics != null && !runtimeOnlyMissingDatabaseSource && !databaseAvailabilityOnly;
  const writeLocked = producerWriteLocked || collaborationStructuralConflict ||
    (collaborationRequired && !localSourceImportDraft && collaborationStatus !== "synced");
  const writeLockedRef = useRef(writeLocked);
  writeLockedRef.current = writeLocked;
  const documentGenerationRef = useRef(0);
  const appliedReloadGeneration = useRef(reloadGeneration);
  const [pendingTransition, setPendingTransition] = useState<PendingTransition | null>(null);
  const [pendingAnnotationOperation, setPendingAnnotationOperation] = useState<PendingAnnotationOperation | null>(null);
  const [projectionStatus, setProjectionStatus] = useState<ProjectionStatus | null>(null);
  const [projectionState, setProjectionState] = useState<"idle" | "checking" | "ready" | "failed" | "unknown">("idle");
  const [projectionMessage, setProjectionMessage] = useState("");
  const projectionSequence = useRef(0);
  const linesContainerRef = useRef<HTMLDivElement | null>(null);
  const segmentInputRefs = useRef<Record<string, HTMLInputElement | null>>({});
  const previewTabRefs = useRef<Record<"ja-JP" | "zh-CN" | "en-US", HTMLButtonElement | null>>({
    "ja-JP": null, "zh-CN": null, "en-US": null,
  });

  const dirty = lyrics != null && canonicalLyricsJSON(lyrics) !== baseline;
  const lyricsRef = useRef<SongLyricsDocument | null>(lyrics);
  const baselineRef = useRef(baseline);
  const activeTranslationEditionKeyRef = useRef(activeTranslationEditionKey);
  lyricsRef.current = lyrics;
  baselineRef.current = baseline;
  activeTranslationEditionKeyRef.current = activeTranslationEditionKey;
  const hasLocalUndo = localSourceImportDraft || collaborationRef.current?.undoManager.canUndo() === true;
  const renditionKeys = useMemo(() => (lyrics ? lyricsRenditionKeys(lyrics) : []), [lyrics]);
  const activeRendition = lyrics && isRenditionLyricsDocument(lyrics)
    ? lyricsRenditionByKey(lyrics, activeRenditionKey)
    : null;
  const legacyLyrics = lyrics && isLegacyLyricsDocument(lyrics) ? lyrics : null;
  const renditionDocument: RenditionLyricsDocument | null = lyrics && isRenditionLyricsDocument(lyrics)
    ? lyrics as RenditionLyricsDocument
    : null;
  const translationEditions = renditionDocument?.translationEditions || [];
  const activeTranslationEdition = translationEditions.find((edition) => edition.key === activeTranslationEditionKey)
    || translationEditions[0]
    || null;
  const availableVersions = useMemo(
    () => (lyrics ? normalizedLyricsVersions(lyrics, activeRenditionKey) : ["full"]),
    [activeRenditionKey, lyrics],
  );
  const hasGameVersion = availableVersions.includes("game") || (Boolean(legacyLyrics?.gameProjection));
  const projectionKind = lyrics ? renditionProjectionStatus(lyrics, activeRenditionKey) : "full_only";
  const activeSide = activeRendition?.[activeVersion];
  const activeLines: LyricsEditorLine[] = activeRendition
    ? (activeSide?.lines || [])
    : legacyLyrics?.lines || [];
  const activePerformerOptions: Array<CatalogPerformerItem | LyricsRenditionPerformer> = useMemo(() => {
    if (!activeRendition) return performers;
    const renditionIds = new Set(activeRendition.performers.map((p: LyricsRenditionPerformer) => String(p.performerId)));
    const catalogExtra = performers.filter((p: CatalogPerformerItem) => !renditionIds.has(String(p.performerId)));
    return [...activeRendition.performers, ...catalogExtra];
  }, [activeRendition, performers]);
  const gameSideReadOnlyReason = !activeRendition || activeRendition.relation.kind === "exact_projection"
    ? "exact_projection"
    : null;
  const activeSideReadOnly = activeVersion === "game" && gameSideReadOnlyReason !== null;
  const activeSourceFactsReadOnly = Boolean(activeRendition) || activeSideReadOnly;
  const activeSideSourceMutable = Boolean(lyrics && lyrics.revision === 0 && !activeSourceFactsReadOnly);
  const activeTranslationCredit = activeRendition
    ? activeRendition.translationCredits?.translation || ""
    : legacyLyrics?.translationCredit || "";
  const activeProofreadingCredit = activeRendition
    ? activeRendition.translationCredits?.proofreading || ""
    : legacyLyrics?.proofreadingCredit || "";
  const gameProjection: { ok: boolean; lines: LyricsEditorLine[]; lineIds: string[]; errors: string[] } = lyrics
    ? projectGameLyricsLines(lyrics, activeRenditionKey)
    : { ok: true, lines: [], lineIds: [], errors: [] };
  const versionSaveProblems = lyrics ? lyricsVersionSaveProblems(lyrics) : [];
  const componentProvenance = lyrics ? resolvedLyricsComponentProvenance(lyrics, activeRenditionKey) : [];
  const hasPerformerSegmentation = lyrics
    ? lyricsHasPerformerSegmentation(lyrics, activeRenditionKey, activeVersion)
    : false;

  useEffect(() => onDirtyChange?.(dirty), [dirty, onDirtyChange]);
  useEffect(() => {
    if (!renditionDocument) {
      if (activeTranslationEditionKey) setActiveTranslationEditionKey("");
      return;
    }
    const retained = selectTranslationEditionKey(
      activeTranslationEditionKey || renditionDocument.translationEditionKey,
      renditionDocument.defaultTranslationEditionKey,
      renditionDocument.translationEditions,
    );
    if (retained !== activeTranslationEditionKey) setActiveTranslationEditionKey(retained);
  }, [activeTranslationEditionKey, renditionDocument]);
  useEffect(() => {
    if (!lyrics || isLegacyLyricsDocument(lyrics)) {
      if (activeRenditionKey) setActiveRenditionKey("");
      return;
    }
    if (!renditionKeys.includes(activeRenditionKey)) {
      setActiveRenditionKey(renditionKeys[0] || "");
    }
  }, [activeRenditionKey, lyrics, renditionKeys]);
  useEffect(() => {
    if (availableVersions.includes(activeVersion)) return;
    setActiveVersion(availableVersions.includes("full") ? "full" : "game");
  }, [activeVersion, availableVersions]);

  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  const loadCatalog = useCallback(async (search: string) => {
    const sequence = ++requestSequence.current;
    setCatalogLoading(true);
    try {
      const result = await getCatalogMusic(search, false);
      if (requestSequence.current === sequence) {
        setCatalog(result.items);
        setCatalogError(false);
      }
    } catch (reason) {
      if (requestSequence.current === sequence) {
        setCatalogError(true);
        show(reason instanceof Error ? reason.message : "曲目目录加载失败", "err");
      }
    } finally {
      if (requestSequence.current === sequence) setCatalogLoading(false);
    }
  }, [show]);

  const loadPerformers = useCallback(async () => {
    const sequence = ++performerSequence.current;
    try {
      const result = await getCatalogPerformers();
      if (performerSequence.current === sequence) {
        setPerformers(result.items);
        setPerformerError(false);
      }
    } catch (reason) {
      if (performerSequence.current === sequence) {
        setPerformerError(true);
        show(reason instanceof Error ? reason.message : "演唱者目录加载失败", "err");
      }
    }
  }, [show]);

  const stopCollaboration = useCallback(() => {
    collaborationGenerationRef.current++;
    collaborationRef.current?.destroy();
    collaborationRef.current = null;
    collaborationSyncedRef.current = false;
    collaborationInitialBaselineRef.current = null;
    collaborationDocumentJSONRef.current = "";
    collaborationAuthoritativeRef.current = null;
    collaborationStructuralConflictRef.current = false;
    setCollaborationPeers([]);
    setCollaborationError("");
    setCollaborationStructuralConflict(false);
    setCollaborationStatus("offline");
  }, []);

  const startCollaboration = useCallback((musicID: number) => {
    const generation = ++collaborationGenerationRef.current;
    collaborationRef.current?.destroy();
    collaborationRef.current = null;
    collaborationSyncedRef.current = false;
    collaborationInitialBaselineRef.current = null;
    collaborationDocumentJSONRef.current = "";
    collaborationStructuralConflictRef.current = false;
    setCollaborationPeers([]);
    setCollaborationError("");
    setCollaborationStructuralConflict(false);
    setCollaborationStatus("connecting");
    const clientId = getClientID();
    const username = getUsername();
    const collaboration = new LyricsCollaboration({
      musicId: musicID,
      clientId,
      username,
      color: collaborationColor(`${username}:${clientId}`),
      issueTicket: issueLyricsCollabTicket,
      onSnapshot: (snapshot) => {
        if (collaborationGenerationRef.current !== generation || selectedMusicIDRef.current !== musicID) return;
        const structuralConflict = snapshot.error?.message === "invalid_lyrics_collaboration_document" ||
          (snapshot.synced && snapshot.document === null);
        if (structuralConflict) {
          if (collaborationStructuralConflictRef.current) return;
          collaborationStructuralConflictRef.current = true;
          // Fence first: destroy/provider callbacks from the retired instance
          // must not overwrite this terminal read-only conflict state.
          collaborationGenerationRef.current++;
          const conflicted = collaborationRef.current;
          collaborationRef.current = null;
          conflicted?.destroy();
          collaborationSyncedRef.current = false;
          setCollaborationStructuralConflict(true);
          setCollaborationStatus("error");
          setCollaborationPeers([]);
          setCollaborationError("");
          const authoritative = collaborationAuthoritativeRef.current;
          if (authoritative) {
            const editable = editableLyricsDocument(authoritative);
            const serialized = canonicalLyricsJSON(editable);
            collaborationInitialBaselineRef.current = editable.revision;
            collaborationDocumentJSONRef.current = serialized;
            documentGenerationRef.current++;
            setLyrics(editable);
            setBaseline(serialized);
          }
          setLoading(false);
          setError(null);
          return;
        }
        collaborationSyncedRef.current = snapshot.synced;
        setCollaborationStatus(snapshot.status);
        setCollaborationPeers(snapshot.peers);
        setCollaborationError(snapshot.error?.message || "");
        if (snapshot.status === "error" && !snapshot.document) {
          setLoading(false);
          setError(new APIError(503, { error: "lyrics_collaboration_unavailable" }));
        }
        if (!snapshot.synced || !snapshot.document) return;
        const editable = editableLyricsDocument(snapshot.document);
        const serialized = canonicalLyricsJSON(editable);
        if (collaborationInitialBaselineRef.current === null) {
          collaborationInitialBaselineRef.current = editable.revision;
          setBaseline(serialized);
        }
        if (collaborationDocumentJSONRef.current !== serialized) {
          collaborationDocumentJSONRef.current = serialized;
          documentGenerationRef.current++;
          setLyrics(editable);
        }
        setLoading(false);
        setError(null);
      },
    });
    collaborationRef.current = collaboration;
  }, []);

  useEffect(() => () => stopCollaboration(), [stopCollaboration]);
  useEffect(() => subscribeSessionChanged(() => collaborationRef.current?.reconnectNow()), []);

  useEffect(() => {
    const timer = window.setTimeout(() => { void loadCatalog(query); }, 200);
    return () => window.clearTimeout(timer);
  }, [loadCatalog, query]);

  useEffect(() => { void loadPerformers(); }, [loadPerformers]);

  const requestIsCurrent = useCallback((sequence: number, musicID: number) =>
    lyricsLoadSequence.current === sequence && selectedMusicIDRef.current === musicID, []);

  const replaceEditionURL = useCallback((editionKey: string) => {
    if (typeof window === "undefined") return;
    const url = new URL(window.location.href);
    if (editionKey) url.searchParams.set("edition", editionKey);
    else url.searchParams.delete("edition");
    window.history.replaceState(window.history.state, "", `${url.pathname}${url.search}${url.hash}`);
  }, []);

  const acceptAuthoritativeDocument = useCallback((
    loaded: SongLyricsDocument,
    preferredRenditionKey = "",
    preferredVersion: "full" | "game" = "full",
  ) => {
    const editable = editableLyricsDocument(loaded);
    const retainedTarget = retainedLyricsTranslationTarget(editable, preferredRenditionKey, preferredVersion);
    const editableEditionDocument = isRenditionLyricsDocument(editable) ? editable as RenditionLyricsDocument : null;
    const editionKey = editableEditionDocument
      ? selectTranslationEditionKey(
          editableEditionDocument.translationEditionKey,
          editableEditionDocument.defaultTranslationEditionKey,
          editableEditionDocument.translationEditions,
        )
      : "";
    documentGenerationRef.current++;
    setLyrics(editable);
    setBaseline(canonicalLyricsJSON(editable));
    setActiveTranslationEditionKey(editionKey);
    setActiveRenditionKey(retainedTarget.renditionKey);
    setActiveVersion(retainedTarget.version === "game" ? "game" : "full");
    replaceEditionURL(editionKey);
    return editable;
  }, [replaceEditionURL]);

  const performChooseMusic = useCallback(async (item: CatalogMusicItem) => {
    if (busyRef.current) return false;
    stopCollaboration();
    const preserveTranslationTarget = selectedMusicIDRef.current === item.musicId;
    const currentDocument = lyricsRef.current;
    const currentEditionDocument = currentDocument && isRenditionLyricsDocument(currentDocument)
      ? currentDocument as RenditionLyricsDocument
      : null;
    const preferredEditionKey = preserveTranslationTarget && currentEditionDocument
      ? activeTranslationEditionKeyRef.current || currentEditionDocument.translationEditionKey
      : typeof window !== "undefined" ? translationEditionURLHint(window.location.search) : "";
    const preferredRenditionKey = preserveTranslationTarget ? activeRenditionKey : "";
    const preferredVersion = preserveTranslationTarget ? activeVersion : "full";
    const sequence = ++lyricsLoadSequence.current;
    selectedMusicIDRef.current = item.musicId;
    setSelectedMusic(item);
    setLoading(true);
    setRuntimeOnlyMissingDatabaseSource(false);
    setDatabaseAvailabilityOnly(false);
    setError(null);
    setCandidates([]);
    setSourceSearchCompleted(false);
    setSourceActivity("");
    setSourcePreviewCandidate(null);
    setSourceRetry(null);
    setSourcePreview(null);
    sourceImportTokenRef.current = "";
    setConfirmSourceImport(false);
    setConfirmImportRecovery(null);
    setConfirmConflictReload(false);
    setEditionWorkflow(null);
    setPendingAnnotationOperation(null);
    setActiveTranslationEditionKey("");
    setActiveRenditionKey("");
    setActiveVersion("full");
    projectionSequence.current++;
    setProjectionStatus(null);
    setProjectionState("idle");
    setProjectionMessage("");
    documentGenerationRef.current++;
    setLyrics(null);
    setBaseline("");
    let loadedSuccessfully = false;
    let waitingForCollaboration = false;
    try {
      let loaded: SongLyricsDocument;
      try {
        loaded = await getLyrics(item.musicId, preferredEditionKey || undefined);
      } catch (reason) {
        if (!(preferredEditionKey && reason instanceof APIError && reason.status === 404)) throw reason;
        loaded = await getLyrics(item.musicId);
      }
      if (!requestIsCurrent(sequence, item.musicId)) return false;
      acceptAuthoritativeDocument(loaded, preferredRenditionKey, preferredVersion);
      const authoritative = editableLyricsDocument(loaded);
      collaborationAuthoritativeRef.current = authoritative;
      startCollaboration(item.musicId);
      loadedSuccessfully = true;
      loadedSuccessfully = true;
    } catch (reason) {
      if (!requestIsCurrent(sequence, item.musicId)) return false;
      if (reason instanceof APIError && reason.status === 404 && item.lyricsAvailabilityState) {
        documentGenerationRef.current++;
        setLyrics(null);
        setBaseline("");
        setDatabaseAvailabilityOnly(true);
        loadedSuccessfully = true;
      } else if (reason instanceof APIError && reason.status === 404 && item.runtimeLyrics?.immutableOverlay) {
        // The embedded Public Lyrics release is a read-only runtime overlay, not
        // an editable SQLite revision. Never turn its missing DB detail into a
        // saveable blank draft; the controlled embedded seed import owns that bridge.
        documentGenerationRef.current++;
        setLyrics(null);
        setBaseline("");
        setRuntimeOnlyMissingDatabaseSource(true);
        loadedSuccessfully = true;
      } else if (reason instanceof APIError && reason.status === 404) {
        startCollaboration(item.musicId);
        waitingForCollaboration = true;
        loadedSuccessfully = true;
      } else {
        setLyrics(null);
        setError(reason instanceof APIError ? reason : new APIError(500, { error: "load_failed" }));
      }
    } finally {
      if (requestIsCurrent(sequence, item.musicId) && !waitingForCollaboration) setLoading(false);
    }
    return loadedSuccessfully;
  }, [acceptAuthoritativeDocument, activeRenditionKey, activeVersion, requestIsCurrent, startCollaboration, stopCollaboration]);

  useEffect(() => {
    if (busyRef.current) return;
    if (appliedReloadGeneration.current === reloadGeneration) return;
    appliedReloadGeneration.current = reloadGeneration;
    lyricsLoadSequence.current++;
    requestSequence.current++;
    performerSequence.current++;
    void loadCatalog(query);
    void loadPerformers();
    if (selectedMusic) void performChooseMusic(selectedMusic);
  }, [busy, loadCatalog, loadPerformers, performChooseMusic, query, reloadGeneration, selectedMusic]);

  const chooseMusic = (item: CatalogMusicItem) => {
    if (busyRef.current) return;
    if (item.musicId === selectedMusic?.musicId) return;
    if (dirty) {
      setPendingTransition({ kind: "choose", item });
      return;
    }
    void performChooseMusic(item);
  };

  const loadTranslationEdition = async (requestedEditionKey: string): Promise<boolean> => {
    const current = lyricsRef.current;
    if (!current || !isRenditionLyricsDocument(current) || busyRef.current || writeLockedRef.current) return false;
    const currentEditionDocument = current as RenditionLyricsDocument;
    const editionKey = selectTranslationEditionKey(
      requestedEditionKey,
      currentEditionDocument.defaultTranslationEditionKey,
      currentEditionDocument.translationEditions,
    );
    if (!editionKey) return false;
    if (editionKey === activeTranslationEditionKeyRef.current && currentEditionDocument.translationEditionKey === editionKey) return true;
    const sequence = lyricsLoadSequence.current;
    const musicID = current.musicId;
    const documentGeneration = documentGenerationRef.current;
    const preferredRenditionKey = activeRenditionKey;
    const preferredVersion = activeVersion;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    try {
      const loaded = await getLyrics(musicID, editionKey);
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return false;
      if (!isRenditionLyricsDocument(loaded)) {
        setError(new APIError(502, { error: "invalid_translation_edition", details: ["译本切换响应必须是 source-v3 文档"] }));
        return false;
      }
      const loadedEditionDocument = loaded as RenditionLyricsDocument;
      acceptAuthoritativeDocument(loadedEditionDocument, preferredRenditionKey, preferredVersion);
      sourceImportTokenRef.current = "";
      setPendingTransition(null);
      show(`已切换到译本 ${loadedEditionDocument.translationEditionKey}`, "ok");
      return true;
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return false;
      setError(reason instanceof APIError ? reason : new APIError(500, { error: "load_failed" }));
      return false;
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const openEditionWorkflow = (command: LyricsEditionCommand): boolean => {
    const current = lyricsRef.current;
    if (!current || !isRenditionLyricsDocument(current) || busyRef.current || writeLockedRef.current) return false;
    const currentEditionDocument = current as RenditionLyricsDocument;
    const active = currentEditionDocument.translationEditions.find((edition) => edition.key === activeTranslationEditionKeyRef.current)
      || currentEditionDocument.translationEditions[0];
    if (!active) return false;
    if (command === "set-default" && active.key === currentEditionDocument.defaultTranslationEditionKey) return false;
    const editionKey = command === "create" || command === "clone"
      ? `ed-${crypto.randomUUID()}`
      : active.key;
    const label = command === "create" ? "新译本" : command === "clone" ? `${active.label} 副本` : active.label;
    setError(null);
    setEditionWorkflow({ command, editionKey, label });
    return true;
  };

  const requestEditionSwitch = (editionKey: string) => {
    if (busyRef.current || writeLockedRef.current || editionKey === activeTranslationEditionKeyRef.current) return;
    if (dirty) {
      setPendingTransition({ kind: "edition-switch", editionKey });
      return;
    }
    void loadTranslationEdition(editionKey);
  };

  const requestEditionCommand = (command: LyricsEditionCommand) => {
    if (busyRef.current || writeLockedRef.current) return;
    if (dirty) {
      setPendingTransition({ kind: "edition-command", command });
      return;
    }
    openEditionWorkflow(command);
  };

  const updateLyrics = (patch: Partial<SongLyrics> | Partial<RenditionLyricsDocument>) => {
    if (!lyrics || busyRef.current || writeLocked) return;
    const next = { ...lyrics, ...patch } as SongLyricsDocument;
    documentGenerationRef.current++;
    if (!localSourceImportDraft && collaborationRef.current?.updateDocument(next)) {
      collaborationDocumentJSONRef.current = canonicalLyricsJSON(next);
    } else {
      setLyrics(next);
    }
    setError(null);
  };

  const updateActiveRendition = (patch: Partial<LyricsRendition>) => {
    if (!lyrics || isLegacyLyricsDocument(lyrics) || !activeRendition || busyRef.current || writeLocked) return;
    const renditionDocument = lyrics as RenditionLyricsDocument;
    updateLyrics({
      renditions: renditionDocument.renditions.map((rendition) => rendition.key === activeRendition.key
        ? { ...rendition, ...patch }
        : rendition),
    });
  };

  const updateActiveCredits = (field: "translation" | "proofreading", value: string) => {
    if (!lyrics) return;
    if (isLegacyLyricsDocument(lyrics)) {
      updateLyrics(field === "translation" ? { translationCredit: value } : { proofreadingCredit: value });
      return;
    }
    if (!activeRendition) return;
    const next = { ...(activeRendition.translationCredits || {}) };
    if (value) next[field] = value;
    else delete next[field];
    updateActiveRendition({ translationCredits: Object.keys(next).length > 0 ? next : undefined });
  };

  const replaceActiveLines = (lines: LyricsEditorLine[]) => {
    if (!lyrics) return;
    const ordered = lines.map((line, order) => ({ ...line, order })) as LyricsEditorLine[];
    if (isLegacyLyricsDocument(lyrics)) {
      updateLyrics({ lines: ordered as LyricLine[] });
      return;
    }
    if (!activeRendition || !activeSide) return;
    const patch: Partial<LyricsRendition> = {
      [activeVersion]: { ...activeSide, lines: ordered },
    };
    if (activeVersion === "full" && activeRendition.relation.kind === "exact_projection" && activeRendition.game) {
      const fullMap = new Map(ordered.map((line) => [line.id, line]));
      const nextGameLines: LyricsEditorLine[] = activeRendition.game.lines.map((gameLine: LyricsEditorLine) => {
        const fullLine = fullMap.get(gameLine.id);
        if (!fullLine) return gameLine;
        return {
          ...gameLine,
          segments: fullLine.segments.map((seg) => ({
            ...seg,
            performerIds: seg.performerIds ? [...seg.performerIds].map(String) : [],
            ruby: seg.ruby.map((r) => ({ ...r })),
          })),
          trailingPerformerIds: fullLine.trailingPerformerIds ? [...fullLine.trailingPerformerIds].map(String) : [],
          stanzaBreakBefore: fullLine.stanzaBreakBefore,
          "zh-CN": fullLine["zh-CN"],
          "en-US": fullLine["en-US"],
        };
      });
      patch.game = { ...activeRendition.game, lines: nextGameLines };
    }
    const usedPerformerIds = new Set<string>();
    for (const line of ordered) {
      for (const id of (line.trailingPerformerIds || []) as LyricsPerformerID[]) usedPerformerIds.add(String(id));
      for (const seg of line.segments || []) {
        for (const id of (seg.performerIds || []) as LyricsPerformerID[]) usedPerformerIds.add(String(id));
      }
    }
    const currentRenditionIds = new Set(activeRendition.performers.map((p: LyricsRenditionPerformer) => String(p.performerId)));
    const missing = Array.from(usedPerformerIds).filter((id) => !currentRenditionIds.has(id));
    if (missing.length > 0) {
      const added: LyricsRenditionPerformer[] = missing.map((id) => {
        const catalogPerformer = performers.find((p: CatalogPerformerItem) => String(p.performerId) === id);
        const nameStr = typeof catalogPerformer?.name === "string"
          ? catalogPerformer.name
          : (catalogPerformer?.name?.["zh-CN"] || catalogPerformer?.name?.["ja-JP"] || id);
        return {
          performerId: id,
          name: nameStr,
          color: performerRepresentativeColor(id, activeRendition?.version.label),
        };
      });
      patch.performers = [...activeRendition.performers, ...added];
    }
    updateActiveRendition(patch);
  };

  const updateLine = (index: number, patch: Partial<LyricsEditorLine>) => {
    if (!lyrics || activeSideReadOnly) return;
    replaceActiveLines(activeLines.map((line, lineIndex) => {
      if (lineIndex !== index) return line;
      const updated = { ...line };
      if (patch["zh-CN"] !== undefined) updated["zh-CN"] = patch["zh-CN"];
      if (patch["en-US"] !== undefined) updated["en-US"] = patch["en-US"];
      if (patch.stanzaBreakBefore !== undefined) updated.stanzaBreakBefore = patch.stanzaBreakBefore;
      if (patch.trailingPerformerIds !== undefined) updated.trailingPerformerIds = patch.trailingPerformerIds;
      if (patch.segments !== undefined) updated.segments = patch.segments;
      return updated;
    }) as LyricsEditorLine[]);
  };

  const setSegments = (lineIndex: number, segments: LyricsEditorSegment[], sourceMayChange = false) => {
    const patch: Partial<LyricsEditorLine> = { segments } as Partial<LyricsEditorLine>;
    if (sourceMayChange) patch.japanese = segments.map((segment) => segment.text).join("");
    updateLine(lineIndex, patch);
  };

  const updateSegmentText = (lineIndex: number, segmentIndex: number, text: string, confirmed = false) => {
    if (!lyrics || activeSideReadOnly) return;
    const line = activeLines[lineIndex];
    const result = replaceLyricSegmentText(line.segments[segmentIndex], text, confirmed);
    if ("reason" in result) {
      setPendingAnnotationOperation({ kind: "segment-text", lineIndex, segmentIndex, text });
      return;
    }
    const segments = line.segments.map((segment, index) => index === segmentIndex ? result.segment : segment);
    setSegments(lineIndex, segments, lyrics.revision === 0);
  };

  const updateSegment = (lineIndex: number, segmentIndex: number, text: string, performerIds?: LyricsPerformerID[]) => {
    if (!lyrics || activeSideReadOnly) return;
    if (performerIds !== undefined) {
      const segments = activeLines[lineIndex].segments.map((segment, index) =>
        index === segmentIndex ? { ...segment, performerIds } : segment) as LyricsEditorSegment[];
      setSegments(lineIndex, segments);
      return;
    }
    updateSegmentText(lineIndex, segmentIndex, text);
  };

  const addSegment = (lineIndex: number, after: number) => {
    if (!lyrics || activeSideReadOnly) return;
    const segments = [...activeLines[lineIndex].segments] as LyricsEditorSegment[];
    segments.splice(after + 1, 0, { text: "", performerIds: [], ruby: [] });
    setSegments(lineIndex, segments);
  };

  const focusSegment = (lineIndex: number, segmentIndex: number, selection: "start" | "end") => {
    window.requestAnimationFrame(() => {
      const target = segmentInputRefs.current[`${lineIndex}-${segmentIndex}`];
      if (!target) return;
      target.focus();
      const offset = selection === "start" ? 0 : target.value.length;
      target.setSelectionRange(offset, offset);
    });
  };

  const applySegmentSplit = (lineIndex: number, segmentIndex: number, splitOffset: number, confirmed = false) => {
    if (!lyrics || activeSideReadOnly) return;
    const result = splitLyricSegmentAt(activeLines[lineIndex].segments, segmentIndex, splitOffset, confirmed);
    if (!result) {
      segmentInputRefs.current[`${lineIndex}-${segmentIndex}`]?.focus();
      return;
    }
    if ("reason" in result) {
      setPendingAnnotationOperation({ kind: "segment-split", lineIndex, segmentIndex, splitOffset });
      return;
    }
    setSegments(lineIndex, result.segments);
    focusSegment(lineIndex, segmentIndex + 1, "start");
  };

  const splitSegment = (lineIndex: number, segmentIndex: number) => {
    if (!lyrics) return;
    const input = segmentInputRefs.current[`${lineIndex}-${segmentIndex}`];
    const selectionStart = input?.selectionStart;
    const selectionEnd = input?.selectionEnd;
    if (selectionStart == null || selectionEnd == null || selectionStart !== selectionEnd) {
      input?.focus();
      return;
    }
    applySegmentSplit(lineIndex, segmentIndex, selectionStart);
  };

  const mergeWithPreviousSegment = (lineIndex: number, segmentIndex: number) => {
    if (!lyrics || activeSideReadOnly || segmentIndex <= 0) return;
    const segments = activeLines[lineIndex].segments;
    const nextSegments = mergeAdjacentLyricSegments(segments, segmentIndex - 1);
    if (!nextSegments) return;
    const previousLength = segments[segmentIndex - 1].text.length;
    setSegments(lineIndex, nextSegments);
    window.requestAnimationFrame(() => {
      const target = segmentInputRefs.current[`${lineIndex}-${segmentIndex - 1}`];
      target?.focus();
      target?.setSelectionRange(previousLength, previousLength);
    });
  };

  const applyRubyEdit = (lineIndex: number, segmentIndex: number, rubyIndex: number,
    patch: { text?: string; reading?: string }, confirmed = false) => {
    if (!lyrics || activeSideReadOnly) return;
    const result = replaceLyricRubySpan(activeLines[lineIndex].segments, segmentIndex, rubyIndex, patch, confirmed);
    if (!result) return;
    if ("reason" in result) {
      setPendingAnnotationOperation({ kind: "ruby-edit", lineIndex, segmentIndex, rubyIndex, patch });
      return;
    }
    setSegments(lineIndex, result.segments, lyrics.revision === 0);
  };

  const updateRubySpan = (lineIndex: number, segmentIndex: number, rubyIndex: number, patch: { text?: string; reading?: string }) => {
    applyRubyEdit(lineIndex, segmentIndex, rubyIndex, patch);
  };

  const applyRubySplit = (lineIndex: number, segmentIndex: number, rubyIndex: number, splitOffset: number, confirmed = false) => {
    if (!lyrics || activeSideReadOnly) return;
    const result = splitLyricRubySpanAt(activeLines[lineIndex].segments, segmentIndex, rubyIndex, splitOffset, confirmed);
    if (!result) return;
    if ("reason" in result) {
      setPendingAnnotationOperation({ kind: "ruby-split", lineIndex, segmentIndex, rubyIndex, splitOffset });
      return;
    }
    setSegments(lineIndex, result.segments);
  };

  const splitRubySpan = (lineIndex: number, segmentIndex: number, rubyIndex: number) => {
    if (!lyrics || activeSideReadOnly) return;
    const span = activeLines[lineIndex].segments[segmentIndex].ruby[rubyIndex];
    const splitOffset = lyricGraphemeMidpoint(span.text);
    if (splitOffset == null) return;
    applyRubySplit(lineIndex, segmentIndex, rubyIndex, splitOffset);
  };

  const applyRubyMerge = (lineIndex: number, segmentIndex: number, rubyIndex: number, confirmed = false) => {
    if (!lyrics || activeSideReadOnly || rubyIndex <= 0) return;
    const result = mergeAdjacentLyricRubySpans(activeLines[lineIndex].segments, segmentIndex, rubyIndex - 1, confirmed);
    if (!result) return;
    if ("reason" in result) {
      setPendingAnnotationOperation({ kind: "ruby-merge", lineIndex, segmentIndex, rubyIndex });
      return;
    }
    setSegments(lineIndex, result.segments);
  };

  const mergeRubyWithPrevious = (lineIndex: number, segmentIndex: number, rubyIndex: number) => {
    applyRubyMerge(lineIndex, segmentIndex, rubyIndex);
  };

  const confirmAnnotationOperation = () => {
    const operation = pendingAnnotationOperation;
    if (!operation) return;
    setPendingAnnotationOperation(null);
    switch (operation.kind) {
      case "segment-text":
        updateSegmentText(operation.lineIndex, operation.segmentIndex, operation.text, true);
        break;
      case "segment-split":
        applySegmentSplit(operation.lineIndex, operation.segmentIndex, operation.splitOffset, true);
        break;
      case "ruby-edit":
        applyRubyEdit(operation.lineIndex, operation.segmentIndex, operation.rubyIndex, operation.patch, true);
        break;
      case "ruby-split":
        applyRubySplit(operation.lineIndex, operation.segmentIndex, operation.rubyIndex, operation.splitOffset, true);
        break;
      case "ruby-merge":
        applyRubyMerge(operation.lineIndex, operation.segmentIndex, operation.rubyIndex, true);
        break;
    }
  };

  const removeSegment = (lineIndex: number, segmentIndex: number) => {
    if (!lyrics || activeSideReadOnly) return;
    const segments = activeLines[lineIndex].segments.map((segment) => ({ ...segment, performerIds: [...segment.performerIds] })) as LyricsEditorSegment[];
    if (segments.length <= 1) return;
    const [removed] = segments.splice(segmentIndex, 1);
    const mergeIndex = segmentIndex > 0 ? segmentIndex - 1 : 0;
    const mergedPerformers = Array.from(new Set([...segments[mergeIndex].performerIds, ...removed.performerIds])) as LyricsEditorSegment["performerIds"];
    segments[mergeIndex].performerIds = mergedPerformers;
    if (segmentIndex > 0) {
      segments[mergeIndex].text += removed.text;
      segments[mergeIndex].ruby = [...segments[mergeIndex].ruby, ...removed.ruby];
    } else {
      segments[mergeIndex].text = removed.text + segments[mergeIndex].text;
      segments[mergeIndex].ruby = [...removed.ruby, ...segments[mergeIndex].ruby];
    }
    setSegments(lineIndex, segments);
    focusSegment(lineIndex, mergeIndex, segmentIndex > 0 ? "end" : "start");
  };

  const moveSegment = (lineIndex: number, segmentIndex: number, direction: -1 | 1) => {
    if (!lyrics || lyrics.revision > 0 || activeSideReadOnly) return;
    const target = segmentIndex + direction;
    const segments = [...activeLines[lineIndex].segments] as LyricsEditorSegment[];
    if (target < 0 || target >= segments.length) return;
    [segments[segmentIndex], segments[target]] = [segments[target], segments[segmentIndex]];
    setSegments(lineIndex, segments, true);
  };

  const addLine = () => {
    if (!lyrics || lyrics.revision > 0 || activeSideReadOnly) return;
    const order = activeLines.length;
    const line: LyricsEditorLine = {
      id: `manual-${lyrics.musicId}-${activeRenditionKey || "legacy"}-${activeVersion}-${Date.now()}-${order}`,
      order, japanese: "", "zh-CN": "", "en-US": "", segments: [{ text: "", performerIds: [], ruby: [] }],
      ...(activeRendition ? { trailingPerformerIds: [] } : {}),
    } as LyricsEditorLine;
    replaceActiveLines([...activeLines, line]);
  };

  const removeLine = (lineIndex: number) => {
    if (!lyrics || lyrics.revision > 0 || activeSideReadOnly || activeLines.length <= 1) return;
    const removedLineID = activeLines[lineIndex]?.id;
    const focusLineIndex = Math.max(0, Math.min(lineIndex, activeLines.length - 2));
    replaceActiveLines(activeLines.filter((_, index) => index !== lineIndex));
    if (removedLineID && referencedGameFullLineIds(lyrics, activeRenditionKey).includes(removedLineID)) {
      show(`Full 行 ${removedLineID} 仍被 Game 投影引用；恢复该行后才能保存`, "err");
    }
    window.requestAnimationFrame(() => {
      const target = linesContainerRef.current?.querySelector<HTMLElement>(`[data-line-index="${focusLineIndex}"] textarea`)
        || linesContainerRef.current?.querySelector<HTMLElement>(".lyrics-add-line");
      target?.focus();
    });
  };

  const moveLine = (lineIndex: number, direction: -1 | 1) => {
    if (!lyrics || lyrics.revision > 0 || activeSideReadOnly) return;
    const target = lineIndex + direction;
    if (target < 0 || target >= activeLines.length) return;
    const lines = [...activeLines];
    [lines[lineIndex], lines[target]] = [lines[target], lines[lineIndex]];
    replaceActiveLines(lines);
  };

  const saveDocument = async (): Promise<SongLyricsDocument | null> => {
    if (!lyrics || busyRef.current || writeLockedRef.current) return null;
    const preflightProblems = lyricsVersionSaveProblems(lyrics);
    if (preflightProblems.length > 0) {
      setError(new APIError(422, { error: "invalid_game_projection", details: preflightProblems }));
      show("Rendition / projection 或公开署名合同无效，未发送保存请求", "err");
      return null;
    }
    const attempted = lyrics;
    const sequence = lyricsLoadSequence.current;
    const musicID = lyrics.musicId;
    const documentGeneration = documentGenerationRef.current;
    const importToken = lyrics.revision === 0 ? sourceImportTokenRef.current : "";
    const collaboration = collaborationRef.current;
    const checkpointBoundary = importToken ? 0 : collaboration?.beginCheckpoint() ?? 0;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    setConfirmImportRecovery(null);
    try {
      const saved = importToken
        ? await saveLyrics(lyrics, importToken)
        : await checkpointLyrics(musicID);
      if (!requestIsCurrent(sequence, musicID)) return null;
      if (importToken && documentGenerationRef.current !== documentGeneration) return null;
      const persisted = preserveReadOnlyLyricsSourceFacts(saved, attempted);
      collaborationAuthoritativeRef.current = editableLyricsDocument(persisted);
      if (importToken) {
        documentGenerationRef.current++;
        setLyrics(persisted);
      } else {
        collaboration?.checkpointCommitted(checkpointBoundary);
        collaboration?.updateAuthoritativeEnvelope(persisted);
      }
      setBaseline(canonicalLyricsJSON(persisted));
      if (isRenditionLyricsDocument(persisted)) {
        const persistedEditionDocument = persisted as RenditionLyricsDocument;
        setActiveTranslationEditionKey(persistedEditionDocument.translationEditionKey);
        replaceEditionURL(persistedEditionDocument.translationEditionKey);
      }
      sourceImportTokenRef.current = "";
      setSourcePreview(null);
      setSourcePreviewCandidate(null);
      setSourceRetry(null);
      setConfirmSourceImport(false);
      setConfirmImportRecovery(null);
      setCandidates([]);
      setSourceSearchCompleted(false);
      void loadCatalog(query);
      if (importToken) startCollaboration(musicID);
      show("歌词草稿已保存", "ok");
      return persisted;
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID)) return null;
      if (importToken && documentGenerationRef.current !== documentGeneration) return null;
      const apiError = reason instanceof APIError ? reason : new APIError(500, { error: "save_failed" });
      if (importToken && apiError.code !== "invalid_lyrics_response") {
        try {
          const authoritative = await getLyrics(musicID);
          if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return null;
          if (isLegacyLyricsDocument(attempted) && isLegacyLyricsDocument(authoritative) &&
              sameImportedLyricsFrozenIdentity(attempted, authoritative)) {
            sourceImportTokenRef.current = "";
            setSourcePreview(null);
            setSourcePreviewCandidate(null);
            setSourceRetry(null);
            setConfirmSourceImport(false);
            setConfirmImportRecovery(authoritative);
            setError(null);
            show("首次保存可能已成功；已找到相同固定来源的服务器版本，请确认载入", "err");
            return null;
          }
        } catch {
          // Reconciliation is best-effort. Preserve the original save failure and
          // its retry semantics when the authoritative read is unavailable.
        }
      }
      const terminalImportFailure = Boolean(importToken) && sourceImportFailureIsTerminal(apiError);
      if (terminalImportFailure) {
        sourceImportTokenRef.current = "";
        setSourcePreview(null);
        setSourceRetry(sourcePreviewCandidate ? { kind: "preview", candidate: sourcePreviewCandidate } : { kind: "search" });
      }
      setError(apiError);
      return null;
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const save = async () => (await saveDocument()) != null;

  const performEditionWorkflow = async () => {
    const workflow = editionWorkflow;
    const current = lyricsRef.current;
    if (!workflow || !current || !isRenditionLyricsDocument(current) || busyRef.current || writeLockedRef.current) return;
    const currentEditionDocument = current as RenditionLyricsDocument;
    if (currentEditionDocument.translationEditionKey !== activeTranslationEditionKeyRef.current) {
      setError(new APIError(409, { error: "translation_edition_conflict", details: ["当前译本身份与已载入文档不一致，请重新载入"] }));
      return;
    }
    if (workflow.command !== "set-default" && !isTranslationEditionLabel(workflow.label)) {
      setError(new APIError(422, { error: "invalid_translation_edition", details: ["译本名称必须是 1-256 字节的 UTF-8 文本，且不能带首尾空白"] }));
      return;
    }
    const sequence = lyricsLoadSequence.current;
    const musicID = current.musicId;
    const documentGeneration = documentGenerationRef.current;
    const preferredRenditionKey = activeRenditionKey;
    const preferredVersion = activeVersion;
    const common = { musicId: current.musicId, revision: current.revision };
    const mutation = workflow.command === "create"
      ? { ...common, operation: "create" as const, editionKey: workflow.editionKey, label: workflow.label }
      : workflow.command === "clone"
        ? { ...common, operation: "clone" as const, sourceEditionKey: currentEditionDocument.translationEditionKey, editionKey: workflow.editionKey, label: workflow.label }
        : workflow.command === "rename"
          ? { ...common, operation: "rename" as const, editionKey: currentEditionDocument.translationEditionKey, label: workflow.label }
          : { ...common, operation: "set-default" as const, editionKey: currentEditionDocument.translationEditionKey };
    busyRef.current = true;
    setBusy(true);
    setError(null);
    try {
      const materialized = await mutateLyricsTranslationEdition(mutation);
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      if (!isRenditionLyricsDocument(materialized)) {
        setError(new APIError(502, { error: "invalid_translation_edition", details: ["译本操作响应必须是 source-v3 文档"] }));
        return;
      }
      acceptAuthoritativeDocument(materialized, preferredRenditionKey, preferredVersion);
      setEditionWorkflow(null);
      setPendingTransition(null);
      void loadCatalog(query);
      const message = workflow.command === "create" ? "空白译本已创建"
        : workflow.command === "clone" ? "当前服务器译本已克隆"
          : workflow.command === "rename" ? "译本已重命名"
            : "默认译本已更新";
      show(message, "ok");
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      const apiError = reason instanceof APIError ? reason : new APIError(500, { error: "save_failed" });
      if (apiError.code === "translation_edition_exists" && (workflow.command === "create" || workflow.command === "clone")) {
        setEditionWorkflow((currentWorkflow) => currentWorkflow ? { ...currentWorkflow, editionKey: `ed-${crypto.randomUUID()}` } : currentWorkflow);
      }
      setError(apiError);
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const discard = (): boolean => {
    if (busyRef.current) return false;
    if (!sourceImportTokenRef.current && collaborationRef.current) {
      collaborationRef.current.discardLocalChanges();
      setError(null);
      setPendingAnnotationOperation(null);
      return true;
    }
    const authoritative = baselineRef.current;
    if (!authoritative) return false;
    const restored = JSON.parse(authoritative) as SongLyricsDocument;
    documentGenerationRef.current++;
    setLyrics(restored);
    if (isRenditionLyricsDocument(restored)) setActiveTranslationEditionKey((restored as RenditionLyricsDocument).translationEditionKey);
    sourceImportTokenRef.current = "";
    setSourcePreview(null);
    setSourcePreviewCandidate(null);
    setSourceRetry(null);
    setConfirmSourceImport(false);
    setConfirmImportRecovery(null);
    setPendingAnnotationOperation(null);
    setCandidates([]);
    setSourceSearchCompleted(false);
    setError(null);
    if (selectedMusicIDRef.current === restored.musicId) startCollaboration(restored.musicId);
    return true;
  };

  const reloadAuthoritative = async (): Promise<boolean> => {
    if (busyRef.current) return false;
    setPendingTransition(null);
    setEditionWorkflow(null);
    sourceImportTokenRef.current = "";
    setSourcePreview(null);
    setSourcePreviewCandidate(null);
    setSourceRetry(null);
    setConfirmSourceImport(false);
    setConfirmImportRecovery(null);
    setPendingAnnotationOperation(null);
    requestSequence.current++;
    performerSequence.current++;
    await Promise.all([loadCatalog(query), loadPerformers()]);
    if (!selectedMusic) return true;
    return performChooseMusic(selectedMusic);
  };

  const loadConflictAuthoritative = async () => {
    const conflictCurrent = error?.current;
    if (!conflictCurrent || busyRef.current) return;
    const sequence = lyricsLoadSequence.current;
    const musicID = conflictCurrent.musicId;
    const documentGeneration = documentGenerationRef.current;
    const preferredEditionKey = activeTranslationEditionKeyRef.current;
    busyRef.current = true;
    setBusy(true);
    try {
      let authoritative = conflictCurrent;
      if (preferredEditionKey && isRenditionLyricsDocument(conflictCurrent) &&
          (conflictCurrent as RenditionLyricsDocument).translationEditionKey !== preferredEditionKey) {
        authoritative = await getLyrics(musicID, preferredEditionKey);
      }
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      acceptAuthoritativeDocument(authoritative, activeRenditionKey, activeVersion);
      sourceImportTokenRef.current = "";
      setSourcePreview(null);
      setSourcePreviewCandidate(null);
      setSourceRetry(null);
      setConfirmSourceImport(false);
      setCandidates([]);
      setSourceSearchCompleted(false);
      setEditionWorkflow(null);
      setError(null);
      setConfirmConflictReload(false);
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      show(reason instanceof Error ? reason.message : "服务器版本载入失败", "err");
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  useImperativeHandle(ref, () => ({
    save,
    discard,
    isDirty: () => lyricsRef.current != null && canonicalLyricsJSON(lyricsRef.current) !== baselineRef.current,
    snapshot: () => ({
      dirty: lyricsRef.current != null && canonicalLyricsJSON(lyricsRef.current) !== baselineRef.current,
      document: lyricsRef.current ? JSON.parse(JSON.stringify(lyricsRef.current)) as SongLyricsDocument : null,
      generation: documentGenerationRef.current,
      editionKey: activeTranslationEditionKeyRef.current,
    }),
    isEditing: (musicID: number) => selectedMusicIDRef.current === musicID,
    activeTarget: () => selectedMusicIDRef.current == null ? null : {
      musicId: selectedMusicIDRef.current,
      editionKey: activeTranslationEditionKeyRef.current,
      renditionKey: activeRendition?.key || "",
      side: activeVersion,
      locale: "zh-CN",
      projectionKind,
    },
    reloadCatalog: () => { void loadCatalog(query); },
    exportDraft: () => lyrics ? JSON.parse(JSON.stringify(lyrics)) as SongLyricsDocument : null,
    reloadAuthoritative,
  }));

  const readableProjectionTime = (value?: string) => {
    if (!value) return "";
    const timestamp = Date.parse(value);
    return Number.isFinite(timestamp) ? new Date(timestamp).toLocaleString() : "";
  };

  const refreshProjectionStatus = async () => {
    const sequence = ++projectionSequence.current;
    const musicID = selectedMusicIDRef.current;
    setProjectionState("checking");
    setProjectionMessage("正在核对公共文件 generation…");
    try {
      const status = await getProjectionStatus(musicID ?? undefined);
      if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
      setProjectionStatus(status);
      if (status.lastError) {
        setProjectionState("failed");
        setProjectionMessage(`公共文件生成失败：${status.lastError}`);
      } else if (status.pending) {
        setProjectionState("checking");
        setProjectionMessage(`公共文件正在生成（generation ${status.generation}）。`);
      } else {
        const completedAt = readableProjectionTime(status.lastSuccessAt);
        setProjectionState("ready");
        setProjectionMessage(completedAt
          ? `公共文件 generation ${status.generation} 最近成功生成于 ${completedAt}。`
          : `公共文件 generation ${status.generation} 已完成。`);
      }
    } catch {
      if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
      setProjectionState("unknown");
      setProjectionMessage("公共文件状态暂时不可用，请稍后重新核对。");
    }
  };

  const waitForProjection = async (previousGeneration: number | null, nextPublished: boolean, musicID: number) => {
    const sequence = ++projectionSequence.current;
    setProjectionState("checking");
    setProjectionMessage(nextPublished
      ? "数据库发布已提交，正在等待公共文件生成…"
      : "数据库撤回已提交，正在等待公共文件更新…");
    const deadline = Date.now() + 15_000;
    while (projectionSequence.current === sequence && selectedMusicIDRef.current === musicID && Date.now() < deadline) {
      try {
        const status = await getProjectionStatus(musicID);
        if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
        setProjectionStatus(status);
        if (status.lastError) {
          setProjectionState("failed");
          setProjectionMessage(`数据库操作已完成，但公共文件生成失败：${status.lastError}`);
          show("数据库状态已更新，但公共文件生成失败", "err");
          return;
        }
        if (previousGeneration == null) {
          setProjectionState("unknown");
          setProjectionMessage(status.pending
            ? `数据库操作已完成，公共文件仍在生成（generation ${status.generation}）；因提交前状态不可用，无法将本次变更绑定到该 generation。`
            : `数据库操作已完成，公共文件 generation ${status.generation} 当前无待处理任务；因提交前状态不可用，无法确认本次变更对应的 generation。`);
          show("数据库状态已更新，但无法确认本次公共文件 generation", "err");
          return;
        }
        if (!status.pending && !status.lastError && status.generation > previousGeneration) {
          setProjectionState("ready");
          setProjectionMessage(nextPublished
            ? `公共文件已完成新一代生成；歌曲 ${musicID} 的数据库发布状态已进入 generation ${status.generation}。`
            : `公共文件已完成新一代生成；歌曲 ${musicID} 的数据库撤回状态已进入 generation ${status.generation}。`);
          show(nextPublished ? "数据库发布与公共文件生成均已完成" : "数据库撤回与公共文件更新均已完成", "ok");
          return;
        }
        setProjectionMessage(`数据库操作已完成，正在等待公共文件 generation 超过 ${previousGeneration}（当前 ${status.generation}）。`);
      } catch {
        if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
        setProjectionState("unknown");
        setProjectionMessage("数据库操作已完成，但公共文件状态核对失败；请稍后重新核对。");
        show("数据库状态已更新，但公共文件状态未知", "err");
        return;
      }
      await new Promise((resolve) => window.setTimeout(resolve, 300));
    }
    if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
    setProjectionState("unknown");
    setProjectionMessage("数据库操作已完成，但公共文件长时间仍未确认完成；请稍后重新核对。");
    show("数据库状态已更新，公共文件仍在生成或状态未知", "err");
  };

  const performPublication = async (nextPublished: boolean, document: SongLyricsDocument) => {
    if (busyRef.current || writeLockedRef.current) return;
    const sequence = lyricsLoadSequence.current;
    const musicID = document.musicId;
    const documentGeneration = documentGenerationRef.current;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    let previousProjectionGeneration: number | null = null;
    try {
      try {
        const status = await getProjectionStatus(musicID);
        if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
        setProjectionStatus(status);
        previousProjectionGeneration = status.generation;
      } catch {
        if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
        setProjectionState("unknown");
        setProjectionMessage("提交前无法读取公共文件 generation；数据库操作仍可继续，但提交后只能报告公共文件状态未知。");
      }
      const response = nextPublished
        ? await publishLyrics(document.musicId, document.revision)
        : await unpublishLyrics(document.musicId, document.revision);
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      const result = preserveReadOnlyLyricsSourceFacts(response, document);
      documentGenerationRef.current++;
      collaborationRef.current?.updateAuthoritativeEnvelope(result);
      setBaseline(canonicalLyricsJSON(result));
      void loadCatalog(query);
      show(nextPublished ? "数据库发布已提交，正在核对公共文件" : "数据库撤回已提交，正在核对公共文件", "ok");
      void waitForProjection(previousProjectionGeneration, nextPublished, musicID);
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      const apiError = reason instanceof APIError ? reason : new APIError(500, { error: "publication_failed" });
      setError(apiError);
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const publish = (nextPublished: boolean) => {
    if (!lyrics || !isLegacyLyricsDocument(lyrics) || writeLocked) return;
    const preflightProblems = lyricsVersionSaveProblems(lyrics);
    if (preflightProblems.length > 0) {
      setError(new APIError(422, { error: "invalid_game_projection", details: preflightProblems }));
      show("Game 投影需要先修复，未打开发布操作", "err");
      return;
    }
    setPendingTransition({ kind: "publish", nextPublished });
  };

  const findSource = async () => {
    if (!lyrics || isRenditionLyricsDocument(lyrics) || role !== "admin" || busyRef.current || writeLockedRef.current) return;
    const sequence = lyricsLoadSequence.current;
    const musicID = lyrics.musicId;
    busyRef.current = true;
    setBusy(true);
    setSourceActivity("searching");
    setError(null);
    setSourceRetry(null);
    setSourcePreviewCandidate(null);
    setSourcePreview(null);
    sourceImportTokenRef.current = "";
    setConfirmSourceImport(false);
    setSourceSearchCompleted(false);
    try {
      const result = await searchLyricsSource(lyrics.musicId);
      if (!requestIsCurrent(sequence, musicID)) return;
      setCandidates(result.items);
      setSourceSearchCompleted(true);
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID)) return;
      const apiError = reason instanceof APIError ? reason : new APIError(500, { error: "source_unavailable" });
      setError(apiError);
      setSourceRetry({ kind: "search" });
    } finally {
      busyRef.current = false;
      setBusy(false);
      setSourceActivity("");
    }
  };

  const previewSource = async (candidate: LyricsSourceCandidate) => {
    if (!lyrics || isRenditionLyricsDocument(lyrics) || role !== "admin" || busyRef.current || writeLockedRef.current) return;
    const sequence = lyricsLoadSequence.current;
    const musicID = lyrics.musicId;
    busyRef.current = true;
    setBusy(true);
    setSourceActivity("previewing");
    setError(null);
    setSourceRetry(null);
    setSourcePreviewCandidate(candidate);
    setSourcePreview(null);
    sourceImportTokenRef.current = "";
    setConfirmSourceImport(false);
    try {
      const preview = await previewLyricsSource(lyrics.musicId, candidate.pageId, candidate.revisionId);
      if (!requestIsCurrent(sequence, musicID)) return;
      setSourcePreview(preview);
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID)) return;
      const apiError = reason instanceof APIError ? reason : new APIError(500, { error: "source_unavailable" });
      setError(apiError);
      setSourceRetry({ kind: "preview", candidate });
    } finally {
      busyRef.current = false;
      setBusy(false);
      setSourceActivity("");
    }
  };

  const acceptPreview = () => {
    if (!lyrics || isRenditionLyricsDocument(lyrics) || lyrics.revision !== 0 || !sourcePreview || role !== "admin" || busyRef.current || writeLockedRef.current) return;
    const preview = sourcePreview;
    const imported = buildLyricsLinesFromSourcePreview(preview, performers);
    if (!imported.ok) {
      setConfirmSourceImport(false);
      setError(new APIError(422, { error: imported.code, details: imported.details }));
      return;
    }
    stopCollaboration();
    updateLyrics({
      lines: imported.lines,
      sourceUrl: preview.canonicalUrl, sourcePageId: preview.pageId,
      sourceRevisionId: preview.revisionId, sourceSha1: preview.sha1,
      sourceFetchedAt: preview.fetchedAt,
    });
    sourceImportTokenRef.current = preview.importToken;
    setConfirmSourceImport(false);
    setSourcePreview(null);
    setCandidates([]);
    setSourceSearchCompleted(false);
    setSourceRetry(null);
    show("固定 revision 已载入草稿，并保留了可安全映射的演唱者证据；请核对后首次保存", "ok");
  };

  const performerName = (id: LyricsPerformerID) => {
    const performer = activePerformerOptions.find((item) => String(item.performerId) === String(id));
    if (!performer) return String(id);
    return typeof performer.name === "string"
      ? performer.name
      : performer.name["zh-CN"] || performer.name["ja-JP"] || String(id);
  };

  const performerColor = (id: LyricsPerformerID) => {
    const performer = activePerformerOptions.find((item) => String(item.performerId) === String(id));
    return "color" in (performer || {}) && typeof (performer as LyricsRenditionPerformer | undefined)?.color === "string"
      ? (performer as LyricsRenditionPerformer).color
      : performerRepresentativeColor(performerName(id));
  };

  const continuePendingTransition = async (saveFirst: boolean) => {
    const pending = pendingTransition;
    if (!pending) return;
    const editionTransition = pending.kind === "edition-switch" || pending.kind === "edition-command";
    if ((saveFirst || pending.kind === "publish" || editionTransition) && writeLockedRef.current) return;
    let document = lyrics;
    if (saveFirst) {
      document = await saveDocument();
      if (!document) return;
      const currentSharedDocument = collaborationRef.current?.getSnapshot().document;
      if (collaborationRef.current?.undoManager.canUndo() ||
          (currentSharedDocument && canonicalLyricsJSON(editableLyricsDocument(currentSharedDocument)) !== canonicalLyricsJSON(document))) {
        show("保存期间又产生了新修改，请再次保存后再继续", "err");
        return;
      }
    } else if (pending.kind !== "publish" && baseline) {
      lyricsLoadSequence.current++;
      documentGenerationRef.current++;
      if (sourceImportTokenRef.current || !collaborationRef.current) {
        document = JSON.parse(baseline) as SongLyricsDocument;
        setLyrics(document);
        if (isRenditionLyricsDocument(document)) setActiveTranslationEditionKey((document as RenditionLyricsDocument).translationEditionKey);
      } else {
        if (hasLocalUndo) collaborationRef.current?.discardLocalChanges();
        document = collaborationRef.current?.getSnapshot().document ?? (JSON.parse(baseline) as SongLyricsDocument);
      }
      sourceImportTokenRef.current = "";
      setSourcePreview(null);
      setSourcePreviewCandidate(null);
      setSourceRetry(null);
      setConfirmSourceImport(false);
      setConfirmImportRecovery(null);
      setCandidates([]);
      setSourceSearchCompleted(false);
      setError(null);
    }
    if (pending.kind === "choose") {
      setPendingTransition(null);
      await performChooseMusic(pending.item);
    } else if (pending.kind === "publish" && document) {
      setPendingTransition(null);
      await performPublication(pending.nextPublished, document);
    } else if (pending.kind === "edition-switch") {
      const switched = await loadTranslationEdition(pending.editionKey);
      if (!switched) {
        setPendingTransition(null);
        if (writeLockedRef.current) show("实时校对已锁定，译本未切换", "err");
      }
    } else if (pending.kind === "edition-command") {
      if (openEditionWorkflow(pending.command)) setPendingTransition(null);
    }
  };

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

  const applyPerformerToAllSegments = (performerID: LyricsPerformerID) => {
    if (!lyrics || activeSideReadOnly || String(performerID) === "") return;
    replaceActiveLines(activeLines.map((line) => ({
      ...line,
      segments: line.segments.map((segment) => ({ ...segment, performerIds: [performerID] })) as LyricsEditorSegment[],
    })) as LyricsEditorLine[]);
  };

  const handleEditorKeyDown = (event: React.KeyboardEvent<HTMLFieldSetElement>) => {
    if (!(event.metaKey || event.ctrlKey) || event.key.toLowerCase() !== "s") return;
    event.preventDefault();
    if (!busy && !writeLocked && dirty) void save();
  };

  const previewLocales = ["ja-JP", "zh-CN", "en-US"] as const;
  const handlePreviewTabKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, locale: typeof previewLocales[number]) => {
    const index = previewLocales.indexOf(locale);
    let target = index;
    if (event.key === "ArrowRight") target = (index + 1) % previewLocales.length;
    else if (event.key === "ArrowLeft") target = (index - 1 + previewLocales.length) % previewLocales.length;
    else if (event.key === "Home") target = 0;
    else if (event.key === "End") target = previewLocales.length - 1;
    else return;
    event.preventDefault();
    const nextLocale = previewLocales[target];
    setPreviewLocale(nextLocale);
    previewTabRefs.current[nextLocale]?.focus();
  };

  return (
    <div className="lyrics-workspace">
      <LyricsCatalogSidebar
        query={query}
        onQueryChange={setQuery}
        catalog={catalog}
        catalogLoading={catalogLoading}
        catalogError={catalogError}
        selectedMusic={selectedMusic}
        busy={busy}
        onChooseMusic={chooseMusic}
        onRetryCatalog={() => void loadCatalog(query)}
      />

      <section className="lyrics-editor">
        {!selectedMusic ? <div className="center-state"><p>从目录选择一首曲目</p></div> : loading ? (
          <div className="center-state" role="status" aria-live="polite"><div className="spinner" />加载歌词…</div>
        ) : databaseAvailabilityOnly && selectedMusic.lyricsAvailabilityState ? (
          <div className="lyrics-runtime-only" role="status">
            <strong>数据库已记录歌词可用性，但当前没有可编辑正文</strong>
            <p>{databaseAvailabilityDescription(selectedMusic.lyricsAvailabilityState)}</p>
            <p>这不是“数据库未录入”：状态已持久化到 SQLite，系统只是不把无正文或未闭合来源伪装成可保存的空歌词。</p>
            <button className="btn btn-secondary" onClick={() => void performChooseMusic(selectedMusic)}>重新检查数据库</button>
          </div>
        ) : runtimeOnlyMissingDatabaseSource && selectedMusic.runtimeLyrics ? (
          <div className="lyrics-runtime-only" role="status">
            <strong>公开镜像仍在，后台数据库尚无可编辑源</strong>
            <p>这首歌包含在 embedded Public Lyrics {selectedMusic.runtimeLyrics.releaseId} 中，状态为“{runtimeLyricsStateLabel(selectedMusic.runtimeLyrics.state)}”，可用版本为 {runtimeLyricsVersionsLabel(selectedMusic.runtimeLyrics.availableVersions)}。该镜像是 standalone binary 内的只读发布资产，不是 SQLite 草稿或发布记录。</p>
            <p>系统不会把 detail 404 静默转换成可保存的空草稿。生产启动时的私有 embedded editor seed 负责补入缺失正文；该流程不会在此页面触发，也不会覆盖账号、普通翻译、剧情、审核或既有歌词。</p>
            <button className="btn btn-secondary" onClick={() => void performChooseMusic(selectedMusic)}>重新检查数据库</button>
          </div>
        ) : collaborationStructuralConflict && !lyrics ? (
          <div className="lyrics-runtime-only lyrics-structural-conflict-empty" role="alert">
            <strong>协作文档发生结构冲突，当前没有可安全显示的编辑副本</strong>
            <p>Yjs 文档的歌词行、分段或注音结构无法安全物化。协作连接已停止，编辑和保存均已禁用，以避免覆盖其他协作者的内容。</p>
            <p>请重新加载当前歌词；若问题仍然出现，请联系其他协作者停止编辑，并由管理员检查协作文档。</p>
            <button className="btn btn-secondary" onClick={() => void performChooseMusic(selectedMusic)} disabled={busy}>重新加载歌词</button>
          </div>
        ) : lyrics ? (
          <fieldset className="lyrics-edit-fence" disabled={busy} aria-busy={busy} aria-disabled={writeLocked} onKeyDown={handleEditorKeyDown}>
            <div className="lyrics-editor-head">
              <div className="lyrics-editor-head-copy">
                <div className="lyrics-editor-title-row">
                  <h2>{selectedMusic.title["zh-CN"] || selectedMusic.title["ja-JP"]}</h2>
                  {renditionDocument && <LyricsEditionMenu
                    editions={translationEditions}
                    activeEditionKey={activeTranslationEditionKey}
                    defaultEditionKey={renditionDocument.defaultTranslationEditionKey}
                    disabled={busy || writeLocked}
                    onSelect={requestEditionSwitch}
                    onCommand={requestEditionCommand}
                  />}
                </div>
                <span>musicId {lyrics.musicId} · revision {lyrics.revision} · {lyrics.status === "published" ? "当前修订已发布" : lyrics.status === "draft-published" ? `草稿（revision ${lyrics.publishedRevision} 仍公开）` : "草稿"}{activeTranslationEdition ? ` · 译本 ${activeTranslationEdition.label}` : ""}</span>
              </div>
              <div className="lyrics-actions">
                {role === "admin" && isLegacyLyricsDocument(lyrics) && <button className="btn btn-secondary" onClick={findSource} disabled={busy || writeLocked || lyrics.revision > 0}>{sourceActivity === "searching" ? "正在查找…" : "查找来源"}</button>}
                <button className="btn btn-primary" onClick={save} disabled={busy || writeLocked || !dirty}>保存草稿</button>
                {role === "admin" && isLegacyLyricsDocument(lyrics) && lyrics.revision > 0 && lyrics.status !== "published" && <button className="btn btn-secondary" onClick={() => publish(true)} disabled={busy || writeLocked}>发布当前修订</button>}
                {role === "admin" && isLegacyLyricsDocument(lyrics) && Boolean(lyrics.publishedRevision) && <button className="btn btn-secondary" onClick={() => publish(false)} disabled={busy || writeLocked}>取消发布 revision {lyrics.publishedRevision}</button>}
              </div>
            </div>

            <LyricsCollaborationBanner
              collaborationStructuralConflict={collaborationStructuralConflict}
              localSourceImportDraft={localSourceImportDraft}
              collaborationStatus={collaborationStatus}
              collaborationError={collaborationError}
              collaborationPeers={collaborationPeers}
              busy={busy}
              onReload={() => void performChooseMusic(selectedMusic)}
              onReconnect={() => collaborationRef.current?.reconnectNow()}
            />

            <LyricsVocalCard musicId={selectedMusic.musicId} />

            {renditionDocument ? (
              <div className="lyrics-lock-notice locked" role="status">
                <strong>Plural source facts 已由固定证据永久锁定</strong>
                <span>当前编辑器按 stable rendition key 与 Full/Game side 分开保存简中译文，并保留每个 rendition 的翻译/校对署名。Full/Game source text、行 ID/顺序、relation、provenance、演唱者、分段、ruby 与英文均不会通过此路由改写；exact projection 的 Game 仍跟随 Full。</span>
              </div>
            ) : lyrics.revision === 0 ? (
              <div className="lyrics-lock-notice" role="note">
                <strong>首次保存后永久锁定来源、行序/ID 与日文原文</strong>
                <span>保存后，来源资料、歌词行顺序与编号、日文原文将不可直接修改。请先完成来源核对、行顺序和分段文字检查；后续仍可在不改变日文全文的前提下重新分段，并编辑中英翻译、演唱者与备注。</span>
              </div>
            ) : (
              <div className="lyrics-lock-notice locked" role="status">
                <strong>来源、行序/ID 与日文原文已永久锁定</strong>
                <span>当前修订可在保持每行日文拼接结果完全一致的前提下重新分段，并调整中英翻译、演唱者与备注。如需修正来源、行序或日文原文，必须另行设计并审核显式迁移流程。</span>
              </div>
            )}

            {renditionDocument ? (
              <div className="lyrics-publication-progress" role="note">
                <div><strong>Public v3 发布由 recovery batch 管理</strong><span>此页面不会调用 legacy publish/unpublish</span></div>
                <ul><li className="complete"><span aria-hidden="true">✓</span>可按 stable key 独立保存 Full、Game-only 与 independent Game 简中译文；exact projection Game 继续由 Full 推导</li></ul>
              </div>
            ) : (
              <div className="lyrics-publication-progress" role="status" aria-live="polite">
                <div><strong>发布准备</strong><span>{publicationComplete ? "已满足发布前置条件" : `还需完成 ${publicationChecks.filter((check) => !check.complete).length} 项`}</span></div>
                <ul>{publicationChecks.map((check) => <li key={check.label} className={check.complete ? "complete" : "pending"}><span aria-hidden="true">{check.complete ? "✓" : "○"}</span>{check.label}</li>)}</ul>
              </div>
            )}

            <LyricsProjectionStatusCard
              projectionState={projectionState}
              projectionStatus={projectionStatus}
              projectionMessage={projectionMessage}
              busy={busy}
              onRefresh={() => void refreshProjectionStatus()}
            />

            {error && (
              <div className="lyrics-error" role="alert">
                <strong>{sourceLabel(error)}</strong>
                {sourceImportTokenRef.current && !sourceImportFailureIsTerminal(error) && <span>未确认首次保存已提交；已保留固定修订授权和 verified draft，可直接重试保存。</span>}
                {!sourceImportTokenRef.current && sourceRetry && <span>固定修订授权或 verified draft 状态已失效，请重新预览后再保存。</span>}
                {error.details.map((detail) => <span key={detail}>{detailLabel(detail)}</span>)}
                {sourceRetry && lyrics.revision === 0 && role === "admin" && (
                  <button className="btn btn-secondary btn-sm" onClick={() => sourceRetry.kind === "search" ? void findSource() : void previewSource(sourceRetry.candidate)} disabled={busy || writeLocked}>{sourceRetry.kind === "search" ? "重试查找来源" : "重试载入固定修订"}</button>
                )}
                {error.code === "revision_conflict" && error.current && (
                  <button className="btn btn-secondary btn-sm" onClick={() => setConfirmConflictReload(true)}>载入服务器版本</button>
                )}
              </div>
            )}

            {versionSaveProblems.length > 0 && (
              <div className="lyrics-error lyrics-version-error" role="alert">
                <strong>Rendition / projection 或公开署名合同需要修复后才能保存</strong>
                <span>每个 stable key 的 Full 与 Game 都保持在本 family 内；不会因为文本相同而跨 family 合并。</span>
                {versionSaveProblems.map((problem) => <span key={problem}>{problem}</span>)}
              </div>
            )}

            {role === "admin" && lyrics.revision === 0 && sourceActivity === "searching" && (
              <div className="lyrics-source-panel lyrics-source-loading" role="status" aria-live="polite"><div className="spinner" /><span>正在搜索并核对候选来源…</span></div>
            )}

            {role === "admin" && lyrics.revision === 0 && sourceSearchCompleted && (
              <div className="lyrics-source-panel" aria-live="polite">
                <div className="lyrics-source-title"><strong>候选来源</strong><button type="button" className="btn btn-ghost btn-sm" onClick={() => void findSource()} disabled={busy || writeLocked}>重新搜索</button></div>
                {candidates.length === 0 ? <span className="lyrics-muted">没有找到可核对的歌词来源。可以调整曲目资料后再试，或使用手动歌词行。</span> : candidates.map((candidate) => (
                  <button key={`${candidate.pageId}-${candidate.revisionId}`} aria-label={`预览候选来源 ${candidate.title} 的固定修订 ${candidate.revisionId}`} onClick={() => previewSource(candidate)} disabled={busy || writeLocked}>
                    {candidate.title}<span>固定修订 {candidate.revisionId}</span>
                  </button>
                ))}
              </div>
            )}

            {role === "admin" && lyrics.revision === 0 && sourceActivity === "previewing" && (
              <div className="lyrics-source-preview lyrics-source-loading" role="status" aria-live="polite"><div className="spinner" /><span>正在载入固定修订 {sourcePreviewCandidate?.revisionId || ""} 的完整预览…</span></div>
            )}

            {sourcePreview && lyrics.revision === 0 && (
              <div className="lyrics-source-preview" aria-labelledby="lyrics-source-preview-title">
                <div><strong id="lyrics-source-preview-title">固定修订预览 · 共 {sourcePreview.lines.length} 行</strong><a href={sourcePreview.canonicalUrl} target="_blank" rel="noopener noreferrer">打开来源</a></div>
                <p className="lyrics-muted">下方展示解析后的全部 {sourcePreview.lines.length} 行，不会只截取前几行；滚动区域仅影响显示。请核对完整日文歌词。使用此版本会把固定 revision 与一次性导入授权载入 revision 0 草稿，并清空现有中英翻译；来源中的演唱者证据会在可安全映射时保留，无法映射时会显示错误并阻止载入。网络或服务器瞬时失败会保留授权和 verified draft，可直接重试。仅当服务端明确拒绝授权、来源身份/修订或内容生产者已变化等终态发生时，才需要重新预览。只有首次保存成功才会永久锁定来源、行顺序与日文原文。</p>
                <pre tabIndex={0} aria-label={`固定修订 ${sourcePreview.revisionId} 的完整歌词，共 ${sourcePreview.lines.length} 行`}>{sourcePreview.lines.map((line) => `${line.stanzaBreakBefore ? "\n" : ""}${line.japanese}`).join("\n")}</pre>
                <div className="lyrics-actions"><button className="btn btn-primary" onClick={() => setConfirmSourceImport(true)} disabled={writeLocked}>使用此版本</button><button className="btn btn-ghost" onClick={() => { setSourcePreview(null); setSourcePreviewCandidate(null); sourceImportTokenRef.current = ""; }}>取消</button></div>
              </div>
            )}

            {activeRendition && <div className="lyrics-version-switcher lyrics-rendition-switcher">
              <div className="lyrics-version-tabs" role="tablist" aria-label="稳定 rendition family">
                {renditionKeys.map((renditionKey: string) => {
                  const rendition = lyricsRenditionByKey(lyrics, renditionKey);
                  return <button type="button" role="tab" key={renditionKey} aria-selected={activeRenditionKey === renditionKey} tabIndex={activeRenditionKey === renditionKey ? 0 : -1} className={activeRenditionKey === renditionKey ? "active" : ""} onClick={() => {
                    setActiveRenditionKey(renditionKey);
                    const versions = normalizedLyricsVersions(lyrics, renditionKey);
                    setActiveVersion(versions.includes("full") ? "full" : "game");
                  }}>{rendition?.label || renditionKey}<span>{renditionKey}</span></button>;
                })}
              </div>
              <p>每个 stable key 都保留自己的 Full / Game、relation、演唱者分段、ruby、翻译与翻译/校对署名；即使文本相同也不会与其他 family 合并。</p>
            </div>}

            <div className="lyrics-version-switcher">
              <div className="lyrics-version-tabs" role="tablist" aria-label="歌词版本工作区">
                {availableVersions.includes("full") && <button type="button" role="tab" id="lyrics-version-full-tab" aria-controls="lyrics-version-panel" aria-selected={activeVersion === "full"} tabIndex={activeVersion === "full" ? 0 : -1} className={activeVersion === "full" ? "active" : ""} onClick={() => setActiveVersion("full")}>Full <span>{activeRendition ? "仅简中可编辑" : "可编辑"}</span></button>}
                {hasGameVersion && <button type="button" role="tab" id="lyrics-version-game-tab" aria-controls="lyrics-version-panel" aria-selected={activeVersion === "game"} tabIndex={activeVersion === "game" ? 0 : -1} className={activeVersion === "game" ? "active" : ""} onClick={() => setActiveVersion("game")}>Game <span>{gameSideReadOnlyReason === "exact_projection" ? "只读 exact projection" : "独立简中可编辑"}</span></button>}
              </div>
              <p>{activeRendition
                ? activeVersion === "game" && gameSideReadOnlyReason === "exact_projection"
                  ? `Game 只引用同一 stable key（${activeRendition.key}）的 Full 行 ID；Game 自有分段和 ruby 原样保留，简中译文由 Full 对应行同步。`
                  : activeVersion === "game" && projectionKind === "game_only"
                    ? `${activeRendition.key} 是 Game-only rendition，没有 Full peer；Game 简中按该 stable key/side 独立保存，source facts 与英文保持只读。`
                    : activeVersion === "game"
                      ? `${activeRendition.key} 的 independent Game 简中按该 stable key/side 独立保存，不会覆盖 Full 或其他 rendition family。`
                      : `${activeRendition.key} 的 Full 简中按该 stable key/side 独立保存；source facts 与英文保持只读。`
                : activeVersion === "full"
                  ? "singular v2 Full 保持原有可编辑行为。"
                  : "singular v2 Game 继续作为同一 Full 的只读行 ID 投影，不做有损 v2 coercion。"}</p>
            </div>

            <LyricsMetadataCard
              activeTranslationCredit={activeTranslationCredit}
              activeProofreadingCredit={activeProofreadingCredit}
              activeRendition={activeRendition}
              legacyLyrics={legacyLyrics}
              activeVersion={activeVersion}
              projectionKind={projectionKind}
              writeLocked={writeLocked}
              updateActiveCredits={updateActiveCredits}
              onUpdateLyrics={updateLyrics}
            />

            <section className="lyrics-component-provenance" aria-labelledby="lyrics-component-provenance-title">
              <div><strong id="lyrics-component-provenance-title">组件 provenance</strong><span>仅认证编辑器显示固定证据；公开输出使用对应版本的严格 attribution contract</span></div>
              {componentProvenance.length === 0 ? (
                <p>当前歌词没有组件级固定来源映射；旧版单一来源字段仍保持只读，不会被伪装成 Full / Game / ruby 的独立证据。</p>
              ) : (
                <dl>{componentProvenance.map((row: ResolvedLyricsComponentProvenanceRow) => <div key={row.component}>
                  <dt>{row.label}</dt>
                  <dd>
                    <code>{row.renditionKey}</code>
                    {row.identity ? <>
                      <span>{row.identity.provider === "moegirl" || row.identity.provider === "moegirl_public_exact"
                        ? "萌娘百科"
                        : row.identity.provider === "sekaipedia" ? "Sekaipedia" : "Vocaloid Wiki"} · revision {row.identity.revisionId} · {row.identity.section}</span>
                      <a href={row.identity.canonicalUrl} target="_blank" rel="noopener noreferrer">打开固定来源</a>
                    </> : <span>未找到对应固定来源详情</span>}
                  </dd>
                </div>)}</dl>
              )}
            </section>

            {hasPerformerSegmentation && !activeRendition && performerError && (
              <div className="lyrics-error" role="alert">
                <strong>演唱者目录加载失败，角色分词歌曲发布前需要重新载入</strong>
                <button className="btn btn-secondary btn-sm" onClick={() => void loadPerformers()}>重试加载演唱者</button>
              </div>
            )}

            {hasPerformerSegmentation && !activeRendition && !activeSideReadOnly && activeLines.length > 0 && activePerformerOptions.length > 0 && (
              <div className="lyrics-bulk-performer">
                <label htmlFor="lyrics-all-performer">统一设置当前 side 全部分段演唱者</label>
                <select id="lyrics-all-performer" defaultValue="" disabled={writeLocked} onChange={(event) => {
                  const value = event.target.value;
                  const performerID: LyricsPerformerID = activeRendition ? value : Number(value);
                  if (value) applyPerformerToAllSegments(performerID);
                  event.currentTarget.value = "";
                }}>
                  <option value="">选择一位演唱者…</option>
                  {activePerformerOptions.map((performer) => <option key={performer.performerId} value={performer.performerId}>{typeof performer.name === "string" ? performer.name : performer.name["zh-CN"] || performer.name["ja-JP"]}</option>)}
                </select>
                <span>只覆盖 {activeRenditionKey || "legacy-v2"} 的 {activeVersion} side，不影响其他 rendition family。</span>
              </div>
            )}

            {!activeSideReadOnly && (!activeRendition || activeSide) && <div id="lyrics-version-panel" role="tabpanel" aria-labelledby={`lyrics-version-${activeVersion}-tab`} className="lyrics-lines" ref={linesContainerRef}>
              {activeLines.length === 0 && <div className="lyrics-empty-lines"><strong>当前 {activeVersion === "full" ? "Full" : "Game"} side 还没有歌词行</strong><span>{role === "admin" && !activeRendition ? "可以查找 Wiki 来源，或手动添加歌词行。" : "请在来源仍可修改时添加该 side 的歌词行。"}</span></div>}
              {activeLines.map((line, lineIndex) => (
                <LyricsLineEditor
                  key={`${activeRenditionKey || "legacy-v2"}:${activeVersion}:${line.id}`}
                  line={line}
                  lineIndex={lineIndex}
                  lineCount={activeLines.length}
                  sourceMutable={activeSideSourceMutable}
                  writeLocked={writeLocked || activeSideReadOnly}
                  showPerformerSegmentation={hasPerformerSegmentation}
                  performers={activePerformerOptions}
                  performerName={performerName}
                  performerColor={performerColor}
                  registerSegmentInput={(segmentIndex, element) => { segmentInputRefs.current[`${activeRenditionKey}:${activeVersion}:${lineIndex}-${segmentIndex}`] = element; segmentInputRefs.current[`${lineIndex}-${segmentIndex}`] = element; }}
                  onUpdateLine={(patch) => updateLine(lineIndex, patch)}
                  onMoveLine={(direction) => moveLine(lineIndex, direction)}
                  onRemoveLine={() => removeLine(lineIndex)}
                  onUpdateSegment={(segmentIndex, text, performerIds) => updateSegment(lineIndex, segmentIndex, text, performerIds)}
                  onUpdateRubySpan={(segmentIndex, rubyIndex, patch) => updateRubySpan(lineIndex, segmentIndex, rubyIndex, patch)}
                  onSplitRubySpan={(segmentIndex, rubyIndex) => splitRubySpan(lineIndex, segmentIndex, rubyIndex)}
                  onMergeRubyWithPrevious={(segmentIndex, rubyIndex) => mergeRubyWithPrevious(lineIndex, segmentIndex, rubyIndex)}
                  onAddSegment={(segmentIndex) => addSegment(lineIndex, segmentIndex)}
                  onSplitSegment={(segmentIndex) => splitSegment(lineIndex, segmentIndex)}
                  onMergeWithPreviousSegment={(segmentIndex) => mergeWithPreviousSegment(lineIndex, segmentIndex)}
                  onRemoveSegment={(segmentIndex) => removeSegment(lineIndex, segmentIndex)}
                  onMoveSegment={(segmentIndex, direction) => moveSegment(lineIndex, segmentIndex, direction)}
                />
              ))}
              {activeSideSourceMutable && <button className="btn btn-secondary lyrics-add-line" onClick={addLine} disabled={writeLocked}>添加 {activeVersion === "full" ? "Full" : "Game"} 歌词行</button>}
            </div>}

            {!activeSideReadOnly ? <div className="lyrics-public-preview">
              <strong>{activeRenditionKey || "legacy-v2"} · {activeVersion === "full" ? "Full" : "Game"} 公开文件预览</strong>
              <div className="lyrics-preview-tabs" role="tablist" aria-label="歌词预览语言">
                {previewLocales.map((locale) => <button
                  key={locale}
                  ref={(element) => { previewTabRefs.current[locale] = element; }}
                  id={`lyrics-preview-tab-${locale}`}
                  type="button"
                  role="tab"
                  tabIndex={previewLocale === locale ? 0 : -1}
                  aria-selected={previewLocale === locale}
                  aria-controls="lyrics-preview-panel"
                  className={previewLocale === locale ? "active" : ""}
                  onClick={() => setPreviewLocale(locale)}
                  onKeyDown={(event) => handlePreviewTabKeyDown(event, locale)}
                >{locale === "ja-JP" ? "日文" : locale === "zh-CN" ? "简中" : "英文"}</button>)}
              </div>
              <div id="lyrics-preview-panel" role="tabpanel" aria-labelledby={`lyrics-preview-tab-${previewLocale}`} lang={previewLocale === "ja-JP" ? "ja" : previewLocale === "zh-CN" ? "zh-CN" : "en"}>
                {activeLines.length === 0 ? <p className="lyrics-muted">保存当前 side 的歌词行后会在这里显示公开文件效果。</p> : activeLines.map((line) => <p key={line.id} className={line.stanzaBreakBefore ? "lyrics-stanza-start" : undefined}>{previewLocale === "ja-JP" ? line.segments.map((segment, segmentIndex) => <span key={`${line.id}:${segmentIndex}`} className="lyrics-ruby-preview-segment">{segment.performerIds.length > 0 && <span className="lyric-performer-squares" role="img" aria-label={`演唱者：${segment.performerIds.map(performerName).join("、")}`}>{segment.performerIds.map((performerId) => <i key={performerId} className="lyric-performer-swatch" aria-hidden="true" style={performerColor(performerId) ? { backgroundColor: performerColor(performerId) } : undefined} />)}</span>}{renderRubySpans(segment.ruby)}</span>) : line[previewLocale] || ""}</p>)}
              </div>
            </div> : <section id="lyrics-version-panel" role="tabpanel" aria-labelledby="lyrics-version-game-tab" className="lyrics-game-preview">
              <header>
                <div><strong>Game 只读 exact-projection</strong><span>{activeRendition ? `${activeRendition.key} → ${activeRendition.relation.fullRenditionKey}` : legacyLyrics ? legacyLyrics.gameProjection?.reasonCode || "缺少版本判定" : "缺少稳定 rendition"}</span></div>
                <p>投影严格限制在同一 stable key。Game side 自有的演唱者分段与 ruby 会按原样展示，简中译文按 relation 指向的 Full 行同步，不会从其他 family 的相同文本推断或合并。</p>
              </header>
              {!gameProjection.ok ? <div className="lyrics-error" role="alert">{gameProjection.errors.map((problem) => <span key={problem}>{problem}</span>)}</div> : gameProjection.lines.length === 0 ? <p className="lyrics-muted">当前没有可预览的 Game 行投影。</p> : <ol>
                {gameProjection.lines.map((line, index) => <li key={line.id} className={line.stanzaBreakBefore ? "lyrics-stanza-start" : undefined}>
                  <header><strong>{String(index + 1).padStart(2, "0")}</strong><code>{line.id}</code></header>
                  <p lang="ja">{line.segments.map((segment, segmentIndex) => <span key={`${line.id}:game:${segmentIndex}`} className="lyrics-ruby-preview-segment">{segment.performerIds.length > 0 && <span className="lyric-performer-squares" role="img" aria-label={`演唱者：${segment.performerIds.map(performerName).join("、")}`}>{segment.performerIds.map((performerId) => <i key={performerId} className="lyric-performer-swatch" aria-hidden="true" title={performerName(performerId)} style={performerColor(performerId) ? { backgroundColor: performerColor(performerId) } : undefined} />)}</span>}{renderRubySpans(segment.ruby)}</span>)}</p>
                  <div className="lyrics-game-translations"><span lang="zh-CN">{line["zh-CN"] || "简中待翻译"}</span><span lang="en">{line["en-US"] || "English pending"}</span></div>
                </li>)}
              </ol>}
            </section>}
          </fieldset>
        ) : <div className="center-state" role="alert"><p>{error ? sourceLabel(error) : "歌词加载失败"}</p><button className="btn btn-secondary" onClick={() => selectedMusic && void performChooseMusic(selectedMusic)}>重试加载歌词</button></div>}
      </section>
      <Modal open={pendingAnnotationOperation != null} onClose={() => setPendingAnnotationOperation(null)} title="确认移除受影响的 ruby 注音" maxWidth={500}>
        <p className="dirty-guard-copy">这次拆分或文字修改落在已有注音范围内，无法在不猜测读音对应关系的情况下自动保留该注音。</p>
        <p className="dirty-guard-copy">确认后只会移除直接受影响的 ruby reading；其他分段和未受影响的注音会原样保留。系统不会把一个完整读音复制到拆分后的两边。</p>
        <div className="dirty-guard-actions">
          <button type="button" className="btn btn-secondary" onClick={confirmAnnotationOperation} disabled={writeLocked}>确认移除受影响注音并继续</button>
          <button type="button" className="btn btn-ghost" onClick={() => setPendingAnnotationOperation(null)}>取消</button>
        </div>
      </Modal>
      <Modal open={confirmSourceImport && sourcePreview != null && lyrics?.revision === 0} onClose={() => setConfirmSourceImport(false)} title="确认替换歌词草稿" maxWidth={500}>
        <p className="dirty-guard-copy">将载入固定修订 {sourcePreview?.revisionId} 的 {sourcePreview?.lines.length} 行日文歌词，并替换当前全部歌词行。已有中英翻译和分段会被替换；来源演唱者证据只会在可安全映射为当前数字角色 ID 时保留，否则会阻止载入并显示错误。</p>
        <p className="dirty-guard-copy">此操作只更新 revision 0 本地草稿。请再次核对；网络或服务器瞬时失败会保留一次性授权和 verified draft，可直接重试保存。仅在授权过期、身份/来源或内容生产者变化等终态时需要重新预览。首次保存成功后，来源资料、行顺序与编号、日文原文才会永久锁定。</p>
        <div className="dirty-guard-actions">
          <button className="btn btn-primary" onClick={acceptPreview} disabled={writeLocked}>确认载入草稿</button>
          <button className="btn btn-ghost" onClick={() => setConfirmSourceImport(false)}>返回核对</button>
        </div>
      </Modal>
      <Modal open={confirmImportRecovery != null} onClose={() => setConfirmImportRecovery(null)} title="确认首次保存结果" maxWidth={520}>
        <p className="dirty-guard-copy">保存请求没有返回可验证结果，但服务器现在已有 revision {confirmImportRecovery?.revision || 0}，且固定来源 page/revision/SHA1、行 ID/顺序和日文原文与本次导入完全一致。这通常表示首次保存已经提交，只是响应在传输中丢失。</p>
        <p className="dirty-guard-copy">载入服务器版本会清除已消费的一次性授权，并以服务器内容作为后续编辑基线；不会要求重新预览。若本地中英翻译或演唱者与服务器版本不同，请先复制需要保留的内容再载入。</p>
        <div className="dirty-guard-actions">
          <button className="btn btn-primary" onClick={() => {
            if (!confirmImportRecovery) return;
            documentGenerationRef.current++;
            setLyrics(confirmImportRecovery);
            setBaseline(canonicalLyricsJSON(confirmImportRecovery));
            setConfirmImportRecovery(null);
            setPendingTransition(null);
            setError(null);
            void loadCatalog(query);
            startCollaboration(confirmImportRecovery.musicId);
            show("已载入服务器首次保存结果，可继续编辑", "ok");
          }}>载入服务器版本</button>
          <button className="btn btn-ghost" onClick={() => setConfirmImportRecovery(null)}>先保留本地内容</button>
        </div>
      </Modal>
      <Modal open={confirmConflictReload && error?.code === "revision_conflict" && error.current != null} onClose={() => setConfirmConflictReload(false)} title="载入服务器版本" maxWidth={500} closeDisabled={busy}>
        <p className="dirty-guard-copy">载入服务器版本会覆盖当前未保存草稿。建议先使用浏览器的保存页面或复制文本方式保留需要手动合并的内容。</p>
        <p className="dirty-guard-copy">如果全歌曲 revision 是由其他译本推进，系统会重新读取你当前译本的最新权威文档，并尽量保留当前 rendition 与 Full/Game side。</p>
        <div className="dirty-guard-actions">
          <button className="btn btn-secondary" onClick={() => void loadConflictAuthoritative()} disabled={busy}>确认载入服务器版本</button>
          <button className="btn btn-ghost" onClick={() => setConfirmConflictReload(false)} disabled={busy}>取消</button>
        </div>
      </Modal>
      <Modal
        open={editionWorkflow != null}
        onClose={() => setEditionWorkflow(null)}
        title={editionWorkflow?.command === "create" ? "新建空白译本"
          : editionWorkflow?.command === "clone" ? "克隆当前译本"
            : editionWorkflow?.command === "rename" ? "重命名当前译本"
              : "设为默认译本"}
        maxWidth={520}
        closeDisabled={busy}
      >
        {editionWorkflow && <div className="lyrics-edition-workflow" aria-busy={busy}>
          {editionWorkflow.command === "create" && <p className="dirty-guard-copy">将创建不含简中译文的新译本。稳定 source rendition、Full/Game 关系、分段、ruby 与英文仍由服务器按 source-v3 合同物化。</p>}
          {editionWorkflow.command === "clone" && <p className="dirty-guard-copy">只克隆当前译本在服务器上已保存的内容，不会读取或复制任何未保存的浏览器草稿。</p>}
          {editionWorkflow.command === "rename" && <p className="dirty-guard-copy">重命名只修改显示名称；稳定译本 key <code>{editionWorkflow.editionKey}</code> 保持不变。</p>}
          {editionWorkflow.command === "set-default" && <p className="dirty-guard-copy">将 <strong>{activeTranslationEdition?.label || activeTranslationEditionKey}</strong> 设为缺省打开的译本。当前译本 key 和译文内容不会改变。</p>}
          {error && (error.code.startsWith("translation_edition_") || error.code === "invalid_translation_edition" || error.code === "revision_conflict") && <div className="lyrics-error" role="alert"><strong>{sourceLabel(error)}</strong>{error.details.map((detail) => <span key={detail}>{detail}</span>)}{error.code === "revision_conflict" && error.current && <button type="button" className="btn btn-secondary btn-sm" onClick={() => { setEditionWorkflow(null); setConfirmConflictReload(true); }}>载入服务器版本</button>}</div>}
          {editionWorkflow.command !== "set-default" && <div className="lyrics-edition-workflow-fields">
            <label>译本名称<input autoFocus value={editionWorkflow.label} maxLength={256} onChange={(event) => { setEditionWorkflow((current) => current ? { ...current, label: event.target.value } : current); setError(null); }} /></label>
            <label>稳定 key<input value={editionWorkflow.editionKey} readOnly /></label>
          </div>}
          <div className="dirty-guard-actions">
            <button type="button" className="btn btn-primary" onClick={() => void performEditionWorkflow()} disabled={busy || writeLocked || (editionWorkflow.command !== "set-default" && !isTranslationEditionLabel(editionWorkflow.label)) || (editionWorkflow.command === "rename" && editionWorkflow.label === activeTranslationEdition?.label)}>
              {editionWorkflow.command === "create" ? "创建空白译本" : editionWorkflow.command === "clone" ? "克隆服务器译本" : editionWorkflow.command === "rename" ? "保存新名称" : "设为默认译本"}
            </button>
            <button type="button" className="btn btn-ghost" onClick={() => setEditionWorkflow(null)} disabled={busy}>取消</button>
          </div>
        </div>}
      </Modal>
      <Modal open={pendingTransition != null} onClose={() => setPendingTransition(null)} title={pendingTransition?.kind === "publish" ? (pendingTransition.nextPublished ? "确认发布歌词" : "确认取消发布") : "处理未保存歌词"} maxWidth={500} closeDisabled={busy}>
        <div aria-busy={busy}>
          {busy && <p className="dirty-guard-copy" role="status" aria-live="polite">正在保存或提交歌词，请等待服务器确认…</p>}
        {pendingTransition?.kind === "publish" ? (
          <>
            <p className="dirty-guard-copy">{pendingTransition.nextPublished ? `将把当前 revision ${lyrics?.revision || 0} 发布到公共歌词文件。` : `将从公共歌词文件撤下 revision ${lyrics?.publishedRevision || lyrics?.revision || 0}。`}</p>
            {dirty && <p className="dirty-guard-copy">当前还有未保存修改。必须先保存成功，才能发布刚刚核对的内容。</p>}
            {pendingTransition.nextPublished && publicationProblems.length > 0 && (
              <div className="lyrics-publication-check" role="alert"><strong>发布前还需补齐：</strong><ul>{publicationProblems.slice(0, 8).map((problem) => <li key={problem}>{problem}</li>)}</ul>{publicationProblems.length > 8 && <span>另有 {publicationProblems.length - 8} 项未列出</span>}</div>
            )}
            <div className="dirty-guard-actions">
              {dirty ? <button className="btn btn-primary" onClick={() => void continuePendingTransition(true)} disabled={busy || writeLocked}>保存并继续</button> : <button className="btn btn-primary" onClick={() => void continuePendingTransition(false)} disabled={busy || writeLocked || (pendingTransition.nextPublished && publicationProblems.length > 0)}>{pendingTransition.nextPublished ? "确认发布" : "确认取消发布"}</button>}
              <button className="btn btn-ghost" onClick={() => setPendingTransition(null)} disabled={busy}>取消</button>
            </div>
          </>
        ) : (
          <>
            <p className="dirty-guard-copy">当前歌词有未保存修改。继续前请选择如何处理。</p>
            {pendingTransition?.kind === "edition-switch" && <p className="dirty-guard-copy">切换译本会整篇载入服务器权威文档；当前译本的本地草稿不会带到目标译本。</p>}
            {pendingTransition?.kind === "edition-command" && pendingTransition.command === "clone" && <p className="dirty-guard-copy"><strong>如果选择“放弃并继续”，克隆只会复制服务器上已保存的当前译本，明确不会复制这份未保存草稿。</strong></p>}
            {pendingTransition?.kind === "edition-command" && pendingTransition.command !== "clone" && <p className="dirty-guard-copy">译本元数据操作使用全歌曲 revision/CAS；继续前必须先同步处理当前译本草稿。</p>}
            <div className="dirty-guard-actions">
              <button className="btn btn-primary" onClick={() => void continuePendingTransition(true)} disabled={busy || writeLocked}>保存并继续</button>
              <button className="btn btn-secondary" onClick={() => void continuePendingTransition(false)} disabled={busy || ((pendingTransition?.kind === "edition-switch" || pendingTransition?.kind === "edition-command") && writeLocked)}>放弃并继续</button>
              <button className="btn btn-ghost" onClick={() => setPendingTransition(null)} disabled={busy}>取消</button>
            </div>
          </>
        )}
        </div>
      </Modal>
    </div>
  );
});
