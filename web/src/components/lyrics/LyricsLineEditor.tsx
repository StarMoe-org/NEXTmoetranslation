import type {
  CatalogPerformerItem, LyricRubySpan, LyricsEditorLine, LyricsEditorSegment, LyricsPerformerID, LyricsRenditionPerformer,
} from "@/lib/api";
import { canMergeAdjacentLyricSegments, lyricSegmentCanSplit } from "@/lib/lyrics-segmentation.mjs";

type LyricsPerformerOption = CatalogPerformerItem | LyricsRenditionPerformer;

function performerOptionName(performer: LyricsPerformerOption): string {
  return typeof performer.name === "string"
    ? performer.name
    : performer.name["zh-CN"] || performer.name["ja-JP"];
}

interface LyricRubySpanEditorProps {
  lineNumber: number;
  segmentNumber: number;
  rubyIndex: number;
  span: LyricRubySpan;
  sourceMutable: boolean;
  writeLocked: boolean;
  onChange: (patch: { text?: string; reading?: string }) => void;
  onSplit: () => void;
  onMergeWithPrevious: () => void;
}

function LyricRubySpanEditor({
  lineNumber,
  segmentNumber,
  rubyIndex,
  span,
  sourceMutable,
  writeLocked,
  onChange,
  onSplit,
  onMergeWithPrevious,
}: LyricRubySpanEditorProps) {
  const rubyNumber = rubyIndex + 1;
  const rubyLocked = !sourceMutable || writeLocked;
  return (
    <div className="lyric-ruby-span">
      <input
        aria-label={`第 ${lineNumber} 行分段 ${segmentNumber} ruby ${rubyNumber} 原文`}
        lang="ja"
        value={span.text}
        readOnly={rubyLocked}
        onChange={(event) => onChange({ text: event.target.value })}
      />
      <input
        aria-label={`第 ${lineNumber} 行分段 ${segmentNumber} ruby ${rubyNumber} 注音`}
        lang="ja"
        value={span.reading || ""}
        readOnly={rubyLocked}
        placeholder={rubyLocked ? "注音（已锁死）" : "注音"}
        onChange={(event) => onChange({ reading: event.target.value })}
      />
      <span className="lyric-ruby-actions">
        <button type="button" className="btn btn-ghost btn-sm" onClick={onSplit} disabled={rubyLocked}>拆分 ruby</button>
        <button type="button" className="btn btn-ghost btn-sm" onClick={onMergeWithPrevious} disabled={rubyLocked || rubyIndex === 0}>与上一 ruby 合并</button>
      </span>
    </div>
  );
}

interface LyricSegmentEditorProps {
  line: LyricsEditorLine;
  lineIndex: number;
  segment: LyricsEditorSegment;
  segmentIndex: number;
  sourceMutable: boolean;
  writeLocked: boolean;
  showPerformerSegmentation: boolean;
  performers: LyricsPerformerOption[];
  performerName: (id: LyricsPerformerID) => string;
  performerColor: (id: LyricsPerformerID) => string | undefined;
  registerInput: (element: HTMLInputElement | null) => void;
  onChange: (text: string, performerIds?: LyricsPerformerID[]) => void;
  onRubyChange: (rubyIndex: number, patch: { text?: string; reading?: string }) => void;
  onSplitRuby: (rubyIndex: number) => void;
  onMergeRubyWithPrevious: (rubyIndex: number) => void;
  onAdd: () => void;
  onSplit: () => void;
  onMergeWithPrevious: () => void;
  onRemove: () => void;
  onMove: (direction: -1 | 1) => void;
}

