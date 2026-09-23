"use client";

import { useCallback, useEffect } from "react";
import { useToast } from "@/app/providers";
import { collaborationColor, editableLyricsDocument } from "@/components/lyrics/lyricsDocumentModel";
import type { LyricsEditorState } from "@/components/lyrics/lyricsEditorState";
import { selectTranslationEditionKey, translationEditionURLHint } from "@/lib/lyrics-editions.mjs";
import { isRenditionLyricsDocument, retainedLyricsTranslationTarget } from "@/lib/lyrics-versioning.mjs";
import {
  APIError, CatalogMusicItem, RenditionLyricsDocument, SongLyricsDocument,
  getCatalogMusic, getCatalogPerformers, getClientID, getUsername,
  getLyrics, issueLyricsCollabTicket, subscribeSessionChanged,
} from "@/lib/api";
import { LyricsCollaboration, canonicalLyricsJSON } from "@/lib/yjs-lyrics";

/** Owns catalog and performer loading, song selection, the Yjs provider lifecycle and the stale-load fence. */
export function useLyricsDocumentLoader(state: LyricsEditorState) {
  const { show } = useToast();
  const {
    query, setCatalog, setCatalogLoading, setCatalogError,
    setPerformers, setPerformerError,
    selectedMusic, setSelectedMusic, setLyrics, setBaseline, setLoading, setError,
    setRuntimeOnlyMissingDatabaseSource, setDatabaseAvailabilityOnly,
    setCandidates, setSourceSearchCompleted, setSourceActivity, setSourcePreviewCandidate,
    setSourceRetry, setSourcePreview, sourceImportTokenRef,
    setConfirmSourceImport, setConfirmImportRecovery, setConfirmConflictReload,
    collaborationRef, collaborationGenerationRef, collaborationSyncedRef, collaborationInitialBaselineRef,
    collaborationDocumentJSONRef, collaborationAuthoritativeRef, collaborationStructuralConflictRef,
    setCollaborationStatus, setCollaborationPeers, setCollaborationError, setCollaborationStructuralConflict,
    setActiveTranslationEditionKey, activeTranslationEditionKeyRef,
    activeRenditionKey, setActiveRenditionKey, activeVersion, setActiveVersion,
    setEditionWorkflow, setPendingTransition, setPendingAnnotationOperation,
    setProjectionStatus, setProjectionState, setProjectionMessage, projectionSequence,
    requestSequence, performerSequence, lyricsLoadSequence, selectedMusicIDRef,
    busyRef, dirty, lyricsRef, documentGenerationRef,
  } = state;

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
  }, [requestSequence, setCatalog, setCatalogError, setCatalogLoading, show]);

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
  }, [performerSequence, setPerformerError, setPerformers, show]);

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
  }, [collaborationAuthoritativeRef, collaborationDocumentJSONRef, collaborationGenerationRef, collaborationInitialBaselineRef, collaborationRef, collaborationStructuralConflictRef, collaborationSyncedRef, setCollaborationError, setCollaborationPeers, setCollaborationStatus, setCollaborationStructuralConflict]);

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
  }, [collaborationAuthoritativeRef, collaborationDocumentJSONRef, collaborationGenerationRef, collaborationInitialBaselineRef, collaborationRef, collaborationStructuralConflictRef, collaborationSyncedRef, documentGenerationRef, selectedMusicIDRef, setBaseline, setCollaborationError, setCollaborationPeers, setCollaborationStatus, setCollaborationStructuralConflict, setError, setLoading, setLyrics]);

  useEffect(() => () => stopCollaboration(), [stopCollaboration]);
  useEffect(() => subscribeSessionChanged(() => collaborationRef.current?.reconnectNow()), [collaborationRef]);

  useEffect(() => {
    const timer = window.setTimeout(() => { void loadCatalog(query); }, 200);
    return () => window.clearTimeout(timer);
  }, [loadCatalog, query]);

  useEffect(() => { void loadPerformers(); }, [loadPerformers]);

  const requestIsCurrent = useCallback((sequence: number, musicID: number) =>
    lyricsLoadSequence.current === sequence && selectedMusicIDRef.current === musicID, [lyricsLoadSequence, selectedMusicIDRef]);

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
  }, [documentGenerationRef, replaceEditionURL, setActiveRenditionKey, setActiveTranslationEditionKey, setActiveVersion, setBaseline, setLyrics]);

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
  }, [acceptAuthoritativeDocument, activeRenditionKey, activeTranslationEditionKeyRef, activeVersion, busyRef, collaborationAuthoritativeRef, documentGenerationRef, lyricsLoadSequence, lyricsRef, projectionSequence, requestIsCurrent, selectedMusicIDRef, setActiveRenditionKey, setActiveTranslationEditionKey, setActiveVersion, setBaseline, setCandidates, setConfirmConflictReload, setConfirmImportRecovery, setConfirmSourceImport, setDatabaseAvailabilityOnly, setEditionWorkflow, setError, setLoading, setLyrics, setPendingAnnotationOperation, setProjectionMessage, setProjectionState, setProjectionStatus, setRuntimeOnlyMissingDatabaseSource, setSelectedMusic, setSourceActivity, setSourcePreview, setSourcePreviewCandidate, setSourceRetry, setSourceSearchCompleted, sourceImportTokenRef, startCollaboration, stopCollaboration]);

  const chooseMusic = (item: CatalogMusicItem) => {
    if (busyRef.current) return;
    if (item.musicId === selectedMusic?.musicId) return;
    if (dirty) {
      setPendingTransition({ kind: "choose", item });
      return;
    }
    void performChooseMusic(item);
  };

  return {
    loadCatalog, loadPerformers, stopCollaboration, startCollaboration,
    requestIsCurrent, replaceEditionURL, acceptAuthoritativeDocument, performChooseMusic, chooseMusic,
  };
}

export type LyricsDocumentLoader = ReturnType<typeof useLyricsDocumentLoader>;
