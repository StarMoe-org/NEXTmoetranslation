"use client";

import { useToast } from "@/app/providers";
import type { LyricsEditionCommand } from "@/components/LyricsEditionMenu";
import {
  editableLyricsDocument, isLegacyLyricsDocument, preserveReadOnlyLyricsSourceFacts, sourceImportFailureIsTerminal,
  sourceV3SaveWording,
} from "@/components/lyrics/lyricsDocumentModel";
import type { LyricsEditorState } from "@/components/lyrics/lyricsEditorState";
import type { LyricsDocumentLoader } from "@/components/lyrics/useLyricsDocumentLoader";
import { sameImportedLyricsFrozenIdentity } from "@/lib/lyrics-recovery.mjs";
import { isTranslationEditionLabel, selectTranslationEditionKey } from "@/lib/lyrics-editions.mjs";
import { isRenditionLyricsDocument, lyricsVersionSaveProblems } from "@/lib/lyrics-versioning.mjs";
import {
  APIError, RenditionLyricsDocument, SongLyricsDocument,
  checkpointLyrics, getLyrics, getProjectionStatus, mutateLyricsTranslationEdition,
  publishLyrics, saveLyrics, unpublishLyrics,
} from "@/lib/api";
import { canonicalLyricsJSON, lyricsAuthorityEnvelopeKey, lyricsDocumentSaveable } from "@/lib/yjs-lyrics";

