import assert from "node:assert/strict";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";
import { read } from "./source-surfaces.mjs";

// All titles below are synthetic test data.
function summary(overrides = {}) {
  return {
    kind: "card", id: "101", title: "测试卡面", characterId: 1, areaId: 0, areaCategory: "", actionSetId: 0,
    releasedAt: 0, episodeCount: 2, fetchedEpisodeCount: 1, lineCount: 10, translatedCount: 2, untranslatedCount: 8,
    sourceCounts: { official: 0, llm: 2, human: 0 }, primarySource: "llm", status: "partial", updatedAt: 100,
    ...overrides,
  };
}

test("a sync reloads the open story only for what a backfill alone changes, and keeps a draft behind a notice", () => {
  const reloads = (after, { before = summary(), entryDirty = false } = {}) => {
    const harness = realtimeHarness({ entryDirty });
    harness.emit("sidestory.sync");
    harness.calls.refresh[0][1]("card", [before], [after]);
    return [harness.calls.loadEntries, harness.calls.remoteConflict.length];
  };
  assert.deepEqual(reloads(summary({ fetchedEpisodeCount: 2, lineCount: 30, untranslatedCount: 28 })), [1, 0], "the script was fetched");
  assert.deepEqual(reloads(summary({ episodeCount: 3 })), [1, 0]);
  assert.deepEqual(reloads(summary({ lineCount: 12 })), [1, 0]);
  assert.deepEqual(reloads(summary({ title: "测试卡面二" })), [1, 0]);
  assert.deepEqual(reloads(summary({ sourceCounts: { official: 2, llm: 0, human: 0 }, primarySource: "official" })), [1, 0],
    "official lines replaced AI lines");
  assert.deepEqual(reloads(summary({ fetchedEpisodeCount: 2 }), { entryDirty: true }), [0, 1], "an unsaved draft is never reloaded away");
  assert.deepEqual(reloads(summary()), [0, 0]);
  assert.deepEqual(reloads(summary({
    translatedCount: 3, untranslatedCount: 7, sourceCounts: { official: 0, llm: 2, human: 1 }, updatedAt: 150,
  })), [0, 0], "a save of an untranslated line");
  assert.deepEqual(reloads(summary({
    sourceCounts: { official: 0, llm: 1, human: 1 }, primarySource: "human", status: "translated", updatedAt: 150,
  })), [0, 0], "a save over an AI line");
  assert.deepEqual(reloads(summary({ sourceCounts: { official: 0, llm: 0, human: 1 }, updatedAt: 150 }), {
    before: summary({ sourceCounts: { official: 1, llm: 0, human: 0 } }),
  }), [0, 0], "a save over an official line");
  assert.deepEqual(reloads(summary({ fetchedEpisodeCount: 2 }), { before: summary({ id: "102" }) }), [0, 0],
    "the list had not shown the story yet");
});

test("a list refresh reports the stories it replaced to the sync callback", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const runtime = createHookRuntime();
  const lists = [[summary()], [summary({ updatedAt: 200 })]];
  const api = {
    getSideStories: async () => ({ stories: lists.shift() ?? [] }),
    getSideStorySyncStatus: async () => ({ state: {}, totals: {} }),
    triggerSideStorySync: async () => ({ started: true, state: {} }),
  };
  const { useSideStoryCatalog } = loadSourceModule("components/console/useSideStoryCatalog.ts", { react: runtime.react, "@/lib/api": api });
  const catalog = useSideStoryCatalog({ locale: "zh-CN", show() {} });
  const seen = [];
  const onRefreshed = (kind, before, after) => seen.push([kind, before?.map((story) => story.updatedAt) ?? null, after.map((story) => story.updatedAt)]);
  const flush = () => new Promise((resolve) => setImmediate(resolve));

  catalog.ensureList("card");
  await flush();
  catalog.refreshLists(undefined, onRefreshed);
  catalog.refreshLists("card", onRefreshed);
  t.mock.timers.tick(1500);
  await flush();
  assert.deepEqual(seen, [["card", [100], [200]]], "one debounced load, one call per callback, only for loaded kinds");

  catalog.refreshLists();
  t.mock.timers.tick(1500);
  await flush();
  assert.equal(seen.length, 1, "a refresh without the callback does not report");
});

