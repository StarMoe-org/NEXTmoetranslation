import type { CatalogMusicItem } from "@/lib/api";

export function databaseLyricsStatusLabel(item: CatalogMusicItem): string {
  const withdrawn = item.lyricsWithdrawn ? "（已撤下）" : "";
  // A source-v3 document has no separate publication: saving it goes public.
  if (item.lyricsSourceV3) return `source-v3 文档${withdrawn}`;
  if (item.lyricsStatus === "published") return "已发布";
  if (item.lyricsStatus === "draft-published") return "草稿（旧版公开）";
  if (item.lyricsStatus === "draft") return `草稿${withdrawn}`;
  if (item.lyricsAvailabilityState === "satisfied_no_lyrics") return "无需歌词（已记录）";
  if (item.lyricsAvailabilityState === "incomplete") return "来源未完成（已记录）";
  if (item.lyricsAvailabilityState === "ambiguous") return "来源有歧义（已记录）";
  if (item.lyricsAvailabilityState === "missing") return "来源缺失（已记录）";
  if (item.lyricsAvailabilityState === "failed") return "来源失败（已记录）";
  return "未录入";
}

export function runtimeLyricsStateLabel(state: string): string {
  if (state === "complete") return "完整公开";
  if (state === "game_only") return "仅 Game 公开";
  if (state === "satisfied_no_lyrics") return "无需歌词";
  if (state === "incomplete") return "未完成";
  return state;
}

export function runtimeLyricsVersionsLabel(versions: string[]): string {
  return versions.length > 0
    ? versions.map((version) => (version === "full" ? "Full" : version === "game" ? "Game" : version)).join("/")
    : "无 detail";
}

export interface LyricsCatalogSidebarProps {
  query: string;
  onQueryChange: (query: string) => void;
  catalog: CatalogMusicItem[];
  catalogLoading: boolean;
  catalogError: boolean;
  selectedMusic: CatalogMusicItem | null;
  busy: boolean;
  onChooseMusic: (item: CatalogMusicItem) => void;
  onRetryCatalog: () => void;
}

export function LyricsCatalogSidebar({
  query,
  onQueryChange,
  catalog,
  catalogLoading,
  catalogError,
  selectedMusic,
  busy,
  onChooseMusic,
  onRetryCatalog,
}: LyricsCatalogSidebarProps) {
  return (
    <aside className="lyrics-catalog">
      <input
        aria-label="搜索歌词曲目"
        value={query}
        onChange={(event) => onQueryChange(event.target.value)}
        placeholder="搜索曲目或 musicId…"
        disabled={busy}
      />
      <div className="lyrics-catalog-list" aria-busy={catalogLoading}>
        {catalogLoading && catalog.length === 0 ? (
          <div className="lyrics-inline-state" role="status">
            <div className="spinner" />
            正在加载曲目目录…
          </div>
        ) : catalogError && catalog.length === 0 ? (
          <div className="lyrics-inline-state" role="alert">
            <span>曲目目录加载失败</span>
            <button className="btn btn-secondary btn-sm" onClick={onRetryCatalog}>
              重试
            </button>
          </div>
        ) : catalog.length === 0 ? (
          <div className="lyrics-inline-state">
            <span>{query.trim() ? "没有匹配的曲目" : "暂无可编辑曲目"}</span>
          </div>
        ) : (
          catalog.map((item) => (
            <button
              key={item.musicId}
              className={selectedMusic?.musicId === item.musicId ? "active" : ""}
              aria-current={selectedMusic?.musicId === item.musicId ? "page" : undefined}
              onClick={() => onChooseMusic(item)}
              disabled={busy}
            >
              <strong>{item.title["zh-CN"] || item.title["ja-JP"]}</strong>
              <span>#{item.musicId} · 数据库：{databaseLyricsStatusLabel(item)}</span>
              {item.runtimeLyrics && (
                <span>
                  公开镜像：{runtimeLyricsStateLabel(item.runtimeLyrics.state)} · {runtimeLyricsVersionsLabel(item.runtimeLyrics.availableVersions)}
                </span>
              )}
            </button>
          ))
        )}
      </div>
    </aside>
  );
}
