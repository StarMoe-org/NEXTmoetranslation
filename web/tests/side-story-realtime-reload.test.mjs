import assert from "node:assert/strict";
import test from "node:test";

import { loadSourceModule } from "./source-module-harness.mjs";

// All lines and titles below are synthetic test data.

// Re-renderable stand-in for React hooks: slots persist across render() calls in call
// order, and effects run after each render whose deps changed.
function createRenderer() {
  const slots = [];
  let cursor = 0;
  let queue = [];
  const changed = (previous, deps) => !previous || !deps || deps.some((dep, index) => !Object.is(dep, previous.deps[index]));
  const react = {
    useState(initial) {
      const slot = (slots[cursor++] ??= { value: typeof initial === "function" ? initial() : initial });
      return [slot.value, (next) => { slot.value = typeof next === "function" ? next(slot.value) : next; }];
    },
    useRef: (current) => (slots[cursor++] ??= { current }),
    useCallback(callback, deps) {
      const index = cursor++;
      if (changed(slots[index], deps)) slots[index] = { value: callback, deps };
      return slots[index].value;
    },
    useEffect(effect, deps) {
      const index = cursor++;
      const previous = slots[index];
      if (!changed(previous, deps)) return;
      slots[index] = { deps };
      queue.push(() => { previous?.cleanup?.(); slots[index].cleanup = effect(); });
    },
  };
  return {
    react,
    render(hook, props) {
      cursor = 0;
      const output = hook(props);
      const effects = queue;
      queue = [];
      effects.forEach((run) => run());
      return output;
    },
  };
}

function summary(overrides = {}) {
  return {
    kind: "card", id: "101", title: "测试卡面", characterId: 1, areaId: 0, areaCategory: "", actionSetId: 0,
    releasedAt: 0, episodeCount: 2, fetchedEpisodeCount: 1, lineCount: 10, translatedCount: 2, untranslatedCount: 8,
    sourceCounts: { official: 0, llm: 2, human: 0 }, primarySource: "llm", status: "partial", updatedAt: 100,
    ...overrides,
  };
}

const line = (index, text, revision = 1) => ({
  key: `1|テスト台詞${index}`, text, japanese: `テスト台詞${index}`, episodeNo: "1", revision,
});

function reloadHarness(t) {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const renderer = createRenderer();
  let handler = null;
  const calls = { loadEntries: 0, refresh: [], remoteConflict: [], toasts: [], selectedKey: [], editValue: [] };
  const api = {
    acceptLoadedProducerState: () => true, clearLoadedProducerState() {}, getEditorGateStatus: async () => ({}),
    subscribeProducerProofInvalidated: () => () => {},
  };
  const { useConsoleRealtime } = loadSourceModule("components/console/useConsoleRealtime.ts", {
    react: renderer.react,
    "@/lib/api": api,
    "@/lib/sse": { useSSE: (next) => { handler = next; } },
    "@/lib/lyrics-collaboration.mjs": {
      lyricsUpdateMatchesEditorTarget: () => false, lyricsUpdateTargetLabel: () => "", normalizeLyricsUpdateEvent: () => null,
    },
  });
  const stable = {
    username: "tester", clientID: "tab-a", locale: "zh-CN", show: (message, tone) => calls.toasts.push([tone, message]),
    category: "cardStory", field: "101", isEventStory: false, sideStoryKind: "card", isLyrics: false, isLyricsSourceReview: false,
    setEntries() {}, setSelectedKey: (key) => calls.selectedKey.push(key), setEditValue: (value) => calls.editValue.push(value),
    eventTxtDraft: null, setEventTxtDraft() {}, eventTxtDraftDirty: false,
    lyricsDirty: false, lyricsEditorRef: { current: null }, lyricsSourceReviewRef: { current: null },
    setRemoteConflict: (next) => calls.remoteConflict.push(next), contextGenerationRef: { current: 0 }, invalidatePendingAction() {},
    loadEntries: async () => { calls.loadEntries++; return true; }, reloadSidebar: async () => true,
    refreshSideStoryLists: (kind, onRefreshed) => calls.refresh.push([kind, onRefreshed]),
  };
  const render = (entries, selectedKey, editValue) => {
    const selectedEntry = entries.find((entry) => entry.key === selectedKey) ?? null;
    renderer.render(useConsoleRealtime, {
      ...stable, entries, selectedKey, selectedEntry, editValue,
      entryDirty: selectedEntry != null && editValue !== selectedEntry.text,
    });
  };
  const sync = (after) => {
    handler("sidestory.sync", {});
    calls.refresh.at(-1)[1]("card", [summary()], [after]);
  };
  return { calls, render, sync };
}

