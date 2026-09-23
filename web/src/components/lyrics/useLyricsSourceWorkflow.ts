"use client";

import { useToast } from "@/app/providers";
import type { LyricsDocumentCommands } from "@/components/lyrics/useLyricsDocumentCommands";
import type { LyricsDocumentLoader } from "@/components/lyrics/useLyricsDocumentLoader";
import type { LyricsEditorState } from "@/components/lyrics/lyricsEditorState";
import { buildLyricsLinesFromSourcePreview } from "@/lib/lyrics-source-import.mjs";
import { isRenditionLyricsDocument } from "@/lib/lyrics-versioning.mjs";
import {
  APIError, LyricsSourceCandidate,
  previewLyricsSource, searchLyricsSource,
} from "@/lib/api";

/** Runs the admin-only verified source search, pinned-revision preview and one-time draft import. */
export function useLyricsSourceWorkflow(
  state: LyricsEditorState,
  loader: LyricsDocumentLoader,
  commands: LyricsDocumentCommands,
  role: "admin" | "editor" | "",
) {
  const { show } = useToast();
  const {
    lyrics, performers, setBusy, busyRef, writeLockedRef, lyricsLoadSequence, setError,
    setSourceActivity, setSourceRetry, setSourcePreviewCandidate, setSourcePreview, sourcePreview,
    sourceImportTokenRef, setConfirmSourceImport, setSourceSearchCompleted, setCandidates,
  } = state;
  const { requestIsCurrent, stopCollaboration } = loader;
  const { updateLyrics } = commands;

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

  return { findSource, previewSource, acceptPreview };
}

export type LyricsSourceWorkflow = ReturnType<typeof useLyricsSourceWorkflow>;
