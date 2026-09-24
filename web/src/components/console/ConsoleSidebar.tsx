import type { ReactNode } from "react";
import type { CategoryInfo, EventStorySummary, Locale } from "@/lib/api";
import { CATEGORY_LABELS, fieldLabel } from "@/lib/labels";

// ---- Inline SVG icons (lucide-style, 24×24 viewBox) ----

const IconSettings = () => (
  <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
);
const IconShield = () => (
  <svg viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg>
);
const IconLogout = () => (
  <svg viewBox="0 0 24 24"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></svg>
);
const IconChevronLeft = () => (
  <svg viewBox="0 0 24 24"><polyline points="15 18 9 12 15 6"/></svg>
);
const IconGlobe = () => (
  <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><line x1="2" y1="12" x2="22" y2="12"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg>
);

export interface ConsoleSidebarProps {
  username: string;
  role: "admin" | "editor" | "";
  locale: Locale;
  isLyrics: boolean;
  isLyricsSourceReview: boolean;
  publishing: boolean;
  writesLocked: boolean;
  categories: CategoryInfo[];
  category: string;
  field: string;
  hiddenBadges: Set<string>;
  visibleEventStoryCount: number;
  filteredEventStories: EventStorySummary[];
  eventNameQuery: string;
  setEventNameQuery: (query: string) => void;
  eventStoriesExpanded: boolean;
  setEventStoriesExpanded: (expanded: boolean) => void;
  /** Card story, area talk and backfill groups, placed between event stories and music. */
  sideStories: ReactNode;
  doPublish: () => void;
  onOpenSettings: () => void;
  onOpenAdmin: () => void;
  onRequestLogout: () => void;
  onCollapse: () => void;
  requestLocaleChange: (locale: Locale) => void;
  selectField: (category: string, field: string) => void;
}

