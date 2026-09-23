import type { EventStorySummary, Locale, TranslationEntry } from "@/lib/api";
import { EventStoryTxtImport, type EventStoryTxtDraft } from "@/components/EventStoryTxtImport";
import { eventStoryEpisodeNo } from "@/lib/event-story-console";
import type { ChapterTab } from "@/components/console/types";

export interface EventStoryToolbarProps {
  role: "admin" | "editor" | "";
  locale: Locale;
  field: string;
  entries: TranslationEntry[];
  selectedEntry: TranslationEntry | null;
  currentStory?: EventStorySummary;
  chapters: ChapterTab[];
  selectedEpisode: string;
  selectChapter: (episodeNo: string) => void;
  busy: boolean;
  saving: boolean;
  writesLocked: boolean;
  entryDirty: boolean;
  eventTxtDraftDirty: boolean;
  eventTxtDraft: EventStoryTxtDraft | null;
  applyEventTxtDraft: (draft: EventStoryTxtDraft) => void;
  undoEventTxtDraft: () => void;
  onAIStory: () => void;
  onPromoteStory: () => void;
  onRetryStory: () => void;
  onReorderStory: () => void;
}

export function EventStoryToolbar({
  role,
  locale,
  field,
  entries,
  selectedEntry,
  currentStory,
  chapters,
  selectedEpisode,
  selectChapter,
  busy,
  saving,
  writesLocked,
  entryDirty,
  eventTxtDraftDirty,
  eventTxtDraft,
  applyEventTxtDraft,
  undoEventTxtDraft,
  onAIStory,
  onPromoteStory,
  onRetryStory,
  onReorderStory,
}: EventStoryToolbarProps) {
  return (
    <div className="story-toolbar">
      {chapters.length > 0 ? (
        <label className="chapter-selector">
          <span>章节</span>
          <select
            aria-label="选择活动剧情章节"
            value={selectedEpisode}
            onChange={(event) => selectChapter(event.target.value)}
            disabled={saving}
          >
            <option value="all">全部章节 · {entries.length} 条</option>
            {chapters.map((chapter) => (
              <option key={chapter.episodeNo} value={chapter.episodeNo}>
                {`第 ${chapter.episodeNo} 话${chapter.title ? ` · ${chapter.title}` : ""} · ${chapter.untranslated > 0 ? `未翻译 ${chapter.untranslated} 条` : "已完成"}`}
              </option>
            ))}
          </select>
        </label>
      ) : (
        <span className="story-status">
          {currentStory && currentStory.untranslatedCount > 0
            ? <><span className="story-dot pending" /> {currentStory.untranslatedCount} 条未翻译</>
            : <><span className="story-dot done" /> 已全部翻译</>}
        </span>
      )}
      {locale !== "ja-JP" && (
        <div className="story-toolbar-actions">
          <EventStoryTxtImport
            eventId={Number(field)}
            locale={locale}
            entries={entries}
            defaultEpisodeNo={selectedEpisode !== "all" ? selectedEpisode : selectedEntry ? eventStoryEpisodeNo(selectedEntry) : undefined}
            disabled={busy || saving || writesLocked || entryDirty || eventTxtDraftDirty}
            onApply={applyEventTxtDraft}
          />
          {eventTxtDraft && <>
            <span className="event-txt-import-pending" role="status">TXT 本地草稿剩余 {eventTxtDraft.translations.length} 条；只会通过现有保存按钮逐条提交</span>
            {eventTxtDraft.undoAvailable && <button type="button" className="btn btn-ghost btn-sm" onClick={undoEventTxtDraft} disabled={busy || saving || writesLocked || entryDirty}>撤销本次导入</button>}
          </>}
          {role === "admin" && locale === "zh-CN" && <>
            <button className="btn btn-primary btn-sm" onClick={onAIStory} disabled={busy}>AI 补充剧情翻译</button>
            <button className="btn btn-secondary btn-sm" onClick={onPromoteStory} disabled={busy}>整篇标记人工</button>
            <button className="btn btn-secondary btn-sm" onClick={onRetryStory} disabled={busy}>重新获取剧情</button>
            <button className="btn btn-secondary btn-sm" onClick={onReorderStory} disabled={busy}>重排序对话</button>
          </>}
        </div>
      )}
    </div>
  );
}
