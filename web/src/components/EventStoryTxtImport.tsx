"use client";

import React, { useEffect, useMemo, useRef, useState } from "react";
import { useToast } from "@/app/providers";
import { Modal } from "@/components/Modal";
import {
  getEventEpisodeSnapshot,
  type TranslationEntry,
} from "@/lib/api";
import {
  eventEpisodeTxtImportPreview,
  parseEventTxtContent,
  validateEventEpisodeSnapshot,
} from "@/lib/event-txt-import.mjs";
import type { EventTxtImportPreview, EventTxtImportPreviewRow } from "@/lib/event-txt-import";

export interface EventStoryTxtDraftTranslation {
  segmentId: string;
  sourceHash: string;
  revision: number;
  authoritativeText: string;
  text: string;
}

export interface EventStoryTxtDraft {
  eventId: number;
  episodeNo: string;
  locale: "zh-CN" | "en-US";
  snapshotRevision: string;
  fileName: string;
  undoAvailable: boolean;
  translations: EventStoryTxtDraftTranslation[];
}

interface Props {
  eventId: number;
  locale: "zh-CN" | "en-US";
  entries: readonly TranslationEntry[];
  defaultEpisodeNo?: string;
  disabled?: boolean;
  onApply: (draft: EventStoryTxtDraft) => void;
}

const MAX_EVENT_TXT_BYTES = 768 << 10;

function episodeOrder(left: string, right: string): number {
  return left.localeCompare(right, undefined, { numeric: true });
}

function statusLabel(status: EventTxtImportPreviewRow["status"]): string {
  return status === "matched" ? "已匹配" : status === "conflict" ? "冲突" : status === "missing" ? "缺失" : "未匹配";
}

function targetLabel(target: EventTxtImportPreviewRow["target"]): string {
  return target === "body" ? "正文" : target === "speaker" ? "说话人" : "结构";
}

export async function readUTF8File(file: File): Promise<string> {
  if (file.size > MAX_EVENT_TXT_BYTES) throw new Error("TXT 文件超过 768 KiB 的本地草稿上限");
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(await file.arrayBuffer());
  } catch {
    throw new Error("TXT 文件不是有效 UTF-8 文本");
  }
}

