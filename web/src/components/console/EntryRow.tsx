import React from "react";
import type { TranslationEntry } from "@/lib/api";
import { SOURCE_LABELS, storyEntrySourceText } from "@/lib/labels";

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
  /** Sources an editor may choose; others stay visible only as the current value. Defaults to all. */
  sourceOptions?: readonly string[];
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
  sourceOptions,
  onSelect,
  onSourceChange,
}: EntryRowProps) {
  // Side-story entries carry a line role; their Japanese text is shown like an event story's.
  const sourceText = isEventStory || entry.lineRole ? storyEntrySourceText(entry) : entry.key;
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
          aria-label={`${sourceText} 的来源`}
          title={isEventStory && !hasCanonicalIdentity ? "当前剧情行缺少权威来源身份，请重新获取剧情后再编辑" : undefined}
        >
          {Object.entries(SOURCE_LABELS)
            .filter(([k]) => !sourceOptions || sourceOptions.includes(k) || k === entry.source)
            .map(([k, v]) => (
              <option key={k} value={k} disabled={Boolean(sourceOptions) && !sourceOptions?.includes(k)}>{v}</option>
            ))}
        </select>
      </td>
      <td>
        <div className="jp">
          {entry.speakerName && <div className="speaker">{entry.speakerName}</div>}
          <div className="entry-preview">{sourceText}</div>
        </div>
      </td>
      <td><div className="cn entry-preview">{entry.text}</div></td>
    </tr>
  );
});
