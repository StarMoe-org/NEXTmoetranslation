import assert from "node:assert/strict";
import test from "node:test";

import { createRenderer, loadSourceModule } from "./source-module-harness.mjs";
import { read } from "./source-surfaces.mjs";

// All titles and lines below are synthetic test data.
const line = (position, jp, text) => ({ jp, role: "talk", speaker: "测试角色", position, text, source: text ? "human" : "", revision: text ? 1 : 0 });
const detail = {
  kind: "card", id: "101", title: "测试卡面", characterId: 1, areaId: 0, areaCategory: "", actionSetId: 0, locale: "zh-CN",
  episodes: [{
    key: "1", scenarioId: "test_01", title: "第一话", fetched: true, scriptSha256: "", cnState: "absent", enState: "absent", lastError: "",
    lines: [line(0, "テスト一", "测试一"), line(1, "テスト二", "测试二"), line(2, "テスト三", "")],
  }],
};
const flush = () => new Promise((resolve) => setImmediate(resolve));

test("重新载入本篇 reselects the open line; the first open of a story selects its first line", async (t) => {
  const renderer = createRenderer();
  t.after(() => renderer.unmount());
  const api = {
    getCategories: async () => [], getEventStories: async () => [], getEventAssociations: async () => ({ categories: {} }),
    getSideStory: async () => detail,
  };
  const { useConsoleEntries } = loadSourceModule("components/console/useConsoleEntries.ts", { react: renderer.react, "@/lib/api": api });
  const props = {
    username: "tester", locale: "zh-CN", setLocale() {}, show() {},
    savingRef: { current: false }, contextGenerationRef: { current: 0 }, setRemoteConflict() {}, setSidebarOpen() {},
    freezeRecoveredConflictRef: { current() {} },
  };
  renderer.render(useConsoleEntries, props).performSelectField("cardStory", "101");
  renderer.render(useConsoleEntries, props);
  await flush();
  let hook = renderer.render(useConsoleEntries, props);
  assert.equal(hook.selectedEntry?.japanese, "テスト一");

  const third = hook.entries.find((entry) => entry.japanese === "テスト三");
  hook.setSelectedKey(third.key);
  hook = renderer.render(useConsoleEntries, props);
  void hook.loadEntries(hook.selectedKey);
  await flush();
  hook = renderer.render(useConsoleEntries, props);
  assert.equal(hook.selectedEntry?.japanese, "テスト三", "the reload keeps the line");
  assert.equal(hook.editValue, "");

  void hook.loadEntries("1|已不存在的行");
  await flush();
  hook = renderer.render(useConsoleEntries, props);
  assert.equal(hook.selectedEntry?.japanese, "テスト一", "a line the reload no longer has falls back to the first");
});

test("the console's 重新载入本篇 passes the selected line to the reload", async () => {
  assert.match(await read("src/components/Console.tsx"), /const reloadStory = \(\) => runOrGuard\("重新载入剧情", \(\) => \{ void loadEntries\(selectedKey\); \}\);/);
});
