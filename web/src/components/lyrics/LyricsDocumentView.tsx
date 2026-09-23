"use client";

import { useRef, useState } from "react";
import { LyricsEditionMenu } from "@/components/LyricsEditionMenu";
import { LyricsCatalogSidebar, runtimeLyricsStateLabel, runtimeLyricsVersionsLabel } from "@/components/lyrics/LyricsCatalogSidebar";
import { LyricsCollaborationBanner } from "@/components/lyrics/LyricsCollaborationBanner";
import { LyricsLineEditor } from "@/components/lyrics/LyricsLineEditor";
import { LyricsMetadataCard } from "@/components/lyrics/LyricsMetadataCard";
import { LyricsProjectionStatusCard } from "@/components/lyrics/LyricsProjectionStatusCard";
import { LyricsVocalCard } from "@/components/lyrics/VocalPlayer";
import {
  databaseAvailabilityDescription, detailLabel, isLegacyLyricsDocument, sourceImportFailureIsTerminal, sourceLabel,
} from "@/components/lyrics/lyricsDocumentModel";
import type { LyricsActiveTarget, LyricsEditorState } from "@/components/lyrics/lyricsEditorState";
import type { LyricsDocumentCommands } from "@/components/lyrics/useLyricsDocumentCommands";
import type { LyricsDocumentLoader } from "@/components/lyrics/useLyricsDocumentLoader";
import type { LyricsPersistence } from "@/components/lyrics/useLyricsPersistence";
import type { LyricsSourceWorkflow } from "@/components/lyrics/useLyricsSourceWorkflow";
import { lyricsRenditionByKey, normalizedLyricsVersions } from "@/lib/lyrics-versioning.mjs";
import { performerRepresentativeColor } from "@/lib/performer-colors.mjs";
import type { LyricRubySpan, LyricsPerformerID, LyricsRenditionPerformer } from "@/lib/api";

interface ResolvedLyricsComponentProvenanceRow {
  component: string;
  label: string;
  renditionKey: string;
  identity: {
    provider: string;
    revisionId: number;
    canonicalUrl: string;
    section: string;
  } | null;
}

function renderRubySpans(spans: LyricRubySpan[]) {
  return spans.map((span, index) => span.reading
    ? <ruby key={`${index}:${span.text}:${span.reading}`}>{span.text}<rt>{span.reading}</rt></ruby>
    : <span key={`${index}:${span.text}`}>{span.text}</span>);
}

export interface LyricsDocumentViewProps {
  state: LyricsEditorState;
  activeTarget: LyricsActiveTarget;
  loader: LyricsDocumentLoader;
  commands: LyricsDocumentCommands;
  source: LyricsSourceWorkflow;
  persistence: LyricsPersistence;
  role: "admin" | "editor" | "";
  publicationChecks: Array<{ label: string; complete: boolean }>;
  publicationComplete: boolean;
}

