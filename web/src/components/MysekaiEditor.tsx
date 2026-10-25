"use client";

import { forwardRef, useCallback, useEffect, useImperativeHandle, useRef, useState } from "react";
import { useToast } from "@/app/providers";
import { readUTF8File } from "@/components/EventStoryTxtImport";
import { getProjectionStatus } from "@/lib/api";
import { getMysekaiDetail, getMysekaiHistory, getMysekaiList, getMysekaiFontCoverage, importMysekai, publishMysekai, saveMysekai, type MysekaiDetail, type MysekaiEdit, type MysekaiHistory, type MysekaiPage } from "@/lib/mysekai-api";
import { missingFontCharacters, type FontCoverage } from "@/lib/mysekai-fonts";
import { mysekaiTemplate, parseMysekaiTemplate, type MysekaiImportRow } from "@/lib/mysekai-txt.mjs";
import { mysekaiPreviewContent, openMysekaiPreview } from "@/lib/mysekai-preview";

export interface MysekaiEditorHandle { isDirty: () => boolean; save: () => Promise<boolean>; discard: () => boolean }
interface Props { role: string; locale: string; writeLocked: boolean; onDirtyChange: (dirty: boolean) => void }
function download(text: string, name: string) { const url = URL.createObjectURL(new Blob([text], { type: "text/plain;charset=utf-8" })); const a = document.createElement("a"); a.href = url; a.download = name; a.click(); URL.revokeObjectURL(url); }

