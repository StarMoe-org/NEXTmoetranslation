import React from "react";
import type { TranslationEntry } from "@/lib/api";
import { SOURCE_LABELS, eventStoryEntryLabel } from "@/lib/labels";

export interface EntryRowProps {
  entry: TranslationEntry;
  isSelected: boolean;
  isRemoteHighlighted: boolean;
  remoteHighlightUser?: string;
  isEventStory: boolean;
  isReadOnly: boolean;
  writesLocked: boolean;
  eventTxtDraftDirty: boolean;
  hasRemoteConflict: boolean;
  hasCanonicalIdentity: boolean;
  onSelect: (entry: TranslationEntry) => void;
  onSourceChange: (key: string, source: string) => void;
}

export const EntryRow = React.memo(function EntryRow({
  entry,
  isSelected,
  isRemoteHighlighted,
  remoteHighlightUser,
  isEventStory,
  isReadOnly,
  writesLocked,
  eventTxtDraftDirty,
  hasRemoteConflict,
  hasCanonicalIdentity,
  onSelect,
  onSourceChange,
}: EntryRowProps) {
  return (
    <tr
      data-key={entry.key}
      className={`entry-row ${isSelected ? "active" : ""}${isRemoteHighlighted ? " remote-highlight" : ""}`}
      onClick={() => onSelect(entry)}
      onKeyDown={(event) => {
        if (event.target !== event.currentTarget) return;
        if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onSelect(entry); }
      }}
      tabIndex={0}
      aria-selected={isSelected}
      title={isRemoteHighlighted && remoteHighlightUser ? `${remoteHighlightUser} 刚刚修改了这一行` : undefined}
    >
      <td className="col-source" onClick={(e) => e.stopPropagation()}>
        <select
          value={entry.source}
          onChange={(e) => onSourceChange(entry.key, e.target.value)}
          className={`source-tag ${entry.source}`}
          disabled={isReadOnly || writesLocked || eventTxtDraftDirty || hasRemoteConflict || (isEventStory && !hasCanonicalIdentity)}
          aria-label={`${isEventStory ? (entry.japanese || eventStoryEntryLabel(entry.key)) : entry.key} 的来源`}
          title={isEventStory && !hasCanonicalIdentity ? "当前剧情行缺少权威来源身份，请重新获取剧情后再编辑" : undefined}
        >
          {Object.entries(SOURCE_LABELS).map(([k, v]) => (
            <option key={k} value={k}>{v}</option>
          ))}
        </select>
      </td>
      <td>
        <div className="jp">
          {entry.speakerName && <div className="speaker">{entry.speakerName}</div>}
          {isEventStory ? (entry.japanese || eventStoryEntryLabel(entry.key)) : entry.key}
        </div>
      </td>
      <td><div className="cn">{entry.text}</div></td>
    </tr>
  );
});
