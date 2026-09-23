import type {
  CatalogPerformerItem, LyricsEditorLine, LyricsEditorSegment, LyricsPerformerID,
  LyricsRendition, LyricsRenditionPerformer, LyricsRenditionSide, LyricsRenditionTranslationCredits,
  LyricsRenditionVersion,
} from "@/lib/api";
import { performerRepresentativeColor } from "@/lib/performer-colors.mjs";

export function orderedLyricsLines(lines: LyricsEditorLine[]): LyricsEditorLine[] {
  return lines.map((line, order) => ({ ...line, order })) as LyricsEditorLine[];
}

export function lineWithEditablePatch(line: LyricsEditorLine, patch: Partial<LyricsEditorLine>): LyricsEditorLine {
  const updated = { ...line };
  if (patch["zh-CN"] !== undefined) updated["zh-CN"] = patch["zh-CN"];
  if (patch["en-US"] !== undefined) updated["en-US"] = patch["en-US"];
  if (patch.stanzaBreakBefore !== undefined) updated.stanzaBreakBefore = patch.stanzaBreakBefore;
  if (patch.trailingPerformerIds !== undefined) updated.trailingPerformerIds = patch.trailingPerformerIds;
  if (patch.segments !== undefined) updated.segments = patch.segments;
  return updated;
}

/** Builds the rendition patch for one side, mirroring exact-projection Game rows and backfilling newly used performers. */
export function renditionSideLinesPatch(
  rendition: LyricsRendition,
  side: LyricsRenditionSide,
  version: "full" | "game",
  ordered: LyricsEditorLine[],
  performers: CatalogPerformerItem[],
): Partial<LyricsRendition> {
  const patch: Partial<LyricsRendition> = {
    [version]: { ...side, lines: ordered },
  };
  if (version === "full" && rendition.relation.kind === "exact_projection" && rendition.game) {
    const fullMap = new Map(ordered.map((line) => [line.id, line]));
    const nextGameLines: LyricsEditorLine[] = rendition.game.lines.map((gameLine: LyricsEditorLine) => {
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
    patch.game = { ...rendition.game, lines: nextGameLines } as LyricsRenditionSide;
  }
  const usedPerformerIds = new Set<string>();
  for (const line of ordered) {
    for (const id of (line.trailingPerformerIds || []) as LyricsPerformerID[]) usedPerformerIds.add(String(id));
    for (const seg of line.segments || []) {
      for (const id of (seg.performerIds || []) as LyricsPerformerID[]) usedPerformerIds.add(String(id));
    }
  }
  const currentRenditionIds = new Set(rendition.performers.map((p: LyricsRenditionPerformer) => String(p.performerId)));
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
        // `version` lives on the rendition sides, not on the rendition itself.
        color: performerRepresentativeColor(id, (rendition as LyricsRendition & { version: LyricsRenditionVersion }).version.label),
      };
    });
    patch.performers = [...rendition.performers, ...added];
  }
  return patch;
}

export function nextTranslationCredits(
  credits: LyricsRenditionTranslationCredits | undefined,
  field: "translation" | "proofreading",
  value: string,
): LyricsRenditionTranslationCredits | undefined {
  const next = { ...(credits || {}) };
  if (value) next[field] = value;
  else delete next[field];
  return Object.keys(next).length > 0 ? next : undefined;
}

/** Removes one segment and folds its text, ruby and performers into the adjacent segment that keeps the cursor. */
export function segmentsWithSegmentRemoved(
  source: LyricsEditorSegment[],
  segmentIndex: number,
): { segments: LyricsEditorSegment[]; mergeIndex: number } | null {
  const segments = source.map((segment) => ({ ...segment, performerIds: [...segment.performerIds] })) as LyricsEditorSegment[];
  if (segments.length <= 1) return null;
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
  return { segments, mergeIndex };
}

export function segmentsWithSegmentMoved(
  source: LyricsEditorSegment[],
  segmentIndex: number,
  direction: -1 | 1,
): LyricsEditorSegment[] | null {
  const target = segmentIndex + direction;
  const segments = [...source] as LyricsEditorSegment[];
  if (target < 0 || target >= segments.length) return null;
  [segments[segmentIndex], segments[target]] = [segments[target], segments[segmentIndex]];
  return segments;
}

export function linesWithLineMoved(
  source: LyricsEditorLine[],
  lineIndex: number,
  direction: -1 | 1,
): LyricsEditorLine[] | null {
  const target = lineIndex + direction;
  if (target < 0 || target >= source.length) return null;
  const lines = [...source];
  [lines[lineIndex], lines[target]] = [lines[target], lines[lineIndex]];
  return lines;
}

export function linesWithPerformerApplied(lines: LyricsEditorLine[], performerID: LyricsPerformerID): LyricsEditorLine[] {
  return lines.map((line) => ({
    ...line,
    segments: line.segments.map((segment) => ({ ...segment, performerIds: [performerID] })) as LyricsEditorSegment[],
  })) as LyricsEditorLine[];
}