/** Owns draft checkpoints, translation-edition mutations, publication and the public-file projection watch. */
export function useLyricsPersistence(state: LyricsEditorState, loader: LyricsDocumentLoader) {
  const { show } = useToast();
  const {
    lyrics, setLyrics, baseline, setBaseline, lyricsRef, baselineRef, dirty, sourceV3PublicState,
    busyRef, setBusy, writeLocked, writeLockedRef, error, setError,
    query, selectedMusic, selectedMusicIDRef, lyricsLoadSequence, documentGenerationRef,
    requestSequence, performerSequence,
    sourceImportTokenRef, sourcePreviewCandidate,
    setSourcePreview, setSourcePreviewCandidate, setSourceRetry, setConfirmSourceImport,
    setConfirmImportRecovery, setConfirmConflictReload, setCandidates, setSourceSearchCompleted,
    pendingTransition, setPendingTransition, setPendingAnnotationOperation,
    editionWorkflow, setEditionWorkflow,
    setActiveTranslationEditionKey, activeTranslationEditionKeyRef, activeRenditionKey, activeVersion,
    collaborationRef, collaborationAuthoritativeRef, collaborationBaselineEnvelopeRef,
    projectionSequence, setProjectionStatus, setProjectionState, setProjectionMessage,
  } = state;
  const {
    loadCatalog, loadPerformers, performChooseMusic, acceptAuthoritativeDocument, startCollaboration,
    requestIsCurrent, replaceEditionURL,
  } = loader;

  const loadTranslationEdition = async (requestedEditionKey: string): Promise<boolean> => {
    const current = lyricsRef.current;
    if (!current || !isRenditionLyricsDocument(current) || busyRef.current || writeLockedRef.current) return false;
    const currentEditionDocument = current as RenditionLyricsDocument;
    const editionKey = selectTranslationEditionKey(
      requestedEditionKey,
      currentEditionDocument.defaultTranslationEditionKey,
      currentEditionDocument.translationEditions,
    );
    if (!editionKey) return false;
    if (editionKey === activeTranslationEditionKeyRef.current && currentEditionDocument.translationEditionKey === editionKey) return true;
    const sequence = lyricsLoadSequence.current;
    const musicID = current.musicId;
    const documentGeneration = documentGenerationRef.current;
    const preferredRenditionKey = activeRenditionKey;
    const preferredVersion = activeVersion;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    try {
      const loaded = await getLyrics(musicID, editionKey);
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return false;
      if (!isRenditionLyricsDocument(loaded)) {
        setError(new APIError(502, { error: "invalid_translation_edition", details: ["译本切换响应必须是 source-v3 文档"] }));
        return false;
      }
      const loadedEditionDocument = loaded as RenditionLyricsDocument;
      acceptAuthoritativeDocument(loadedEditionDocument, preferredRenditionKey, preferredVersion);
      sourceImportTokenRef.current = "";
      setPendingTransition(null);
      show(`已切换到译本 ${loadedEditionDocument.translationEditionKey}`, "ok");
      return true;
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return false;
      setError(reason instanceof APIError ? reason : new APIError(500, { error: "load_failed" }));
      return false;
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const openEditionWorkflow = (command: LyricsEditionCommand): boolean => {
    const current = lyricsRef.current;
    if (!current || !isRenditionLyricsDocument(current) || busyRef.current || writeLockedRef.current) return false;
    const currentEditionDocument = current as RenditionLyricsDocument;
    const active = currentEditionDocument.translationEditions.find((edition) => edition.key === activeTranslationEditionKeyRef.current)
      || currentEditionDocument.translationEditions[0];
    if (!active) return false;
    if (command === "set-default" && active.key === currentEditionDocument.defaultTranslationEditionKey) return false;
    const editionKey = command === "create" || command === "clone"
      ? `ed-${crypto.randomUUID()}`
      : active.key;
    const label = command === "create" ? "新译本" : command === "clone" ? `${active.label} 副本` : active.label;
    setError(null);
    setEditionWorkflow({ command, editionKey, label });
    return true;
  };

  const requestEditionSwitch = (editionKey: string) => {
    if (busyRef.current || writeLockedRef.current || editionKey === activeTranslationEditionKeyRef.current) return;
    if (dirty) {
      setPendingTransition({ kind: "edition-switch", editionKey });
      return;
    }
    void loadTranslationEdition(editionKey);
  };

  const requestEditionCommand = (command: LyricsEditionCommand) => {
    if (busyRef.current || writeLockedRef.current) return;
    if (dirty) {
      setPendingTransition({ kind: "edition-command", command });
      return;
    }
    openEditionWorkflow(command);
  };

  const saveDocument = async (): Promise<SongLyricsDocument | null> => {
    if (!lyrics || busyRef.current || writeLockedRef.current) return null;
    const preflightProblems = lyricsVersionSaveProblems(lyrics);
    if (preflightProblems.length > 0) {
      setError(new APIError(422, { error: "invalid_game_projection", details: preflightProblems }));
      show("Rendition / projection 或公开署名合同无效，未发送保存请求", "err");
      return null;
    }
    const attempted = lyrics;
    const sequence = lyricsLoadSequence.current;
    const musicID = lyrics.musicId;
    const documentGeneration = documentGenerationRef.current;
    const importToken = lyrics.revision === 0 ? sourceImportTokenRef.current : "";
    const collaboration = collaborationRef.current;
    const checkpointBoundary = importToken ? null : collaboration?.beginCheckpoint() ?? null;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    setConfirmImportRecovery(null);
    try {
      const saved = importToken
        ? await saveLyrics(lyrics, importToken)
        : await checkpointLyrics(musicID);
      if (!requestIsCurrent(sequence, musicID)) return null;
      if (importToken && documentGenerationRef.current !== documentGeneration) return null;
      const persisted = preserveReadOnlyLyricsSourceFacts(saved, attempted);
      collaborationAuthoritativeRef.current = editableLyricsDocument(persisted);
      if (importToken) {
        documentGenerationRef.current++;
        setLyrics(persisted);
      } else {
        collaboration?.checkpointCommitted(checkpointBoundary);
        collaboration?.updateAuthoritativeEnvelope(persisted);
      }
      setBaseline(canonicalLyricsJSON(persisted));
      collaborationBaselineEnvelopeRef.current = lyricsAuthorityEnvelopeKey(persisted);
      if (isRenditionLyricsDocument(persisted)) {
        const persistedEditionDocument = persisted as RenditionLyricsDocument;
        setActiveTranslationEditionKey(persistedEditionDocument.translationEditionKey);
        replaceEditionURL(persistedEditionDocument.translationEditionKey);
      }
      sourceImportTokenRef.current = "";
      setSourcePreview(null);
      setSourcePreviewCandidate(null);
      setSourceRetry(null);
      setConfirmSourceImport(false);
      setConfirmImportRecovery(null);
      setCandidates([]);
      setSourceSearchCompleted(false);
      void loadCatalog(query);
      if (importToken) startCollaboration(musicID);
      show(sourceV3SaveWording(sourceV3PublicState)?.saved ?? "歌词草稿已保存", "ok");
      return persisted;
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID)) return null;
      if (importToken && documentGenerationRef.current !== documentGeneration) return null;
      const apiError = reason instanceof APIError ? reason : new APIError(500, { error: "save_failed" });
      if (importToken && apiError.code !== "invalid_lyrics_response") {
        try {
          const authoritative = await getLyrics(musicID);
          if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return null;
          if (isLegacyLyricsDocument(attempted) && isLegacyLyricsDocument(authoritative) &&
              sameImportedLyricsFrozenIdentity(attempted, authoritative)) {
            sourceImportTokenRef.current = "";
            setSourcePreview(null);
            setSourcePreviewCandidate(null);
            setSourceRetry(null);
            setConfirmSourceImport(false);
            setConfirmImportRecovery(authoritative);
            setError(null);
            show("首次保存可能已成功；已找到相同固定来源的服务器版本，请确认载入", "err");
            return null;
          }
        } catch {
          // Reconciliation is best-effort. Preserve the original save failure and
          // its retry semantics when the authoritative read is unavailable.
        }
      }
      const terminalImportFailure = Boolean(importToken) && sourceImportFailureIsTerminal(apiError);
      if (terminalImportFailure) {
        sourceImportTokenRef.current = "";
        setSourcePreview(null);
        setSourceRetry(sourcePreviewCandidate ? { kind: "preview", candidate: sourcePreviewCandidate } : { kind: "search" });
      }
      setError(apiError);
      return null;
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const save = async () => (await saveDocument()) != null;

  const performEditionWorkflow = async () => {
    const workflow = editionWorkflow;
    const current = lyricsRef.current;
    if (!workflow || !current || !isRenditionLyricsDocument(current) || busyRef.current || writeLockedRef.current) return;
    const currentEditionDocument = current as RenditionLyricsDocument;
    if (currentEditionDocument.translationEditionKey !== activeTranslationEditionKeyRef.current) {
      setError(new APIError(409, { error: "translation_edition_conflict", details: ["当前译本身份与已载入文档不一致，请重新载入"] }));
      return;
    }
    if (workflow.command !== "set-default" && !isTranslationEditionLabel(workflow.label)) {
      setError(new APIError(422, { error: "invalid_translation_edition", details: ["译本名称必须是 1-256 字节的 UTF-8 文本，且不能带首尾空白"] }));
      return;
    }
    const sequence = lyricsLoadSequence.current;
    const musicID = current.musicId;
    const documentGeneration = documentGenerationRef.current;
    const preferredRenditionKey = activeRenditionKey;
    const preferredVersion = activeVersion;
    const common = { musicId: current.musicId, revision: current.revision };
    const mutation = workflow.command === "create"
      ? { ...common, operation: "create" as const, editionKey: workflow.editionKey, label: workflow.label }
      : workflow.command === "clone"
        ? { ...common, operation: "clone" as const, sourceEditionKey: currentEditionDocument.translationEditionKey, editionKey: workflow.editionKey, label: workflow.label }
        : workflow.command === "rename"
          ? { ...common, operation: "rename" as const, editionKey: currentEditionDocument.translationEditionKey, label: workflow.label }
          : { ...common, operation: "set-default" as const, editionKey: currentEditionDocument.translationEditionKey };
    busyRef.current = true;
    setBusy(true);
    setError(null);
    try {
      const materialized = await mutateLyricsTranslationEdition(mutation);
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      if (!isRenditionLyricsDocument(materialized)) {
        setError(new APIError(502, { error: "invalid_translation_edition", details: ["译本操作响应必须是 source-v3 文档"] }));
        return;
      }
      acceptAuthoritativeDocument(materialized, preferredRenditionKey, preferredVersion);
      setEditionWorkflow(null);
      setPendingTransition(null);
      void loadCatalog(query);
      const message = workflow.command === "create" ? "空白译本已创建"
        : workflow.command === "clone" ? "当前服务器译本已克隆"
          : workflow.command === "rename" ? "译本已重命名"
            : "默认译本已更新";
      show(message, "ok");
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      const apiError = reason instanceof APIError ? reason : new APIError(500, { error: "save_failed" });
      if (apiError.code === "translation_edition_exists" && (workflow.command === "create" || workflow.command === "clone")) {
        setEditionWorkflow((currentWorkflow) => currentWorkflow ? { ...currentWorkflow, editionKey: `ed-${crypto.randomUUID()}` } : currentWorkflow);
      }
      setError(apiError);
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const discard = (): boolean => {
    if (busyRef.current) return false;
    if (!sourceImportTokenRef.current && collaborationRef.current) {
      collaborationRef.current.discardLocalChanges();
      setError(null);
      setPendingAnnotationOperation(null);
      return true;
    }
    const authoritative = baselineRef.current;
    if (!authoritative) return false;
    const restored = JSON.parse(authoritative) as SongLyricsDocument;
    documentGenerationRef.current++;
    setLyrics(restored);
    if (isRenditionLyricsDocument(restored)) setActiveTranslationEditionKey((restored as RenditionLyricsDocument).translationEditionKey);
    sourceImportTokenRef.current = "";
    setSourcePreview(null);
    setSourcePreviewCandidate(null);
    setSourceRetry(null);
    setConfirmSourceImport(false);
    setConfirmImportRecovery(null);
    setPendingAnnotationOperation(null);
    setCandidates([]);
    setSourceSearchCompleted(false);
    setError(null);
    if (selectedMusicIDRef.current === restored.musicId) startCollaboration(restored.musicId);
    return true;
  };

  const reloadAuthoritative = async (): Promise<boolean> => {
    if (busyRef.current) return false;
    setPendingTransition(null);
    setEditionWorkflow(null);
    sourceImportTokenRef.current = "";
    setSourcePreview(null);
    setSourcePreviewCandidate(null);
    setSourceRetry(null);
    setConfirmSourceImport(false);
    setConfirmImportRecovery(null);
    setPendingAnnotationOperation(null);
    requestSequence.current++;
    performerSequence.current++;
    await Promise.all([loadCatalog(query), loadPerformers()]);
    if (!selectedMusic) return true;
    return performChooseMusic(selectedMusic);
  };

  const loadConflictAuthoritative = async () => {
    const conflictCurrent = error?.current;
    if (!conflictCurrent || busyRef.current) return;
    const sequence = lyricsLoadSequence.current;
    const musicID = conflictCurrent.musicId;
    const documentGeneration = documentGenerationRef.current;
    const preferredEditionKey = activeTranslationEditionKeyRef.current;
    busyRef.current = true;
    setBusy(true);
    try {
      let authoritative = conflictCurrent;
      if (preferredEditionKey && isRenditionLyricsDocument(conflictCurrent) &&
          (conflictCurrent as RenditionLyricsDocument).translationEditionKey !== preferredEditionKey) {
        authoritative = await getLyrics(musicID, preferredEditionKey);
      }
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      acceptAuthoritativeDocument(authoritative, activeRenditionKey, activeVersion);
      sourceImportTokenRef.current = "";
      setSourcePreview(null);
      setSourcePreviewCandidate(null);
      setSourceRetry(null);
      setConfirmSourceImport(false);
      setCandidates([]);
      setSourceSearchCompleted(false);
      setEditionWorkflow(null);
      setError(null);
      setConfirmConflictReload(false);
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return;
      show(reason instanceof Error ? reason.message : "服务器版本载入失败", "err");
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const readableProjectionTime = (value?: string) => {
    if (!value) return "";
    const timestamp = Date.parse(value);
    return Number.isFinite(timestamp) ? new Date(timestamp).toLocaleString() : "";
  };

  const refreshProjectionStatus = async () => {
    const sequence = ++projectionSequence.current;
    const musicID = selectedMusicIDRef.current;
    setProjectionState("checking");
    setProjectionMessage("正在核对公共文件 generation…");
    try {
      const status = await getProjectionStatus(musicID ?? undefined);
      if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
      setProjectionStatus(status);
      if (status.lastError) {
        setProjectionState("failed");
        setProjectionMessage(`公共文件生成失败：${status.lastError}`);
      } else if (status.pending) {
        setProjectionState("checking");
        setProjectionMessage(`公共文件正在生成（generation ${status.generation}）。`);
      } else {
        const completedAt = readableProjectionTime(status.lastSuccessAt);
        setProjectionState("ready");
        setProjectionMessage(completedAt
          ? `公共文件 generation ${status.generation} 最近成功生成于 ${completedAt}。`
          : `公共文件 generation ${status.generation} 已完成。`);
      }
    } catch {
      if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
      setProjectionState("unknown");
      setProjectionMessage("公共文件状态暂时不可用，请稍后重新核对。");
    }
  };

  const waitForProjection = async (previousGeneration: number | null, nextPublished: boolean, musicID: number) => {
    const sequence = ++projectionSequence.current;
    setProjectionState("checking");
    setProjectionMessage(nextPublished
      ? "数据库发布已提交，正在等待公共文件生成…"
      : "数据库撤回已提交，正在等待公共文件更新…");
    const deadline = Date.now() + 15_000;
    while (projectionSequence.current === sequence && selectedMusicIDRef.current === musicID && Date.now() < deadline) {
      try {
        const status = await getProjectionStatus(musicID);
        if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
        setProjectionStatus(status);
        if (status.lastError) {
          setProjectionState("failed");
          setProjectionMessage(`数据库操作已完成，但公共文件生成失败：${status.lastError}`);
          show("数据库状态已更新，但公共文件生成失败", "err");
          return;
        }
        if (previousGeneration == null) {
          setProjectionState("unknown");
          setProjectionMessage(status.pending
            ? `数据库操作已完成，公共文件仍在生成（generation ${status.generation}）；因提交前状态不可用，无法将本次变更绑定到该 generation。`
            : `数据库操作已完成，公共文件 generation ${status.generation} 当前无待处理任务；因提交前状态不可用，无法确认本次变更对应的 generation。`);
          show("数据库状态已更新，但无法确认本次公共文件 generation", "err");
          return;
        }
        if (!status.pending && !status.lastError && status.generation > previousGeneration) {
          setProjectionState("ready");
          setProjectionMessage(nextPublished
            ? `公共文件已完成新一代生成；歌曲 ${musicID} 的数据库发布状态已进入 generation ${status.generation}。`
            : `公共文件已完成新一代生成；歌曲 ${musicID} 的数据库撤回状态已进入 generation ${status.generation}。`);
          show(nextPublished ? "数据库发布与公共文件生成均已完成" : "数据库撤回与公共文件更新均已完成", "ok");
          // The catalog's public-mirror line follows the served projection, so
          // refresh it once the new generation is live.
          void loadCatalog(query);
          return;
        }
        setProjectionMessage(`数据库操作已完成，正在等待公共文件 generation 超过 ${previousGeneration}（当前 ${status.generation}）。`);
      } catch {
        if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
        setProjectionState("unknown");
        setProjectionMessage("数据库操作已完成，但公共文件状态核对失败；请稍后重新核对。");
        show("数据库状态已更新，但公共文件状态未知", "err");
        return;
      }
      await new Promise((resolve) => window.setTimeout(resolve, 300));
    }
    if (projectionSequence.current !== sequence || selectedMusicIDRef.current !== musicID) return;
    setProjectionState("unknown");
    setProjectionMessage("数据库操作已完成，但公共文件长时间仍未确认完成；请稍后重新核对。");
    show("数据库状态已更新，公共文件仍在生成或状态未知", "err");
  };

  // Newest content a publication would act on: the shared document while a collaboration owns it.
  const currentDocument = (): SongLyricsDocument | null => {
    const shared = collaborationRef.current?.getSnapshot().document;
    return shared ? editableLyricsDocument(shared) : lyricsRef.current;
  };

  const performPublication = async (nextPublished: boolean, document: SongLyricsDocument, stored: string): Promise<boolean> => {
    if (busyRef.current || writeLockedRef.current) return false;
    const sequence = lyricsLoadSequence.current;
    const musicID = document.musicId;
    const documentGeneration = documentGenerationRef.current;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    let previousProjectionGeneration: number | null = null;
    try {
      try {
        const status = await getProjectionStatus(musicID);
        if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return false;
        setProjectionStatus(status);
        previousProjectionGeneration = status.generation;
      } catch {
        if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return false;
        setProjectionState("unknown");
        setProjectionMessage("提交前无法读取公共文件 generation；数据库操作仍可继续，但提交后只能报告公共文件状态未知。");
      }
      // Publishing and unpublishing reseed the collaboration room from the stored revision.
      if (lyricsDocumentSaveable(currentDocument(), stored)) {
        show("提交前又出现了未保存修改，发布状态没有改变；请先保存", "err");
        return false;
      }
      const response = nextPublished
        ? await publishLyrics(document.musicId, document.revision)
        : await unpublishLyrics(document.musicId, document.revision);
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return false;
      const result = preserveReadOnlyLyricsSourceFacts(response, document);
      documentGenerationRef.current++;
      collaborationRef.current?.updateAuthoritativeEnvelope(result);
      setBaseline(canonicalLyricsJSON(result));
      collaborationBaselineEnvelopeRef.current = lyricsAuthorityEnvelopeKey(result);
      void loadCatalog(query);
      show(nextPublished ? "数据库发布已提交，正在核对公共文件" : "数据库撤回已提交，正在核对公共文件", "ok");
      void waitForProjection(previousProjectionGeneration, nextPublished, musicID);
      return true;
    } catch (reason) {
      if (!requestIsCurrent(sequence, musicID) || documentGenerationRef.current !== documentGeneration) return false;
      const apiError = reason instanceof APIError ? reason : new APIError(500, { error: "publication_failed" });
      setError(apiError);
      return false;
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const publish = (nextPublished: boolean) => {
    if (!lyrics || !isLegacyLyricsDocument(lyrics) || writeLocked) return;
    const preflightProblems = lyricsVersionSaveProblems(lyrics);
    if (preflightProblems.length > 0) {
      setError(new APIError(422, { error: "invalid_game_projection", details: preflightProblems }));
      show("Game 投影需要先修复，未打开发布操作", "err");
      return;
    }
    setError(null);
    setPendingTransition({ kind: "publish", nextPublished });
  };

  const continuePendingTransition = async (saveFirst: boolean) => {
    const pending = pendingTransition;
    if (!pending) return;
    const editionTransition = pending.kind === "edition-switch" || pending.kind === "edition-command";
    if ((saveFirst || pending.kind === "publish" || editionTransition) && writeLockedRef.current) return;
    let document = lyrics;
    let stored = baselineRef.current;
    // A publication discards unsaved shared edits, so it saves them first whatever was clicked.
    if (saveFirst || (pending.kind === "publish" && lyricsDocumentSaveable(currentDocument(), stored))) {
      document = await saveDocument();
      if (!document) return;
      stored = canonicalLyricsJSON(document);
      const currentSharedDocument = collaborationRef.current?.getSnapshot().document;
      if (collaborationRef.current?.undoManager.canUndo() ||
          (currentSharedDocument && canonicalLyricsJSON(editableLyricsDocument(currentSharedDocument)) !== canonicalLyricsJSON(document))) {
        show("保存期间又产生了新修改，请再次保存后再继续", "err");
        return;
      }
    } else if (pending.kind !== "publish" && baseline) {
      lyricsLoadSequence.current++;
      documentGenerationRef.current++;
      if (sourceImportTokenRef.current || !collaborationRef.current) {
        document = JSON.parse(baseline) as SongLyricsDocument;
        setLyrics(document);
        if (isRenditionLyricsDocument(document)) setActiveTranslationEditionKey((document as RenditionLyricsDocument).translationEditionKey);
      } else {
        collaborationRef.current?.discardLocalChanges();
        document = collaborationRef.current?.getSnapshot().document ?? (JSON.parse(baseline) as SongLyricsDocument);
      }
      sourceImportTokenRef.current = "";
      setSourcePreview(null);
      setSourcePreviewCandidate(null);
      setSourceRetry(null);
      setConfirmSourceImport(false);
      setConfirmImportRecovery(null);
      setCandidates([]);
      setSourceSearchCompleted(false);
      setError(null);
    }
    if (pending.kind === "choose") {
      setPendingTransition(null);
      await performChooseMusic(pending.item);
    } else if (pending.kind === "publish" && document) {
      if (await performPublication(pending.nextPublished, document, stored)) setPendingTransition(null);
    } else if (pending.kind === "edition-switch") {
      const switched = await loadTranslationEdition(pending.editionKey);
      if (!switched) {
        setPendingTransition(null);
        if (writeLockedRef.current) show("实时校对已锁定，译本未切换", "err");
      }
    } else if (pending.kind === "edition-command") {
      if (openEditionWorkflow(pending.command)) setPendingTransition(null);
    }
  };

  return {
    requestEditionSwitch, requestEditionCommand, save, performEditionWorkflow, discard, reloadAuthoritative,
    loadConflictAuthoritative, refreshProjectionStatus, publish, continuePendingTransition,
  };
}

export type LyricsPersistence = ReturnType<typeof useLyricsPersistence>;
