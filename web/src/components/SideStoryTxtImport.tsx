"use client";

import { useEffect, useRef, useState } from "react";
import { useToast } from "@/app/providers";
import { Modal } from "@/components/Modal";
import { TxtImportPreviewTable, readUTF8File } from "@/components/EventStoryTxtImport";
import type { SideStoryBatchOutcome } from "@/components/console/useEntryEditor";
import {
  getSideStoryEpisodeSnapshot,
  type SideStoryKind, type SideStoryLineConflict, type SideStoryLineEdit, type SideStoryLocale, type TranslationEntry,
} from "@/lib/api";
import {
  parseEventTxtContent,
  sideStoryEpisodeTxtImportPreview,
  sideStoryTxtImportEdits,
  validateSideStoryEpisodeSnapshot,
} from "@/lib/event-txt-import.mjs";
import type { EventTxtImportPreview, EventTxtImportPreviewRow } from "@/lib/event-txt-import";
import { sideStoryErrorMessage, sideStorySnapshotMismatch } from "@/lib/side-story-console";

interface Props {
  kind: SideStoryKind;
  storyId: string;
  locale: SideStoryLocale;
  entries: readonly TranslationEntry[];
  episodes: readonly { key: string; label: string }[];
  defaultEpisodeKey?: string;
  disabled?: boolean;
  saveBatch: (episodeKey: string, edits: SideStoryLineEdit[]) => Promise<SideStoryBatchOutcome>;
  onReload: () => void;
}

