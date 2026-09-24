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
import { LyricsCollaboration, canonicalLyricsJSON, lyricsAuthorityEnvelopeKey } from "@/lib/yjs-lyrics";

const translationEditionKeyOf = (document: SongLyricsDocument) =>
  isRenditionLyricsDocument(document) ? (document as RenditionLyricsDocument).translationEditionKey : "";

// Rooms are seeded from the default translation edition of the stored document; while the
// shared document still carries that document's envelope, any other difference is unsaved.
function roomHoldsStoredDocument(stored: SongLyricsDocument | null, shared: SongLyricsDocument): stored is SongLyricsDocument {
  return stored != null && lyricsAuthorityEnvelopeKey(stored) === lyricsAuthorityEnvelopeKey(shared) &&
    translationEditionKeyOf(stored) === translationEditionKeyOf(shared);
}

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
    collaborationRef, collaborationGenerationRef, collaborationSyncedRef, collaborationBaselineEnvelopeRef,
    collaborationDocumentJSONRef, collaborationAuthoritativeRef, collaborationStructuralConflictRef,
    setCollaborationStatus, setCollaborationPeers, setCollaborationError, setCollaborationStructuralConflict,
    setCollaborationLocalChanges,
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
    collaborationBaselineEnvelopeRef.current = null;
    collaborationDocumentJSONRef.current = "";
    collaborationAuthoritativeRef.current = null;
    collaborationStructuralConflictRef.current = false;
    setCollaborationPeers([]);
    setCollaborationError("");
    setCollaborationStructuralConflict(false);
    setCollaborationLocalChanges(false);
    setCollaborationStatus("offline");
  }, [collaborationAuthoritativeRef, collaborationBaselineEnvelopeRef, collaborationDocumentJSONRef, collaborationGenerationRef, collaborationRef, collaborationStructuralConflictRef, collaborationSyncedRef, setCollaborationError, setCollaborationLocalChanges, setCollaborationPeers, setCollaborationStatus, setCollaborationStructuralConflict]);

  const replaceEditionURL = useCallback((editionKey: string) => {
    if (typeof window === "undefined") return;
    const url = new URL(window.location.href);
    if (editionKey) url.searchParams.set("edition", editionKey);
    else url.searchParams.delete("edition");
    window.history.replaceState(window.history.state, "", `${url.pathname}${url.search}${url.hash}`);
  }, []);

  const startCollaboration = useCallback((musicID: number) => {
    const generation = ++collaborationGenerationRef.current;
    collaborationRef.current?.destroy();
    collaborationRef.current = null;
    collaborationSyncedRef.current = false;
    collaborationBaselineEnvelopeRef.current = null;
    collaborationDocumentJSONRef.current = "";
    collaborationStructuralConflictRef.current = false;
    setCollaborationPeers([]);
    setCollaborationError("");
    setCollaborationStructuralConflict(false);
    setCollaborationLocalChanges(false);
    setCollaborationStatus("connecting");
    const clientId = getClientID();
    const username = getUsername();
    // Stored content for the current authority envelope once read back from the server.
    let storedJSON: string | null = null;
    // The server snapshots the room before it broadcasts a new envelope, and the shared
    // content the envelope arrives with can hold edits made in between: read the stored
    // document so they stay savable, and dirty for the client that made them.
    const adoptStoredBaseline = async (shared: SongLyricsDocument, sharedJSON: string) => {
      const envelope = lyricsAuthorityEnvelopeKey(shared);
      let stored: SongLyricsDocument;
      try {
        stored = editableLyricsDocument(await getLyrics(musicID, translationEditionKeyOf(shared) || undefined));
      } catch {
        return;
      }
      if (collaborationGenerationRef.current !== generation || selectedMusicIDRef.current !== musicID ||
          collaborationBaselineEnvelopeRef.current !== envelope || !roomHoldsStoredDocument(stored, shared)) return;
      storedJSON = canonicalLyricsJSON(stored);
      collaborationAuthoritativeRef.current = stored;
      setBaseline(storedJSON);
      if (sharedJSON === storedJSON || collaborationDocumentJSONRef.current === storedJSON) {
        collaborationRef.current?.settleRetainedLocalChanges();
      }
    };
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
          setCollaborationLocalChanges(false);
          setCollaborationStatus("error");
          setCollaborationPeers([]);
          setCollaborationError("");
          const authoritative = collaborationAuthoritativeRef.current;
          if (authoritative) {
            const editable = editableLyricsDocument(authoritative);
            const serialized = canonicalLyricsJSON(editable);
            collaborationBaselineEnvelopeRef.current = lyricsAuthorityEnvelopeKey(editable);
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
        setCollaborationLocalChanges(snapshot.localChanges);
        setCollaborationPeers(snapshot.peers);
        setCollaborationError(snapshot.error?.message || "");
        if (snapshot.status === "error" && !snapshot.document) {
          setLoading(false);
          setError(new APIError(503, { error: "lyrics_collaboration_unavailable" }));
        }
        if (!snapshot.synced || !snapshot.document) return;
        const editable = editableLyricsDocument(snapshot.document);
        const serialized = canonicalLyricsJSON(editable);
        const envelope = lyricsAuthorityEnvelopeKey(editable);
        if (collaborationBaselineEnvelopeRef.current === null) {
          collaborationBaselineEnvelopeRef.current = envelope;
          const stored = collaborationAuthoritativeRef.current;
          setBaseline(roomHoldsStoredDocument(stored, editable) ? canonicalLyricsJSON(stored) : serialized);
        } else if (collaborationBaselineEnvelopeRef.current !== envelope) {
          // A checkpoint, publication or reseed moved the envelope. Undoing earlier history
          // would now revert stored content; the shared content stands in as the baseline
          // until the stored document is read.
          collaborationBaselineEnvelopeRef.current = envelope;
          storedJSON = null;
          collaborationRef.current?.retireLocalHistory();
          setBaseline(serialized);
          void adoptStoredBaseline(editable, serialized);
        }
        if (collaborationDocumentJSONRef.current !== serialized) {
          collaborationDocumentJSONRef.current = serialized;
          documentGenerationRef.current++;
          setLyrics(editable);
          // A room holds one translation edition at a time, and saving writes that one.
          const roomEditionKey = translationEditionKeyOf(editable);
          if (roomEditionKey && roomEditionKey !== activeTranslationEditionKeyRef.current) {
            setActiveTranslationEditionKey(roomEditionKey);
            replaceEditionURL(roomEditionKey);
          }
        }
        setLoading(false);
        setError(null);
        if (storedJSON === serialized) collaborationRef.current?.settleRetainedLocalChanges();
      },
    });
    collaborationRef.current = collaboration;
  }, [activeTranslationEditionKeyRef, collaborationAuthoritativeRef, collaborationBaselineEnvelopeRef, collaborationDocumentJSONRef, collaborationGenerationRef, collaborationRef, collaborationStructuralConflictRef, collaborationSyncedRef, documentGenerationRef, replaceEditionURL, selectedMusicIDRef, setActiveTranslationEditionKey, setBaseline, setCollaborationError, setCollaborationLocalChanges, setCollaborationPeers, setCollaborationStatus, setCollaborationStructuralConflict, setError, setLoading, setLyrics]);

  useEffect(() => () => {
    // Retire in-flight loads so a late getLyrics cannot start a connection after unmount.
    lyricsLoadSequence.current++;
    stopCollaboration();
  }, [lyricsLoadSequence, stopCollaboration]);
  useEffect(() => subscribeSessionChanged(() => collaborationRef.current?.reconnectNow()), [collaborationRef]);

  useEffect(() => {
    const timer = window.setTimeout(() => { void loadCatalog(query); }, 200);
    return () => window.clearTimeout(timer);
  }, [loadCatalog, query]);

  useEffect(() => { void loadPerformers(); }, [loadPerformers]);

  const requestIsCurrent = useCallback((sequence: number, musicID: number) =>
    lyricsLoadSequence.current === sequence && selectedMusicIDRef.current === musicID, [lyricsLoadSequence, selectedMusicIDRef]);

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