export const MysekaiEditor = forwardRef<MysekaiEditorHandle, Props>(function MysekaiEditor({ role, locale, writeLocked, onDirtyChange }, ref) {
  const { show } = useToast(); const region = "jp";
  const [page, setPage] = useState<MysekaiPage | null>(null);
  const [detail, setDetail] = useState<MysekaiDetail | null>(null);
  const [query, setQuery] = useState(""); const [state, setState] = useState("");
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [preview, setPreview] = useState<MysekaiImportRow[] | null>(null); const [selected, setSelected] = useState<Set<string>>(new Set());
  const [history, setHistory] = useState<MysekaiHistory[]>([]); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  const [publicationStatus, setPublicationStatus] = useState(""); const [publishConfirm, setPublishConfirm] = useState(false);
  const [fontCoverage, setFontCoverage] = useState<FontCoverage | null>(null); const [fontError, setFontError] = useState("");
  const refreshFontCoverage = async () => { try { setFontCoverage(await getMysekaiFontCoverage()); setFontError(""); } catch (error) { setFontCoverage(null); setFontError(error instanceof Error ? error.message : "字体目录暂不可用"); } };
  useEffect(() => { let live = true; void getMysekaiFontCoverage().then(value => { if (live) { setFontCoverage(value); setFontError(""); } }).catch(error => { if (live) setFontError(error.message); }); return () => { live = false; }; }, [locale]);
  const fileRef = useRef<HTMLInputElement>(null); const sourceRef = useRef<HTMLInputElement>(null); const generation = useRef(0);
  const previewCleanup = useRef<(() => void) | null>(null);
  useEffect(() => () => previewCleanup.current?.(), []);
  const dirty = Object.keys(edits).length > 0 || preview !== null; const locked = busy || writeLocked || locale === "ja-JP";
  const displayedDrafts = preview ? preview.filter(row => selected.has(row.id)).map(row => row.text)
    : detail ? [detail.script, ...detail.related].flatMap(script => script.lines.map(line => edits[line.id]
      ?? (detail.drafts[line.id]?.sourceHash === line.sourceHash ? detail.drafts[line.id]?.text ?? "" : ""))) : [];
  const missingGlyphs = fontCoverage ? missingFontCharacters(displayedDrafts.join("\n"), fontCoverage) : [];
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);
  const reload = useCallback(async (cursor = "") => {
    const result = await getMysekaiList(region, locale, query, state, cursor);
    setPage((old) => cursor && old ? { ...result, items: [...old.items, ...result.items] } : result);
    return result;
  }, [locale, query, state]);
  useEffect(() => { let live = true; const timer = setTimeout(() => { void getMysekaiList(region, locale, query, state).then((p) => { if (live) setPage(p); }).catch((e) => { if (live) setError(e.message); }); }, 200); return () => { live = false; clearTimeout(timer); }; }, [locale, query, state]);
  useEffect(() => { generation.current++; setDetail(null); setEdits({}); setPreview(null); setHistory([]); }, [locale]);
  const open = async (key: string) => {
    if (dirty || busy) { show("请先保存或放弃当前草稿，再切换对话", "err"); return; }
    const token = ++generation.current; setBusy(true); setError("");
    try { const d = await getMysekaiDetail(region, locale, key); if (generation.current === token) setDetail(d); }
    catch (e) { setError(e instanceof Error ? e.message : "载入失败"); } finally { if (generation.current === token) setBusy(false); }
  };
  const save = async () => {
    if (!detail || locked) return false;
    if (preview) { show("请先在上传预览中确认保存所选条目", "err"); return false; }
    const lines = [detail.script, ...detail.related].flatMap((s) => s.lines);
    const batch = lines.filter((l) => Object.hasOwn(edits, l.id)).map((l) => ({ id: l.id, sourceHash: l.sourceHash, text: edits[l.id], expectedRevision: detail.drafts[l.id]?.revision || 0 }));
    if (!batch.length) return true;
    return saveBatch(batch);
  };
  const saveBatch = async (batch: MysekaiEdit[]) => {
    if (!detail || locked || !batch.length) return false;
    setBusy(true); setError("");
    try { const d = await saveMysekai(detail, batch); setDetail(d); setEdits({}); setPreview(null); await reload(); show("草稿已保存，发布后才会出现在网站", "ok"); return true; }
    catch (e) { setError(e instanceof Error ? e.message : "保存失败；草稿仍保留"); return false; } finally { setBusy(false); }
  };
  useImperativeHandle(ref, () => ({ isDirty: () => dirty, save, discard: () => { if (busy) return false; setEdits({}); setPreview(null); return true; } }));
  const upload = async (file: File) => {
    if (!detail || dirty || locked) return;
    setBusy(true); setError("");
    try {
      const fresh = await getMysekaiDetail(region, locale, detail.script.key); const rows = parseMysekaiTemplate(await readUTF8File(file), fresh);
      setDetail(fresh); setPreview(rows); setSelected(new Set(rows.filter((r) => !r.conflict && !r.current).map((r) => r.id)));
    } catch (e) { setError(e instanceof Error ? e.message : "无法读取模板"); } finally { setBusy(false); }
  };
  const publication = async (action: "publish" | "unpublish" | "rollback", revision = "") => {
    if (!page || locked || dirty || (action === "publish" && (!fontCoverage || missingGlyphs.length))) return; setBusy(true); setError(""); setPublishConfirm(false);
    try { const result = await publishMysekai(action, page.channel, page.sourceRevision, page.draftRevision, revision); setPublicationStatus(result.projectionReady ? "公开文件已更新" : "已保存发布请求，公开文件仍在生成；生成失败时继续提供上一版"); setPage({ ...page, channel: result.channel }); setHistory(await getMysekaiHistory(region, locale)); }
    catch (e) { setError(e instanceof Error ? e.message : "发布失败"); } finally { setBusy(false); }
  };
  const importSource = async (file: File) => {
    if (!page || dirty || locked) return; setBusy(true); setError("");
    try { if (file.size > 64 * 1024 * 1024) throw new Error("原文目录超过 64 MB"); await importMysekai(JSON.parse(await file.text()), page.sourceRevision); setDetail(null); await reload(); show("原文已更新，人工译文和发布历史已保留", "ok"); }
    catch (e) { setError(e instanceof Error ? e.message : "原文导入失败"); } finally { setBusy(false); }
  };
  return <section className="mysekai-editor" aria-labelledby="mysekai-title">
    {fontError && <p className="mysekai-error" role="status">{fontError}。仍可保存草稿；请联系管理员恢复字体目录后再发布。<button className="btn btn-sm" onClick={() => void refreshFontCoverage()}>重新检查</button></p>}
    {missingGlyphs.length > 0 && <p className="mysekai-error" role="status">这些字符尚无可用字形：{missingGlyphs.join("、")}。草稿可以保存；发布前请修改文字或联系管理员更新字体。无需上传字库。<button className="btn btn-sm" onClick={() => void refreshFontCoverage()}>重新检查</button></p>}
    <header className="mysekai-heading"><div><h2 id="mysekai-title">MYSEKAI 对话</h2><p>日服场景 · {locale === "zh-CN" ? "简体中文" : locale}文本。保存为草稿，管理员审核后发布。</p></div>
      {role === "admin" && <div className="mysekai-actions"><button className="btn" disabled={locked || dirty} onClick={() => sourceRef.current?.click()}>同步原文目录</button><button className="btn btn-primary" disabled={locked || dirty || !page?.sourceRevision || !fontCoverage || missingGlyphs.length > 0} onClick={() => setPublishConfirm(true)}>发布已校对译文</button></div>}
    </header>
    {error && <p className="mysekai-error" role="alert">{error}</p>}
    {publicationStatus && <div role="status" className="mysekai-note">{publicationStatus} <button className="btn btn-sm" onClick={() => void getProjectionStatus().then((s) => setPublicationStatus(s.pending ? "公开文件仍在生成" : s.lastError ? `公开文件生成失败：${s.lastError}` : "公开文件已更新")).catch((e) => setError(e.message))}>检查发布状态</button></div>}
    {publishConfirm && <div className="mysekai-note"><p>发布当前语言所有与原文一致的非空草稿。原文有更新的旧译文会保留在工作台，等待重新校对。</p><button className="btn btn-primary" onClick={() => void publication("publish")}>确认发布</button> <button className="btn" onClick={() => setPublishConfirm(false)}>取消</button></div>}
    <div className="mysekai-columns">
      <aside className="mysekai-catalog" aria-label="对话目录"><label>查找对话、人物或原文<input value={query} onChange={(e) => setQuery(e.target.value)} type="search" /></label><label>翻译状态<select value={state} onChange={(e) => setState(e.target.value)}><option value="">全部</option><option value="untranslated">尚未译完</option><option value="review">原文有更新</option></select></label>
        <div className="mysekai-list">{page?.items.map((item) => <button key={item.key} className={`field-item ${detail?.script.key === item.key ? "active" : ""}`} aria-current={detail?.script.key === item.key ? "true" : undefined} onClick={() => void open(item.key)}><span>{item.key.startsWith("@tweet/") ? `气泡 ${item.key.slice(7)}` : item.key.replace(/^mysekai_talk_/, "")}</span><small>{item.translated}/{item.lines} 已译{item.review ? ` · ${item.review} 待复核` : ""}</small></button>)}</div>
        {page?.nextCursor && <button className="btn" disabled={busy} onClick={() => void reload(page.nextCursor).catch((e) => setError(e.message))}>载入更多</button>}
        {page && !page.items.length && <p className="mysekai-note">{page.sourceRevision ? "没有符合条件的对话" : "尚未同步原文。管理员导入提取器生成的文本目录后即可开始翻译。"}</p>}
      </aside>
      <div className="mysekai-workspace">
        {!detail ? <p className="center-state">{busy ? "正在载入对话…" : "选择一段对话开始翻译"}</p> : <>
          <div className="mysekai-actions"><h3>{detail.script.key}</h3><button className="btn" disabled={busy || dirty} onClick={() => download(mysekaiTemplate(detail), "mysekai-translation.txt")}>下载 TXT 模板</button><button className="btn" disabled={locked || dirty} onClick={() => fileRef.current?.click()}>上传 TXT</button><button className="btn btn-primary" disabled={locked || !Object.keys(edits).length || !!preview} onClick={() => void save()}>保存草稿</button>{dirty && <button className="btn" disabled={busy} onClick={() => { setEdits({}); setPreview(null); }}>放弃本次编辑</button>}</div>
          <div className="mysekai-actions"><button className="btn" disabled={busy || dirty || !mysekaiPreviewContent(detail)} onClick={() => { try { previewCleanup.current?.(); previewCleanup.current = openMysekaiPreview(detail); } catch (e) { setError(e instanceof Error ? e.message : "无法打开舞台预览"); } }}>舞台预览已保存草稿</button>{!mysekaiPreviewContent(detail) && <span>这类内容暂不支持舞台预览</span>}</div>
          <p className="mysekai-note">正文、姓名和气泡只改变显示文字。中文网站会优先使用已匹配的国服官方文本；这里的人工译文会继续保留。</p>
          {preview ? <div><h3>上传预览</h3><p>空白条目不覆盖已有译文；已有不同译文需要逐条勾选。原文或译文版本冲突的条目不能直接覆盖。</p>{preview.map((row) => <div className="mysekai-line" key={row.id}><label><input type="checkbox" disabled={!!row.conflict || locked} checked={selected.has(row.id)} onChange={(e) => setSelected((old) => { const next = new Set(old); if (e.target.checked) next.add(row.id); else next.delete(row.id); return next; })} />{row.original}</label>{row.current && <p>当前：{row.current}</p>}<p>导入：{row.text}</p>{row.conflict && <p className="mysekai-error">{row.conflict}</p>}</div>)}<button className="btn btn-primary" disabled={locked || !selected.size} onClick={() => void saveBatch(preview.filter((r) => selected.has(r.id) && !r.conflict).map(({ id, sourceHash, text, expectedRevision }) => ({ id, sourceHash, text, expectedRevision })))}>整批保存所选 {selected.size} 条</button> <button className="btn" disabled={busy} onClick={() => setPreview(null)}>关闭预览</button></div> : [detail.script, ...detail.related].map((script) => <div key={script.key}>{script.key !== detail.script.key && <h3>关联气泡</h3>}{script.lines.map((line, index) => { const draft = detail.drafts[line.id]; const changed = !!draft && draft.sourceHash !== line.sourceHash; return <div className="mysekai-line" key={line.id}><div className="mysekai-original"><span>{line.kind === "speaker" ? "显示姓名" : line.kind === "tweet" ? "气泡" : line.speaker || `第 ${index + 1} 句`}</span><p lang="ja">{line.text}</p>{line.cues?.length ? <small>语音：{line.cues.join("、")}</small> : null}</div><label>{changed ? "原文有更新 · 请重新校对" : "译文"}<textarea rows={line.kind === "speaker" ? 2 : 4} value={edits[line.id] ?? draft?.text ?? ""} disabled={locked} onChange={(e) => setEdits((old) => ({ ...old, [line.id]: e.target.value }))} />{changed && <button className="btn btn-sm" disabled={locked} onClick={() => setEdits((old) => ({ ...old, [line.id]: draft.text }))}>确认旧译文仍适用</button>}{draft && <small>{draft.updatedBy} · {new Date(draft.updatedAt * 1000).toLocaleString()}</small>}</label></div>; })}</div>)}
        </>}
      </div>
    </div>
    {role === "admin" && <details className="mysekai-history"><summary onClick={() => void getMysekaiHistory(region, locale).then(setHistory).catch((e) => setError(e.message))}>发布历史与回滚</summary><p>回滚只切换公开版本，当前草稿仍保留。已开始播放的会话继续使用其固定版本。</p>{page?.channel.publication && <button className="btn" disabled={locked || dirty} onClick={() => void publication("unpublish")}>撤下当前公开版本</button>}{history.map((h) => <div className="mysekai-actions" key={h.revision}><span>{new Date(h.updatedAt * 1000).toLocaleString()} · {h.updatedBy} · {h.action === "publish" ? "发布" : h.action === "rollback" ? "回滚" : "撤下"}</span>{h.publication && <button className="btn btn-sm" disabled={locked || dirty || page?.channel.publication === h.publication} onClick={() => void publication("rollback", h.publication)}>恢复此版本</button>}</div>)}</details>}
    <input ref={fileRef} type="file" accept=".txt,text/plain" hidden onChange={(e) => { const file = e.target.files?.[0]; if (file) void upload(file); e.target.value = ""; }} />
    <input ref={sourceRef} type="file" accept=".json,application/json" hidden onChange={(e) => { const file = e.target.files?.[0]; if (file) void importSource(file); e.target.value = ""; }} />
  </section>;
});
