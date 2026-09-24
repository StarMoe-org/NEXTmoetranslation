import { useEffect } from "react";
import type { SideStoryKindProgress, SideStorySyncStatus } from "@/lib/api";

function formatTime(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN", { hour12: false });
}

function KindProgress({ label, progress }: { label: string; progress?: SideStoryKindProgress }) {
  if (!progress) return null;
  return (
    <div className="side-story-backfill-kind">
      <strong>{label}</strong>
      <span>{progress.stories} 篇 · 已获取 {progress.fetched}/{progress.episodes} 话 · 待获取 {progress.pendingFetch}{progress.errors > 0 ? ` · 获取失败 ${progress.errors}` : ""}</span>
      <span>简中官方：已导入 {progress.cnImported} · 待导入 {progress.cnPending} · 无 {progress.cnAbsent} · 不匹配 {progress.cnMismatch} · 失败 {progress.cnError}</span>
      <span>英文官方：已导入 {progress.enImported} · 待导入 {progress.enPending} · 无 {progress.enAbsent} · 不匹配 {progress.enMismatch} · 失败 {progress.enError}</span>
    </div>
  );
}

export interface SideStoryBackfillPanelProps {
  role: "admin" | "editor" | "";
  expanded: boolean;
  setExpanded: (expanded: boolean) => void;
  status: SideStorySyncStatus | null;
  busy: boolean;
  watch: (watching: boolean) => void;
  reload: () => void;
  runSync: (refreshCatalog: boolean) => void;
}

export function SideStoryBackfillPanel({ role, expanded, setExpanded, status, busy, watch, reload, runSync }: SideStoryBackfillPanelProps) {
  useEffect(() => {
    watch(expanded);
    return () => watch(false);
  }, [expanded, watch]);

  const state = status?.state;
  const round = state?.lastRound;
  return (
    <div className="field-group side-story-group">
      <button type="button" className="field-group-toggle" aria-expanded={expanded} aria-controls="side-story-backfill-panel" onClick={() => setExpanded(!expanded)}>
        <span>剧情回填进度{state ? (state.running ? " · 运行中" : state.enabled ? "" : " · 未启用") : ""}</span>
        <span className="field-group-chevron" aria-hidden="true">{expanded ? "▾" : "▸"}</span>
      </button>
      {expanded && (
        <div id="side-story-backfill-panel" className="side-story-backfill" aria-live="polite">
          {!status ? <p className="sidebar-note" role="status">正在载入…</p> : <>
            <KindProgress label="卡牌剧情" progress={status.totals.card} />
            <KindProgress label="区域对话" progress={status.totals.area} />
            <dl>
              <div><dt>状态</dt><dd>{state?.running ? "正在运行" : state?.enabled ? "等待下一轮" : "未启用（管理设置中的“卡牌剧情/区域对话后台回填”为 false，或设置了 SIDE_STORY_BACKFILL_ENABLED=false）"}</dd></div>
              <div><dt>上一轮</dt><dd>{formatTime(state?.lastRoundAt)}{round ? `（${round.episodes} 话，请求 ${round.requests} 次，获取 ${round.fetched}，官方写入 ${round.officialWritten} 行${round.errors > 0 ? `，错误 ${round.errors}` : ""}${round.retrying > 0 ? `，待重试 ${round.retrying}` : ""}）` : ""}</dd></div>
              <div><dt>下一轮</dt><dd>{formatTime(state?.nextRoundAt)}</dd></div>
              <div><dt>目录刷新</dt><dd>{formatTime(state?.catalogRefreshedAt)}</dd></div>
              {state?.lastRoundError && <div><dt>最近错误</dt><dd className="side-story-backfill-error">{state.lastRoundError}</dd></div>}
            </dl>
          </>}
          <div className="side-story-backfill-actions">
            <button type="button" className="btn btn-ghost btn-sm" onClick={reload}>刷新进度</button>
            {role === "admin" && <>
              <button type="button" className="btn btn-secondary btn-sm" onClick={() => runSync(false)} disabled={busy}>立即续跑</button>
              <button type="button" className="btn btn-secondary btn-sm" onClick={() => runSync(true)} disabled={busy}>刷新目录</button>
            </>}
          </div>
        </div>
      )}
    </div>
  );
}
