import type { TranslationEntry } from "./api";

/** The text the server stores for value: whitespace-only text is stored as "". */
export function sideStoryLineText(value: string): string {
  return value.trim() === "" ? "" : value;
}

/**
 * True when saving value to a line the server has never stored changes nothing; the PUT
 * would store a permanent empty human line that AI fill and official import never replace.
 */
export function sideStoryLineSaveIsNoop(entry: Pick<TranslationEntry, "text" | "revision">, value: string): boolean {
  if ((entry.revision ?? 0) !== 0) return false;
  return sideStoryLineText(value) === sideStoryLineText(entry.text);
}
