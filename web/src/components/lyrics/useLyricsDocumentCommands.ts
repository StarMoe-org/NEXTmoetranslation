"use client";

import { useToast } from "@/app/providers";
import { isLegacyLyricsDocument } from "@/components/lyrics/lyricsDocumentModel";
import {
  lineWithEditablePatch, linesWithLineMoved, linesWithPerformerApplied, nextTranslationCredits,
  orderedLyricsLines, renditionSideLinesPatch, segmentsWithSegmentMoved, segmentsWithSegmentRemoved,
} from "@/components/lyrics/lyricsDocumentCommands";
import type { LyricsActiveTarget, LyricsEditorState } from "@/components/lyrics/lyricsEditorState";
import type {
  LyricLine, LyricsEditorLine, LyricsEditorSegment, LyricsPerformerID, LyricsRendition,
  RenditionLyricsDocument, SongLyrics, SongLyricsDocument,
} from "@/lib/api";
import {
  lyricGraphemeMidpoint, mergeAdjacentLyricRubySpans, mergeAdjacentLyricSegments, replaceLyricRubySpan,
  replaceLyricSegmentText, splitLyricRubySpanAt, splitLyricSegmentAt,
} from "@/lib/lyrics-segmentation.mjs";
import { referencedGameFullLineIds } from "@/lib/lyrics-versioning.mjs";
import { canonicalLyricsJSON } from "@/lib/yjs-lyrics";

/** Applies line, segment, ruby and performer edits to the shared document, through Yjs when collaboration owns it. */
export function useLyricsDocumentCommands(state: LyricsEditorState, target: LyricsActiveTarget) {
  const { show } = useToast();
  const {
    lyrics, setLyrics, setError, performers, busyRef, writeLocked, localSourceImportDraft,
    collaborationRef, collaborationDocumentJSONRef, documentGenerationRef,
    pendingAnnotationOperation, setPendingAnnotationOperation,
    activeRenditionKey, activeVersion, segmentInputRefs, linesContainerRef,
  } = state;
  const { activeRendition, activeSide, activeLines, activeSideReadOnly, activeSideSourceMutable, recoveryLedgerOwned } = target;

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
    updateActiveRendition({ translationCredits: nextTranslationCredits(activeRendition.translationCredits, field, value) });
  };

  const replaceActiveLines = (lines: LyricsEditorLine[]) => {
    if (!lyrics) return;
    const ordered = orderedLyricsLines(lines);
    if (isLegacyLyricsDocument(lyrics)) {
      updateLyrics({ lines: ordered as LyricLine[] });
      return;
    }
    if (!activeRendition || !activeSide) return;
    updateActiveRendition(renditionSideLinesPatch(activeRendition, activeSide, activeVersion, ordered, performers));
  };

  const updateLine = (index: number, patch: Partial<LyricsEditorLine>) => {
    if (!lyrics || activeSideReadOnly) return;
    if (recoveryLedgerOwned && Object.keys(patch).some((key) => key !== "zh-CN" && key !== "en-US")) return;
    replaceActiveLines(activeLines.map((line, lineIndex) =>
      lineIndex === index ? lineWithEditablePatch(line, patch) : line) as LyricsEditorLine[]);
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
    setSegments(lineIndex, segments, activeSideSourceMutable);
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
    setSegments(lineIndex, result.segments, activeSideSourceMutable);
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
    const removal = segmentsWithSegmentRemoved(activeLines[lineIndex].segments, segmentIndex);
    if (!removal) return;
    setSegments(lineIndex, removal.segments);
    focusSegment(lineIndex, removal.mergeIndex, segmentIndex > 0 ? "end" : "start");
  };

  const moveSegment = (lineIndex: number, segmentIndex: number, direction: -1 | 1) => {
    if (!lyrics || !activeSideSourceMutable) return;
    const segments = segmentsWithSegmentMoved(activeLines[lineIndex].segments, segmentIndex, direction);
    if (!segments) return;
    setSegments(lineIndex, segments, true);
  };

  const addLine = () => {
    if (!lyrics || !activeSideSourceMutable) return;
    const order = activeLines.length;
    const line: LyricsEditorLine = {
      id: `manual-${lyrics.musicId}-${activeRenditionKey || "legacy"}-${activeVersion}-${Date.now()}-${order}`,
      order, japanese: "", "zh-CN": "", "en-US": "", segments: [{ text: "", performerIds: [], ruby: [] }],
      ...(activeRendition ? { trailingPerformerIds: [] } : {}),
    } as LyricsEditorLine;
    replaceActiveLines([...activeLines, line]);
  };

  const removeLine = (lineIndex: number) => {
    if (!lyrics || !activeSideSourceMutable || activeLines.length <= 1) return;
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
    if (!lyrics || !activeSideSourceMutable) return;
    const lines = linesWithLineMoved(activeLines, lineIndex, direction);
    if (!lines) return;
    replaceActiveLines(lines);
  };

  const applyPerformerToAllSegments = (performerID: LyricsPerformerID) => {
    if (!lyrics || activeSideReadOnly || String(performerID) === "") return;
    replaceActiveLines(linesWithPerformerApplied(activeLines, performerID));
  };

  return {
    updateLyrics, updateActiveCredits, updateLine, updateSegment, addSegment, splitSegment,
    mergeWithPreviousSegment, updateRubySpan, splitRubySpan, mergeRubyWithPrevious, confirmAnnotationOperation,
    removeSegment, moveSegment, addLine, removeLine, moveLine, applyPerformerToAllSegments,
  };
}

export type LyricsDocumentCommands = ReturnType<typeof useLyricsDocumentCommands>;
