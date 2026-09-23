"use client";

import { forwardRef, useEffect, useImperativeHandle } from "react";
import { useToast } from "@/app/providers";
import { Modal } from "@/components/Modal";
import { LyricsDocumentView } from "@/components/lyrics/LyricsDocumentView";
import {
  isLegacyLyricsDocument, lyricsPublicationReadiness, sourceLabel,
} from "@/components/lyrics/lyricsDocumentModel";
import { useLyricsActiveTarget, useLyricsEditorState } from "@/components/lyrics/lyricsEditorState";
import { useLyricsDocumentCommands } from "@/components/lyrics/useLyricsDocumentCommands";
import { useLyricsDocumentLoader } from "@/components/lyrics/useLyricsDocumentLoader";
import { useLyricsPersistence } from "@/components/lyrics/useLyricsPersistence";
import { useLyricsSourceWorkflow } from "@/components/lyrics/useLyricsSourceWorkflow";
import { isTranslationEditionLabel, selectTranslationEditionKey } from "@/lib/lyrics-editions.mjs";
import type { SongLyricsDocument } from "@/lib/api";
import { canonicalLyricsJSON } from "@/lib/yjs-lyrics";

export interface LyricsEditorHandle {
  save: () => Promise<boolean>;
  discard: () => boolean;
  isDirty: () => boolean;
  snapshot: () => { dirty: boolean; document: SongLyricsDocument | null; generation: number; editionKey: string };
  isEditing: (musicID: number) => boolean;
  activeTarget: () => {
    musicId: number;
    editionKey: string;
    renditionKey: string;
    side: "full" | "game";
    locale: "zh-CN";
    projectionKind: "full_only" | "game_only" | "exact_projection" | "independent_game" | "invalid";
  } | null;
  reloadCatalog: () => void;
  exportDraft: () => SongLyricsDocument | null;
  reloadAuthoritative: () => Promise<boolean>;
}

interface LyricsEditorProps {
  role: "admin" | "editor" | "";
  writeLocked?: boolean;
  onDirtyChange?: (dirty: boolean) => void;
}

