import type { RenditionLyricsDocument, SongLyrics, SongLyricsDocument } from "./api";

export type LyricsSavePayload =
  | (SongLyrics & { sourceImportToken?: string; clientId: string })
  | (Omit<RenditionLyricsDocument, "defaultTranslationEditionKey" | "translationEditions" | "recoveryLedgerOwned"> & {
      sourceImportToken?: string;
      clientId: string;
    });

export function buildLyricsSavePayload(
  lyrics: SongLyricsDocument,
  sourceImportToken: string | undefined,
  clientId: string,
): LyricsSavePayload;

export type LyricsMutationExpectation =
  | { operation: "save"; musicId: number; revision: number; document: SongLyricsDocument; editionKey?: string }
  | { operation: "edition"; musicId: number; revision: number; editionKey: string; document?: never }
  | { operation: "conflict"; musicId: number; revision: number; editionKey?: string; document?: never }
  | { operation: "publish" | "unpublish"; musicId: number; revision: number; editionKey?: never; document?: never };

export type SongLyricsMutationValidationResult =
  | { ok: true; value: SongLyricsDocument }
  | { ok: false; details: string[] };

export function validateSongLyricsMutationResponse(
  value: unknown,
  expectation: LyricsMutationExpectation,
): SongLyricsMutationValidationResult;

export function validateSongLyricsCheckpointResponse(
  value: unknown,
  musicId: number,
): SongLyricsMutationValidationResult;