test("a clean sync reload selects the same line and its reloaded text", (t) => {
  const { calls, render, sync } = reloadHarness(t);
  const loaded = [line(1, "测试译文一"), line(2, "测试译文二")];
  render(loaded, loaded[1].key, loaded[1].text);
  sync(summary({ fetchedEpisodeCount: 2 }));
  assert.equal(calls.loadEntries, 1);

  render([], null, "");
  assert.deepEqual(calls.selectedKey, [], "the line waits for the reloaded story");
  const reloaded = [line(1, "测试译文一"), line(2, "测试官方译文", 2), line(3, "测试译文三")];
  render(reloaded, reloaded[0].key, reloaded[0].text);
  assert.deepEqual(calls.selectedKey, [loaded[1].key]);
  assert.deepEqual(calls.editValue, ["测试官方译文"]);
  render([...reloaded], reloaded[1].key, reloaded[1].text);
  assert.deepEqual(calls.selectedKey, [loaded[1].key], "the line is reselected once");
});

for (const [resolution, resolve] of [
  ["saved", (loaded) => [[loaded[0], line(2, "测试草稿", 2)], "测试草稿"]],
  ["discarded", (loaded) => [loaded, loaded[1].text]],
]) {
  test(`a sync over a draft reloads once the draft is ${resolution}, keeping the line`, (t) => {
    const { calls, render, sync } = reloadHarness(t);
    const loaded = [line(1, "测试译文一"), line(2, "测试译文二")];
    render(loaded, loaded[1].key, "测试草稿");
    sync(summary({ fetchedEpisodeCount: 2 }));
    assert.equal(calls.loadEntries, 0, "the draft is not reloaded away");
    assert.deepEqual(calls.remoteConflict, [{ key: loaded[1].key, user: "后台回填" }]);
    assert.match(calls.toasts.at(-1)[1], /保存或放弃本地草稿后将自动重新载入/);
    render(loaded, loaded[1].key, "测试草稿二");
    assert.equal(calls.loadEntries, 0, "still dirty");

    const [entries, editValue] = resolve(loaded);
    render(entries, loaded[1].key, editValue);
    assert.equal(calls.loadEntries, 1, "the reload follows the draft");

    render([], null, "");
    calls.selectedKey.length = 0;
    calls.editValue.length = 0;
    const reloaded = [line(1, "测试译文一"), line(2, "测试官方译文", 3)];
    render(reloaded, reloaded[0].key, reloaded[0].text);
    assert.deepEqual(calls.selectedKey, [loaded[1].key]);
    assert.deepEqual(calls.editValue, ["测试官方译文"]);
    render(reloaded, loaded[1].key, "测试官方译文");
    assert.equal(calls.loadEntries, 1, "one reload per sync");
  });
}

test("another load of the story takes over a sync reload that waited for a draft", (t) => {
  const { calls, render, sync } = reloadHarness(t);
  const loaded = [line(1, "测试译文一"), line(2, "测试译文二")];
  render(loaded, loaded[1].key, "测试草稿");
  sync(summary({ fetchedEpisodeCount: 2 }));
  assert.equal(calls.loadEntries, 0, "the draft is not reloaded away");

  // A reconcile or a collaborator's refresh reloads the story and clears the selection first.
  render([], null, "");
  assert.equal(calls.loadEntries, 0, "no second load races the one under way");
  const reloaded = [line(1, "测试译文一"), line(2, "测试官方译文", 2)];
  render(reloaded, reloaded[0].key, reloaded[0].text);
  render(reloaded, reloaded[1].key, reloaded[1].text);
  assert.equal(calls.loadEntries, 0, "the pending sync reload was dropped");
});
