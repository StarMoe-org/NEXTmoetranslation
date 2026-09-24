import React, { useMemo, useRef, type RefObject } from "react";
import type { TranslationEntry } from "@/lib/api";
import type { RemoteConflict } from "@/components/console/types";
import { SOURCE_LABELS, buildMoesekaiUrl, storyEntrySourceText } from "@/lib/labels";
import { eventStoryEntryHasCanonicalIdentity, eventStoryEpisodeNo } from "@/lib/event-story-console";
import { SIDE_STORY_EDIT_SOURCES } from "@/lib/side-story-console";
import { sideStoryLineSaveIsNoop } from "@/lib/side-story-editor";
import { EntryRow } from "@/components/console/EntryRow";

const IconExternalLink = () => (
  <svg viewBox="0 0 24 24"><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg>
);

export interface TranslationEntryWorkspaceProps {
  category: string;
  field: string;
  isEventStory: boolean;
  isSideStory?: boolean;
  isReadOnly: boolean;
  loading: boolean;
  saving: boolean;
  writesLocked: boolean;
  filtered: TranslationEntry[];
  selectedKey: string | null;
  selectedEntry: TranslationEntry | null;
  selectedIndex: number;
  selectedEventStoryIdentityMissing: boolean;
  remoteHighlights: Record<string, { user: string; until: number }>;
  remoteConflict: RemoteConflict | null;
  setRemoteConflict: (next: RemoteConflict | null) => void;
  eventTxtDraftDirty: boolean;
  editValue: string;
  setEditValue: (value: string) => void;
  editRef: RefObject<HTMLTextAreaElement | null>;
  translationEntryListRef: RefObject<HTMLDivElement | null>;
  enterSaves: boolean;
  setEnterSaves: (value: boolean) => void;
  onTextareaKey: (event: React.KeyboardEvent<HTMLTextAreaElement>) => void;
  navigate: (dir: 1 | -1) => void;
  save: (overrideSource?: string, advance?: boolean) => Promise<boolean>;
  selectEntry: (entry: TranslationEntry) => void;
  handleSourceChange: (key: string, source: string) => void;
  /** Overrides the per-entry Moesekai link (side stories link the whole story). */
  detailUrl?: string | null;
  onReloadStory?: () => void;
}

