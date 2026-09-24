import assert from "node:assert/strict";
import test from "node:test";

import { loadSourceModule } from "./source-module-harness.mjs";

// All lines and titles below are synthetic test data.
const { sideStoryLineSaveIsNoop } = loadSourceModule("lib/side-story-editor.ts");

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
    render(component, props) {
      cursor = 0;
      const output = component(props);
      const effects = queue;
      queue = [];
      effects.forEach((run) => run());
      return output;
    },
  };
}

const flush = () => new Promise((resolve) => setImmediate(resolve));

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

class TestAPIError extends Error {
  constructor(status, body) {
    super(body.error);
    this.status = status;
    this.code = body.error;
    this.current = body.current;
  }
}

const untranslated = { key: "1|テスト台詞一", episodeNo: "1", japanese: "テスト台詞一", text: "", source: "", revision: 0 };
const translated = { key: "1|テスト台詞二", episodeNo: "1", japanese: "テスト台詞二", text: "测试旧译", source: "human", revision: 2 };
const conflictOn = (entry) => new TestAPIError(409, {
  error: "revision_conflict",
  current: { conflicts: [{ jp: entry.japanese, expectedRevision: entry.revision, currentRevision: 7, currentText: "测试服务器译文", currentSource: "official" }] },
});

test("only an unchanged or blank value on a never-stored line skips the side-story PUT", () => {
  assert.equal(sideStoryLineSaveIsNoop(untranslated, ""), true);
  assert.equal(sideStoryLineSaveIsNoop(untranslated, "  \n"), true);
  assert.equal(sideStoryLineSaveIsNoop({ text: "", revision: undefined }, ""), true);
  assert.equal(sideStoryLineSaveIsNoop(untranslated, "测试草稿"), false);
  assert.equal(sideStoryLineSaveIsNoop(translated, "测试旧译"), false, "a stored line may be re-saved as human");
  assert.equal(sideStoryLineSaveIsNoop({ text: "", revision: 3 }, ""), false, "clearing a stored line is a real edit");
});

function editorHarness(respond) {
  const renderer = createRenderer();
  const calls = [];
  const api = {
    APIError: TestAPIError,
    updateSideStoryLines: (...args) => { calls.push(args); return respond(...args); },
    updateEntry: async () => { throw new Error("generic save must not run for a side story"); },
    updateEventStoryLine: async () => { throw new Error("event save must not run for a side story"); },
  };
  const { useEntryEditor } = loadSourceModule("components/console/useEntryEditor.ts", { react: renderer.react, "@/lib/api": api });
  const state = { selectedKey: null, editValue: "", remoteConflict: null, saving: [] };
  const entriesRef = { current: [untranslated, translated] };
  const savingRef = { current: false };
  const render = () => {
    const selectedEntry = entriesRef.current.find((entry) => entry.key === state.selectedKey) ?? null;
    return renderer.render(useEntryEditor, {
      username: "tester", show() {}, category: "cardStory", field: "101", locale: "zh-CN", isEventStory: false,
      sideStoryKind: "card", isReadOnly: false, entries: entriesRef.current, entriesRef,
      setEntries: (next) => { entriesRef.current = typeof next === "function" ? next(entriesRef.current) : next; },
      filtered: entriesRef.current, selectedKey: state.selectedKey, setSelectedKey: (key) => { state.selectedKey = key; },
      selectedEntry, selectedEpisode: "all", editValue: state.editValue, setEditValue: (value) => { state.editValue = value; },
      entryDirty: selectedEntry != null && state.editValue !== selectedEntry.text,
      eventTxtDraft: null, setEventTxtDraft() {}, eventTxtDraftDirty: false, keepTranslationEntryVisible() {},
      reloadSidebar: async () => true, reconcileContentRef: { current: async () => true },
      writeFenceRef: { current: false }, savingRef, setSaving: (saving) => { state.saving.push(saving); },
      remoteConflictRef: { current: null }, setRemoteConflict: (next) => { state.remoteConflict = next; },
      contextGenerationRef: { current: 1 }, onSideStorySaved() {},
    });
  };
  return { render, calls, state, savingRef, entries: () => entriesRef.current };
}

test("save-and-next on an untranslated line advances without storing an empty human line", async () => {
  const harness = editorHarness(() => { throw new Error("no PUT expected"); });
  harness.state.selectedKey = untranslated.key;
  harness.state.editValue = " ";
  assert.equal(await harness.render().save(), true);
  assert.deepEqual(harness.calls, []);
  assert.equal(harness.state.selectedKey, translated.key);
  assert.equal(harness.state.editValue, "测试旧译");
  assert.equal(harness.savingRef.current, false);
  assert.deepEqual(harness.state.saving, [true, false]);
});

