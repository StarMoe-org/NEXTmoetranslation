import type { TranslationEntry } from "./api";

/**
 * True when saving value to a line the server has never stored changes nothing; the PUT
 * would store a permanent empty human line that AI fill and official import never replace.
 */
export function sideStoryLineSaveIsNoop(entry: Pick<TranslationEntry, "text" | "revision">, value: string): boolean {
  if ((entry.revision ?? 0) !== 0) return false;
  return value === entry.text || (value.trim() === "" && entry.text.trim() === "");
}