export function TranslationEntryWorkspace({
  category,
  field,
  isEventStory,
  isSideStory = false,
  isReadOnly,
  loading,
  saving,
  writesLocked,
  filtered,
  selectedKey,
  selectedEntry,
  selectedIndex,
  selectedEventStoryIdentityMissing,
  remoteHighlights,
  remoteConflict,
  setRemoteConflict,
  eventTxtDraftDirty,
  editValue,
  setEditValue,
  editRef,
  translationEntryListRef,
  enterSaves,
  setEnterSaves,
  onTextareaKey,
  navigate,
  save,
  selectEntry,
  handleSourceChange,
  detailUrl,
  onReloadStory,
}: TranslationEntryWorkspaceProps) {
  const translationWorkspaceRef = useRef<HTMLDivElement>(null);
  const saveKeyLabel = enterSaves ? "Enter" : "Shift+Enter";
  const newlineKeyLabel = enterSaves ? "Shift+Enter" : "Enter";
  const isStory = isEventStory || isSideStory;
  const sourceText = selectedEntry
    ? isStory ? storyEntrySourceText(selectedEntry) : selectedEntry.key
    : "";
  const sourceLines = sourceText.split("\n").length;
  // Long originals (gacha descriptions) sit beside the editor on wide screens, so the
  // textarea and save buttons fit inside the fixed-height editor pane.
  const longSource = sourceLines > 4 || sourceText.length > 240;
  const textareaRows = Math.min(longSource ? 10 : 12, Math.max(3, sourceLines));

  // ---- Moesekai URL for the currently selected entry ----
  const moesekaiUrl = useMemo(() => {
    if (!selectedEntry || !category || !field) return null;
    if (detailUrl !== undefined) return detailUrl;
    return buildMoesekaiUrl(category, field, selectedEntry.ids);
  }, [selectedEntry, category, field, detailUrl]);

  return (
    <div className="translation-workspace" ref={translationWorkspaceRef}>
      <section className="translation-editor-pane" aria-label="当前翻译编辑区">
        {selectedEntry && (
        <div className={longSource ? "proof-panel proof-panel-long" : "proof-panel"}>
          <div className="proof-jp">
            <span className="label">日文原文</span>
            {selectedEntry.speakerName && <div className="speaker">{selectedEntry.speakerName}</div>}
            <div className="jp-body">{sourceText}</div>
            {isEventStory && <div className="episode">第 {eventStoryEpisodeNo(selectedEntry)} 章</div>}
            {isSideStory && category === "cardStory" && <div className="episode">第 {eventStoryEpisodeNo(selectedEntry)} 话</div>}
            {moesekaiUrl && (
              <a className="moesekai-link" href={moesekaiUrl} target="_blank" rel="noopener noreferrer" title="在 Moesekai 上查看详情">
                <IconExternalLink /> Moesekai 页面
              </a>
            )}
          </div>
          <div className="proof-edit">
            <div className="proof-edit-head">
              <span className="label">翻译校对 <span className={`source-tag ${selectedEntry.source}`}>{SOURCE_LABELS[selectedEntry.source] || selectedEntry.source}</span></span>
              <div style={{ display: "flex", gap: 6 }}>
                <button className="btn btn-ghost btn-sm" onClick={() => navigate(-1)} disabled={saving || selectedIndex <= 0}>↑ 上一条</button>
                <button className="btn btn-ghost btn-sm" onClick={() => navigate(1)} disabled={saving || selectedIndex >= filtered.length - 1}>下一条 ↓</button>
              </div>
            </div>
            {selectedEventStoryIdentityMissing && (
              <div className="remote-conflict-banner" role="alert">
                当前剧情行缺少权威来源身份，已保持只读。请由管理员执行“重新获取剧情”后再编辑。
              </div>
            )}
            {remoteConflict?.key === selectedEntry.key && remoteConflict.current && (
              <div className="remote-conflict-banner revision-conflict" role="alert">
                保存被拒绝：服务器上这一行已更新到 revision {remoteConflict.current.revision}，你的草稿仍保留在输入框中。
                <div className="remote-conflict-current">服务器当前译文：{remoteConflict.current.text || "（空）"}</div>
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => {
                  setEditValue(selectedEntry.text);
                  setRemoteConflict(null);
                }} disabled={saving}>采用服务器版本</button>
                <button type="button" className="btn btn-secondary btn-sm" onClick={() => setRemoteConflict(null)} disabled={saving}>
                  保留本地并允许覆盖
                </button>
                {onReloadStory && <button type="button" className="btn btn-ghost btn-sm" onClick={onReloadStory} disabled={saving}>重新载入本篇</button>}
              </div>
            )}
            {remoteConflict?.key === selectedEntry.key && !remoteConflict.current && (
              <div className="remote-conflict-banner" role="alert">
                {remoteConflict.user} 刚刚修改了这一行；已保留你的本地草稿，请确认后再保存。
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => {
                  setEditValue(selectedEntry.text);
                  setRemoteConflict(null);
                }} disabled={saving}>采用远端版本</button>
                <button type="button" className="btn btn-secondary btn-sm" onClick={() => setRemoteConflict(null)} disabled={saving}>
                  保留本地并允许覆盖
                </button>
              </div>
            )}
            <textarea
              ref={editRef}
              className="proof-textarea"
              value={editValue}
              onChange={(e) => setEditValue(e.target.value)}
              onKeyDown={onTextareaKey}
              placeholder="输入翻译…"
              rows={textareaRows}
              readOnly={isReadOnly || saving || writesLocked || selectedEventStoryIdentityMissing}
              aria-label="翻译校对内容"
            />
            {!isStory && selectedEntry.source === "cn" && (
              <p className="proof-hints">
                保存后来源变为{SOURCE_LABELS.human}，下次{SOURCE_LABELS.cn}同步仍会覆盖这条译文；需要长期保留请用“{SOURCE_LABELS.pinned}保存”。
              </p>
            )}
            <div className="proof-actions">
              <button
                className="btn btn-primary"
                onClick={() => save()}
                disabled={isReadOnly || saving || writesLocked || selectedEventStoryIdentityMissing || remoteConflict?.key === selectedEntry.key}
                title={selectedEventStoryIdentityMissing
                  ? "当前剧情行缺少权威来源身份"
                  : remoteConflict?.key === selectedEntry.key ? "请先明确处理协作者冲突" : undefined}
              >保存并下一条</button>
              {!isStory && <button
                className="btn btn-secondary"
                onClick={() => save("pinned")}
                disabled={isReadOnly || saving || writesLocked || remoteConflict?.key === selectedEntry.key}
              >锁定保存</button>}
              <button className="btn btn-ghost btn-sm" onClick={() => setEnterSaves(!enterSaves)} title="切换保存快捷键">
                快捷键: {saveKeyLabel}
              </button>
              <div className="proof-hints">
                <span>保存 <kbd>{saveKeyLabel}</kbd></span>
                <span>换行 <kbd>{newlineKeyLabel}</kbd></span>
                <span><kbd>Ctrl+↑↓</kbd> 切换</span>
              </div>
            </div>
          </div>
        </div>
        )}
      </section>


      <div className="translation-entry-list" ref={translationEntryListRef}>
        {loading ? (
          <div className="center-state"><div className="spinner" />加载中…</div>
        ) : filtered.length === 0 ? (
          <div className="center-state"><p>暂无数据</p></div>
        ) : (
          <table className="entry-table">
          <thead>
            <tr><th className="col-source">来源</th><th>日文原文</th><th>当前翻译</th></tr>
          </thead>
          <tbody>
            {filtered.map((entry) => (
              <EntryRow
                key={entry.key}
                entry={entry}
                isSelected={selectedKey === entry.key}
                isRemoteHighlighted={Boolean(remoteHighlights[entry.key])}
                remoteHighlightUser={remoteHighlights[entry.key]?.user}
                isEventStory={isEventStory}
                isReadOnly={isReadOnly}
                // A never-stored side-story line has no source to change until a translation is saved.
                writesLocked={writesLocked || saving || (isSideStory && sideStoryLineSaveIsNoop(entry, entry.text))}
                eventTxtDraftDirty={eventTxtDraftDirty}
                hasRemoteConflict={remoteConflict?.key === entry.key}
                hasCanonicalIdentity={!isEventStory || eventStoryEntryHasCanonicalIdentity(entry)}
                sourceOptions={isSideStory ? SIDE_STORY_EDIT_SOURCES : undefined}
                onSelect={selectEntry}
                onSourceChange={handleSourceChange}
              />
            ))}
          </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