function LyricSegmentEditor({
  line,
  lineIndex,
  segment,
  segmentIndex,
  sourceMutable,
  writeLocked,
  showPerformerSegmentation,
  performers,
  performerName,
  performerColor,
  registerInput,
  onChange,
  onRubyChange,
  onSplitRuby,
  onMergeRubyWithPrevious,
  onAdd,
  onSplit,
  onMergeWithPrevious,
  onRemove,
  onMove,
}: LyricSegmentEditorProps) {
  const lineNumber = lineIndex + 1;
  const segmentNumber = segmentIndex + 1;
  const canMergePrevious = segmentIndex > 0 && canMergeAdjacentLyricSegments(line.segments, segmentIndex - 1);

  return (
    <div className="lyric-segment-editor" data-segment-index={segmentIndex}>
      <input
        aria-label={`第 ${lineNumber} 行分段 ${segmentNumber}`}
        lang="ja"
        value={segment.text}
        readOnly={!sourceMutable || writeLocked}
        onChange={(event) => onChange(event.target.value)}
        ref={registerInput}
      />
      {showPerformerSegmentation && <>
        <label className="sr-only" htmlFor={`performers-${line.id}-${segmentIndex}`}>第 {lineNumber} 行分段 {segmentNumber} 的演唱者</label>
        <select
          id={`performers-${line.id}-${segmentIndex}`}
          aria-label={`第 ${lineNumber} 行分段 ${segmentNumber} 的演唱者`}
          multiple
          disabled={writeLocked}
          value={segment.performerIds.map(String)}
          onChange={(event) => onChange(segment.text, Array.from(event.target.selectedOptions, (option) =>
            performers.find((performer) => String(performer.performerId) === option.value)?.performerId
          ).filter((performerId): performerId is LyricsPerformerID => performerId !== undefined))}
        >
          {performers.map((performer) => <option key={performer.performerId} value={performer.performerId}>{performerOptionName(performer)}</option>)}
        </select>
        <div className="lyric-performer-summary">
          {segment.performerIds.length > 0 ? <>
            <span className="lyric-performer-squares" role="img" aria-label={`演唱者：${segment.performerIds.map(performerName).join("、")}`}>
              {segment.performerIds.map((performerId) => <i key={performerId} className="lyric-performer-swatch" aria-hidden="true" title={performerName(performerId)} style={performerColor(performerId) ? { backgroundColor: performerColor(performerId) } : undefined} />)}
            </span>
            <span>{segment.performerIds.map(performerName).join(" / ")}</span>
          </> : <span>未指定演唱者</span>}
        </div>
      </>}

      <div className="lyric-ruby-spans" aria-label={`第 ${lineNumber} 行分段 ${segmentNumber} 的 ruby 注音`}>
        <strong>Ruby 注音（可编辑）</strong>
        {segment.ruby.map((span, rubyIndex) => (
          <LyricRubySpanEditor
            key={`${line.id}-${segmentIndex}-ruby-${rubyIndex}`}
            lineNumber={lineNumber}
            segmentNumber={segmentNumber}
            rubyIndex={rubyIndex}
            span={span}
            sourceMutable={sourceMutable}
            writeLocked={writeLocked}
            onChange={(patch) => onRubyChange(rubyIndex, patch)}
            onSplit={() => onSplitRuby(rubyIndex)}
            onMergeWithPrevious={() => onMergeRubyWithPrevious(rubyIndex)}
          />
        ))}
      </div>

      {showPerformerSegmentation && <span className="lyric-structure-actions">
        <button type="button" className="btn btn-ghost btn-sm" aria-label={`在第 ${lineNumber} 行第 ${segmentNumber} 分段后新增分段`} onClick={onAdd} disabled={writeLocked}>新增分段</button>
        <button type="button" className="btn btn-ghost btn-sm" aria-label={`在第 ${lineNumber} 行第 ${segmentNumber} 分段的光标位置分段`} title="请先把光标放在分段文字中的边界位置" onClick={onSplit} disabled={writeLocked || !lyricSegmentCanSplit(segment.text)}>在光标处分段</button>
        <button type="button" className="btn btn-ghost btn-sm" aria-label={`将第 ${lineNumber} 行第 ${segmentNumber} 分段与上一分段合并`} title={segmentIndex > 0 && !canMergePrevious ? "演唱者不同，不能直接合并" : undefined} onClick={onMergeWithPrevious} disabled={writeLocked || !canMergePrevious}>与上一段合并</button>
        <button type="button" className="btn btn-ghost btn-sm" aria-label={`移除第 ${lineNumber} 行第 ${segmentNumber} 分段并合并演唱者`} onClick={onRemove} disabled={writeLocked || line.segments.length <= 1}>移除分段</button>
        {sourceMutable && <>
          <button type="button" className="btn btn-ghost btn-sm" aria-label={`左移第 ${lineNumber} 行第 ${segmentNumber} 分段`} onClick={() => onMove(-1)} disabled={writeLocked || segmentIndex === 0}>左移</button>
          <button type="button" className="btn btn-ghost btn-sm" aria-label={`右移第 ${lineNumber} 行第 ${segmentNumber} 分段`} onClick={() => onMove(1)} disabled={writeLocked || segmentIndex === line.segments.length - 1}>右移</button>
        </>}
      </span>}
    </div>
  );
}

export interface LyricsLineEditorProps {
  line: LyricsEditorLine;
  lineIndex: number;
  lineCount: number;
  sourceMutable: boolean;
  writeLocked: boolean;
  showPerformerSegmentation: boolean;
  performers: LyricsPerformerOption[];
  performerName: (id: LyricsPerformerID) => string;
  performerColor: (id: LyricsPerformerID) => string | undefined;
  registerSegmentInput: (segmentIndex: number, element: HTMLInputElement | null) => void;
  onUpdateLine: (patch: Partial<LyricsEditorLine>) => void;
  onMoveLine: (direction: -1 | 1) => void;
  onRemoveLine: () => void;
  onUpdateSegment: (segmentIndex: number, text: string, performerIds?: LyricsPerformerID[]) => void;
  onUpdateRubySpan: (segmentIndex: number, rubyIndex: number, patch: { text?: string; reading?: string }) => void;
  onSplitRubySpan: (segmentIndex: number, rubyIndex: number) => void;
  onMergeRubyWithPrevious: (segmentIndex: number, rubyIndex: number) => void;
  onAddSegment: (segmentIndex: number) => void;
  onSplitSegment: (segmentIndex: number) => void;
  onMergeWithPreviousSegment: (segmentIndex: number) => void;
  onRemoveSegment: (segmentIndex: number) => void;
  onMoveSegment: (segmentIndex: number, direction: -1 | 1) => void;
}

