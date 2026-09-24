import assert from "node:assert/strict";
import test from "node:test";

import { read, readConsoleSurface, readLyricsEditor } from "./source-surfaces.mjs";

test("the full-screen app layout keeps the lyrics workspace stretched with independent scrolling", async () => {
  const css = await read("src/app/globals.css");
  assert.match(css, /\.app \{[\s\S]*position: fixed;[\s\S]*inset: 0;[\s\S]*min-height: 0;[\s\S]*overflow: hidden;/);
  assert.doesNotMatch(css, /\.app \{[^}]*height: 100vh;/);
  assert.match(css, /\.main \{[\s\S]*min-height: 0;[\s\S]*height: 100%;[\s\S]*overflow: hidden;/);
  assert.match(css, /\.lyrics-workspace \{[\s\S]*min-height: 0;[\s\S]*height: 100%;[\s\S]*overflow: hidden;/);
  assert.match(css, /\.lyrics-editor \{[\s\S]*min-height: 0;[\s\S]*height: 100%;[\s\S]*overflow-y: auto;/);
});

test("translation console exposes deterministic sort modes and activity-name filtering", async () => {
  const [consoleSource, toolbar, api, model] = await Promise.all([
    readConsoleSurface(), read("src/components/console/ConsoleToolbar.tsx"),
    read("src/lib/api.ts"), read("../server/internal/model/model.go"),
  ]);
  const combined = `${consoleSource}\n${toolbar}`;
  assert.match(combined, /sortMode.*kana.*id-desc.*time-desc/);
  assert.match(combined, /option value="kana">五十音/);
  assert.match(combined, /option value="id-desc">编号倒序/);
  assert.match(combined, /option value="time-desc">更新时间倒序/);
  assert.match(combined, /按活动名称筛选/);
  assert.match(consoleSource, /story\.eventName|story\.eventNameJapanese/);
  assert.match(consoleSource, /eventStories\.filter\(\(story\) => !story\.allOfficialTagged\)/);
  assert.match(consoleSource, /filteredEventStories\.map\(\(s\) =>/);
  assert.match(consoleSource, /getEventAssociations\(\)/);
  assert.match(consoleSource, /Preserve the last successful client snapshot and retry transient/);
  assert.match(consoleSource, /delay = 30_000/);
  assert.match(consoleSource, /timer = setTimeout\(refresh, delay\)/);
  assert.match(consoleSource, /categoryEventAssociations/);
  assert.match(consoleSource, /relatedEventEntityIDs/);
  assert.match(combined, /aria-label="按活动名称筛选当前分类"/);
  assert.match(consoleSource, /aria-expanded=\{eventStoriesExpanded\}/);
  assert.match(consoleSource, /ui\.eventStoriesExpanded/);
  assert.match(api, /eventName\?: string/);
  assert.match(api, /eventNameJapanese\?: string/);
  assert.match(api, /allOfficialTagged\?: boolean/);
  assert.match(api, /interface EventAssociationIndex[\s\S]*categories: Record<string, Record<string, number\[\]>>/);
  assert.match(api, /getEventAssociations = \(\) => apiFetch<EventAssociationIndex>\("\/event-associations"\)/);
  assert.match(model, /EventName.*eventName/);
  assert.match(model, /AllOfficialTagged.*allOfficialTagged/);
});

test("Chinese and English console requests always carry an explicit locale", async () => {
  const api = await read("src/lib/api.ts");
  assert.match(api, /function addLocale[\s\S]*if \(locale\) params\.set\("locale", locale\)/);
  assert.match(api, /getCategories = \(locale\?: Locale\)/);
  assert.match(api, /updateEntry = async \([\s\S]{0,200}locale\?: Locale/);
  assert.match(api, /\.\.\.\(locale \? \{ locale \} : \{\}\)/);
  assert.doesNotMatch(api, /locale !== "zh-CN"/);
});

test("translation review keeps a fixed editor above an independently scrolling lower list", async () => {
  const [consoleSource, css] = await Promise.all([
    readConsoleSurface(),
    read("src/app/globals.css"),
  ]);
  assert.match(consoleSource, /const translationWorkspaceRef = useRef<HTMLDivElement>\(null\)/);
  assert.match(consoleSource, /const translationEntryListRef = useRef<HTMLDivElement>\(null\)/);
  assert.match(consoleSource, /className="translation-workspace" ref=\{translationWorkspaceRef\}/);
  assert.doesNotMatch(consoleSource, /translation-resizer|startTranslationResize|moveTranslationResize|isTranslationResizing|translationTopHeight/);
  assert.match(consoleSource, /const keepTranslationEntryVisible = useCallback[\s\S]*translationEntryListRef\.current[\s\S]*container\.scrollTo\(\{ top: nextTop/);
  assert.match(consoleSource, /setTimeout\(\(\) => keepTranslationEntryVisible\(next\.key\), 40\)/);
  assert.doesNotMatch(consoleSource, /scrollIntoView/);
  assert.match(css, /\.translation-workspace \{[\s\S]*display: grid;[\s\S]*grid-template-rows: auto minmax\(0, 1fr\);[\s\S]*overflow: hidden;/);
  assert.match(css, /\.translation-editor-pane \{[\s\S]*max-height: 420px;[\s\S]*overflow-y: auto;/);
  assert.match(css, /\.translation-editor-pane:empty \{ display: none; \}/);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*\.translation-editor-pane \{ max-height: none; overflow-y: visible; \}/);
  assert.match(css, /\.translation-entry-list \{[\s\S]*overflow-y: auto;[\s\S]*overscroll-behavior: contain;/);
  assert.doesNotMatch(css, /translation-resizer|row-resize/);
  // A long original (gacha description) sits beside the editor so the save buttons stay in the pane.
  assert.match(consoleSource, /const longSource = sourceLines > 4 \|\| sourceText\.length > 240;/);
  assert.match(consoleSource, /className=\{longSource \? "proof-panel proof-panel-long" : "proof-panel"\}/);
  assert.match(css, /\.proof-panel-long \.proof-jp \.jp-body \{ max-height: 45vh; overflow-y: auto; \}\s*@media \(min-width: 1024px\) \{\s*\.proof-panel-long \{ display: grid; grid-template-columns: minmax\(0, 1fr\) minmax\(0, 1fr\); \}\s*\.proof-panel-long \.proof-jp \{ contain: size; overflow-y: auto;/);
});

test("locale changes expose save discard and cancel dirty choices", async () => {
  const consoleSource = await readConsoleSurface();
  assert.match(consoleSource, /保存并继续/);
  assert.match(consoleSource, /放弃修改/);
  assert.match(consoleSource, />取消</);
  assert.match(consoleSource, /日文（只读）/);
});

test("all destructive console transitions share the dirty guard", async () => {
  const consoleSource = await readConsoleSurface();
  for (const contract of [
    'runOrGuard("切换内容"', 'runOrGuard("切换条目"', 'runOrGuard("关闭当前条目"',
    'runOrGuard("退出登录"', 'runOrGuard("切换编辑语言"', "beforeunload", "lyricsEditorRef.current?.save",
  ]) {
    assert.ok(consoleSource.includes(contract), `missing guarded transition: ${contract}`);
  }
});

test("dirty state survives filtering and event reload tools are guarded", async () => {
  const consoleSource = await readConsoleSurface();
  assert.match(consoleSource, /entries\.find\(\(entry\) => entry\.key === selectedKey\)/);
  for (const label of ["运行 AI 剧情翻译", "重新获取剧情", "重排序对话"]) {
    assert.ok(consoleSource.includes(`guardProducerMutation("${label}"`), `unguarded producer action: ${label}`);
  }
  assert.match(consoleSource, /guardProducerMutation\("整篇标记人工", promoteStory\)/);
  assert.match(consoleSource, /saveEntry\?\.sourceHash/);
  assert.match(consoleSource, /event === "eventstory\.updated" \|\| event === "eventstory\.locale\.updated"/);
  assert.match(consoleSource, /findEventStoryUpdateTarget\(entries, update\)/);
  assert.match(consoleSource, /reconcileContent\("remote", preservedDraft/);
});

test("event story TXT import uses authoritative snapshots, selective local drafts, undo, and the existing save path", async () => {
  const [api, consoleSource, importer, codec] = await Promise.all([
    read("src/lib/api.ts"), readConsoleSurface(), read("src/components/EventStoryTxtImport.tsx"),
    read("src/lib/event-txt-import.mjs"),
  ]);
  assert.match(api, /getEventEpisodeSnapshot[\s\S]*\/event-story\/episode-snapshot/);
  assert.match(importer, /TextDecoder\("utf-8", \{ fatal: true \}\)/);
  assert.match(importer, /validateEventEpisodeSnapshot\(snapshot\)/);
  assert.match(importer, /assertSnapshotMatchesLoaded/);
  assert.match(importer, /selectedByDefault/);
  assert.match(importer, /应用到本地草稿/);
  assert.match(importer, /此步骤不会写入服务器/);
  assert.match(codec, /event TXT alignment is too large for a local preview/);
  assert.match(codec, /未按行号猜测/);
  assert.match(consoleSource, /EventStoryTxtImport/);
  assert.match(consoleSource, /TXT 本地草稿剩余/);
  assert.match(consoleSource, /undoEventTxtDraft/);
  assert.match(consoleSource, /updateEventStoryLine/);
  assert.match(consoleSource, /persistEventTxtDraft/);
  assert.match(consoleSource, /recoverEventTxtDraft/);
  assert.match(consoleSource, /clearPersistedEventTxtDraft/);
  assert.match(consoleSource, /resolveContentConflict[\s\S]*clearPersistedEventTxtDraft/);
  assert.match(consoleSource, /persistContentConflict/);
  assert.match(consoleSource, /recoverPersistedContentConflict/);
  assert.match(consoleSource, /clearPersistedContentConflict/);
  assert.match(consoleSource, /contentConflictStoragePrefix[\s\S]*crypto\.randomUUID\(\)/);
  assert.match(consoleSource, /clearPersistedContentConflict\(username, conflict\)/);
  assert.match(consoleSource, /TXT 草稿仍有剩余条目，请继续逐条保存/);
  assert.match(consoleSource, /selectedImportedBeforeSave[\s\S]*await save\(undefined, false\)/);
  assert.doesNotMatch(consoleSource, /isEventStory && eventTxtDraftDirty && !entryDirty\) \{[\s\S]{0,180}pending\.action\(\)/);
  assert.match(consoleSource, /disabled={isReadOnly \|\| writesLocked \|\| eventTxtDraftDirty \|\| hasRemoteConflict \|\| \(isEventStory && !hasCanonicalIdentity\)}/);
  assert.match(consoleSource, /const restoredEntries = restoreEventStoryDraftEntries\(entriesRef\.current, eventTxtDraft\.translations\)/);
});

test("event story segment revisions survive detail flattening and advance from mutation responses", async () => {
  const [api, labels, consoleSource] = await Promise.all([
    read("src/lib/api.ts"), read("src/lib/labels.ts"), readConsoleSurface(),
  ]);
  assert.match(api, /interface EventStorySegment[\s\S]*revision\?: number/);
  assert.match(api, /interface EventStoryUpdateResult[\s\S]*revision: number/);
  assert.match(api, /sourceHash: string, revision: number/);
  assert.match(api, /JSON\.stringify\(\{ eventId, episodeNo, jpKey, cnText, source, entryType, locale, segmentId, sourceHash,[\s\S]*revision, clientId/);
  assert.match(api, /promote-human[\s\S]*JSON\.stringify\(\{ eventId, clientId: getClientID\(\) \}\)/);
  assert.match(api, /event-story\/retry[\s\S]*JSON\.stringify\(\{ eventId, clientId: getClientID\(\) \}\)/);
  assert.match(api, /event-story\/reorder[\s\S]*JSON\.stringify\(\{ eventId, clientId: getClientID\(\) \}\)/);
  assert.match(api, /translate\/ai-story[\s\S]*JSON\.stringify\(\{ eventId, provider, clientId: getClientID\(\) \}\)/);
  assert.match(api, /status\?: unknown \}\)\.status !== "ok"/);
  assert.match(api, /revision\?: unknown \}\)\.revision !== revision \+ 1/);
  assert.match(api, /invalid_event_story_response/);
  assert.match(labels, /revision: segment\.revision \?\? 0/);
  assert.match(consoleSource, /saveEntry\?\.revision \?\? 0/);
  assert.match(consoleSource, /revision: result\.revision/);
  assert.match(consoleSource, /entry\.revision \?\? 0/);
  assert.match(consoleSource, /nextRevision = result\.revision/);
});

test("console generations fence loads and saves while tab identity reconciles realtime edits", async () => {
  const consoleSource = await readConsoleSurface();
  assert.match(consoleSource, /loadGenerationRef\.current !== generation/);
  assert.equal((consoleSource.match(/loadGenerationRef\.current !== generation\) return false/g) || []).length, 2);
  assert.match(consoleSource, /contextGenerationRef\.current !== generation/);
  assert.match(consoleSource, /const saveCategory = category/);
  assert.match(consoleSource, /d\.clientId !== clientID/);
  assert.doesNotMatch(consoleSource, /d\.user !== username/);
  assert.match(consoleSource, /selectedEntry && entryDirty/);
  assert.match(consoleSource, /setRemoteConflict/);
  assert.match(consoleSource, /const setRemoteConflict = useCallback[\s\S]*remoteConflictRef\.current = next/);
  assert.match(consoleSource, /remoteConflictRef\.current\?\.key === selectedKey[\s\S]*请先选择采用远端版本或明确保留本地草稿/);
  assert.match(consoleSource, /remoteConflictRef\.current\?\.key === key[\s\S]*请先明确处理冲突，再修改来源/);
  assert.match(consoleSource, /hasRemoteConflict={remoteConflict\?\.key === entry\.key}/);
  assert.match(consoleSource, /保留本地并允许覆盖/);
  assert.match(consoleSource, /setEditValue\(selectedEntry\.text\);\s*setRemoteConflict\(null\);/);
  assert.match(consoleSource, /frozenConflictDraftDirty[\s\S]*hasUnsavedChanges/);
  assert.match(consoleSource, /highlightRemoteRow/);
  assert.match(consoleSource, /remote-highlight/);
  assert.match(consoleSource, /event === "presence\.snapshot" \|\| event === "presence\.joined" \|\| event === "presence\.left"/);
  assert.match(consoleSource, /setOnlineUsers/);
  assert.match(consoleSource, /nexttrans-html-etag/);
  assert.match(consoleSource, /页面已有新版本/);
  assert.match(consoleSource, /event === "content\.restored"/);
  assert.doesNotMatch(consoleSource, /setRestoreGeneration/);
  assert.match(consoleSource, /lyricsEditorRef\.current\?\.reloadAuthoritative\(\)/);
  assert.match(consoleSource, /const captured = captureContext\(\)/);
  assert.match(consoleSource, /if \(!contextIsCurrent\(captured\)\) return/);
});

test("lyrics collaboration reads an imperative dirty snapshot before parent effects can report it", async () => {
  const [consoleSource, editor] = await Promise.all([
    readConsoleSurface(), readLyricsEditor(),
  ]);
  assert.match(editor, /snapshot: \(\) => \(\{[\s\S]*dirty: isDirtyNow\(\)/);
  assert.match(editor, /lyricsDocumentDirty\(lyricsRef\.current, baselineRef\.current, collaboration \? collaboration\.hasLocalChanges\(\) : null\)/);
  assert.match(editor, /document: lyricsRef\.current \? JSON\.parse\(JSON\.stringify\(lyricsRef\.current\)\)/);
  assert.match(editor, /editionKey: activeTranslationEditionKeyRef\.current/);
  assert.match(consoleSource, /const lyricsSnapshot = lyricsEditorRef\.current\?\.snapshot\(\) \?\? null/);
  assert.match(consoleSource, /kind: "lyrics", editionKey: lyricsSnapshot\?\.editionKey \|\| ""/);
  assert.match(consoleSource, /if \(lyricsSnapshot\?\.dirty \?\? lyricsDirty\)/);
  assert.match(consoleSource, /共享 revision 已变化，本地未保存草稿已冻结/);
  assert.match(consoleSource, /const captureUnsavedDraft[\s\S]*lyricsEditorRef\.current\?\.snapshot\(\)/);
});

test("parent dirty actions stay non-cancelable and generation-fenced while awaiting save", async () => {
  const consoleSource = await readConsoleSurface();
  assert.match(consoleSource, /const \[pendingActionBusy, setPendingActionBusy\] = useState\(false\)/);
  assert.match(consoleSource, /pendingActionBusyRef\.current = true/);
  assert.match(consoleSource, /pendingActionRef\.current\?\.token === pending\.token/);
  assert.match(consoleSource, /contextGenerationRef\.current === pending\.contextGeneration/);
  assert.match(consoleSource, /if \(!saved \|\| !pendingIsCurrent\(\)\) return/);
  assert.match(consoleSource, /if \(!lyricsEditorRef\.current\?\.discard\(\)\) return/);
  assert.match(consoleSource, /if \(savingRef\.current\)[\s\S]*当前保存尚未完成/);
  assert.match(consoleSource, /closeDisabled=\{pendingActionBusy \|\| saving\}/);
  assert.match(consoleSource, /disabled=\{pendingActionBusy \|\| saving\}>放弃修改/);
  assert.match(consoleSource, /disabled=\{pendingActionBusy \|\| saving\}>取消/);
});

test("restore conflicts never offer save-first and stale drafts can only be exported or discarded", async () => {
  const [consoleSource, shell] = await Promise.all([
    readConsoleSurface(), read("src/components/console/ProducerOperationsShell.tsx"),
  ]);
  assert.match(consoleSource, /event === "content\.restored"[\s\S]*reconcileContent\("restore"\)/);
  assert.match(consoleSource, /captureUnsavedDraft/);
  assert.match(consoleSource, /contentConflict\?\.draft \?\? preservedConflictDraftRef\.current \?\? captureUnsavedDraft\(\)/);
  assert.match(consoleSource, /exportConflictDraft/);
  assert.match(consoleSource, /旧缓冲区仅可导出后手动合并/);
  assert.match(consoleSource, /舍弃旧缓冲区并继续/);
  assert.match(consoleSource, /if \(writeFenceRef\.current \|\| savingRef\.current/);
  const conflict = shell.slice(shell.indexOf('<Modal open={contentConflict != null}'));
  assert.doesNotMatch(conflict, /保存并继续/);
});

test("SSE gaps lock writes until authoritative reconciliation completes", async () => {
  const [consoleSource, ws, editor] = await Promise.all([
    readConsoleSurface(), read("src/lib/sse.ts"),
    readLyricsEditor(),
  ]);
  assert.ok(ws.includes('"sse.disconnected"'), 'missing transport sse.disconnected event');
  assert.ok(ws.includes('"sse.reconnected"'), 'missing transport sse.reconnected event');
  assert.ok(ws.includes('"sse.missed-events"'), 'missing transport sse.missed-events event');
  assert.ok(ws.includes('"presence.snapshot"'), 'missing presence snapshot event');
  assert.match(consoleSource, /useState\(true\)[\s\S]*const writeFenceRef = useRef\(true\)/);
  assert.match(consoleSource, /event === "sse\.disconnected"[\s\S]*setWriteFence\(true\)/);
  assert.match(consoleSource, /event === "sse\.disconnected"[\s\S]*reconcileContentRef\.current\("gap"\)/);
  assert.match(consoleSource, /event === "sse\.disconnected"[\s\S]*reconciliationGenerationRef\.current\+\+/);
  assert.match(consoleSource, /event === "sse\.missed-events"[\s\S]*reconcileContent\("gap"\)/);
  const reconnectBranch = consoleSource.slice(
    consoleSource.indexOf('event === "sse.reconnected"'),
    consoleSource.indexOf('event === "sse.missed-events"'),
  );
  assert.doesNotMatch(reconnectBranch, /show\(/, "transport reconnect must not announce success before reconciliation");
  assert.match(consoleSource, /reconcileContent\("gap"\)\.then\(\(reconciled\) => \{[\s\S]*if \(reconciled\) show\(/);
  assert.match(consoleSource, /d\.initial === true \? "实时连接已建立" : "实时连接已恢复"/);
  assert.match(consoleSource, /Promise\.all\(\[[\s\S]*reloadSidebar\(\), loadEntries\(\), lyricsReload/);
  assert.match(consoleSource, /preservedConflictDraftRef\.current \?\? captureUnsavedDraft\(\)/);
  assert.match(consoleSource, /contentEventGenerationRef\.current !== contentEventGeneration[\s\S]*reconcileContent\(reason, draft, conflictDetail\)/);
  assert.match(consoleSource, /setWriteFence\(false\)/);
  assert.match(consoleSource, /writeLocked={writesLocked}/);
  assert.match(editor, /disabled={busy \|\| writeLocked}/);
  assert.match(editor, /writeLockedRef\.current = writeLocked/);
  assert.match(editor, /if \(busyRef\.current \|\| writeLockedRef\.current\) return/);
  assert.match(editor, /const editionTransition = pending\.kind === "edition-switch" \|\| pending\.kind === "edition-command"/);
  assert.match(editor, /\(saveFirst \|\| pending\.kind === "publish" \|\| editionTransition\) && writeLockedRef\.current/);
  assert.match(editor, /reloadAuthoritative[\s\S]*setPendingTransition\(null\)/);
});

test("stale conflict resolution revalidates proof and live SSE before releasing the write fence", async () => {
  const [consoleSource, realtime] = await Promise.all([
    readConsoleSurface(), read("src/components/console/useConsoleRealtime.ts"),
  ]);
  const reconcile = realtime.slice(realtime.indexOf("const reconcileContent = async"), realtime.indexOf("reconcileContentRef.current = reconcileContent"));
  const resolve = realtime.slice(realtime.indexOf("const resolveContentConflict"), realtime.indexOf("const exportConflictDraft"));
  const sseHandler = realtime.slice(realtime.indexOf("useSSE((event, data)"), realtime.indexOf("  }, true);", realtime.indexOf("useSSE((event, data)")));

  assert.equal((reconcile.match(/if \(!sseConnectedRef\.current\)/g) || []).length, 2);
  assert.match(reconcile, /const failReconcile = \(message: string\)/);
  assert.match(reconcile, /if \(!sseConnectedRef\.current\)[\s\S]*failReconcile\("实时连接尚未恢复/);
  assert.match(reconcile, /producerBefore = await getEditorGateStatus\(\)[\s\S]*producerAfter = await getEditorGateStatus\(\)[\s\S]*retryReconcileLater\(\)[\s\S]*acceptLoadedProducerState\(producerAfter\)/);
  assert.match(resolve, /setContentConflict\(\{ \.\.\.conflict, draft: null, reloadFailed: true \}\)/);
  assert.match(resolve, /reconcileContent\(conflict\.reason, null, conflict\.detail\)/);
  assert.doesNotMatch(resolve, /setWriteFence\(false\)|setContentConflict\(null\)/);
  assert.match(sseHandler, /event === "sse\.disconnected"[\s\S]*sseConnectedRef\.current = false[\s\S]*setWriteFence\(true\)/);
  assert.match(sseHandler, /event === "sse\.reconnected"[\s\S]*sseConnectedRef\.current = true/);
  assert.match(sseHandler, /event === "sse\.missed-events"[\s\S]*sseConnectedRef\.current = true[\s\S]*reconcileContent\("gap"\)/);
  assert.match(consoleSource, /subscribeProducerProofInvalidated\(\(\) => \{[\s\S]*setWriteFence\(true\)[\s\S]*reconcileContentRef\.current\("gap"\)/);
});

test("realtime fence blocks writes without blocking local logout discard or cancel", async () => {
  const [consoleSource, editor] = await Promise.all([
    readConsoleSurface(), readLyricsEditor(),
  ]);
  const localGuard = consoleSource.slice(consoleSource.indexOf("const runOrGuard"), consoleSource.indexOf("const guardProducerMutation"));
  assert.doesNotMatch(localGuard, /writeFenceRef\.current/);
  assert.match(consoleSource, /runOrGuard\("退出登录"/);
  assert.match(consoleSource, /pendingActionBusyRef\.current \|\| savingRef\.current \|\| \(saveFirst && writeFenceRef\.current\)/);
  assert.match(consoleSource, /onClick=\{\(\) => void continuePendingAction\(false\)\} disabled=\{pendingActionBusy \|\| saving\}>放弃修改/);
  assert.match(consoleSource, /onClick=\{closePendingAction\} disabled=\{pendingActionBusy \|\| saving\}>取消/);
  assert.equal((consoleSource.slice(consoleSource.indexOf("const guardProducerMutation"), consoleSource.indexOf("const { save, handleSourceChange")).match(/if \(writeFenceRef\.current\)/g) || []).length, 2);
  assert.match(consoleSource, /保存等待期间实时校对已锁定，上游操作未执行/);
  // promote-human is a strict v1 route: the producer-state proof must survive
  // until the action has run, then be dropped before the gap reconcile.
  const producerGuard = consoleSource.slice(consoleSource.indexOf("const guardProducerMutation"), consoleSource.indexOf("const { save, handleSourceChange"));
  assert.doesNotMatch(producerGuard, /clearLoadedProducerState\(\);\s*void Promise\.resolve\(\)\.then\(action\)/);
  assert.match(producerGuard, /Promise\.resolve\(\)\.then\(action\)\.finally\(\(\) => \{\s*clearLoadedProducerState\(\);\s*reconcileContentRef\.current\("gap"\);/);
  assert.match(editor, /\(saveFirst \|\| pending\.kind === "publish" \|\| editionTransition\) && writeLockedRef\.current/);
  assert.match(editor, /onClick=\{\(\) => void continuePendingTransition\(false\)\}[\s\S]{0,220}>放弃并继续/);
  assert.match(editor, /onClick=\{\(\) => setPendingTransition\(null\)\} disabled=\{busy\}>取消/);
  assert.match(editor, /<fieldset className="lyrics-edit-fence" disabled=\{busy\}/);
});

test("auth initialization preserves shared sessions on transient failures and exposes retry", async () => {
  const [page, api] = await Promise.all([read("src/app/page.tsx"), read("src/lib/api.ts")]);
  assert.match(page, /if \(current\?\.session\)[\s\S]*setInitializationError/);
  assert.match(page, /已保留当前共享会话/);
  assert.match(page, /重试身份验证/);
  assert.doesNotMatch(page, /catch[\s\S]{0,120}clearSession\(dispatched\)/);
  assert.match(api, /if \(res\.status === 401\)[\s\S]*clearSession\(initiated\.envelope\)/);
  assert.doesNotMatch(api, /if \(!res\.ok\)[\s\S]{0,180}clearSession/);
});

test("lyrics workspace covers catalog, verified source import, draft, and publication", async () => {
  const [editor, lineEditor, sidebar, metadata, projection, api, sourceImport] = await Promise.all([
    readLyricsEditor(), read("src/components/lyrics/LyricsLineEditor.tsx"),
    read("src/components/lyrics/LyricsCatalogSidebar.tsx"), read("src/components/lyrics/LyricsMetadataCard.tsx"),
    read("src/components/lyrics/LyricsProjectionStatusCard.tsx"),
    read("src/lib/api.ts"),
    read("src/lib/lyrics-source-import.mjs"),
  ]);
  const combinedEditor = `${editor}\n${sidebar}\n${metadata}\n${projection}`;
  for (const contract of [
    "getCatalogMusic", "保存草稿", "保存并公开", "候选来源", "使用此版本", "载入服务器版本", "取消发布",
    "翻译", "校对", "translationCredit", "proofreadingCredit", "attribution",
  ]) {
    assert.ok(combinedEditor.includes(contract), `missing lyrics console contract: ${contract}`);
  }
  assert.match(editor, /sourceUrl: preview\.canonicalUrl/);
  assert.match(api, /interface LyricsSourcePreview[\s\S]*importToken: string/);
  const songLyricsType = api.slice(api.indexOf("export interface SongLyrics"), api.indexOf("export interface LyricsSourceCandidate"));
  assert.match(songLyricsType, /attribution: string;[\s\S]*translationCredit: string;[\s\S]*proofreadingCredit: string;/);
  assert.doesNotMatch(songLyricsType, /translationCredit\?:|proofreadingCredit\?:|importToken|sourceImportToken/);
  assert.match(editor, /const sourceImportTokenRef = useRef\(""\)/);
  assert.doesNotMatch(editor, /useState[^\n]*sourceImportToken|setSourceImportToken/);
  assert.match(editor, /const findSource = async \(\) => \{[\s\S]*if \(!lyrics \|\| isRenditionLyricsDocument\(lyrics\) \|\| role !== "admin" \|\| busyRef\.current \|\| writeLockedRef\.current\) return/);
  assert.match(editor, /const previewSource = async \(candidate: LyricsSourceCandidate\) => \{[\s\S]*if \(!lyrics \|\| isRenditionLyricsDocument\(lyrics\) \|\| role !== "admin" \|\| busyRef\.current \|\| writeLockedRef\.current\) return/);
  assert.match(editor, /if \(!lyrics \|\| isRenditionLyricsDocument\(lyrics\) \|\| lyrics\.revision !== 0 \|\| !sourcePreview \|\| role !== "admin"/);
  assert.match(editor, /sourceImportTokenRef\.current = preview\.importToken/);
  assert.match(editor, /const importToken = lyrics\.revision === 0 \? sourceImportTokenRef\.current : ""/);
  assert.match(editor, /const saved = importToken[\s\S]*\? await saveLyrics\(lyrics, importToken\)[\s\S]*: await checkpointLyrics\(musicID\)/);
  assert.match(editor, /const authoritative = await getLyrics\(musicID\)/);
  assert.match(editor, /sameImportedLyricsFrozenIdentity\(attempted, authoritative\)/);
  assert.match(editor, /首次保存可能已成功/);
  assert.match(editor, /不会要求重新预览/);
  assert.match(api, /JSON\.stringify\(buildLyricsSavePayload\(lyrics, sourceImportToken, getClientID\(\)\)\)/);
  assert.match(editor, /sourceImportTokenRef\.current = ""/);
  assert.match(editor, /performChooseMusic[\s\S]*sourceImportTokenRef\.current = ""/);
  assert.match(editor, /const discard = \(\): boolean => \{[\s\S]*sourceImportTokenRef\.current = ""/);
  assert.match(editor, /const previewSource = async[\s\S]*sourceImportTokenRef\.current = ""/);
  assert.match(editor, /const reloadAuthoritative = async[\s\S]*sourceImportTokenRef\.current = ""/);
  assert.match(editor, /exportDraft: \(\) => lyrics \? JSON\.parse\(JSON\.stringify\(lyrics\)\) as SongLyricsDocument : null/);
  const conflictReloadHandler = editor.slice(editor.indexOf("const loadConflictAuthoritative = async"));
  assert.match(conflictReloadHandler, /sourceImportTokenRef\.current = ""/);
  assert.match(editor, /const TERMINAL_SOURCE_IMPORT_CODES = new Set/);
  assert.match(editor, /if \(error\.status >= 500\) return false/);
  assert.match(editor, /error\.status === 401 \|\| error\.status === 403\) return true/);
  assert.match(editor, /error\.code === "source_import_in_flight" \|\| error\.status === 428/);
  assert.match(editor, /error\.code === "segment_mismatch" \|\| error\.code === "invalid_performer"\) return false/);
  assert.match(editor, /TERMINAL_SOURCE_IMPORT_CODES\.has\(error\.code\)/);
  assert.match(editor, /token\|grant\|授权[\s\S]*expir\|consum[\s\S]*identity\|producer[\s\S]*source/);
  assert.doesNotMatch(editor, /TERMINAL_SOURCE_IMPORT_STATUSES|\[401, 403, 409, 412, 422, 428\]|status === 412 \|\| error\.status === 428\) return true/);
  const saveFailureHandler = editor.slice(editor.indexOf("const terminalImportFailure = Boolean(importToken)"), editor.indexOf("} finally {", editor.indexOf("const terminalImportFailure = Boolean(importToken)")));
  assert.match(saveFailureHandler, /if \(terminalImportFailure\) \{[\s\S]*sourceImportTokenRef\.current = ""[\s\S]*setSourceRetry\(sourcePreviewCandidate \? \{ kind: "preview", candidate: sourcePreviewCandidate \} : \{ kind: "search" \}\)/);
  assert.equal((saveFailureHandler.match(/sourceImportTokenRef\.current = ""/g) || []).length, 1);
  assert.equal((saveFailureHandler.match(/setSourceRetry\(/g) || []).length, 1);
  assert.doesNotMatch(saveFailureHandler, /setBaseline\(/);
  assert.match(editor, /已保留固定修订授权和 verified draft，可直接重试保存/);
  assert.match(editor, /固定修订授权或 verified draft 状态已失效，请重新预览后再保存/);
  assert.doesNotMatch(editor, /保存尝试开始时即作废|瞬时失败也必须重新预览|瞬时失败也不能重用/);
  assert.match(api, /saveLyrics = \(lyrics: SongLyricsDocument, sourceImportToken\?: string\)/);
  assert.match(editor, /buildLyricsLinesFromSourcePreview\(preview, performers\)/);
  assert.match(editor, /if \(!imported\.ok\)[\s\S]*setError\(new APIError/);
  assert.match(sourceImport, /id: `source-\$\{order \+ 1\}`/);
  assert.match(sourceImport, /CANONICAL_SOURCE_PERFORMERS/);
  assert.match(sourceImport, /performerIds: mapped\.length > 0 \? mapped : \[\.\.\.trailing\]/);
  assert.match(sourceImport, /source_performer_mapping_failed/);
  assert.doesNotMatch(sourceImport, /performerIds: sourceIds|performerIds: segment\.performerIds/);
  assert.doesNotMatch(editor, /id: `wiki-\$\{preview\.pageId\}-\$\{preview\.revisionId\}/);
  assert.doesNotMatch(editor, /sourcePreview\.lines\.slice\(0, 12\)/);
  assert.match(editor, /role === "admin" && isLegacyLyricsDocument\(lyrics\) && <button[\s\S]*查找来源/);
  assert.match(editor, /确认载入草稿/);
  assert.match(editor, /固定来源只能在首次保存前导入/);
  assert.match(editor, /已保存修订仍可调整歌词结构与日文原文/);
  assert.doesNotMatch(editor, /永久锁定来源|已永久锁定|保持每行日文拼接结果完全一致的前提下重新分段/);
  assert.doesNotMatch(lineEditor, /value={segment\.text} readOnly=/);
  assert.match(lineEditor, /aria-label={`第 \$\{lineNumber\} 行分段 \$\{segmentNumber\}`}/);
  assert.match(editor, /<LyricsLineEditor/);
  assert.match(editor, /const patch: Partial<LyricsEditorLine> = \{ segments \} as Partial<LyricsEditorLine>;[\s\S]*if \(sourceMayChange\) patch\.japanese/);
  assert.match(editor, /setSegments\(lineIndex, segments, activeSideSourceMutable\)/);
  assert.doesNotMatch(editor, /lyrics\.revision > 0 \|\| activeSideReadOnly/);
  assert.match(editor, /publicationChecks/);
  assert.match(api, /getProjectionStatus = \(musicId\?: number\) =>/);
  assert.match(editor, /previousProjectionGeneration = status\.generation/);
  assert.match(editor, /void waitForProjection\(previousProjectionGeneration, nextPublished, musicID\)/);
  assert.match(editor, /status\.generation > previousGeneration\) \{[\s\S]*?setProjectionState\("ready"\);[\s\S]*?void loadCatalog\(query\);[\s\S]*?return;/);
  assert.match(editor, /数据库发布已提交，正在核对公共文件/);
  assert.match(combinedEditor, /重新核对公共文件/);
  assert.match(editor, /分段与日文一致/);
  assert.match(editor, /分段文字未完整拼接为日文原文/);
  assert.match(editor, /发布准备/);
  assert.match(editor, /正在搜索并核对候选来源/);
  assert.match(editor, /正在载入固定修订/);
  assert.match(editor, /不会只截取前几行/);
  assert.ok(editor.includes('<pre tabIndex={0} aria-label={`固定修订'));
  assert.match(editor, /lyrics\.revision === 0 && sourceActivity === "searching"/);
  assert.match(editor, /sourceRetry\.kind === "search"/);
  assert.match(editor, /disabled={busy \|\| writeLocked \|\| lyrics\.revision > 0}>\{sourceActivity === "searching" \? "正在查找…" : "查找来源"\}/);
  assert.doesNotMatch(editor, /sourceURL/);
});

test("lyrics mutations validate and correlate 2xx responses before clearing local recovery state", async () => {
  const [api, editor, validator] = await Promise.all([
    read("src/lib/api.ts"), readLyricsEditor(), read("src/lib/lyrics-save.mjs"),
  ]);
  assert.match(api, /body = await res\.json\(\) as T[\s\S]*invalid_json_response/);
  assert.match(api, /response = await apiFetch<unknown>\(path, options, true\)/);
  assert.match(api, /reason instanceof APIError && reason\.code === "invalid_json_response"[\s\S]*invalid_lyrics_response/);
  assert.match(api, /validateSongLyricsMutationResponse\(response, expectation\)/);
  assert.match(api, /invalid_lyrics_response/);
  assert.match(api, /operation: "save", musicId: lyrics\.musicId, revision: lyrics\.revision, document: lyrics/);
  assert.match(api, /operation: "publish", musicId, revision/);
  assert.match(api, /operation: "unpublish", musicId, revision/);
  assert.match(validator, /performerIds must contain unique positive integers/);
  assert.match(validator, /save response content does not match the submitted document/);
  assert.match(validator, /publish response does not confirm the requested publication/);
  assert.match(editor, /if \(importToken && apiError\.code !== "invalid_lyrics_response"\)/);
  assert.match(editor, /if \(error\.status >= 500\) return false/);
  const terminalHandler = editor.slice(editor.indexOf("const terminalImportFailure"), editor.indexOf("} finally {", editor.indexOf("const terminalImportFailure")));
  assert.match(terminalHandler, /if \(terminalImportFailure\)/);
  assert.doesNotMatch(terminalHandler, /setBaseline\(/);
});

test("translation entry payloads carry last-update metadata for client-side sorting", async () => {
  const [store, localization, model, api] = await Promise.all([
    read("../server/internal/store/store.go"), read("../server/internal/store/localization.go"),
    read("../server/internal/model/model.go"), read("src/lib/api.ts"),
  ]);
  assert.match(store, /SELECT jp_key, cn_text, source, ids_json, updated_at/);
  assert.match(localization, /COALESCE\(l\.updated_at, e\.updated_at\)/);
  assert.match(localization, /SELECT jp_key, text, source, updated_at/);
  assert.match(model, /UpdatedAt int64.*updatedAt/);
  assert.match(api, /updatedAt\?: number/);
});

test("entry saves opt into and strictly correlate the additive response contract", async () => {
  const api = await read("src/lib/api.ts");
  assert.match(api, /\/editor\/v1\/entry\?response=correlated-v1/);
  assert.match(api, /const payload = \{ category, field, key, text, source, clientId: getClientID\(\)/);
  assert.match(api, /validateEntryMutationResponse\(response, payload\)/);
  assert.match(api, /required = \["status", "category", "field", "key", "text", "source"\]/);
  assert.match(api, /keys\.some\(\(name\) => !allowed\.has\(name\)\)/);
  assert.match(api, /expected\.locale === undefined[\s\S]*!responseHasLocale[\s\S]*response\.locale === expected\.locale/);
  assert.match(api, /invalid_entry_response/);
});

test("editor UI hides mutations while preserving read-only backup status", async () => {
  const [consoleSource, settings] = await Promise.all([
    readConsoleSurface(),
    read("src/components/SettingsModal.tsx"),
  ]);
  assert.match(consoleSource, /role === "admin" && locale === "zh-CN" && <>/);
  assert.match(settings, /<DataManagementCard canMutate={role === "admin"}/);
  assert.doesNotMatch(settings, /role === "admin" && <DataManagementCard/);
  assert.match(settings, /{canMutate && <>[\s\S]*数据更新（CN 同步）[\s\S]*手动备份[\s\S]*<\/\>}[\s\S]*刷新状态/);
  assert.match(settings, /Git\/S3 仅用于内容数据备份\/归档，不是部署或公开发布流程，也不会触发 CDN 同步或刷新/);
  assert.match(settings, /e instanceof APIError && e\.results/);
});

test("lyrics transitions guard dirty publication and ignore stale song loads", async () => {
  const [editor, sidebar] = await Promise.all([
    readLyricsEditor(),
    read("src/components/lyrics/LyricsCatalogSidebar.tsx"),
  ]);
  const combined = `${editor}\n${sidebar}`;
  assert.match(editor, /requestIsCurrent\(sequence, item\.musicId\)/);
  assert.match(editor, /kind: "publish"/);
  assert.match(editor, /continuePendingTransition\(true\)/);
  assert.match(editor, /beforeunload/);
  assert.match(editor, /requestIsCurrent\(sequence, musicID\)/);
  assert.match(editor, /selectedMusicIDRef\.current = item\.musicId/);
  assert.match(editor, /disabled={busy}/);
  assert.match(editor, /closeDisabled={busy}/);
  assert.match(editor, /确认发布歌词/);
  assert.match(editor, /publicationProblems/);
  assert.match(editor, /event\.key\.toLowerCase\(\) !== "s"/);
  assert.match(editor, /mergedPerformers = Array\.from\(new Set/);
  assert.match(combined, /aria-current={selectedMusic\?\.musicId === item\.musicId/);
  assert.match(editor, /lyrics-stanza-start/);
  assert.match(editor, /void loadCatalog\(query\)/);
  assert.match(editor, /void loadPerformers\(\)/);
  assert.match(editor, /<fieldset className="lyrics-edit-fence" disabled={busy} aria-busy={busy} aria-disabled={writeLocked}/);
  assert.match(editor, /busyRef\.current = true/);
  assert.match(editor, /documentGenerationRef\.current !== documentGeneration/);
  assert.match(editor, /if \(busyRef\.current\) return/);
});

test("lyrics collaboration consumes server-derived rendition targets and freezes dirty shared revisions", async () => {
  const [api, sse, consoleSource, editor, collaboration] = await Promise.all([
    read("src/lib/api.ts"), read("src/lib/sse.ts"), readConsoleSurface(),
    readLyricsEditor(), read("src/lib/lyrics-collaboration.mjs"),
  ]);
  assert.match(api, /JSON\.stringify\(buildLyricsSavePayload\(lyrics, sourceImportToken, getClientID\(\)\)\)/);
  assert.match(api, /musicId, revision, clientId: getClientID\(\)/);
  assert.match(sse, /"lyrics\.updated"/);
  assert.match(consoleSource, /event === "lyrics\.updated"/);
  assert.match(consoleSource, /normalizeLyricsUpdateEvent\(d\)/);
  assert.match(consoleSource, /update\.clientId !== clientID/);
  assert.match(consoleSource, /lyricsEditorRef\.current\?\.activeTarget\(\)/);
  assert.match(consoleSource, /lyricsUpdateMatchesEditorTarget\(update, activeTarget\)/);
  assert.match(consoleSource, /const lyricsSnapshot = lyricsEditorRef\.current\?\.snapshot\(\) \?\? null;[\s\S]*if \(lyricsSnapshot\?\.dirty \?\? lyricsDirty\)[\s\S]*reconcileContent\("remote", draft, detail\)/);
  assert.doesNotMatch(consoleSource, /runOrGuard\("同步协作者更新", \(\) => setRestoreGeneration/);
  assert.match(consoleSource, /lyricsEditorRef\.current\?\.reloadAuthoritative\(\)/);
  assert.match(consoleSource, /reloadCatalog\(\)/);
  assert.match(editor, /activeTarget: \(\) => selectedMusicIDRef\.current == null \? null/);
  assert.match(editor, /renditionKey: activeRendition\?\.key \|\| ""/);
  assert.match(editor, /side: activeVersion/);
  assert.match(editor, /projectionKind,/);
  assert.match(collaboration, /target\.side === "game" && target\.projectionKind === "exact_projection" && update\.side === "full"/);
  assert.match(collaboration, /const TARGET_SIDES = new Set\(\["full", "game", "credits"\]\)/);
  assert.match(collaboration, /malformed additive targets|return \{ musicId, revision, clientId \}/);
  assert.match(api, /let clientID = ""/);
  assert.match(api, /if \(!clientID\) clientID = crypto\.randomUUID\(\)/);
  assert.doesNotMatch(api, /sessionStorage/);
});

test("backup restore requires typed confirmation and enters the shared dirty guard", async () => {
  const [consoleSource, admin, api] = await Promise.all([
    readConsoleSurface(), read("src/components/AdminModal.tsx"), read("src/lib/api.ts"),
  ]);
  assert.match(consoleSource, /<AdminModal[\s\S]*guardProducerMutation={guardProducerMutation}/);
  assert.match(admin, /restoreConfirmation !== `RESTORE:\$\{restoreTarget\}`/);
  assert.match(admin, /guardProducerMutation\(`从 \$\{target\} 恢复`, \(\) => performRestore\(target, confirmation\)\)/);
  assert.match(admin, /const performRestore = async[\s\S]*await restoreBackup\(target, confirmation\)/);
  assert.match(api, /JSON\.stringify\(\{ target, confirmation \}\)/);
});

test("settings and admin upstream producers enter the shared write and dirty fence", async () => {
  const [consoleSource, admin, settings] = await Promise.all([
    readConsoleSurface(), read("src/components/AdminModal.tsx"), read("src/components/SettingsModal.tsx"),
  ]);
  assert.match(consoleSource, /<SettingsModal[\s\S]*locale={locale}[\s\S]*guardProducerMutation={guardProducerMutation}/);
  assert.match(settings, /<BadgeFilterCard locale={locale} \/>/);
  assert.match(settings, /getCategories\(locale\)/);
  assert.match(settings, /getEventStories\(locale\)/);
  assert.match(consoleSource, /runOrGuard\(label, \(\) => \{[\s\S]*if \(writeFenceRef\.current\)[\s\S]*setWriteFence\(true\)[\s\S]*Promise\.resolve\(\)\.then\(action\)\.finally\(\(\) => \{[\s\S]*reconcileContentRef\.current\("gap"\);/);
  assert.match(consoleSource, /guardProducerMutation\("运行 AI 剧情翻译", doAIStory\)/);
  assert.match(consoleSource, /guardProducerMutation\("重新获取剧情", retryStory\)/);
  assert.match(consoleSource, /guardProducerMutation\("重排序对话", reorderStory\)/);
  assert.match(consoleSource, /const withBusy = async \(fn: \(\) => Promise<void>, producerOwnsFence = false\)/);
  assert.match(consoleSource, /if \(writeFenceRef\.current && !producerOwnsFence\)/);
  assert.match(consoleSource, /const doAIStory = \(\) => withBusy\([\s\S]*\}, true\);/);
  assert.match(consoleSource, /const promoteStory = \(\) => withBusy\([\s\S]*\}, true\);/);
  assert.match(consoleSource, /const retryStory = \(\) => withBusy\([\s\S]*\}, true\);/);
  assert.match(consoleSource, /const reorderStory = \(\) => withBusy\([\s\S]*\}, true\);/);
  assert.match(consoleSource, /guardProducerMutation\("整篇标记人工", promoteStory\)/);
  assert.match(settings, /guardProducerMutation\("运行 CN 同步", doSync\)/);
  assert.match(admin, /guardProducerMutation\("检查上游更新", \(\) => check\(false\)\)/);
  assert.match(admin, /guardProducerMutation\("强制同步上游", \(\) => check\(true\)\)/);
});

test("existing account and settings surfaces remain mounted", async () => {
  const [consoleSource, admin] = await Promise.all([
    readConsoleSurface(), read("src/components/AdminModal.tsx"),
  ]);
  assert.match(consoleSource, /<SettingsModal/);
  assert.match(consoleSource, /<AdminModal/);
  assert.match(consoleSource, /clearSession\(\)\.then/);
  assert.match(admin, /await restoreBackup\(target, confirmation\)/);
});

test("console and workspace share the atomic session protocol and token-bound SSE", async () => {
  const [api, session, sse, transport, page] = await Promise.all([
    read("src/lib/api.ts"), read("src/lib/session.ts"), read("src/lib/sse.ts"), read("src/lib/fetch-sse.mjs"), read("src/app/page.tsx"),
  ]);
  assert.match(api, /refreshSession/);
  assert.match(api, /commitIdentitySession/);
  assert.match(api, /commitRefreshedSession/);
  assert.match(api, /sameSessionVersion/);
  assert.match(api, /navigator\.locks\.request\(REFRESH_LOCK/);
  assert.match(session, /moesekai-session-v1/);
  assert.match(session, /moesekai-workspace-identity/);
  assert.match(session, /moesekai-workspace-refresh/);
  assert.match(session, /version: 1, epoch/);
  assert.match(session, /session: null/);
  assert.match(session, /value === "admin" \|\| value === "editor"/);
  assert.match(session, /sameSessionVersion/);
  assert.match(session, /event\.key === SESSION_KEY/);
  assert.match(session, /if \(getStoredSessionEnvelope\(\)\) return/);
  assert.match(sse, /\[enabled, version\]/);
  assert.match(transport, /Authorization.*Bearer/);
  assert.match(sse, /sameSessionIdentity/);
  assert.match(sse, /activeControllerRef\.current\?\.abort/);
  assert.doesNotMatch(`${sse}\n${transport}`, /EventSource|\?token=/);
  assert.match(sse, /content\.restored/);
  assert.match(page, /expiresAt \* 1000 - Date\.now\(\) - 60_000/);
  assert.match(session, /const identityChanged = session\.role !== expected\.session\?\.role/);
  assert.match(session, /identityChanged \? newEpoch\(\) : expected\.epoch/);
  assert.match(page, /subscribeSessionChanged[\s\S]*setSessionEpoch\(getSessionEpoch\(\)\)/);
  assert.match(page, /<Console key={sessionEpoch}/);
});

test("console writes carry tab-memory producer proof through strict editor routes", async () => {
  const [api, consoleSource] = await Promise.all([
    read("src/lib/api.ts"), readConsoleSurface(),
  ]);
  for (const route of [
    "/editor/v1/entry?response=correlated-v1", "/editor/v1/event-story/update", "/editor/v1/event-story/promote-human",
    "/editor/v1/lyrics/save", "/editor/v1/lyrics/publish", "/editor/v1/lyrics/unpublish",
    "/editor/v1/backup/push",
  ]) assert.ok(api.includes(`"${route}"`), `missing strict route ${route}`);
  assert.match(api, /X-Moe-Loaded-Producer-State/);
  assert.match(api, /loadedProducerState = \{[\s\S]*epoch: envelope\.epoch/);
  assert.ok(api.includes("header: `${status.instanceId}:${status.revision}:${status.completedGeneration}`"));
  assert.match(api, /requireProducerProof && \(res\.status === 400 \|\| res\.status === 428 \|\|[\s\S]*res\.status === 409 && isEditorGateStatus\(err\)\)\) invalidateLoadedProducerState/);
  assert.match(consoleSource, /producerBefore = await getEditorGateStatus\(\)/);
  assert.match(consoleSource, /producerAfter = await getEditorGateStatus\(\)/);
  assert.match(consoleSource, /producerBefore\.revision !== producerAfter\.revision/);
  assert.match(consoleSource, /acceptLoadedProducerState\(producerAfter\)/);
  assert.match(consoleSource, /subscribeProducerProofInvalidated/);
});

test("lyrics-source review UI keeps the private admin contract while adding batch decisions", async () => {
  const [api, review, pagination, selection] = await Promise.all([
    read("src/lib/api.ts"), read("src/components/LyricsSourceReview.tsx"), read("src/lib/lyrics-review-pagination.mjs"),
    read("src/lib/lyrics-review-selection.mjs"),
  ]);
  const detailType = api.slice(api.indexOf("export interface LyricsSourceReviewDetail"), api.indexOf("export interface LyricsSourceReviewMutationResult"));
  assert.doesNotMatch(detailType, /artifactId|analysisId/);
  for (const route of [
    "/admin/lyrics-source-reviews", "/admin/lyrics-source-reviews/detail",
    "/admin/lyrics-source-reviews/decision", "/admin/lyrics-source-reviews/candidate-selection",
  ]) assert.ok(api.includes(route), `missing lyrics review admin route ${route}`);
  const reviewAPIs = api.slice(api.indexOf("export const getLyricsSourceReviews"), api.indexOf("// Read-only upstream status"));
  assert.doesNotMatch(reviewAPIs, /, true\);/);
  assert.match(reviewAPIs, /filters\.limit !== undefined/);
  assert.match(review, /const \[nextCursor, setNextCursor\] = useState\(""\)/);
  assert.match(review, /const cursor = nextCursor/);
  assert.match(review, /mergeUniqueReviews/);
  assert.match(review, /"加载更多"/);
  assert.match(review, /gate: "overall"/);
  assert.match(review, /批量确认可用/);
  assert.match(review, /批量标记有问题/);
  assert.match(review, /note: ""/);
  assert.doesNotMatch(review, /lyrics-review-note|必须填写备注/);
  assert.doesNotMatch(review, /\(\["identity", "source_use", "parse"\]/);
  assert.match(pagination, /new Set\(current\.map\(\(item\) => item\.reviewId\)\)/);
  assert.match(selection, /kind === "artifact_review" && item\?\.state === "pending"/);
  assert.match(selection, /MAX_LYRICS_REVIEW_SELECTION = 100/);
  for (const forbidden of ["saveLyrics", "sourceImportToken", "publishLyrics", "unpublishLyrics", "getProjectionStatus"]) {
    assert.doesNotMatch(review, new RegExp(forbidden), `review mutation leaked into review UI: ${forbidden}`);
  }
});

test("light and dark themes retain readable text, controls, and native form color schemes", async () => {
  const css = await read("src/app/globals.css");
  for (const contract of [
    "--text-dim: #6f6c64", "--accent: #ad5032", "--accent-content: #ffffff", "--err: #ac513d",
    "--warn: #85601f", "--src-pinned: #775bba", "--src-llm: #376fa8", "--src-unknown: #6f6c64",
    "--source-tag-wash: 4%", "var(--src-human) var(--source-tag-wash)",
    ".badge.ok { background: color-mix(in srgb, var(--ok) 5%, transparent); color: var(--ok); }",
    ".badge.work { background: color-mix(in srgb, var(--accent) 5%, transparent); color: var(--accent); }",
    "var(--ok) 5%, var(--surface)", "var(--warn) 5%, var(--surface)",
    "--border-strong: #8d8980", "--control-border: #8d8980", "--text-dim: #a09d93", "--accent: #e07e58",
    "--accent-content: #1f1e1b", "--border-strong: #817d73", "--control-border: #817d73",
    'html[data-theme="dark"] { color-scheme: dark; }',
    "input::placeholder, textarea::placeholder { color: var(--text-dim); opacity: 1; }",
    "input:disabled, select:disabled, textarea:disabled { color: var(--text-secondary); -webkit-text-fill-color: var(--text-secondary); }",
    "input, select, textarea { border-color: var(--control-border); }",
    "outline: 2px solid color-mix(in srgb, var(--accent) 54%, var(--text) 46%);",
    ".btn:disabled { opacity: 0.92; cursor: not-allowed; }",
    ".btn-primary { background: var(--accent); color: var(--accent-content); }",
    ".lyric-segments input, .lyric-segments select { min-width: 0; padding: 6px 8px; border: 1px solid var(--control-border);",
    ".lyrics-catalog-list button.active span { color: var(--text-secondary); }",
  ]) assert.ok(css.includes(contract), `missing theme visibility contract: ${contract}`);
  assert.doesNotMatch(css, /--text-dim: #9a978c|--text-dim: #76736a|--text-dim: #747168|\.btn:disabled \{ opacity: 0\.[57]/);
  assert.match(css, /--font-system: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;/);
  assert.match(css, /body \{[\s\S]*font-family: var\(--font-system\);/);
  assert.match(css, /input, select, textarea, code, pre, kbd, samp \{\s*font-family: inherit;/);
  assert.doesNotMatch(css, /--font-(?:sans|serif)|["'](?:Georgia|Songti SC|Inter)["']|ui-monospace|SFMono-Regular|font-family:\s*(?:serif|monospace)/i);
});

test("dialogs, navigation, forms, and live feedback expose accessibility semantics", async () => {
  const [modal, consoleSource, providers, login, register, admin, settings] = await Promise.all([
    read("src/components/Modal.tsx"), readConsoleSurface(), read("src/app/providers.tsx"),
    read("src/components/LoginPage.tsx"), read("src/components/RegisterPage.tsx"),
    read("src/components/AdminModal.tsx"), read("src/components/SettingsModal.tsx"),
  ]);
  for (const contract of ["role=\"dialog\"", "aria-modal=\"true\"", "aria-labelledby", "previousFocusRef", 'e.key !== "Tab"', "closeDisabled"]) {
    assert.ok(modal.includes(contract), `missing modal contract: ${contract}`);
  }
  assert.match(modal, /e\.key === "Escape" && dismissible && !closeDisabled/);
  assert.match(consoleSource, /contentConflict != null[\s\S]*dismissible={false}/);
  assert.match(consoleSource, /<button type="button" key={badgeKey}/);
  assert.match(consoleSource, /tabIndex={0}/);
  assert.match(consoleSource, /event\.target !== event\.currentTarget/);
  assert.match(consoleSource, /aria-label="用户设置"/);
  assert.match(providers, /aria-live="polite"/);
  assert.match(login, /htmlFor="login-username"/);
  assert.match(register, /htmlFor="register-password"/);
  assert.match(admin, /aria-label={`\$\{u\.username\} 的角色`}/);
  assert.match(admin, /u\.username === getUsername\(\)[\s\S]*await clearSession\(\)/);
  assert.match(admin, /<label htmlFor="new-username">/);
  assert.match(admin, /<label htmlFor={inputID}>/);
  assert.match(settings, /<label htmlFor="appearance-theme">/);
  assert.match(settings, /<label htmlFor="save-shortcut">/);
});

test("lyrics editor implements reusable line and segment structure controls", async () => {
  const [editor, lineEditor] = await Promise.all([
    readLyricsEditor(), read("src/components/lyrics/LyricsLineEditor.tsx"),
  ]);
  for (const contract of ["removeLine", "moveLine", "addSegment", "splitSegment", "removeSegment", "moveSegment", "setSourcePreview(null)"]) {
    assert.ok(editor.includes(contract), `missing lyrics structure contract: ${contract}`);
  }
  assert.match(editor, /draft-published/);
  assert.match(editor, /取消发布 revision/);
  assert.match(editor, /<LyricsLineEditor/);
  assert.ok(lineEditor.includes('aria-label={`第 ${lineNumber} 行分段'));
  assert.ok(lineEditor.includes('aria-label={`第 ${lineNumber} 行日文原文`}'));
  assert.ok(lineEditor.includes('data-segment-index={segmentIndex}'));
  assert.match(lineEditor, /function LyricRubySpanEditor/);
  assert.match(lineEditor, /function LyricSegmentEditor/);
  assert.match(editor, /role="tab"[\s\S]*tabIndex={previewLocale === locale \? 0 : -1}[\s\S]*aria-controls="lyrics-preview-panel"/);
  assert.match(editor, /role="tabpanel"[\s\S]*aria-labelledby={`lyrics-preview-tab-\$\{previewLocale\}`}/);
  assert.match(editor, /正在保存或提交歌词，请等待服务器确认/);
});

test("web install and release inputs are immutable and verifiable", async () => {
  const [pkg, lock, dockerfile] = await Promise.all([
    read("package.json"), read("package-lock.json"), read("../Dockerfile"),
  ]);
  const manifest = JSON.parse(pkg);
  for (const dependency of [...Object.values(manifest.dependencies), ...Object.values(manifest.devDependencies)]) {
    assert.match(dependency, /^\d+\.\d+\.\d+$/, `dependency is not exact: ${dependency}`);
  }
  assert.equal(lock.includes("registry.npmmirror.com"), false);
  assert.equal(manifest.scripts.lint, "eslint src --max-warnings=0");
  assert.match(dockerfile, /npm ci --ignore-scripts/);
  assert.match(dockerfile, /ARG NODE_IMAGE_DIGEST/);
  assert.match(dockerfile, /FROM \$\{NODE_IMAGE\}@sha256:\$\{NODE_IMAGE_DIGEST\}/);
  assert.match(dockerfile, /USER 65532:65532/);
  assert.match(dockerfile, /chown -R 0:0 \/app/);
  assert.match(dockerfile, /chmod -R a-w \/app/);
});