// Shared by the event and side-story importers; `note` sits between the counts and the rows.
export function TxtImportPreviewTable({ preview, fileName, selectedRows, onToggle, note }: {
  preview: EventTxtImportPreview;
  fileName: string;
  selectedRows: ReadonlySet<string>;
  onToggle: (row: EventTxtImportPreviewRow) => void;
  note: React.ReactNode;
}) {
  return <>
    <div className="event-txt-import-summary">
      <strong>{fileName}</strong>
      <span>已匹配 {preview.counts.matched}</span>
      <span>冲突 {preview.counts.conflict}</span>
      <span>缺失 {preview.counts.missing}</span>
      <span>未匹配 {preview.counts.unmatched}</span>
    </div>
    {note}
    <div className="event-txt-import-table-wrap">
      <table className="event-txt-import-table">
        <thead><tr><th>应用</th><th>状态</th><th>位置</th><th>日文 / 当前</th><th>TXT 译文</th><th>说明</th></tr></thead>
        <tbody>
          {preview.rows.map((row) => (
            <tr key={row.id} className={`event-txt-import-${row.status}`}>
              <td><input type="checkbox" aria-label={`选择 ${row.id}`} checked={selectedRows.has(row.id)} disabled={!row.selectable} onChange={() => onToggle(row)} /></td>
              <td><span className={`event-txt-import-status ${row.status}`}>{statusLabel(row.status)}</span></td>
              <td>{row.importedLine ? `TXT ${row.importedLine}` : "—"}<br /><span className="event-txt-import-muted">{targetLabel(row.target)}</span></td>
              <td><div>{row.japanese || "—"}</div>{row.current && <div className="event-txt-import-current">当前：{row.current}</div>}</td>
              <td>{row.imported || "—"}</td>
              <td>{row.reason}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  </>;
}

function assertSnapshotMatchesLoaded(entries: readonly TranslationEntry[], eventId: number, episodeNo: string, snapshot: Awaited<ReturnType<typeof getEventEpisodeSnapshot>>) {
  if (snapshot.eventId !== eventId || snapshot.episodeNo !== episodeNo) {
    throw new Error("活动剧情权威快照身份不匹配，请重新加载");
  }
  const loaded = new Map(entries
    .filter((entry) => entry.episodeNo === episodeNo && entry.segmentId)
    .map((entry) => [entry.segmentId as string, entry]));
  if (loaded.size !== snapshot.segments.length) {
    throw new Error("当前活动剧情不是完整权威章节；请重新加载后再导入");
  }
  for (const segment of snapshot.segments) {
    const entry = loaded.get(segment.id);
    if (!entry || entry.japanese !== segment.japanese || entry.sourceHash !== segment.sourceHash ||
        (entry.revision ?? 0) !== (segment.revision ?? 0) || entry.text !== segment.text) {
      throw new Error(`当前章节条目 ${segment.id} 已不等于权威快照，请重新加载后再导入`);
    }
  }
}

export function EventStoryTxtImport({ eventId, locale, entries, defaultEpisodeNo, disabled = false, onApply }: Props) {
  const { show } = useToast();
  const inputRef = useRef<HTMLInputElement>(null);
  const requestRef = useRef(0);
  const episodesKey = useMemo(() => [...new Set(entries.flatMap((entry) => entry.episodeNo ? [entry.episodeNo] : []))].sort(episodeOrder).join("\u0001"), [entries]);
  const episodes = useMemo(() => episodesKey ? episodesKey.split("\u0001") : [], [episodesKey]);
  const [episodeNo, setEpisodeNo] = useState(defaultEpisodeNo || episodes[0] || "");
  const [preview, setPreview] = useState<EventTxtImportPreview | null>(null);
  const [selectedRows, setSelectedRows] = useState<Set<string>>(new Set());
  const [fileName, setFileName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    const availableEpisodes = episodesKey ? episodesKey.split("\u0001") : [];
    const preferred = defaultEpisodeNo && availableEpisodes.includes(defaultEpisodeNo) ? defaultEpisodeNo : availableEpisodes[0] || "";
    setEpisodeNo(preferred);
    setPreview(null);
    setSelectedRows(new Set());
    setFileName("");
    setError("");
    requestRef.current++;
  }, [eventId, locale, defaultEpisodeNo, episodesKey]);

  const closePreview = () => {
    requestRef.current++;
    setPreview(null);
    setSelectedRows(new Set());
    setFileName("");
    setError("");
    setBusy(false);
  };

  const chooseFile = () => {
    if (disabled || busy || !episodeNo) return;
    inputRef.current?.click();
  };

  const previewFile = async (file: File) => {
    if (disabled || !episodeNo) return;
    const request = ++requestRef.current;
    setBusy(true);
    setError("");
    try {
      const [content, snapshot] = await Promise.all([
        readUTF8File(file),
        getEventEpisodeSnapshot(eventId, episodeNo, locale),
      ]);
      if (requestRef.current !== request) return;
      await validateEventEpisodeSnapshot(snapshot);
      if (requestRef.current !== request) return;
      assertSnapshotMatchesLoaded(entries, eventId, episodeNo, snapshot);
      const nextPreview = eventEpisodeTxtImportPreview(snapshot, parseEventTxtContent(content));
      if (requestRef.current !== request) return;
      setPreview(nextPreview);
      setSelectedRows(new Set(nextPreview.rows.filter((row) => row.selectable && row.selectedByDefault).map((row) => row.id)));
      setFileName(file.name || "import.txt");
    } catch (reason) {
      if (requestRef.current !== request) return;
      setPreview(null);
      setSelectedRows(new Set());
      setFileName("");
      setError(reason instanceof Error ? reason.message : "活动剧情 TXT 导入预览失败");
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

  const apply = () => {
    if (!preview || disabled) return;
    const translations = preview.rows.flatMap((row) => row.selectable && selectedRows.has(row.id) && row.segmentId && row.sourceHash !== undefined
      ? [{
          segmentId: row.segmentId,
          sourceHash: row.sourceHash,
          revision: row.revision ?? 0,
          authoritativeText: row.current,
          text: row.imported,
        }]
      : []);
    if (translations.length === 0) {
      setError("请至少选择一条可应用的译文");
      return;
    }
    onApply({
      eventId,
      episodeNo,
      locale,
      snapshotRevision: preview.revision,
      fileName,
      undoAvailable: true,
      translations,
    });
    show(`已将 ${translations.length} 条译文放入本地未保存草稿；请检查后使用现有保存按钮逐条提交`, "ok");
    closePreview();
  };

  const selectedCount = preview?.rows.filter((row) => row.selectable && selectedRows.has(row.id)).length ?? 0;

  return (
    <>
      <div className="event-txt-import-actions">
        <label htmlFor="event-txt-episode">导入章节</label>
        <select id="event-txt-episode" value={episodeNo} onChange={(event) => setEpisodeNo(event.target.value)} disabled={disabled || busy || episodes.length === 0}>
          {episodes.map((episode) => <option key={episode} value={episode}>第 {episode} 章</option>)}
        </select>
        <button type="button" className="btn btn-secondary btn-sm" onClick={chooseFile} disabled={disabled || busy || !episodeNo}>
          {busy ? "正在核对 TXT…" : "导入 TXT"}
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

      <Modal open={preview != null || error !== ""} onClose={closePreview} title="活动剧情 TXT 导入预览" maxWidth={960} closeDisabled={busy}>
        {error && <div className="event-txt-import-error" role="alert">{error}</div>}
        {preview && <>
          <TxtImportPreviewTable preview={preview} fileName={fileName} selectedRows={selectedRows} onToggle={toggleRow} note={
            <p className="dirty-guard-copy">仅默认选择空白译文字段。已有译文不同的行会标记为冲突，必须手动勾选才会覆盖到本地草稿；此步骤不会写入服务器。</p>
          } />
          <div className="dirty-guard-actions">
            <span className="event-txt-import-selection">已选择 {selectedCount} 条</span>
            <button type="button" className="btn btn-ghost" onClick={closePreview}>取消</button>
            <button type="button" className="btn btn-primary" onClick={apply} disabled={selectedCount === 0}>应用到本地草稿</button>
          </div>
        </>}
      </Modal>
    </>
  );
}
