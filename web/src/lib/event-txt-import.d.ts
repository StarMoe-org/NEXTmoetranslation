import type { EventEpisodeSnapshot, SideStoryEpisodeSnapshot, SideStoryKind, SideStoryLineEdit, SideStoryLocale } from "./api";

export type EventTxtImportStatus = "matched" | "conflict" | "missing" | "unmatched";
export type EventTxtImportTarget = "body" | "speaker" | "structure";

export interface ParsedEventTxtTalk {
  idx: number;
  speaker: string;
  text: string;
  start: boolean;
  end: boolean;
  checked: boolean;
  save: boolean;
  dstidx: number;
}

export interface EventTxtImportPreviewRow {
  id: string;
  status: EventTxtImportStatus;
  target: EventTxtImportTarget;
  sourceOrder?: number;
  importedLine?: number;
  segmentId?: string;
  segmentPosition?: number;
  sourceHash?: string;
  revision?: number;
  speaker: string;
  japanese: string;
  current: string;
  imported: string;
  reason: string;
  selectable: boolean;
  selectedByDefault: boolean;
}

export interface EventTxtImportPreview {
  revision: string;
  rows: EventTxtImportPreviewRow[];
  counts: Record<EventTxtImportStatus, number>;
}

export function parseEventTxtContent(content: string): ParsedEventTxtTalk[];
export function validateEventEpisodeSnapshot(snapshot: EventEpisodeSnapshot): Promise<void>;
export function eventEpisodeTxtImportPreview(snapshot: EventEpisodeSnapshot, talks: readonly ParsedEventTxtTalk[]): EventTxtImportPreview;
export function validateSideStoryEpisodeSnapshot(
  snapshot: SideStoryEpisodeSnapshot,
  expected: { kind: SideStoryKind; id: string; episode: string; locale: SideStoryLocale },
): Promise<void>;
export function sideStoryEpisodeTxtImportPreview(snapshot: SideStoryEpisodeSnapshot, talks: readonly ParsedEventTxtTalk[]): EventTxtImportPreview;
export function sideStoryTxtImportEdits(preview: EventTxtImportPreview, selectedRowIDs: ReadonlySet<string>): SideStoryLineEdit[];
