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

// A collaborator's change to the selected line; `current` is set when the server
// rejected a save because the line's revision had moved on.
export interface RemoteConflict {
  key: string;
  user: string;
  current?: { text: string; revision: number };
}