export function SideStoryTxtImport({ kind, storyId, locale, entries, episodes, defaultEpisodeKey, disabled = false, saveBatch, onReload }: Props) {
  const { show } = useToast();
  const inputRef = useRef<HTMLInputElement>(null);
  const requestRef = useRef(0);
  const episodeKeys = episodes.map((episode) => episode.key).join("\u0001");
  const [episodeKey, setEpisodeKey] = useState("");
  const [preview, setPreview] = useState<EventTxtImportPreview | null>(null);
  const [selectedRows, setSelectedRows] = useState<Set<string>>(new Set());
  const [fileName, setFileName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [conflicts, setConflicts] = useState<SideStoryLineConflict[]>([]);

  useEffect(() => {
    const keys = episodeKeys ? episodeKeys.split("\u0001") : [];
    setEpisodeKey(defaultEpisodeKey && keys.includes(defaultEpisodeKey) ? defaultEpisodeKey : keys[0] || "");
    setPreview(null);
    setSelectedRows(new Set());
    setFileName("");
    setError("");
    setConflicts([]);
    setBusy(false);
    requestRef.current++;
  }, [kind, storyId, locale, defaultEpisodeKey, episodeKeys]);

  const close = () => {
    if (busy) return;
    requestRef.current++;
    setPreview(null);
    setSelectedRows(new Set());
    setFileName("");
    setError("");
    setConflicts([]);
  };

  const previewFile = async (file: File) => {
    if (disabled || !episodeKey) return;
    const request = ++requestRef.current;
    const episode = episodeKey;
    setBusy(true);
    setError("");
    setConflicts([]);
    try {
      const [content, snapshot] = await Promise.all([
        readUTF8File(file),
        getSideStoryEpisodeSnapshot(kind, storyId, episode, locale),
      ]);
      if (requestRef.current !== request) return;
      await validateSideStoryEpisodeSnapshot(snapshot, { kind, id: storyId, episode, locale });
      if (requestRef.current !== request) return;
      const mismatch = sideStorySnapshotMismatch(entries, snapshot);
      if (mismatch) throw new Error(mismatch);
      const nextPreview = sideStoryEpisodeTxtImportPreview(snapshot, parseEventTxtContent(content));
      setPreview(nextPreview);
      setSelectedRows(new Set(nextPreview.rows.filter((row) => row.selectable && row.selectedByDefault).map((row) => row.id)));
      setFileName(file.name || "import.txt");
    } catch (reason) {
      if (requestRef.current !== request) return;
      setPreview(null);
      setSelectedRows(new Set());
      setError(sideStoryErrorMessage(reason, "剧情 TXT 导入预览失败"));
    } finally {
      if (requestRef.current === request) setBusy(false);
    }
  };

  const toggleRow = (row: EventTxtImportPreviewRow) => {
    if (!row.selectable) return;
    setSelectedRows((current) => {
      const next = new Set(current);
      if (next.has(row.id)) next.delete(row.id);
      else next.add(row.id);
      return next;
    });
  };

  const save = async () => {
    if (!preview || disabled || busy) return;
    let edits: SideStoryLineEdit[];
    try {
      edits = sideStoryTxtImportEdits(preview, selectedRows);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "TXT 导入选择无效");
      return;
    }
    if (edits.length === 0) {
      setError("请至少选择一条可保存的译文");
      return;
    }
    const request = ++requestRef.current;
    setBusy(true);
    setError("");
    try {
      const outcome = await saveBatch(episodeKey, edits);
      if (requestRef.current !== request) return;
      if (outcome.status === "saved") {
        show(`TXT 导入已保存 ${outcome.updated} 行${outcome.unchanged ? `，${outcome.unchanged} 行无变化` : ""}`, "ok");
        setBusy(false);
        requestRef.current++;
        setPreview(null);
        setSelectedRows(new Set());
        setFileName("");
      } else if (outcome.status === "conflict") {
        setConflicts(outcome.conflicts);
        setError(`${outcome.conflicts.length} 行在预览之后已被修改，整批未写入。请重新载入本篇后再导入。`);
      } else {
        setError(outcome.message);
      }
    } finally {
      if (requestRef.current === request) setBusy(false);
    }
  };

  const selectedCount = preview?.rows.filter((row) => row.selectable && selectedRows.has(row.id)).length ?? 0;

  return (
    <>
      <div className="event-txt-import-actions">
        <label htmlFor="side-story-txt-episode">导入话数</label>
        <select id="side-story-txt-episode" value={episodeKey} onChange={(event) => setEpisodeKey(event.target.value)} disabled={disabled || busy || episodes.length === 0}>
          {episodes.map((episode) => <option key={episode.key} value={episode.key}>{episode.label}</option>)}
        </select>
        <button type="button" className="btn btn-secondary btn-sm" onClick={() => inputRef.current?.click()} disabled={disabled || busy || !episodeKey}>
          {busy && !preview ? "正在核对 TXT…" : "导入 TXT"}
        </button>
        <input
          ref={inputRef}
          className="sr-only"
          type="file"
          accept=".txt,text/plain"
          tabIndex={-1}
          onChange={(event) => {
            const file = event.target.files?.[0];
            event.target.value = "";
            if (file) void previewFile(file);
          }}
        />
      </div>

      <Modal open={preview != null || error !== ""} onClose={close} title="剧情 TXT 导入预览" maxWidth={960} closeDisabled={busy}>
        {error && <div className="event-txt-import-error" role="alert">{error}</div>}
        {conflicts.length > 0 && (
          <ul className="side-story-conflict-list">
            {conflicts.map((conflict) => (
              <li key={conflict.jp}><span>{conflict.jp}</span><span>服务器当前：{conflict.currentText || "（空）"}（revision {conflict.currentRevision}）</span></li>
            ))}
          </ul>
        )}
        {preview && conflicts.length === 0 && (
          <TxtImportPreviewTable preview={preview} fileName={fileName} selectedRows={selectedRows} onToggle={toggleRow} note={
            <p className="dirty-guard-copy">仅默认选择空白译文字段；已有不同译文的行需手动勾选。保存时所选行以一次批量请求写入服务器，任何一行在预览后被他人修改则整批都不写入。</p>
          } />
        )}
        <div className="dirty-guard-actions">
          {preview && conflicts.length === 0 && <span className="event-txt-import-selection">已选择 {selectedCount} 条</span>}
          {conflicts.length > 0 && <button type="button" className="btn btn-primary" onClick={() => { close(); onReload(); }} disabled={busy}>重新载入本篇</button>}
          <button type="button" className="btn btn-ghost" onClick={close} disabled={busy}>{preview && conflicts.length === 0 ? "取消" : "关闭"}</button>
          {preview && conflicts.length === 0 && (
            <button type="button" className="btn btn-primary" onClick={() => void save()} disabled={busy || disabled || selectedCount === 0}>
              {busy ? "正在保存…" : `保存 ${selectedCount} 条到服务器`}
            </button>
          )}
        </div>
      </Modal>
    </>
  );
}