export function ConsoleSidebar({
  username,
  role,
  locale,
  isLyrics,
  isLyricsSourceReview,
  publishing,
  writesLocked,
  categories,
  category,
  field,
  hiddenBadges,
  visibleEventStoryCount,
  filteredEventStories,
  eventNameQuery,
  setEventNameQuery,
  eventStoriesExpanded,
  setEventStoriesExpanded,
  sideStories,
  doPublish,
  onOpenSettings,
  onOpenAdmin,
  onRequestLogout,
  onCollapse,
  requestLocaleChange,
  selectField,
}: ConsoleSidebarProps) {
  return (
    <aside className="sidebar" aria-label="翻译类别导航">
      <div className="sidebar-header">
        <div className="sidebar-title-row">
          <div>
            <h1>翻译校对</h1>
            <span className="sub">{username}{role === "admin" ? " · 管理员" : ""}</span>
          </div>
          <div className="sidebar-icon-row">
            <button className="icon-btn" onClick={() => void doPublish()} aria-label="立即发布全量公开文件" title={publishing ? "正在全量发布…" : "立即发布公开文件（全量构建最新 JSON）"} disabled={publishing || writesLocked}><IconGlobe /></button>
            <button className="icon-btn" onClick={onOpenSettings} aria-label="用户设置" title="用户设置"><IconSettings /></button>
            {role === "admin" && <button className="icon-btn" onClick={onOpenAdmin} aria-label="管理设置" title="管理设置"><IconShield /></button>}
            <button className="icon-btn" onClick={onRequestLogout} aria-label="退出登录" title="退出登录"><IconLogout /></button>
            <button className="icon-btn" onClick={onCollapse} aria-label="收起侧边栏" title="收起侧边栏"><IconChevronLeft /></button>
          </div>
        </div>
        <label className="locale-selector">
          <span>{isLyrics || isLyricsSourceReview ? "其他内容编辑语言" : "编辑语言"}</span>
          <select value={locale} onChange={(event) => requestLocaleChange(event.target.value as Locale)} disabled={isLyrics || isLyricsSourceReview} aria-describedby={isLyrics ? "lyrics-locale-note" : isLyricsSourceReview ? "lyrics-review-locale-note" : undefined}>
            <option value="zh-CN">简体中文</option>
            <option value="en-US">英文</option>
            <option value="ja-JP">日文（只读）</option>
          </select>
          {isLyrics && <span id="lyrics-locale-note" className="locale-note">歌词页同时编辑日文、简中和英文</span>}
          {isLyricsSourceReview && <span id="lyrics-review-locale-note" className="locale-note">原文抓取审核与编辑语言无关，不包含翻译</span>}
        </label>
      </div>

      <div className="sidebar-scroll">
        {categories.map((cat) => (
          <div className="field-group" key={cat.name}>
            <div className="field-group-title">{CATEGORY_LABELS[cat.name] || cat.name}</div>
            {cat.fields?.map((f) => {
              const work = f.llmCount + f.unknownCount;
              const active = category === cat.name && field === f.name;
              const badgeKey = `${cat.name}:${f.name}`;
              const hideBadge = hiddenBadges.has(badgeKey);
              return (
                <button type="button" key={badgeKey} className={`field-item ${active ? "active" : ""}`} aria-current={active ? "page" : undefined} onClick={() => selectField(cat.name, f.name)}>
                  <span>{fieldLabel(cat.name, f.name)}</span>
                  {work > 0 && !hideBadge && <span className="badge work">{work}</span>}
                </button>
              );
            })}
          </div>
        ))}

        {visibleEventStoryCount > 0 && (
          <div className="field-group event-story-group">
            <button
              type="button"
              className="field-group-toggle"
              aria-expanded={eventStoriesExpanded}
              aria-controls="event-story-sidebar-list"
              onClick={() => setEventStoriesExpanded(!eventStoriesExpanded)}
            >
              <span>活动剧情 ({filteredEventStories.length}/{visibleEventStoryCount})</span>
              <span className="field-group-chevron" aria-hidden="true">{eventStoriesExpanded ? "▾" : "▸"}</span>
            </button>
            {eventStoriesExpanded && (
              <div id="event-story-sidebar-list">
                <input className="sidebar-filter" aria-label="按活动名称筛选" placeholder="按活动名称筛选…" value={eventNameQuery} onChange={(event) => setEventNameQuery(event.target.value)} />
                {filteredEventStories.map((s) => {
                  const active = category === "eventStory" && field === String(s.eventId);
                  const done = s.untranslatedCount === 0;
                  const badgeKey = `eventStory:${s.eventId}`;
                  const hideBadge = hiddenBadges.has(badgeKey);
                  return (
                    <button type="button" key={s.eventId} className={`field-item ${active ? "active" : ""}`} aria-current={active ? "page" : undefined} onClick={() => selectField("eventStory", String(s.eventId))}>
                      <span className="field-item-copy">
                        <span>
                          <span className={`story-dot ${done ? "done" : "pending"}`} title={done ? "已翻译" : "有未翻译内容"} />
                          {s.eventName || s.eventNameJapanese || `Event #${s.eventId}`}
                        </span>
                        <small>#{s.eventId}{s.eventName && s.eventNameJapanese && s.eventName !== s.eventNameJapanese ? ` · ${s.eventNameJapanese}` : ""}</small>
                      </span>
                      {!hideBadge && (
                        s.untranslatedCount > 0
                          ? <span className="badge work" title="未翻译条数">{s.untranslatedCount}</span>
                          : <span className="badge ok" title="已全部翻译">✓</span>
                      )}
                    </button>
                  );
                })}
              </div>
            )}
          </div>
        )}

        {sideStories}

        <div className="field-group">
          <div className="field-group-title">音乐内容</div>
          <button type="button" className={`field-item ${isLyrics ? "active" : ""}`} aria-current={isLyrics ? "page" : undefined} onClick={() => selectField("lyrics", "catalog")}>
            <span>歌词编辑与发布</span>
          </button>
          {role === "admin" && <button type="button" className={`field-item ${isLyricsSourceReview ? "active" : ""}`} aria-current={isLyricsSourceReview ? "page" : undefined} onClick={() => selectField("lyricsSourceReview", "queue")}>
            <span>歌词原文抓取审核</span>
          </button>}
        </div>
      </div>
    </aside>
  );
}
