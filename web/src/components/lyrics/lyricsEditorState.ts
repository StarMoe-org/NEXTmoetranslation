"use client";

import { useMemo, useRef, useState } from "react";
import type {
  APIError, CatalogMusicItem, CatalogPerformerItem, LyricsEditorLine, LyricsRenditionPerformer,
  LyricsSourceCandidate, LyricsSourcePreview, ProjectionStatus, RenditionLyricsDocument, SongLyricsDocument,
} from "@/lib/api";
import { isLegacyLyricsDocument, type LyricsProjectionKind } from "@/components/lyrics/lyricsDocumentModel";
import {
  isRenditionLyricsDocument, lyricsHasPerformerSegmentation, lyricsRenditionByKey, lyricsRenditionKeys,
  lyricsVersionSaveProblems, normalizedLyricsVersions, projectGameLyricsLines,
  renditionProjectionStatus, resolvedLyricsComponentProvenance,
} from "@/lib/lyrics-versioning.mjs";
import type {
  EditionWorkflow, PendingAnnotationOperation, PendingTransition,
} from "@/components/lyrics/lyricsDocumentModel";
import {
  LyricsCollaboration,
  type LyricsCollaborationPeer,
  type LyricsCollaborationStatus,
  canonicalLyricsJSON,
} from "@/lib/yjs-lyrics";

/** Holds the document-scoped editor state shared by the loader, command, source and persistence hooks. */
export function useLyricsEditorState(producerWriteLocked: boolean) {
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
  const [pendingTransition, setPendingTransition] = useState<PendingTransition | null>(null);
  const [pendingAnnotationOperation, setPendingAnnotationOperation] = useState<PendingAnnotationOperation | null>(null);
  const [projectionStatus, setProjectionStatus] = useState<ProjectionStatus | null>(null);
  const [projectionState, setProjectionState] = useState<"idle" | "checking" | "ready" | "failed" | "unknown">("idle");
  const [projectionMessage, setProjectionMessage] = useState("");
  const projectionSequence = useRef(0);
  const linesContainerRef = useRef<HTMLDivElement | null>(null);
  const segmentInputRefs = useRef<Record<string, HTMLInputElement | null>>({});

  const dirty = lyrics != null && canonicalLyricsJSON(lyrics) !== baseline;
  const lyricsRef = useRef<SongLyricsDocument | null>(lyrics);
  const baselineRef = useRef(baseline);
  const activeTranslationEditionKeyRef = useRef(activeTranslationEditionKey);
  lyricsRef.current = lyrics;
  baselineRef.current = baseline;
  activeTranslationEditionKeyRef.current = activeTranslationEditionKey;

  return {
    query, setQuery,
    catalog, setCatalog,
    performers, setPerformers,
    selectedMusic, setSelectedMusic,
    lyrics, setLyrics,
    runtimeOnlyMissingDatabaseSource, setRuntimeOnlyMissingDatabaseSource,
    databaseAvailabilityOnly, setDatabaseAvailabilityOnly,
    baseline, setBaseline,
    loading, setLoading,
    catalogLoading, setCatalogLoading,
    catalogError, setCatalogError,
    performerError, setPerformerError,
    busy, setBusy,
    error, setError,
    candidates, setCandidates,
    sourceSearchCompleted, setSourceSearchCompleted,
    sourceActivity, setSourceActivity,
    sourcePreviewCandidate, setSourcePreviewCandidate,
    sourceRetry, setSourceRetry,
    sourcePreview, setSourcePreview,
    sourceImportTokenRef,
    collaborationRef, collaborationGenerationRef, collaborationSyncedRef, collaborationInitialBaselineRef,
    collaborationDocumentJSONRef, collaborationAuthoritativeRef, collaborationStructuralConflictRef,
    collaborationStatus, setCollaborationStatus,
    collaborationPeers, setCollaborationPeers,
    collaborationError, setCollaborationError,
    collaborationStructuralConflict, setCollaborationStructuralConflict,
    confirmSourceImport, setConfirmSourceImport,
    confirmImportRecovery, setConfirmImportRecovery,
    confirmConflictReload, setConfirmConflictReload,
    activeTranslationEditionKey, setActiveTranslationEditionKey,
    editionWorkflow, setEditionWorkflow,
    activeRenditionKey, setActiveRenditionKey,
    activeVersion, setActiveVersion,
    requestSequence, performerSequence, lyricsLoadSequence, selectedMusicIDRef, busyRef,
    localSourceImportDraft, collaborationRequired, writeLocked, writeLockedRef, documentGenerationRef,
    pendingTransition, setPendingTransition,
    pendingAnnotationOperation, setPendingAnnotationOperation,
    projectionStatus, setProjectionStatus,
    projectionState, setProjectionState,
    projectionMessage, setProjectionMessage,
    projectionSequence,
    linesContainerRef, segmentInputRefs,
    dirty, lyricsRef, baselineRef, activeTranslationEditionKeyRef,
  };
}

export type LyricsEditorState = ReturnType<typeof useLyricsEditorState>;

/** Derives the active rendition/side working set the command, persistence and view layers operate on. */
export function useLyricsActiveTarget(state: LyricsEditorState) {
  const { lyrics, performers, activeTranslationEditionKey, activeRenditionKey, activeVersion } = state;
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
  const projectionKind: LyricsProjectionKind = lyrics ? renditionProjectionStatus(lyrics, activeRenditionKey) : "full_only";
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

  return {
    renditionKeys, activeRendition, legacyLyrics, renditionDocument, translationEditions, activeTranslationEdition,
    availableVersions, hasGameVersion, projectionKind, activeSide, activeLines, activePerformerOptions,
    gameSideReadOnlyReason, activeSideReadOnly, activeSideSourceMutable,
    activeTranslationCredit, activeProofreadingCredit, gameProjection, versionSaveProblems, componentProvenance,
    hasPerformerSegmentation,
  };
}

export type LyricsActiveTarget = ReturnType<typeof useLyricsActiveTarget>;
