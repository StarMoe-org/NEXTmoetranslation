"use client";

import { useRef, useState } from "react";
import { useToast } from "@/app/providers";
import { lyricsRecoveryTakeoverFailure } from "@/components/lyrics/lyricsDocumentModel";
import type { LyricsEditorState } from "@/components/lyrics/lyricsEditorState";
import type { LyricsDocumentLoader } from "@/components/lyrics/useLyricsDocumentLoader";
import { isRenditionLyricsDocument } from "@/lib/lyrics-versioning.mjs";
import {
  APIError, getLyricsDocumentExport, takeOverLyricsDocument,
  type LyricsDocumentWarning, type RenditionLyricsDocument,
} from "@/lib/api";

export type LyricsRecoveryTakeoverPhase = "checking" | "confirm" | "submitting" | "failed" | "done";

export interface LyricsRecoveryTakeoverDialog {
  musicId: number;
  phase: LyricsRecoveryTakeoverPhase;
  /** The revision takeover compares against, read from the export when the dialog opens. */
  expectedRevision: number | null;
  /** The site serves nothing for the song, so there is no public content to convert. */
  notServed: boolean;
  /** Export warnings before the conversion; the takeover's own warnings once it is done. */
  warnings: LyricsDocumentWarning[];
  error: APIError | null;
}

/** Owns the admin action that converts a recovery-ledger song into an editable document. */
export function useLyricsRecoveryTakeover(state: LyricsEditorState, loader: LyricsDocumentLoader, role: "admin" | "editor" | "") {
  const { show } = useToast();
  const { lyricsRef, saveable, selectedMusic, selectedMusicIDRef, busyRef, setBusy, writeLockedRef, query } = state;
  const { performChooseMusic, loadCatalog } = loader;
  const [dialog, setDialog] = useState<LyricsRecoveryTakeoverDialog | null>(null);
  const sequenceRef = useRef(0);

  const open = async () => {
    const current = lyricsRef.current;
    if (role !== "admin" || !current || !isRenditionLyricsDocument(current) ||
        (current as RenditionLyricsDocument).recoveryLedgerOwned !== true || busyRef.current || writeLockedRef.current) return;
    const musicId = current.musicId;
    const sequence = ++sequenceRef.current;
    setDialog({ musicId, phase: "checking", expectedRevision: null, notServed: false, warnings: [], error: null });
    try {
      const exported = await getLyricsDocumentExport(musicId);
      if (sequenceRef.current !== sequence || selectedMusicIDRef.current !== musicId) return;
      const expectedRevision = exported.document?.expectedRevision;
      if (!Number.isSafeInteger(expectedRevision) || Number(expectedRevision) < 0 || !Array.isArray(exported.warnings)) {
        throw new APIError(502, { error: "invalid_lyrics_response", details: ["导出响应没有给出当前 revision"] });
      }
      setDialog({
        musicId, phase: "confirm", expectedRevision: expectedRevision as number,
        notServed: exported.from === "database", warnings: exported.warnings, error: null,
      });
    } catch (reason) {
      if (sequenceRef.current !== sequence || selectedMusicIDRef.current !== musicId) return;
      const error = reason instanceof APIError ? reason : new APIError(500, { error: "load_failed" });
      setDialog({ musicId, phase: "failed", expectedRevision: null, notServed: false, warnings: [], error });
    }
  };

  const confirm = async () => {
    const current = dialog;
    const retrying = current?.phase === "failed" && current.error != null && lyricsRecoveryTakeoverFailure(current.error).retry;
    if (!current || (current.phase !== "confirm" && !retrying) || current.expectedRevision == null || current.notServed || saveable ||
        role !== "admin" || busyRef.current || writeLockedRef.current || selectedMusicIDRef.current !== current.musicId) return;
    const sequence = ++sequenceRef.current;
    setDialog({ ...current, phase: "submitting", error: null });
    busyRef.current = true;
    setBusy(true);
    let warnings: LyricsDocumentWarning[];
    try {
      warnings = (await takeOverLyricsDocument(current.musicId, current.expectedRevision)).warnings;
    } catch (reason) {
      if (sequenceRef.current === sequence) {
        const error = reason instanceof APIError ? reason : new APIError(500, { error: "save_failed" });
        setDialog({ ...current, phase: "failed", error });
      }
      return;
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
    if (sequenceRef.current !== sequence) return;
    setDialog(warnings.length > 0 ? { ...current, phase: "done", warnings, error: null } : null);
    show("已转为可编辑文档", "ok");
    void loadCatalog(query);
    if (selectedMusic && selectedMusicIDRef.current === current.musicId) await performChooseMusic(selectedMusic);
  };

  const close = () => {
    if (dialog?.phase === "submitting") return;
    sequenceRef.current++;
    setDialog(null);
  };

  const reload = async () => {
    if (dialog?.phase === "submitting") return;
    sequenceRef.current++;
    setDialog(null);
    if (selectedMusic) await performChooseMusic(selectedMusic);
  };

  return { dialog, open, confirm, close, reload };
}

export type LyricsRecoveryTakeover = ReturnType<typeof useLyricsRecoveryTakeover>;
