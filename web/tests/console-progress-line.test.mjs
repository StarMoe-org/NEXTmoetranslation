import assert from "node:assert/strict";
import test from "node:test";

import { createRenderer, loadSourceModule } from "./source-module-harness.mjs";

// All labels below are synthetic test data.
function mountRealtime(t) {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const renderer = createRenderer();
  t.after(() => renderer.unmount());
  let handler = null;
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
  const props = {
    username: "tester", clientID: "tab-a", locale: "zh-CN", show() {},
    category: "cardStory", field: "101", isEventStory: false, sideStoryKind: "card", isLyrics: false, isLyricsSourceReview: false,
    entries: [], setEntries() {}, selectedKey: null, setSelectedKey() {}, selectedEntry: null, entryDirty: false,
    editValue: "", setEditValue() {}, eventTxtDraft: null, setEventTxtDraft() {}, eventTxtDraftDirty: false,
    lyricsDirty: false, lyricsEditorRef: { current: null }, lyricsSourceReviewRef: { current: null },
    setRemoteConflict() {}, contextGenerationRef: { current: 0 }, invalidatePendingAction() {},
    loadEntries: async () => true, reloadSidebar: async () => true, refreshSideStoryLists() {},
  };
  const render = () => renderer.render(useConsoleRealtime, props);
  render();
  return {
    emit: (event, data) => { handler(event, data); return render(); },
    render,
  };
}

const idleGate = { instanceId: "test-instance", running: false };

test("a producer job that ends short of its total clears the progress line", (t) => {
  const realtime = mountRealtime(t);
  realtime.emit("gate.status", { ...idleGate, running: true });
  let state = realtime.emit("translate.progress", { detail: "测试进度 0/66", current: 0, total: 66 });
  assert.deepEqual(state.progress, { label: "测试进度 0/66", current: 0, total: 66 });
  state = realtime.emit("gate.status", idleGate);
  assert.equal(state.progress, null, "the failed job's line is gone");
});

test("a finished job's last progress stays for its short delay", (t) => {
  const realtime = mountRealtime(t);
  realtime.emit("gate.status", { ...idleGate, running: true });
  realtime.emit("translate.progress", { detail: "测试已保存 66/66", current: 66, total: 66 });
  let state = realtime.emit("gate.status", idleGate);
  assert.equal(state.progress?.label, "测试已保存 66/66");
  t.mock.timers.tick(1500);
  state = realtime.render();
  assert.equal(state.progress, null);
});
