export type ShowToast = (msg: string, type?: "ok" | "err") => void;

export type ReconciliationReason = "restore" | "gap" | "remote";

export interface ContentConflict {
  reason: ReconciliationReason;
  draft: string | null;
  reloadFailed: boolean;
  detail?: string;
  storageKey?: string;
}

export interface ChapterTab {
  episodeNo: string;
  title: string;
  total: number;
  untranslated: number;
}