export function LyricsDocumentView({
  state, activeTarget, loader, commands, source, persistence, role, publicationChecks, publicationComplete,
}: LyricsDocumentViewProps) {
  const {
    query, setQuery, catalog, catalogLoading, catalogError, selectedMusic, busy, loading, lyrics, dirty,
    databaseAvailabilityOnly, runtimeOnlyMissingDatabaseSource, collaborationStructuralConflict,
    collaborationRef, collaborationStatus, collaborationError, collaborationPeers, localSourceImportDraft,
    writeLocked, error, performerError, sourceImportTokenRef, sourceRetry, sourceActivity,
    sourcePreview, setSourcePreview, sourcePreviewCandidate, setSourcePreviewCandidate,
    candidates, sourceSearchCompleted, setConfirmSourceImport, setConfirmConflictReload,
    activeTranslationEditionKey, activeRenditionKey, setActiveRenditionKey, activeVersion, setActiveVersion,
    projectionState, projectionStatus, projectionMessage, linesContainerRef, segmentInputRefs,
  } = state;
  const {
    renditionDocument, translationEditions, activeTranslationEdition, renditionKeys, availableVersions,
    hasGameVersion, gameSideReadOnlyReason, projectionKind, activeRendition, legacyLyrics, activeSide,
    activeLines, activeSideReadOnly, activeSideSourceMutable, activePerformerOptions,
    activeTranslationCredit, activeProofreadingCredit, componentProvenance, versionSaveProblems,
    gameProjection, hasPerformerSegmentation,
  } = activeTarget;
  const { performChooseMusic, loadCatalog, loadPerformers, chooseMusic } = loader;
  const {
    updateLyrics, updateActiveCredits, updateLine, moveLine, removeLine, updateSegment, updateRubySpan,
    splitRubySpan, mergeRubyWithPrevious, addSegment, splitSegment, mergeWithPreviousSegment,
    removeSegment, moveSegment, addLine, applyPerformerToAllSegments,
  } = commands;
  const { findSource, previewSource } = source;
  const { save, publish, requestEditionSwitch, requestEditionCommand, refreshProjectionStatus } = persistence;
  const [previewLocale, setPreviewLocale] = useState<"ja-JP" | "zh-CN" | "en-US">("zh-CN");
  const previewTabRefs = useRef<Record<"ja-JP" | "zh-CN" | "en-US", HTMLButtonElement | null>>({
    "ja-JP": null, "zh-CN": null, "en-US": null,
  });

  const performerName = (id: LyricsPerformerID) => {
    const performer = activePerformerOptions.find((item) => String(item.performerId) === String(id));
    if (!performer) return String(id);
    return typeof performer.name === "string"
      ? performer.name
      : performer.name["zh-CN"] || performer.name["ja-JP"] || String(id);
  };

  const performerColor = (id: LyricsPerformerID) => {
    const performer = activePerformerOptions.find((item) => String(item.performerId) === String(id));
    return "color" in (performer || {}) && typeof (performer as LyricsRenditionPerformer | undefined)?.color === "string"
      ? (performer as LyricsRenditionPerformer).color
      : performerRepresentativeColor(performerName(id));
  };

  const handleEditorKeyDown = (event: React.KeyboardEvent<HTMLFieldSetElement>) => {
    if (!(event.metaKey || event.ctrlKey) || event.key.toLowerCase() !== "s") return;
    event.preventDefault();
    if (!busy && !writeLocked && dirty) void save();
  };

  const previewLocales = ["ja-JP", "zh-CN", "en-US"] as const;
  const handlePreviewTabKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, locale: typeof previewLocales[number]) => {
    const index = previewLocales.indexOf(locale);
    let target = index;
    if (event.key === "ArrowRight") target = (index + 1) % previewLocales.length;
    else if (event.key === "ArrowLeft") target = (index - 1 + previewLocales.length) % previewLocales.length;
    else if (event.key === "Home") target = 0;
    else if (event.key === "End") target = previewLocales.length - 1;
    else return;
    event.preventDefault();
    const nextLocale = previewLocales[target];
    setPreviewLocale(nextLocale);
    previewTabRefs.current[nextLocale]?.focus();
  };

  return (
    <>
      <LyricsCatalogSidebar
        query={query}
        onQueryChange={setQuery}
        catalog={catalog}
        catalogLoading={catalogLoading}
        catalogError={catalogError}
        selectedMusic={selectedMusic}
        busy={busy}
        onChooseMusic={chooseMusic}
        onRetryCatalog={() => void loadCatalog(query)}
      />

      <section className="lyrics-editor">
        {!selectedMusic ? <div className="center-state"><p>从目录选择一首曲目</p></div> : loading ? (
          <div className="center-state" role="status" aria-live="polite"><div className="spinner" />加载歌词…</div>
        ) : databaseAvailabilityOnly && selectedMusic.lyricsAvailabilityState ? (
          <div className="lyrics-runtime-only" role="status">
            <strong>数据库已记录歌词可用性，但当前没有可编辑正文</strong>
            <p>{databaseAvailabilityDescription(selectedMusic.lyricsAvailabilityState)}</p>
            <p>这不是“数据库未录入”：状态已持久化到 SQLite，系统只是不把无正文或未闭合来源伪装成可保存的空歌词。</p>
            <button className="btn btn-secondary" onClick={() => void performChooseMusic(selectedMusic)}>重新检查数据库</button>
          </div>
        ) : runtimeOnlyMissingDatabaseSource && selectedMusic.runtimeLyrics ? (
          <div className="lyrics-runtime-only" role="status">
            <strong>公开镜像仍在，后台数据库尚无可编辑源</strong>
            <p>这首歌包含在 embedded Public Lyrics {selectedMusic.runtimeLyrics.releaseId} 中，状态为“{runtimeLyricsStateLabel(selectedMusic.runtimeLyrics.state)}”，可用版本为 {runtimeLyricsVersionsLabel(selectedMusic.runtimeLyrics.availableVersions)}。该镜像是 standalone binary 内的只读发布资产，不是 SQLite 草稿或发布记录。</p>
            <p>系统不会把 detail 404 静默转换成可保存的空草稿。生产启动时的私有 embedded editor seed 负责补入缺失正文；该流程不会在此页面触发，也不会覆盖账号、普通翻译、剧情、审核或既有歌词。</p>
            <button className="btn btn-secondary" onClick={() => void performChooseMusic(selectedMusic)}>重新检查数据库</button>
          </div>
        ) : collaborationStructuralConflict && !lyrics ? (
          <div className="lyrics-runtime-only lyrics-structural-conflict-empty" role="alert">
            <strong>协作文档发生结构冲突，当前没有可安全显示的编辑副本</strong>
            <p>Yjs 文档的歌词行、分段或注音结构无法安全物化。协作连接已停止，编辑和保存均已禁用，以避免覆盖其他协作者的内容。</p>
            <p>请重新加载当前歌词；若问题仍然出现，请联系其他协作者停止编辑，并由管理员检查协作文档。</p>
            <button className="btn btn-secondary" onClick={() => void performChooseMusic(selectedMusic)} disabled={busy}>重新加载歌词</button>
          </div>
        ) : lyrics ? (
          <fieldset className="lyrics-edit-fence" disabled={busy} aria-busy={busy} aria-disabled={writeLocked} onKeyDown={handleEditorKeyDown}>
            <div className="lyrics-editor-head">
              <div className="lyrics-editor-head-copy">
                <div className="lyrics-editor-title-row">
                  <h2>{selectedMusic.title["zh-CN"] || selectedMusic.title["ja-JP"]}</h2>
                  {renditionDocument && <LyricsEditionMenu
                    editions={translationEditions}
                    activeEditionKey={activeTranslationEditionKey}
                    defaultEditionKey={renditionDocument.defaultTranslationEditionKey}
                    disabled={busy || writeLocked}
                    onSelect={requestEditionSwitch}
                    onCommand={requestEditionCommand}
                  />}
                </div>
                <span>musicId {lyrics.musicId} · revision {lyrics.revision} · {lyrics.status === "published" ? "当前修订已发布" : lyrics.status === "draft-published" ? `草稿（revision ${lyrics.publishedRevision} 仍公开）` : "草稿"}{activeTranslationEdition ? ` · 译本 ${activeTranslationEdition.label}` : ""}</span>
              </div>
              <div className="lyrics-actions">
                {role === "admin" && isLegacyLyricsDocument(lyrics) && <button className="btn btn-secondary" onClick={findSource} disabled={busy || writeLocked || lyrics.revision > 0}>{sourceActivity === "searching" ? "正在查找…" : "查找来源"}</button>}
                <button className="btn btn-primary" onClick={save} disabled={busy || writeLocked || !dirty}>保存草稿</button>
                {role === "admin" && isLegacyLyricsDocument(lyrics) && lyrics.revision > 0 && lyrics.status !== "published" && <button className="btn btn-secondary" onClick={() => publish(true)} disabled={busy || writeLocked}>发布当前修订</button>}
                {role === "admin" && isLegacyLyricsDocument(lyrics) && Boolean(lyrics.publishedRevision) && <button className="btn btn-secondary" onClick={() => publish(false)} disabled={busy || writeLocked}>取消发布 revision {lyrics.publishedRevision}</button>}
              </div>
            </div>

            <LyricsCollaborationBanner
              collaborationStructuralConflict={collaborationStructuralConflict}
              localSourceImportDraft={localSourceImportDraft}
              collaborationStatus={collaborationStatus}
              collaborationError={collaborationError}
              collaborationPeers={collaborationPeers}
              busy={busy}
              onReload={() => void performChooseMusic(selectedMusic)}
              onReconnect={() => collaborationRef.current?.reconnectNow()}
            />

            <LyricsVocalCard musicId={selectedMusic.musicId} />

            {renditionDocument ? (
              <div className="lyrics-lock-notice locked" role="status">
                <strong>Plural source facts 已由固定证据永久锁定</strong>
                <span>当前编辑器按 stable rendition key 与 Full/Game side 分开保存简中译文，并保留每个 rendition 的翻译/校对署名。Full/Game source text、行 ID/顺序、relation、provenance、演唱者、分段、ruby 与英文均不会通过此路由改写；exact projection 的 Game 仍跟随 Full。</span>
              </div>
            ) : lyrics.revision === 0 ? (
              <div className="lyrics-lock-notice" role="note">
                <strong>首次保存后永久锁定来源、行序/ID 与日文原文</strong>
                <span>保存后，来源资料、歌词行顺序与编号、日文原文将不可直接修改。请先完成来源核对、行顺序和分段文字检查；后续仍可在不改变日文全文的前提下重新分段，并编辑中英翻译、演唱者与备注。</span>
              </div>
            ) : (
              <div className="lyrics-lock-notice locked" role="status">
                <strong>来源、行序/ID 与日文原文已永久锁定</strong>
                <span>当前修订可在保持每行日文拼接结果完全一致的前提下重新分段，并调整中英翻译、演唱者与备注。如需修正来源、行序或日文原文，必须另行设计并审核显式迁移流程。</span>
              </div>
            )}

            {renditionDocument ? (
              <div className="lyrics-publication-progress" role="note">
                <div><strong>Public v3 发布由 recovery batch 管理</strong><span>此页面不会调用 legacy publish/unpublish</span></div>
                <ul><li className="complete"><span aria-hidden="true">✓</span>可按 stable key 独立保存 Full、Game-only 与 independent Game 简中译文；exact projection Game 继续由 Full 推导</li></ul>
              </div>
            ) : (
              <div className="lyrics-publication-progress" role="status" aria-live="polite">
                <div><strong>发布准备</strong><span>{publicationComplete ? "已满足发布前置条件" : `还需完成 ${publicationChecks.filter((check) => !check.complete).length} 项`}</span></div>
                <ul>{publicationChecks.map((check) => <li key={check.label} className={check.complete ? "complete" : "pending"}><span aria-hidden="true">{check.complete ? "✓" : "○"}</span>{check.label}</li>)}</ul>
              </div>
            )}

            <LyricsProjectionStatusCard
              projectionState={projectionState}
              projectionStatus={projectionStatus}
              projectionMessage={projectionMessage}
              busy={busy}
              onRefresh={() => void refreshProjectionStatus()}
            />

            {error && (
              <div className="lyrics-error" role="alert">
                <strong>{sourceLabel(error)}</strong>
                {sourceImportTokenRef.current && !sourceImportFailureIsTerminal(error) && <span>未确认首次保存已提交；已保留固定修订授权和 verified draft，可直接重试保存。</span>}
                {!sourceImportTokenRef.current && sourceRetry && <span>固定修订授权或 verified draft 状态已失效，请重新预览后再保存。</span>}
                {error.details.map((detail) => <span key={detail}>{detailLabel(detail)}</span>)}
                {sourceRetry && lyrics.revision === 0 && role === "admin" && (
                  <button className="btn btn-secondary btn-sm" onClick={() => sourceRetry.kind === "search" ? void findSource() : void previewSource(sourceRetry.candidate)} disabled={busy || writeLocked}>{sourceRetry.kind === "search" ? "重试查找来源" : "重试载入固定修订"}</button>
                )}
                {error.code === "revision_conflict" && error.current && (
                  <button className="btn btn-secondary btn-sm" onClick={() => setConfirmConflictReload(true)}>载入服务器版本</button>
                )}
              </div>
            )}

            {versionSaveProblems.length > 0 && (
              <div className="lyrics-error lyrics-version-error" role="alert">
                <strong>Rendition / projection 或公开署名合同需要修复后才能保存</strong>
                <span>每个 stable key 的 Full 与 Game 都保持在本 family 内；不会因为文本相同而跨 family 合并。</span>
                {versionSaveProblems.map((problem) => <span key={problem}>{problem}</span>)}
              </div>
            )}

            {role === "admin" && lyrics.revision === 0 && sourceActivity === "searching" && (
              <div className="lyrics-source-panel lyrics-source-loading" role="status" aria-live="polite"><div className="spinner" /><span>正在搜索并核对候选来源…</span></div>
            )}

            {role === "admin" && lyrics.revision === 0 && sourceSearchCompleted && (
              <div className="lyrics-source-panel" aria-live="polite">
                <div className="lyrics-source-title"><strong>候选来源</strong><button type="button" className="btn btn-ghost btn-sm" onClick={() => void findSource()} disabled={busy || writeLocked}>重新搜索</button></div>
                {candidates.length === 0 ? <span className="lyrics-muted">没有找到可核对的歌词来源。可以调整曲目资料后再试，或使用手动歌词行。</span> : candidates.map((candidate) => (
                  <button key={`${candidate.pageId}-${candidate.revisionId}`} aria-label={`预览候选来源 ${candidate.title} 的固定修订 ${candidate.revisionId}`} onClick={() => previewSource(candidate)} disabled={busy || writeLocked}>
                    {candidate.title}<span>固定修订 {candidate.revisionId}</span>
                  </button>
                ))}
              </div>
            )}

            {role === "admin" && lyrics.revision === 0 && sourceActivity === "previewing" && (
              <div className="lyrics-source-preview lyrics-source-loading" role="status" aria-live="polite"><div className="spinner" /><span>正在载入固定修订 {sourcePreviewCandidate?.revisionId || ""} 的完整预览…</span></div>
            )}

            {sourcePreview && lyrics.revision === 0 && (
              <div className="lyrics-source-preview" aria-labelledby="lyrics-source-preview-title">
                <div><strong id="lyrics-source-preview-title">固定修订预览 · 共 {sourcePreview.lines.length} 行</strong><a href={sourcePreview.canonicalUrl} target="_blank" rel="noopener noreferrer">打开来源</a></div>
                <p className="lyrics-muted">下方展示解析后的全部 {sourcePreview.lines.length} 行，不会只截取前几行；滚动区域仅影响显示。请核对完整日文歌词。使用此版本会把固定 revision 与一次性导入授权载入 revision 0 草稿，并清空现有中英翻译；来源中的演唱者证据会在可安全映射时保留，无法映射时会显示错误并阻止载入。网络或服务器瞬时失败会保留授权和 verified draft，可直接重试。仅当服务端明确拒绝授权、来源身份/修订或内容生产者已变化等终态发生时，才需要重新预览。只有首次保存成功才会永久锁定来源、行顺序与日文原文。</p>
                <pre tabIndex={0} aria-label={`固定修订 ${sourcePreview.revisionId} 的完整歌词，共 ${sourcePreview.lines.length} 行`}>{sourcePreview.lines.map((line) => `${line.stanzaBreakBefore ? "\n" : ""}${line.japanese}`).join("\n")}</pre>
                <div className="lyrics-actions"><button className="btn btn-primary" onClick={() => setConfirmSourceImport(true)} disabled={writeLocked}>使用此版本</button><button className="btn btn-ghost" onClick={() => { setSourcePreview(null); setSourcePreviewCandidate(null); sourceImportTokenRef.current = ""; }}>取消</button></div>
              </div>
            )}

            {activeRendition && <div className="lyrics-version-switcher lyrics-rendition-switcher">
              <div className="lyrics-version-tabs" role="tablist" aria-label="稳定 rendition family">
                {renditionKeys.map((renditionKey: string) => {
                  const rendition = lyricsRenditionByKey(lyrics, renditionKey);
                  return <button type="button" role="tab" key={renditionKey} aria-selected={activeRenditionKey === renditionKey} tabIndex={activeRenditionKey === renditionKey ? 0 : -1} className={activeRenditionKey === renditionKey ? "active" : ""} onClick={() => {
                    setActiveRenditionKey(renditionKey);
                    const versions = normalizedLyricsVersions(lyrics, renditionKey);
                    setActiveVersion(versions.includes("full") ? "full" : "game");
                  }}>{rendition?.label || renditionKey}<span>{renditionKey}</span></button>;
                })}
              </div>
              <p>每个 stable key 都保留自己的 Full / Game、relation、演唱者分段、ruby、翻译与翻译/校对署名；即使文本相同也不会与其他 family 合并。</p>
            </div>}

            <div className="lyrics-version-switcher">
              <div className="lyrics-version-tabs" role="tablist" aria-label="歌词版本工作区">
                {availableVersions.includes("full") && <button type="button" role="tab" id="lyrics-version-full-tab" aria-controls="lyrics-version-panel" aria-selected={activeVersion === "full"} tabIndex={activeVersion === "full" ? 0 : -1} className={activeVersion === "full" ? "active" : ""} onClick={() => setActiveVersion("full")}>Full <span>{activeRendition ? "仅简中可编辑" : "可编辑"}</span></button>}
                {hasGameVersion && <button type="button" role="tab" id="lyrics-version-game-tab" aria-controls="lyrics-version-panel" aria-selected={activeVersion === "game"} tabIndex={activeVersion === "game" ? 0 : -1} className={activeVersion === "game" ? "active" : ""} onClick={() => setActiveVersion("game")}>Game <span>{gameSideReadOnlyReason === "exact_projection" ? "只读 exact projection" : "独立简中可编辑"}</span></button>}
              </div>
              <p>{activeRendition
                ? activeVersion === "game" && gameSideReadOnlyReason === "exact_projection"
                  ? `Game 只引用同一 stable key（${activeRendition.key}）的 Full 行 ID；Game 自有分段和 ruby 原样保留，简中译文由 Full 对应行同步。`
                  : activeVersion === "game" && projectionKind === "game_only"
                    ? `${activeRendition.key} 是 Game-only rendition，没有 Full peer；Game 简中按该 stable key/side 独立保存，source facts 与英文保持只读。`
                    : activeVersion === "game"
                      ? `${activeRendition.key} 的 independent Game 简中按该 stable key/side 独立保存，不会覆盖 Full 或其他 rendition family。`
                      : `${activeRendition.key} 的 Full 简中按该 stable key/side 独立保存；source facts 与英文保持只读。`
                : activeVersion === "full"
                  ? "singular v2 Full 保持原有可编辑行为。"
                  : "singular v2 Game 继续作为同一 Full 的只读行 ID 投影，不做有损 v2 coercion。"}</p>
            </div>

            <LyricsMetadataCard
              activeTranslationCredit={activeTranslationCredit}
              activeProofreadingCredit={activeProofreadingCredit}
              activeRendition={activeRendition}
              legacyLyrics={legacyLyrics}
              activeVersion={activeVersion}
              projectionKind={projectionKind}
              writeLocked={writeLocked}
              updateActiveCredits={updateActiveCredits}
              onUpdateLyrics={updateLyrics}
            />

            <section className="lyrics-component-provenance" aria-labelledby="lyrics-component-provenance-title">
              <div><strong id="lyrics-component-provenance-title">组件 provenance</strong><span>仅认证编辑器显示固定证据；公开输出使用对应版本的严格 attribution contract</span></div>
              {componentProvenance.length === 0 ? (
                <p>当前歌词没有组件级固定来源映射；旧版单一来源字段仍保持只读，不会被伪装成 Full / Game / ruby 的独立证据。</p>
              ) : (
                <dl>{componentProvenance.map((row: ResolvedLyricsComponentProvenanceRow) => <div key={row.component}>
                  <dt>{row.label}</dt>
                  <dd>
                    <code>{row.renditionKey}</code>
                    {row.identity ? <>
                      <span>{row.identity.provider === "moegirl" || row.identity.provider === "moegirl_public_exact"
                        ? "萌娘百科"
                        : row.identity.provider === "sekaipedia" ? "Sekaipedia" : "Vocaloid Wiki"} · revision {row.identity.revisionId} · {row.identity.section}</span>
                      <a href={row.identity.canonicalUrl} target="_blank" rel="noopener noreferrer">打开固定来源</a>
                    </> : <span>未找到对应固定来源详情</span>}
                  </dd>
                </div>)}</dl>
              )}
            </section>

            {hasPerformerSegmentation && !activeRendition && performerError && (
              <div className="lyrics-error" role="alert">
                <strong>演唱者目录加载失败，角色分词歌曲发布前需要重新载入</strong>
                <button className="btn btn-secondary btn-sm" onClick={() => void loadPerformers()}>重试加载演唱者</button>
              </div>
            )}

            {hasPerformerSegmentation && !activeRendition && !activeSideReadOnly && activeLines.length > 0 && activePerformerOptions.length > 0 && (
              <div className="lyrics-bulk-performer">
                <label htmlFor="lyrics-all-performer">统一设置当前 side 全部分段演唱者</label>
                <select id="lyrics-all-performer" defaultValue="" disabled={writeLocked} onChange={(event) => {
                  const value = event.target.value;
                  const performerID: LyricsPerformerID = activeRendition ? value : Number(value);
                  if (value) applyPerformerToAllSegments(performerID);
                  event.currentTarget.value = "";
                }}>
                  <option value="">选择一位演唱者…</option>
                  {activePerformerOptions.map((performer) => <option key={performer.performerId} value={performer.performerId}>{typeof performer.name === "string" ? performer.name : performer.name["zh-CN"] || performer.name["ja-JP"]}</option>)}
                </select>
                <span>只覆盖 {activeRenditionKey || "legacy-v2"} 的 {activeVersion} side，不影响其他 rendition family。</span>
              </div>
            )}

            {!activeSideReadOnly && (!activeRendition || activeSide) && <div id="lyrics-version-panel" role="tabpanel" aria-labelledby={`lyrics-version-${activeVersion}-tab`} className="lyrics-lines" ref={linesContainerRef}>
              {activeLines.length === 0 && <div className="lyrics-empty-lines"><strong>当前 {activeVersion === "full" ? "Full" : "Game"} side 还没有歌词行</strong><span>{role === "admin" && !activeRendition ? "可以查找 Wiki 来源，或手动添加歌词行。" : "请在来源仍可修改时添加该 side 的歌词行。"}</span></div>}
              {activeLines.map((line, lineIndex) => (
                <LyricsLineEditor
                  key={`${activeRenditionKey || "legacy-v2"}:${activeVersion}:${line.id}`}
                  line={line}
                  lineIndex={lineIndex}
                  lineCount={activeLines.length}
                  sourceMutable={activeSideSourceMutable}
                  writeLocked={writeLocked || activeSideReadOnly}
                  showPerformerSegmentation={hasPerformerSegmentation}
                  performers={activePerformerOptions}
                  performerName={performerName}
                  performerColor={performerColor}
                  registerSegmentInput={(segmentIndex, element) => { segmentInputRefs.current[`${activeRenditionKey}:${activeVersion}:${lineIndex}-${segmentIndex}`] = element; segmentInputRefs.current[`${lineIndex}-${segmentIndex}`] = element; }}
                  onUpdateLine={(patch) => updateLine(lineIndex, patch)}
                  onMoveLine={(direction) => moveLine(lineIndex, direction)}
                  onRemoveLine={() => removeLine(lineIndex)}
                  onUpdateSegment={(segmentIndex, text, performerIds) => updateSegment(lineIndex, segmentIndex, text, performerIds)}
                  onUpdateRubySpan={(segmentIndex, rubyIndex, patch) => updateRubySpan(lineIndex, segmentIndex, rubyIndex, patch)}
                  onSplitRubySpan={(segmentIndex, rubyIndex) => splitRubySpan(lineIndex, segmentIndex, rubyIndex)}
                  onMergeRubyWithPrevious={(segmentIndex, rubyIndex) => mergeRubyWithPrevious(lineIndex, segmentIndex, rubyIndex)}
                  onAddSegment={(segmentIndex) => addSegment(lineIndex, segmentIndex)}
                  onSplitSegment={(segmentIndex) => splitSegment(lineIndex, segmentIndex)}
                  onMergeWithPreviousSegment={(segmentIndex) => mergeWithPreviousSegment(lineIndex, segmentIndex)}
                  onRemoveSegment={(segmentIndex) => removeSegment(lineIndex, segmentIndex)}
                  onMoveSegment={(segmentIndex, direction) => moveSegment(lineIndex, segmentIndex, direction)}
                />
              ))}
              {activeSideSourceMutable && <button className="btn btn-secondary lyrics-add-line" onClick={addLine} disabled={writeLocked}>添加 {activeVersion === "full" ? "Full" : "Game"} 歌词行</button>}
            </div>}

            {!activeSideReadOnly ? <div className="lyrics-public-preview">
              <strong>{activeRenditionKey || "legacy-v2"} · {activeVersion === "full" ? "Full" : "Game"} 公开文件预览</strong>
              <div className="lyrics-preview-tabs" role="tablist" aria-label="歌词预览语言">
                {previewLocales.map((locale) => <button
                  key={locale}
                  ref={(element) => { previewTabRefs.current[locale] = element; }}
                  id={`lyrics-preview-tab-${locale}`}
                  type="button"
                  role="tab"
                  tabIndex={previewLocale === locale ? 0 : -1}
                  aria-selected={previewLocale === locale}
                  aria-controls="lyrics-preview-panel"
                  className={previewLocale === locale ? "active" : ""}
                  onClick={() => setPreviewLocale(locale)}
                  onKeyDown={(event) => handlePreviewTabKeyDown(event, locale)}
                >{locale === "ja-JP" ? "日文" : locale === "zh-CN" ? "简中" : "英文"}</button>)}
              </div>
              <div id="lyrics-preview-panel" role="tabpanel" aria-labelledby={`lyrics-preview-tab-${previewLocale}`} lang={previewLocale === "ja-JP" ? "ja" : previewLocale === "zh-CN" ? "zh-CN" : "en"}>
                {activeLines.length === 0 ? <p className="lyrics-muted">保存当前 side 的歌词行后会在这里显示公开文件效果。</p> : activeLines.map((line) => <p key={line.id} className={line.stanzaBreakBefore ? "lyrics-stanza-start" : undefined}>{previewLocale === "ja-JP" ? line.segments.map((segment, segmentIndex) => <span key={`${line.id}:${segmentIndex}`} className="lyrics-ruby-preview-segment">{segment.performerIds.length > 0 && <span className="lyric-performer-squares" role="img" aria-label={`演唱者：${segment.performerIds.map(performerName).join("、")}`}>{segment.performerIds.map((performerId) => <i key={performerId} className="lyric-performer-swatch" aria-hidden="true" style={performerColor(performerId) ? { backgroundColor: performerColor(performerId) } : undefined} />)}</span>}{renderRubySpans(segment.ruby)}</span>) : line[previewLocale] || ""}</p>)}
              </div>
            </div> : <section id="lyrics-version-panel" role="tabpanel" aria-labelledby="lyrics-version-game-tab" className="lyrics-game-preview">
              <header>
                <div><strong>Game 只读 exact-projection</strong><span>{activeRendition ? `${activeRendition.key} → ${activeRendition.relation.fullRenditionKey}` : legacyLyrics ? legacyLyrics.gameProjection?.reasonCode || "缺少版本判定" : "缺少稳定 rendition"}</span></div>
                <p>投影严格限制在同一 stable key。Game side 自有的演唱者分段与 ruby 会按原样展示，简中译文按 relation 指向的 Full 行同步，不会从其他 family 的相同文本推断或合并。</p>
              </header>
              {!gameProjection.ok ? <div className="lyrics-error" role="alert">{gameProjection.errors.map((problem) => <span key={problem}>{problem}</span>)}</div> : gameProjection.lines.length === 0 ? <p className="lyrics-muted">当前没有可预览的 Game 行投影。</p> : <ol>
                {gameProjection.lines.map((line, index) => <li key={line.id} className={line.stanzaBreakBefore ? "lyrics-stanza-start" : undefined}>
                  <header><strong>{String(index + 1).padStart(2, "0")}</strong><code>{line.id}</code></header>
                  <p lang="ja">{line.segments.map((segment, segmentIndex) => <span key={`${line.id}:game:${segmentIndex}`} className="lyrics-ruby-preview-segment">{segment.performerIds.length > 0 && <span className="lyric-performer-squares" role="img" aria-label={`演唱者：${segment.performerIds.map(performerName).join("、")}`}>{segment.performerIds.map((performerId) => <i key={performerId} className="lyric-performer-swatch" aria-hidden="true" title={performerName(performerId)} style={performerColor(performerId) ? { backgroundColor: performerColor(performerId) } : undefined} />)}</span>}{renderRubySpans(segment.ruby)}</span>)}</p>
                  <div className="lyrics-game-translations"><span lang="zh-CN">{line["zh-CN"] || "简中待翻译"}</span><span lang="en">{line["en-US"] || "English pending"}</span></div>
                </li>)}
              </ol>}
            </section>}
          </fieldset>
        ) : <div className="center-state" role="alert"><p>{error ? sourceLabel(error) : "歌词加载失败"}</p><button className="btn btn-secondary" onClick={() => selectedMusic && void performChooseMusic(selectedMusic)}>重试加载歌词</button></div>}
      </section>
    </>
  );
}