test("a source-change conflict raises the server banner for the line selected when it arrives", async () => {
  const response = deferred();
  const harness = editorHarness(() => response.promise);
  harness.state.selectedKey = untranslated.key;
  const pending = harness.render().handleSourceChange(translated.key, "llm");
  harness.state.selectedKey = translated.key;
  harness.state.editValue = translated.text;
  harness.render();
  response.reject(conflictOn(translated));
  await pending;
  assert.equal(harness.entries()[1].text, "测试服务器译文");
  assert.deepEqual(harness.state.remoteConflict, { key: translated.key, user: "服务器", current: { text: "测试服务器译文", revision: 7 } });
});

test("a TXT batch conflict on the selected line raises the server banner", async () => {
  const harness = editorHarness(async () => { throw conflictOn(translated); });
  harness.state.selectedKey = translated.key;
  harness.state.editValue = translated.text;
  const outcome = await harness.render().saveSideStoryBatch("1", [{ jp: translated.japanese, text: "测试导入", source: "human", expectedRevision: 2 }]);
  assert.equal(outcome.status, "conflict");
  assert.deepEqual(harness.state.remoteConflict, { key: translated.key, user: "服务器", current: { text: "测试服务器译文", revision: 7 } });
});

test("a debounced list refresh scheduled before a locale switch loads the new locale", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const renderer = createRenderer();
  const calls = [];
  const api = {
    getSideStories: async (kind, locale) => { calls.push([kind, locale]); return { stories: [{ id: `${locale}-story` }] }; },
    getSideStorySyncStatus: async () => ({}),
    triggerSideStorySync: async () => ({ started: false, state: "idle" }),
  };
  const { useSideStoryCatalog } = loadSourceModule("components/console/useSideStoryCatalog.ts", { react: renderer.react, "@/lib/api": api });
  const render = (locale) => renderer.render(useSideStoryCatalog, { locale, show() {} });
  const zh = render("zh-CN");
  zh.ensureList("card");
  await flush();
  zh.refreshLists("card");
  render("en-US");
  await flush();
  t.mock.timers.tick(1500);
  await flush();
  zh.refreshLists("card");
  t.mock.timers.tick(1500);
  await flush();
  assert.deepEqual(calls, [["card", "zh-CN"], ["card", "en-US"], ["card", "en-US"], ["card", "en-US"]]);
  assert.deepEqual(render("en-US").sideStoryLists.card.stories, [{ id: "en-US-story" }]);
});

function findElements(node, predicate, found = []) {
  if (Array.isArray(node)) node.forEach((child) => findElements(child, predicate, found));
  else if (node && typeof node === "object" && node.props) {
    if (predicate(node)) found.push(node);
    findElements(node.props.children, predicate, found);
  }
  return found;
}

test("switching story while the TXT snapshot loads re-enables the import button", async () => {
  const renderer = createRenderer();
  const snapshot = deferred();
  const { SideStoryTxtImport } = loadSourceModule("components/SideStoryTxtImport.tsx", {
    react: renderer.react,
    "@/app/providers": { useToast: () => ({ show() {} }) },
    "@/components/Modal": { Modal: () => null },
    "@/components/EventStoryTxtImport": { TxtImportPreviewTable: () => null, readUTF8File: async () => "" },
    "@/lib/api": { getSideStoryEpisodeSnapshot: () => snapshot.promise },
    "@/lib/event-txt-import.mjs": {
      parseEventTxtContent: () => [], sideStoryEpisodeTxtImportPreview: () => ({ rows: [] }), sideStoryTxtImportEdits: () => [],
      validateSideStoryEpisodeSnapshot: async () => { throw new Error("a superseded preview must not validate"); },
    },
  });
  const props = (storyId) => ({
    kind: "card", storyId, locale: "zh-CN", entries: [], episodes: [{ key: "1", label: "测试第 1 话" }], defaultEpisodeKey: "1",
    saveBatch: async () => ({ status: "saved", updated: 0, unchanged: 0 }), onReload() {},
  });
  const render = (storyId) => {
    renderer.render(SideStoryTxtImport, props(storyId));
    const tree = renderer.render(SideStoryTxtImport, props(storyId));
    const [button] = findElements(tree, (element) => element.type === "button" && element.props.className === "btn btn-secondary btn-sm");
    const [input] = findElements(tree, (element) => element.type === "input" && element.props.type === "file");
    return { button, input };
  };
  render("101").input.props.onChange({ target: { files: [{ name: "test.txt" }], value: "test.txt" } });
  const loading = render("101").button;
  assert.equal(loading.props.disabled, true);
  assert.equal(loading.props.children, "正在核对 TXT…");
  const switched = render("102").button;
  assert.equal(switched.props.disabled, false);
  assert.equal(switched.props.children, "导入 TXT");
  snapshot.resolve({});
  await flush();
  assert.equal(render("102").button.props.disabled, false);
});