export const LyricsEditor = forwardRef<LyricsEditorHandle, LyricsEditorProps>(function LyricsEditor({ role, writeLocked: producerWriteLocked = false, onDirtyChange }, ref) {
  const { show } = useToast();
  const state = useLyricsEditorState(producerWriteLocked);
  const loader = useLyricsDocumentLoader(state);
  const activeTarget = useLyricsActiveTarget(state);
  const commands = useLyricsDocumentCommands(state, activeTarget);
  const source = useLyricsSourceWorkflow(state, loader, commands, role);
  const persistence = useLyricsPersistence(state, loader);
  const {
    lyrics, setLyrics, setBaseline, busy, error, setError, dirty, writeLocked, query,
    documentGenerationRef, lyricsRef, baselineRef, selectedMusicIDRef,
    activeTranslationEditionKey, setActiveTranslationEditionKey, activeTranslationEditionKeyRef,
    activeRenditionKey, setActiveRenditionKey, activeVersion, setActiveVersion,
    pendingTransition, setPendingTransition, pendingAnnotationOperation, setPendingAnnotationOperation,
    sourcePreview, confirmSourceImport, setConfirmSourceImport, confirmImportRecovery, setConfirmImportRecovery,
    confirmConflictReload, setConfirmConflictReload, editionWorkflow, setEditionWorkflow,
  } = state;
  const {
    renditionKeys, renditionDocument, legacyLyrics, activeRendition, activeTranslationEdition,
    availableVersions, projectionKind,
  } = activeTarget;
  const { loadCatalog, startCollaboration } = loader;
  const { confirmAnnotationOperation } = commands;
  const { acceptPreview } = source;
  const {
    save, discard, reloadAuthoritative, loadConflictAuthoritative, performEditionWorkflow, continuePendingTransition,
  } = persistence;
  const { publicationProblems, publicationChecks, publicationComplete } =
    lyricsPublicationReadiness(lyrics, renditionDocument, legacyLyrics, dirty);

  useEffect(() => onDirtyChange?.(dirty), [dirty, onDirtyChange]);
  useEffect(() => {
    if (!renditionDocument) {
      if (activeTranslationEditionKey) setActiveTranslationEditionKey("");
      return;
    }
    const retained = selectTranslationEditionKey(
      activeTranslationEditionKey || renditionDocument.translationEditionKey,
      renditionDocument.defaultTranslationEditionKey,
      renditionDocument.translationEditions,
    );
    if (retained !== activeTranslationEditionKey) setActiveTranslationEditionKey(retained);
  }, [activeTranslationEditionKey, renditionDocument, setActiveTranslationEditionKey]);
  useEffect(() => {
    if (!lyrics || isLegacyLyricsDocument(lyrics)) {
      if (activeRenditionKey) setActiveRenditionKey("");
      return;
    }
    if (!renditionKeys.includes(activeRenditionKey)) {
      setActiveRenditionKey(renditionKeys[0] || "");
    }
  }, [activeRenditionKey, lyrics, renditionKeys, setActiveRenditionKey]);
  useEffect(() => {
    if (availableVersions.includes(activeVersion)) return;
    setActiveVersion(availableVersions.includes("full") ? "full" : "game");
  }, [activeVersion, availableVersions, setActiveVersion]);

  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  useImperativeHandle(ref, () => ({
    save,
    discard,
    isDirty: () => lyricsRef.current != null && canonicalLyricsJSON(lyricsRef.current) !== baselineRef.current,
    snapshot: () => ({
      dirty: lyricsRef.current != null && canonicalLyricsJSON(lyricsRef.current) !== baselineRef.current,
      document: lyricsRef.current ? JSON.parse(JSON.stringify(lyricsRef.current)) as SongLyricsDocument : null,
      generation: documentGenerationRef.current,
      editionKey: activeTranslationEditionKeyRef.current,
    }),
    isEditing: (musicID: number) => selectedMusicIDRef.current === musicID,
    activeTarget: () => selectedMusicIDRef.current == null ? null : {
      musicId: selectedMusicIDRef.current,
      editionKey: activeTranslationEditionKeyRef.current,
      renditionKey: activeRendition?.key || "",
      side: activeVersion,
      locale: "zh-CN",
      projectionKind,
    },
    reloadCatalog: () => { void loadCatalog(query); },
    exportDraft: () => lyrics ? JSON.parse(JSON.stringify(lyrics)) as SongLyricsDocument : null,
    reloadAuthoritative,
  }));

  return (
    <div className="lyrics-workspace">
      <LyricsDocumentView
        state={state}
        activeTarget={activeTarget}
        loader={loader}
        commands={commands}
        source={source}
        persistence={persistence}
        role={role}
        publicationChecks={publicationChecks}
        publicationComplete={publicationComplete}
      />
      <Modal open={pendingAnnotationOperation != null} onClose={() => setPendingAnnotationOperation(null)} title="确认移除受影响的 ruby 注音" maxWidth={500}>
        <p className="dirty-guard-copy">这次拆分或文字修改落在已有注音范围内，无法在不猜测读音对应关系的情况下自动保留该注音。</p>
        <p className="dirty-guard-copy">确认后只会移除直接受影响的 ruby reading；其他分段和未受影响的注音会原样保留。系统不会把一个完整读音复制到拆分后的两边。</p>
        <div className="dirty-guard-actions">
          <button type="button" className="btn btn-secondary" onClick={confirmAnnotationOperation} disabled={writeLocked}>确认移除受影响注音并继续</button>
          <button type="button" className="btn btn-ghost" onClick={() => setPendingAnnotationOperation(null)}>取消</button>
        </div>
      </Modal>
      <Modal open={confirmSourceImport && sourcePreview != null && lyrics?.revision === 0} onClose={() => setConfirmSourceImport(false)} title="确认替换歌词草稿" maxWidth={500}>
        <p className="dirty-guard-copy">将载入固定修订 {sourcePreview?.revisionId} 的 {sourcePreview?.lines.length} 行日文歌词，并替换当前全部歌词行。已有中英翻译和分段会被替换；来源演唱者证据只会在可安全映射为当前数字角色 ID 时保留，否则会阻止载入并显示错误。</p>
        <p className="dirty-guard-copy">此操作只更新 revision 0 本地草稿。请再次核对；网络或服务器瞬时失败会保留一次性授权和 verified draft，可直接重试保存。仅在授权过期、身份/来源或内容生产者变化等终态时需要重新预览。首次保存成功后，来源资料、行顺序与编号、日文原文才会永久锁定。</p>
        <div className="dirty-guard-actions">
          <button className="btn btn-primary" onClick={acceptPreview} disabled={writeLocked}>确认载入草稿</button>
          <button className="btn btn-ghost" onClick={() => setConfirmSourceImport(false)}>返回核对</button>
        </div>
      </Modal>
      <Modal open={confirmImportRecovery != null} onClose={() => setConfirmImportRecovery(null)} title="确认首次保存结果" maxWidth={520}>
        <p className="dirty-guard-copy">保存请求没有返回可验证结果，但服务器现在已有 revision {confirmImportRecovery?.revision || 0}，且固定来源 page/revision/SHA1、行 ID/顺序和日文原文与本次导入完全一致。这通常表示首次保存已经提交，只是响应在传输中丢失。</p>
        <p className="dirty-guard-copy">载入服务器版本会清除已消费的一次性授权，并以服务器内容作为后续编辑基线；不会要求重新预览。若本地中英翻译或演唱者与服务器版本不同，请先复制需要保留的内容再载入。</p>
        <div className="dirty-guard-actions">
          <button className="btn btn-primary" onClick={() => {
            if (!confirmImportRecovery) return;
            documentGenerationRef.current++;
            setLyrics(confirmImportRecovery);
            setBaseline(canonicalLyricsJSON(confirmImportRecovery));
            setConfirmImportRecovery(null);
            setPendingTransition(null);
            setError(null);
            void loadCatalog(query);
            startCollaboration(confirmImportRecovery.musicId);
            show("已载入服务器首次保存结果，可继续编辑", "ok");
          }}>载入服务器版本</button>
          <button className="btn btn-ghost" onClick={() => setConfirmImportRecovery(null)}>先保留本地内容</button>
        </div>
      </Modal>
      <Modal open={confirmConflictReload && error?.code === "revision_conflict" && error.current != null} onClose={() => setConfirmConflictReload(false)} title="载入服务器版本" maxWidth={500} closeDisabled={busy}>
        <p className="dirty-guard-copy">载入服务器版本会覆盖当前未保存草稿。建议先使用浏览器的保存页面或复制文本方式保留需要手动合并的内容。</p>
        <p className="dirty-guard-copy">如果全歌曲 revision 是由其他译本推进，系统会重新读取你当前译本的最新权威文档，并尽量保留当前 rendition 与 Full/Game side。</p>
        <div className="dirty-guard-actions">
          <button className="btn btn-secondary" onClick={() => void loadConflictAuthoritative()} disabled={busy}>确认载入服务器版本</button>
          <button className="btn btn-ghost" onClick={() => setConfirmConflictReload(false)} disabled={busy}>取消</button>
        </div>
      </Modal>
      <Modal
        open={editionWorkflow != null}
        onClose={() => setEditionWorkflow(null)}
        title={editionWorkflow?.command === "create" ? "新建空白译本"
          : editionWorkflow?.command === "clone" ? "克隆当前译本"
            : editionWorkflow?.command === "rename" ? "重命名当前译本"
              : "设为默认译本"}
        maxWidth={520}
        closeDisabled={busy}
      >
        {editionWorkflow && <div className="lyrics-edition-workflow" aria-busy={busy}>
          {editionWorkflow.command === "create" && <p className="dirty-guard-copy">将创建不含简中译文的新译本。稳定 source rendition、Full/Game 关系、分段、ruby 与英文仍由服务器按 source-v3 合同物化。</p>}
          {editionWorkflow.command === "clone" && <p className="dirty-guard-copy">只克隆当前译本在服务器上已保存的内容，不会读取或复制任何未保存的浏览器草稿。</p>}
          {editionWorkflow.command === "rename" && <p className="dirty-guard-copy">重命名只修改显示名称；稳定译本 key <code>{editionWorkflow.editionKey}</code> 保持不变。</p>}
          {editionWorkflow.command === "set-default" && <p className="dirty-guard-copy">将 <strong>{activeTranslationEdition?.label || activeTranslationEditionKey}</strong> 设为缺省打开的译本。当前译本 key 和译文内容不会改变。</p>}
          {error && (error.code.startsWith("translation_edition_") || error.code === "invalid_translation_edition" || error.code === "revision_conflict") && <div className="lyrics-error" role="alert"><strong>{sourceLabel(error)}</strong>{error.details.map((detail) => <span key={detail}>{detail}</span>)}{error.code === "revision_conflict" && error.current && <button type="button" className="btn btn-secondary btn-sm" onClick={() => { setEditionWorkflow(null); setConfirmConflictReload(true); }}>载入服务器版本</button>}</div>}
          {editionWorkflow.command !== "set-default" && <div className="lyrics-edition-workflow-fields">
            <label>译本名称<input autoFocus value={editionWorkflow.label} maxLength={256} onChange={(event) => { setEditionWorkflow((current) => current ? { ...current, label: event.target.value } : current); setError(null); }} /></label>
            <label>稳定 key<input value={editionWorkflow.editionKey} readOnly /></label>
          </div>}
          <div className="dirty-guard-actions">
            <button type="button" className="btn btn-primary" onClick={() => void performEditionWorkflow()} disabled={busy || writeLocked || (editionWorkflow.command !== "set-default" && !isTranslationEditionLabel(editionWorkflow.label)) || (editionWorkflow.command === "rename" && editionWorkflow.label === activeTranslationEdition?.label)}>
              {editionWorkflow.command === "create" ? "创建空白译本" : editionWorkflow.command === "clone" ? "克隆服务器译本" : editionWorkflow.command === "rename" ? "保存新名称" : "设为默认译本"}
            </button>
            <button type="button" className="btn btn-ghost" onClick={() => setEditionWorkflow(null)} disabled={busy}>取消</button>
          </div>
        </div>}
      </Modal>
      <Modal open={pendingTransition != null} onClose={() => setPendingTransition(null)} title={pendingTransition?.kind === "publish" ? (pendingTransition.nextPublished ? "确认发布歌词" : "确认取消发布") : "处理未保存歌词"} maxWidth={500} closeDisabled={busy}>
        <div aria-busy={busy}>
          {busy && <p className="dirty-guard-copy" role="status" aria-live="polite">正在保存或提交歌词，请等待服务器确认…</p>}
        {pendingTransition?.kind === "publish" ? (
          <>
            <p className="dirty-guard-copy">{pendingTransition.nextPublished ? `将把当前 revision ${lyrics?.revision || 0} 发布到公共歌词文件。` : `将从公共歌词文件撤下 revision ${lyrics?.publishedRevision || lyrics?.revision || 0}。`}</p>
            {dirty && <p className="dirty-guard-copy">当前还有未保存修改。必须先保存成功，才能发布刚刚核对的内容。</p>}
            {pendingTransition.nextPublished && publicationProblems.length > 0 && (
              <div className="lyrics-publication-check" role="alert"><strong>发布前还需补齐：</strong><ul>{publicationProblems.slice(0, 8).map((problem) => <li key={problem}>{problem}</li>)}</ul>{publicationProblems.length > 8 && <span>另有 {publicationProblems.length - 8} 项未列出</span>}</div>
            )}
            <div className="dirty-guard-actions">
              {dirty ? <button className="btn btn-primary" onClick={() => void continuePendingTransition(true)} disabled={busy || writeLocked}>保存并继续</button> : <button className="btn btn-primary" onClick={() => void continuePendingTransition(false)} disabled={busy || writeLocked || (pendingTransition.nextPublished && publicationProblems.length > 0)}>{pendingTransition.nextPublished ? "确认发布" : "确认取消发布"}</button>}
              <button className="btn btn-ghost" onClick={() => setPendingTransition(null)} disabled={busy}>取消</button>
            </div>
          </>
        ) : (
          <>
            <p className="dirty-guard-copy">当前歌词有未保存修改。继续前请选择如何处理。</p>
            {pendingTransition?.kind === "edition-switch" && <p className="dirty-guard-copy">切换译本会整篇载入服务器权威文档；当前译本的本地草稿不会带到目标译本。</p>}
            {pendingTransition?.kind === "edition-command" && pendingTransition.command === "clone" && <p className="dirty-guard-copy"><strong>如果选择“放弃并继续”，克隆只会复制服务器上已保存的当前译本，明确不会复制这份未保存草稿。</strong></p>}
            {pendingTransition?.kind === "edition-command" && pendingTransition.command !== "clone" && <p className="dirty-guard-copy">译本元数据操作使用全歌曲 revision/CAS；继续前必须先同步处理当前译本草稿。</p>}
            <div className="dirty-guard-actions">
              <button className="btn btn-primary" onClick={() => void continuePendingTransition(true)} disabled={busy || writeLocked}>保存并继续</button>
              <button className="btn btn-secondary" onClick={() => void continuePendingTransition(false)} disabled={busy || ((pendingTransition?.kind === "edition-switch" || pendingTransition?.kind === "edition-command") && writeLocked)}>放弃并继续</button>
              <button className="btn btn-ghost" onClick={() => setPendingTransition(null)} disabled={busy}>取消</button>
            </div>
          </>
        )}
        </div>
      </Modal>
    </div>
  );
});