function realtimeHarness({ entryDirty = false, refreshSideStoryLists } = {}) {
  const runtime = createHookRuntime();
  let handler = null;
  const calls = { loadEntries: 0, refresh: [], remoteConflict: [], toasts: [] };
  const api = {
    acceptLoadedProducerState: () => true, clearLoadedProducerState() {}, getEditorGateStatus: async () => ({}),
    subscribeProducerProofInvalidated: () => () => {},
  };
  const { useConsoleRealtime } = loadSourceModule("components/console/useConsoleRealtime.ts", {
    react: runtime.react,
    "@/lib/api": api,
    "@/lib/sse": { useSSE: (next) => { handler = next; } },
    "@/lib/lyrics-collaboration.mjs": {
      lyricsUpdateMatchesEditorTarget: () => false, lyricsUpdateTargetLabel: () => "", normalizeLyricsUpdateEvent: () => null,
    },
  });
  const entry = { key: "1|テスト台詞", text: "测试译文", japanese: "テスト台詞", episodeNo: "1", revision: 1 };
  useConsoleRealtime({
    username: "tester", clientID: "tab-a", locale: "zh-CN", show: (message, tone) => calls.toasts.push([tone, message]),
    category: "cardStory", field: "101", isEventStory: false, sideStoryKind: "card", isLyrics: false, isLyricsSourceReview: false,
    entries: [entry], setEntries() {}, selectedKey: entry.key, setSelectedKey() {}, selectedEntry: entry, entryDirty,
    editValue: entryDirty ? "测试草稿" : entry.text, setEditValue() {}, eventTxtDraft: null, setEventTxtDraft() {}, eventTxtDraftDirty: false,
    lyricsDirty: false, lyricsEditorRef: { current: null }, lyricsSourceReviewRef: { current: null },
    setRemoteConflict: (next) => calls.remoteConflict.push(next), contextGenerationRef: { current: 0 }, invalidatePendingAction() {},
    loadEntries: async () => { calls.loadEntries++; return true; }, reloadSidebar: async () => true,
    refreshSideStoryLists: refreshSideStoryLists ?? ((kind, onRefreshed) => calls.refresh.push([kind, onRefreshed])),
  });
  return { calls, emit: (event, data = {}) => handler(event, data) };
}

test("sidestory.sync reloads a changed open story, or shows the notice over a draft", () => {
  const clean = realtimeHarness();
  clean.emit("sidestory.sync");
  assert.equal(clean.calls.refresh.length, 1);
  const [kind, onRefreshed] = clean.calls.refresh[0];
  assert.equal(kind, undefined, "every loaded kind refreshes");
  onRefreshed("area", [summary({ kind: "area" })], [summary({ kind: "area", fetchedEpisodeCount: 2 })]);
  onRefreshed("card", [summary({ id: "102" })], [summary({ id: "102", fetchedEpisodeCount: 2 })]);
  onRefreshed("card", [summary()], [summary()]);
  assert.equal(clean.calls.loadEntries, 0, "another kind, another story or an unchanged summary is ignored");
  onRefreshed("card", [summary()], [summary({ fetchedEpisodeCount: 2 })]);
  assert.equal(clean.calls.loadEntries, 1);
  assert.deepEqual(clean.calls.remoteConflict, []);

  const dirty = realtimeHarness({ entryDirty: true });
  dirty.emit("sidestory.sync");
  dirty.calls.refresh[0][1]("card", [summary()], [summary({ fetchedEpisodeCount: 2 })]);
  assert.equal(dirty.calls.loadEntries, 0, "the draft is not reloaded away");
  assert.deepEqual(dirty.calls.remoteConflict, [{ key: "1|テスト台詞", user: "后台回填" }]);
  assert.match(dirty.calls.toasts.at(-1)[1], /保存或放弃本地草稿后将自动重新载入/);
});

test("a save merged into the sync's list load is not taken for a backfill change", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const saved = summary({ translatedCount: 3, untranslatedCount: 7, sourceCounts: { official: 0, llm: 2, human: 1 }, updatedAt: 150 });
  for (const entryDirty of [false, true]) {
    const lists = [[summary()], [saved]];
    const api = {
      getSideStories: async () => ({ stories: lists.shift() ?? [] }),
      getSideStorySyncStatus: async () => ({ state: {}, totals: {} }),
      triggerSideStorySync: async () => ({ started: true, state: {} }),
    };
    const { useSideStoryCatalog } = loadSourceModule("components/console/useSideStoryCatalog.ts", {
      react: createHookRuntime().react, "@/lib/api": api,
    });
    const catalog = useSideStoryCatalog({ locale: "zh-CN", show() {} });
    const harness = realtimeHarness({ entryDirty, refreshSideStoryLists: catalog.refreshLists });
    const flush = () => new Promise((resolve) => setImmediate(resolve));

    catalog.ensureList("card");
    await flush();
    catalog.refreshLists("card");
    t.mock.timers.tick(700);
    harness.emit("sidestory.sync");
    t.mock.timers.tick(1500);
    await flush();
    assert.equal(lists.length, 0, "the save and the sync shared one list load");
    assert.equal(harness.calls.loadEntries, 0);
    assert.deepEqual(harness.calls.remoteConflict, []);
    assert.deepEqual(harness.calls.toasts, []);
  }
});

test("the console hands setSelectedKey to the realtime hook so a sync reload keeps the line", async () => {
  const source = await read("src/components/Console.tsx");
  const start = source.indexOf("useConsoleRealtime({");
  const call = source.slice(start, source.indexOf("});", start));
  assert.match(call, /\bsetSelectedKey\b/);
});
