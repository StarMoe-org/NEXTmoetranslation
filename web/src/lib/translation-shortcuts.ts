/** Console actions bound to keys in the translation textarea. */
export type TranslationTextareaAction = "save" | "close" | "previous" | "next";

export interface TranslationTextareaKeyEvent {
  key: string;
  shiftKey: boolean;
  ctrlKey: boolean;
  metaKey: boolean;
  keyCode: number;
  nativeEvent: { isComposing: boolean };
}

/**
 * Maps a translation-textarea keydown to its console action. Keys pressed while an IME is
 * composing belong to the IME: the Enter that confirms a candidate must never save.
 */
export function translationTextareaAction(
  event: TranslationTextareaKeyEvent,
  enterSaves: boolean,
): TranslationTextareaAction | null {
  // Safari ends the composition before the confirming keydown, which then only carries keyCode 229.
  if (event.nativeEvent.isComposing || event.keyCode === 229) return null;
  if (event.key === "Enter") {
    const saves = enterSaves ? !event.shiftKey : event.shiftKey;
    return saves ? "save" : null;
  }
  if (event.key === "Escape") return "close";
  if (event.ctrlKey || event.metaKey) {
    if (event.key === "ArrowUp") return "previous";
    if (event.key === "ArrowDown") return "next";
  }
  return null;
}
