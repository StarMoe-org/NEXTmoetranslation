import type { Locale, SideStoryDetail, SideStoryKind, SideStoryLineEdit, TranslationEntry } from "@/lib/api";
import { SideStoryTxtImport } from "@/components/SideStoryTxtImport";
import type { SideStoryBatchOutcome } from "@/components/console/useEntryEditor";
import type { ChapterTab } from "@/components/console/types";
import { sideStoryMoesekaiUrl } from "@/lib/labels";
import {
  SIDE_STORY_FETCH_STATE_LABELS, sideStoryEntryUntranslated, sideStoryEpisodeNotice, sideStoryLocale,
} from "@/lib/side-story-console";

export interface SideStoryToolbarProps {
  role: "admin" | "editor" | "";
  locale: Locale;
  kind: SideStoryKind;
  storyId: string;
  detail: SideStoryDetail | null;
  entries: TranslationEntry[];
  chapters: ChapterTab[];
  selectedEpisode: string;
  selectChapter: (episodeKey: string) => void;
  busy: boolean;
  saving: boolean;
  writesLocked: boolean;
  entryDirty: boolean;
  saveBatch: (episodeKey: string, edits: SideStoryLineEdit[]) => Promise<SideStoryBatchOutcome>;
  onAI: () => void;
  onRefresh: () => void;
  onReload: () => void;
}

export function SideStoryToolbar({
  role, locale, kind, storyId, detail, entries, chapters, selectedEpisode, selectChapter,
  busy, saving, writesLocked, entryDirty, saveBatch, onAI, onRefresh, onReload,
}: SideStoryToolbarProps) {
  const episodes = detail?.episodes ?? [];
  const visibleEpisodes = selectedEpisode === "all" ? episodes : episodes.filter((episode) => episode.key === selectedEpisode);
  const moesekaiUrl = sideStoryMoesekaiUrl(kind, storyId, detail?.areaCategory);
  const untranslated = entries.filter(sideStoryEntryUntranslated).length;
  const officialLabel = locale === "en-US" ? "英文官方" : "简中官方";
  const episodeLabel = (key: string) => kind === "card" ? `第 ${key} 话` : "本篇";
  const importEpisodes = episodes.filter((episode) => episode.fetched).map((episode) => ({ key: episode.key, label: episodeLabel(episode.key) }));
  const writable = locale !== "ja-JP";
  const aiScope = selectedEpisode === "all" || episodes.length < 2 ? "" : `（第 ${selectedEpisode} 话）`;

  return (
    <div className="story-toolbar">
      {chapters.length > 1 ? (
        <label className="chapter-selector">
          <span>话数</span>
          <select aria-label="选择剧情话数" value={selectedEpisode} onChange={(event) => selectChapter(event.target.value)} disabled={saving}>
            <option value="all">全部 · {entries.length} 条</option>
            {chapters.map((chapter) => (
              <option key={chapter.episodeNo} value={chapter.episodeNo}>
                {`第 ${chapter.episodeNo} 话${chapter.title ? ` · ${chapter.title}` : ""} · ${
                  episodes.some((episode) => episode.key === chapter.episodeNo && !episode.fetched) ? "剧本尚未获取"
                    : chapter.untranslated > 0 ? `未翻译 ${chapter.untranslated} 条` : "已完成"}`}
              </option>
            ))}
          </select>
        </label>
      ) : (
        <span className="story-status">
          {writable && untranslated > 0
            ? <><span className="story-dot pending" /> {untranslated} 条未翻译</>
            : <><span className="story-dot done" /> {writable ? "已全部翻译" : "日文原文（只读）"}</>}
        </span>
      )}
      {visibleEpisodes.length > 0 && (
        <span className="story-episode-state">
          {visibleEpisodes.map((episode) => {
            const notice = sideStoryEpisodeNotice(episode, locale);
            return (
              <span key={episode.key} title={episode.lastError || undefined}>
                {episodeLabel(episode.key)}：{episode.fetched
                  ? `${officialLabel}${SIDE_STORY_FETCH_STATE_LABELS[locale === "en-US" ? episode.enState : episode.cnState] ?? "—"}`
                  : "剧本尚未获取"}
                {notice === "error" ? " · 有错误" : notice === "retry" ? " · 待重试" : ""}
              </span>
            );
          })}
        </span>
      )}
      <div className="story-toolbar-actions">
        {moesekaiUrl && <a className="moesekai-link" href={moesekaiUrl} target="_blank" rel="noopener noreferrer">在 Moesekai 打开</a>}
        {writable && (
          <SideStoryTxtImport
            kind={kind}
            storyId={storyId}
            locale={sideStoryLocale(locale)}
            entries={entries}
            episodes={importEpisodes}
            defaultEpisodeKey={selectedEpisode !== "all" ? selectedEpisode : undefined}
            disabled={busy || saving || writesLocked || entryDirty}
            saveBatch={saveBatch}
            onReload={onReload}
          />
        )}
        {role === "admin" && <>
          {writable && <button type="button" className="btn btn-primary btn-sm" onClick={onAI} disabled={busy}>AI 补充翻译{aiScope}</button>}
          <button type="button" className="btn btn-secondary btn-sm" onClick={onRefresh} disabled={busy}>重新获取剧本</button>
        </>}
      </div>
    </div>
  );
}
