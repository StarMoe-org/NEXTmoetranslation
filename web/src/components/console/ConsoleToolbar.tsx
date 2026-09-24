import type { Locale } from "@/lib/api";

export interface ConsoleToolbarProps {
  isStory: boolean;
  locale: Locale;
  relatedEventFilterAvailable: boolean;
  relatedEventQuery: string;
  onRelatedEventQueryChange: (query: string) => void;
  query: string;
  onQueryChange: (query: string) => void;
  sortMode: "kana" | "id-desc" | "time-desc";
  onSortModeChange: (mode: "kana" | "id-desc" | "time-desc") => void;
}

export function ConsoleToolbar({
  isStory,
  locale,
  relatedEventFilterAvailable,
  relatedEventQuery,
  onRelatedEventQueryChange,
  query,
  onQueryChange,
  sortMode,
  onSortModeChange,
}: ConsoleToolbarProps) {
  return (
    <div className="search-bar">
      {relatedEventFilterAvailable && !isStory && (
        <input
          className="related-event-filter"
          aria-label="按活动名称筛选当前分类"
          placeholder="按活动名称筛选…"
          value={relatedEventQuery}
          onChange={(event) => onRelatedEventQueryChange(event.target.value)}
        />
      )}
      <input
        aria-label="搜索当前翻译"
        placeholder={`搜索日文或${locale === "en-US" ? "英文" : "中文"}…`}
        value={query}
        onChange={(event) => onQueryChange(event.target.value)}
      />
      {!isStory && (
        <label className="sort-selector">
          <span>排序</span>
          <select
            aria-label="翻译条目排序"
            value={sortMode}
            onChange={(event) => onSortModeChange(event.target.value as typeof sortMode)}
          >
            <option value="kana">五十音</option>
            <option value="id-desc">编号倒序</option>
            <option value="time-desc">更新时间倒序</option>
          </select>
        </label>
      )}
    </div>
  );
}
