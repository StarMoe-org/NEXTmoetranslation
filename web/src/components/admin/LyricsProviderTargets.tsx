"use client";

import React, { useCallback, useEffect, useState } from "react";
import {
  APIError, LyricsProviderContributorAlias, LyricsProviderTarget,
  deleteLyricsProviderTarget, getLyricsProviderTargets, putLyricsProviderTarget,
} from "@/lib/api";

type ShowFn = (msg: string, type?: "ok" | "err") => void;

// The registry validator's reason travels in `details`; the bare code alone
// would not tell the admin what to fix.
function errorMessage(e: unknown, fallback: string): string {
  if (e instanceof APIError && e.details.length) return e.details.join("；");
  return e instanceof Error ? e.message : fallback;
}

// The server validates the whole would-be map before it stores an edit, so the
// form only has to send what the admin typed.
function parseAliases(text: string): LyricsProviderContributorAlias[] {
  return text.split("\n").map((line) => line.trim()).filter(Boolean).map((line) => {
    const separator = line.indexOf("=");
    return {
      catalogContributor: separator < 0 ? line : line.slice(0, separator).trim(),
      providerContributor: separator < 0 ? "" : line.slice(separator + 1).trim(),
    };
  });
}

function formatAliases(aliases: LyricsProviderContributorAlias[]): string {
  return aliases.map((alias) => `${alias.catalogContributor}=${alias.providerContributor}`).join("\n");
}

export function LyricsProviderTargets({ show }: { show: ShowFn }) {
  const [targets, setTargets] = useState<LyricsProviderTarget[]>([]);
  const [musicId, setMusicId] = useState("");
  const [pageTitle, setPageTitle] = useState("");
  const [resolvedPageTitle, setResolvedPageTitle] = useState("");
  const [aliases, setAliases] = useState("");
  const [busy, setBusy] = useState(false);

  const reload = useCallback(() => {
    getLyricsProviderTargets().then((r) => setTargets(r.items)).catch((e) => show(e.message, "err"));
  }, [show]);
  useEffect(() => { reload(); }, [reload]);

  const edit = (target: LyricsProviderTarget) => {
    setMusicId(String(target.musicId));
    setPageTitle(target.pageTitle);
    setResolvedPageTitle(target.resolvedPageTitle);
    setAliases(formatAliases(target.aliases));
  };

  const save = async () => {
    const id = Number(musicId);
    if (!Number.isInteger(id) || id <= 0) { show("乐曲 ID 必须为正整数", "err"); return; }
    setBusy(true);
    try {
      await putLyricsProviderTarget(id, {
        pageTitle: pageTitle.trim(),
        resolvedPageTitle: resolvedPageTitle.trim(),
        aliases: parseAliases(aliases),
      });
      setMusicId(""); setPageTitle(""); setResolvedPageTitle(""); setAliases("");
      reload();
      show("已保存歌词来源映射", "ok");
    } catch (e) { show(errorMessage(e, "保存失败"), "err"); }
    finally { setBusy(false); }
  };

  const remove = async (target: LyricsProviderTarget) => {
    if (!confirm(`删除乐曲 ${target.musicId} 的 Sekaipedia 映射「${target.pageTitle}」？`)) return;
    setBusy(true);
    try { await deleteLyricsProviderTarget(target.musicId); reload(); show("已删除", "ok"); }
    catch (e) { show(errorMessage(e, "删除失败"), "err"); }
    finally { setBusy(false); }
  };

  return (
    <div className="card">
      <h3>歌词来源映射（Sekaipedia）</h3>
      <p style={{ fontSize: 13, color: "var(--text-secondary)", marginBottom: 12 }}>
        绑定乐曲 ID 与 Sekaipedia 页面标题；保存后立即生效，无需重启。词曲作者别名每行一条，格式为
        <code> 目录名=Sekaipedia 名</code>。
      </p>
      <table className="data-table">
        <thead><tr><th>乐曲 ID</th><th>页面标题</th><th>实际页面</th><th>作者别名</th><th>操作</th></tr></thead>
        <tbody>
          {targets.map((target) => (
            <tr key={target.musicId}>
              <td>{target.musicId}</td>
              <td>{target.pageTitle}</td>
              <td>{target.resolvedPageTitle || "—"}</td>
              <td style={{ whiteSpace: "pre-line" }}>{target.aliases.length ? formatAliases(target.aliases) : "—"}</td>
              <td style={{ display: "flex", gap: 6 }}>
                <button className="btn btn-ghost btn-sm" onClick={() => edit(target)} disabled={busy}>编辑</button>
                <button className="btn btn-ghost btn-sm" onClick={() => remove(target)} disabled={busy}>删除</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div style={{ display: "flex", gap: 8, marginTop: 14, alignItems: "flex-end", flexWrap: "wrap" }}>
        <div className="form-row" style={{ margin: 0 }}>
          <label htmlFor="provider-target-music-id">乐曲 ID</label>
          <input id="provider-target-music-id" value={musicId} onChange={(e) => setMusicId(e.target.value)} />
        </div>
        <div className="form-row" style={{ margin: 0 }}>
          <label htmlFor="provider-target-page-title">页面标题</label>
          <input id="provider-target-page-title" value={pageTitle} onChange={(e) => setPageTitle(e.target.value)} />
        </div>
        <div className="form-row" style={{ margin: 0 }}>
          <label htmlFor="provider-target-resolved-title">实际页面（可选）</label>
          <input id="provider-target-resolved-title" value={resolvedPageTitle} onChange={(e) => setResolvedPageTitle(e.target.value)} />
        </div>
        <div className="form-row" style={{ margin: 0 }}>
          <label htmlFor="provider-target-aliases">作者别名（可选）</label>
          <textarea id="provider-target-aliases" rows={3} value={aliases} onChange={(e) => setAliases(e.target.value)} />
        </div>
        <button className="btn btn-primary" onClick={save} disabled={busy || !musicId || !pageTitle}>保存映射</button>
      </div>
    </div>
  );
}
