import assert from "node:assert/strict";
import test from "node:test";
import ts from "typescript";

import { read, readConsoleSurface } from "./source-surfaces.mjs";

const helperSource = await read("src/lib/event-story-console.ts");
const helperCompiled = ts.transpileModule(helperSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText;
const helper = await import(`data:text/javascript;base64,${Buffer.from(helperCompiled).toString("base64")}`);

test("event story chapter selection preserves all and supports legacy encoded episode keys", () => {
  const episodes = helper.listEventStoryEpisodeNos([
    { key: "segment-2", episodeNo: "2" },
    { key: "10|__title__|终章" },
    { key: "3|台词" },
    { key: "segment-2b", episodeNo: "2" },
  ]);

  assert.deepEqual(episodes, ["2", "3", "10"]);
  assert.equal(helper.resolveSelectedEventStoryEpisode("all", episodes), "all");
  assert.equal(helper.resolveSelectedEventStoryEpisode("3", episodes), "3");
  assert.equal(helper.resolveSelectedEventStoryEpisode("missing", episodes), "2");
  assert.equal(helper.resolveSelectedEventStoryEpisode("1", []), "all");
});

test("event story draft discard restores authoritative rows and legacy fallbacks remain read-only", () => {
  const entries = [
    { key: "segment-1", segmentId: "segment-1", sourceHash: "hash-1", text: "本地草稿" },
    { key: "segment-2", segmentId: "segment-2", sourceHash: "hash-2", text: "已保存内容" },
    { key: "3|旧台词", text: "旧兼容内容" },
  ];
  const restored = helper.restoreEventStoryDraftEntries(entries, [
    { segmentId: "segment-1", authoritativeText: "服务器内容" },
  ]);

  assert.equal(restored[0].text, "服务器内容");
  assert.equal(restored[1], entries[1]);
  assert.equal(restored[2], entries[2]);
  assert.equal(helper.eventStoryEntryHasCanonicalIdentity(entries[0]), true);
  assert.equal(helper.eventStoryEntryHasCanonicalIdentity(entries[2]), false);
});

test("event story SSE target lookup handles canonical segments and legacy title/talk keys", () => {
  const entries = [
    { key: "segment-title", segmentId: "segment-title", episodeNo: "1", entryType: "title", japanese: "第一话" },
    { key: "segment-talk", segmentId: "segment-talk", episodeNo: "1", entryType: "talk", japanese: "こんにちは" },
    { key: "2|__title__|第二话" },
    { key: "2|さようなら" },
  ];

  assert.equal(helper.findEventStoryUpdateTarget(entries, {
    segmentId: "segment-talk", episodeNo: "1", jpKey: "こんにちは", entryType: "talk",
  })?.key, "segment-talk");
  assert.equal(helper.findEventStoryUpdateTarget(entries, {
    segmentId: "", episodeNo: "2", jpKey: "", entryType: "title",
  })?.key, "2|__title__|第二话");
  assert.equal(helper.findEventStoryUpdateTarget(entries, {
    segmentId: "", episodeNo: "2", jpKey: "さようなら", entryType: "talk",
  })?.key, "2|さようなら");
  assert.equal(helper.findEventStoryUpdateTarget(entries, {
    segmentId: "missing", episodeNo: "9", jpKey: "missing", entryType: "talk",
  }), undefined);
  assert.equal(helper.findEventStoryUpdateTarget(entries, {
    segmentId: "stale-segment", episodeNo: "1", jpKey: "こんにちは", entryType: "talk",
  }), undefined, "a stale canonical segment must not fall back to a weaker Japanese-text match");
});

test("structural event story updates reconcile every locale while localized updates stay scoped", () => {
  assert.equal(helper.eventStoryUpdateAffectsLocale("en-US", "", "retry"), true);
  assert.equal(helper.eventStoryUpdateAffectsLocale("ja-JP", "", "reorder"), true);
  assert.equal(helper.eventStoryUpdateAffectsLocale("en-US", "", "ai-translate"), false);
  assert.equal(helper.eventStoryUpdateAffectsLocale("zh-CN", "", "ai-translate"), true);
  assert.equal(helper.eventStoryUpdateAffectsLocale("en-US", "en-US", ""), true);
  assert.equal(helper.eventStoryUpdateAffectsLocale("zh-CN", "en-US", ""), false);
});

test("chapter changes are local filters and use a vertical selector instead of a horizontal strip", async () => {
  const [consoleSource, entriesHook, css] = await Promise.all([
    readConsoleSurface(),
    read("src/components/console/useConsoleEntries.ts"),
    read("src/app/globals.css"),
  ]);
  const loadEntriesBlock = entriesHook.slice(
    entriesHook.indexOf("const loadEntries = useCallback"),
    entriesHook.indexOf("useEffect(() => { void loadEntries();"),
  );

  assert.match(loadEntriesBlock, /listEventStoryEpisodeNos\(visible\)/);
  assert.match(loadEntriesBlock, /resolveSelectedEventStoryEpisode\(selectedEpisodeRef\.current, availableEpisodes\)/);
  assert.doesNotMatch(loadEntriesBlock, /\[category,[^\]]*\bselectedEpisode\b/);
  assert.match(consoleSource, /<select[\s\S]*aria-label="选择活动剧情章节"[\s\S]*value=\{selectedEpisode\}/);
  assert.match(consoleSource, /<option value="all">全部章节/);
  assert.doesNotMatch(consoleSource, /className="chapter-nav"|className=\{`chapter-tab/);
  assert.match(css, /\.chapter-selector \{/);
  const chapterCSS = css.slice(css.indexOf("/* ---- Chapter navigation ---- */"), css.indexOf("/* ---- Moesekai detail link ---- */"));
  assert.doesNotMatch(chapterCSS, /overflow-x|chapter-tab/);
});

test("memoized entry rows receive stable current-state selection callbacks", async () => {
  const [consoleSource, entryRow] = await Promise.all([
    readConsoleSurface(), read("src/components/console/EntryRow.tsx"),
  ]);
  const entryRowBlock = entryRow.slice(entryRow.indexOf("const EntryRow = React.memo"));

  assert.match(consoleSource, /const selectionStateRef = useRef\(\{ selectedKey, entryDirty, eventTxtDraftDirty \}\)/);
  assert.match(consoleSource, /const runOrGuardRef = useRef\(runOrGuard\)/);
  assert.match(consoleSource, /const selectEntry = useCallback\(\(entry: TranslationEntry\) => \{/);
  assert.match(consoleSource, /selectionStateRef\.current/);
  assert.match(consoleSource, /runOrGuardRef\.current\("切换条目", action\)/);
  assert.doesNotMatch(entryRowBlock, /\}, \(prev, next\) =>/);
});

test("event story collaboration freezes conflicting TXT drafts and reloads authoritative revisions", async () => {
  const [consoleSource, producerOperations, drafts] = await Promise.all([
    readConsoleSurface(), read("src/components/console/ProducerOperationsShell.tsx"),
    read("src/components/console/console-drafts.ts"),
  ]);
  const promoteBlock = producerOperations.slice(
    producerOperations.indexOf("const promoteStory ="),
    producerOperations.indexOf("const retryStory ="),
  );
  const recoveryBlock = drafts.slice(
    drafts.indexOf("function recoverEventTxtDraft"),
    drafts.indexOf("function overlayEventTxtDraft"),
  );

  assert.match(consoleSource, /findEventStoryUpdateTarget\(entries, update\)/);
  assert.doesNotMatch(consoleSource, /let targetKey: string \| null = null/);
  assert.match(consoleSource, /eventTxtDraft\?\.translations\.some/);
  assert.match(consoleSource, /TXT 本地草稿中的同一行；草稿已冻结，禁止静默覆盖/);
  assert.match(consoleSource, /d\.promote === "human" \|\| isBulkAction/);
  assert.match(consoleSource, /reconcileContent\("remote", preservedDraft/);
  assert.match(consoleSource, /await Promise\.all\(\[loadEntries\(\), reloadSidebar\(\)\]\)/);
  assert.match(consoleSource, /权威 revision 未完整重新载入/);
  assert.match(promoteBlock, /\}, true\);\s*$/);
  assert.match(recoveryBlock, /const stale = draft\.translations\.some/);
  assert.match(recoveryBlock, /原 TXT 本地草稿已冻结，未覆盖也未删除/);
  assert.match(recoveryBlock, /persistContentConflict\(username, conflict\)/);
  assert.match(consoleSource, /clearPersistedEventTxtDraftFromConflict\(username, conflict\)/);
  assert.match(consoleSource, /const restoredEntries = restoreEventStoryDraftEntries\(entriesRef\.current, eventTxtDraft\.translations\)/);
  assert.match(consoleSource, /!update\.segmentId \|\| !eventStoryEntryHasCanonicalIdentity\(targetEntry\) \|\| nextRevision === undefined/);
  assert.match(consoleSource, /事件未携带可继续编辑的权威 revision/);
  assert.match(consoleSource, /剧情保存结果无法确认；本地草稿已冻结/);
  assert.match(consoleSource, /剧情来源修改结果无法确认；修改意图已冻结/);
  assert.match(consoleSource, /eventStoryMutationResultIsAmbiguous\(err\)/);
  assert.match(consoleSource, /entriesRef\.current = restoredEntries/);
  assert.match(consoleSource, /entriesRef\.current\.find\(\(candidate\) => candidate\.key === entry\.key\)/);
  assert.match(consoleSource, /sidebarReloadGenerationRef\.current/);
  assert.match(consoleSource, /sidebarReloadRef\.current\?\.generation !== result\.generation/);
  assert.doesNotMatch(consoleSource, /getEventStories\(locale\)\.then\(setEventStories\)\.catch\(\(\) => \{ loaded = false; setEventStories\(\[\]\); \}\)/);
  assert.match(consoleSource, /selectedEventStoryIdentityMissing/);
  assert.match(consoleSource, /当前剧情行缺少权威来源身份，已保持只读/);
  assert.match(consoleSource, /void reloadSidebar\(\)/);
  assert.match(consoleSource, /const currentEntry = entriesRef\.current\.find\(\(e\) => e\.key === saveKey\)/);
  assert.ok(consoleSource.includes("currentEntry && typeof currentEntry.revision === \"number\" && currentEntry.revision > (saveEntry?.revision ?? -1)"));
  assert.match(consoleSource, /保存期间协作者提交了同一行的更高 revision/);
  assert.match(consoleSource, /const saveStartRevision = entry\.revision/);
  assert.ok(consoleSource.includes("isEventStory && currentEntry && typeof currentEntry.revision === \"number\" && typeof saveStartRevision === \"number\" && currentEntry.revision > saveStartRevision"));
  assert.match(consoleSource, /来源修改期间协作者提交了同一行的更高 revision/);
  assert.match(consoleSource, /sidebarReloadRef\.current = null/);
  assert.match(consoleSource, /result\.locale !== locale/);
  assert.match(consoleSource, /sidebarSnapshotRef\.current\.set\(result\.locale/);
  assert.match(consoleSource, /const snapshot = sidebarSnapshotRef\.current\.get\(result\.locale\)/);
  assert.match(consoleSource, /disabled=\{saving\}>采用远端版本/);
  assert.match(consoleSource, /disabled=\{saving\}>[\s\S]*保留本地并允许覆盖/);
});

test("event story toolbar stacks its selector and actions safely on mobile", async () => {
  const css = await read("src/app/globals.css");

  assert.match(css, /\.story-toolbar-actions \{[^}]*min-width: 0;[^}]*flex: 1 1 auto;/);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*\.chapter-selector \{ grid-template-columns: 1fr; flex-basis: 100%;/);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*\.story-toolbar-actions \{ width: 100%; flex-basis: 100%;/);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*\.app \{ position: relative; min-height: 100dvh; overflow: visible; \}/);
});