export function LyricsLineEditor({
  line,
  lineIndex,
  lineCount,
  sourceMutable,
  writeLocked,
  showPerformerSegmentation,
  performers,
  performerName,
  performerColor,
  registerSegmentInput,
  onUpdateLine,
  onMoveLine,
  onRemoveLine,
  onUpdateSegment,
  onUpdateRubySpan,
  onSplitRubySpan,
  onMergeRubyWithPrevious,
  onAddSegment,
  onSplitSegment,
  onMergeWithPreviousSegment,
  onRemoveSegment,
  onMoveSegment,
}: LyricsLineEditorProps) {
  const lineNumber = lineIndex + 1;
  return (
    <article className="lyric-line" data-line-index={lineIndex} aria-labelledby={`lyric-line-${line.id}-title`}>
      <span id={`lyric-line-${line.id}-title`} className="sr-only">第 {lineNumber} 行歌词</span>
      <header>
        <strong>{lineNumber}</strong>
        <code>{line.id}</code>
        <label><input type="checkbox" checked={Boolean(line.stanzaBreakBefore)} disabled={writeLocked} onChange={(event) => onUpdateLine({ stanzaBreakBefore: event.target.checked })} /> 段落前空行</label>
        {sourceMutable && <span className="lyric-structure-actions">
          <button type="button" className="btn btn-ghost btn-sm" aria-label={`上移第 ${lineNumber} 行`} disabled={writeLocked || lineIndex === 0} onClick={() => onMoveLine(-1)}>上移</button>
          <button type="button" className="btn btn-ghost btn-sm" aria-label={`下移第 ${lineNumber} 行`} disabled={writeLocked || lineIndex === lineCount - 1} onClick={() => onMoveLine(1)}>下移</button>
          <button type="button" className="btn btn-ghost btn-sm" aria-label={`删除第 ${lineNumber} 行`} disabled={writeLocked || lineCount <= 1} onClick={onRemoveLine}>删除行</button>
        </span>}
      </header>

      <div className="lyric-translations">
        <label>日文<textarea aria-label={`第 ${lineNumber} 行日文原文`} lang="ja" value={line.japanese} readOnly rows={2} /></label>
        <label>简中<textarea aria-label={`第 ${lineNumber} 行简体中文译文`} lang="zh-CN" value={line["zh-CN"] || ""} readOnly={writeLocked} onChange={(event) => onUpdateLine({ "zh-CN": event.target.value })} rows={2} /></label>
        <label>英文<textarea aria-label={`第 ${lineNumber} 行英文译文`} lang="en" value={line["en-US"] || ""} readOnly={writeLocked} onChange={(event) => onUpdateLine({ "en-US": event.target.value })} rows={2} /></label>
        {showPerformerSegmentation && line.trailingPerformerIds !== undefined && <label>行尾演唱者<select aria-label={`第 ${lineNumber} 行尾演唱者`} multiple disabled={writeLocked} value={line.trailingPerformerIds.map(String)} onChange={(event) => onUpdateLine({ trailingPerformerIds: Array.from(event.target.selectedOptions, (option) => performers.find((performer) => String(performer.performerId) === option.value)?.performerId).filter((id): id is LyricsPerformerID => id !== undefined) } as Partial<LyricsEditorLine>)}>
          {performers.map((performer) => <option key={performer.performerId} value={performer.performerId}>{performerOptionName(performer)}</option>)}
        </select></label>}
      </div>

      <div className="lyric-segments">
        {line.segments.map((segment, segmentIndex) => (
          <LyricSegmentEditor
            key={`${line.id}-${segmentIndex}`}
            line={line}
            lineIndex={lineIndex}
            segment={segment}
            segmentIndex={segmentIndex}
            sourceMutable={sourceMutable}
            writeLocked={writeLocked}
            showPerformerSegmentation={showPerformerSegmentation}
            performers={performers}
            performerName={performerName}
            performerColor={performerColor}
            registerInput={(element) => registerSegmentInput(segmentIndex, element)}
            onChange={(text, performerIds) => onUpdateSegment(segmentIndex, text, performerIds)}
            onRubyChange={(rubyIndex, patch) => onUpdateRubySpan(segmentIndex, rubyIndex, patch)}
            onSplitRuby={(rubyIndex) => onSplitRubySpan(segmentIndex, rubyIndex)}
            onMergeRubyWithPrevious={(rubyIndex) => onMergeRubyWithPrevious(segmentIndex, rubyIndex)}
            onAdd={() => onAddSegment(segmentIndex)}
            onSplit={() => onSplitSegment(segmentIndex)}
            onMergeWithPrevious={() => onMergeWithPreviousSegment(segmentIndex)}
            onRemove={() => onRemoveSegment(segmentIndex)}
            onMove={(direction) => onMoveSegment(segmentIndex, direction)}
          />
        ))}
      </div>
    </article>
  );
}
