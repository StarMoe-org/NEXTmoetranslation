import assert from "node:assert/strict";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

function catalogHarness(trigger) {
  const runtime = createHookRuntime();
  const toasts = [];
  const calls = [];
  const api = {
    getSideStories: async () => ({ stories: [] }),
    getSideStorySyncStatus: async () => ({ state: { enabled: true, running: false, lastRound: {} }, totals: {} }),
    triggerSideStorySync: async (refreshCatalog) => { calls.push(refreshCatalog); return trigger(refreshCatalog); },
  };
  const { useSideStoryCatalog } = loadSourceModule("components/console/useSideStoryCatalog.ts", { react: runtime.react, "@/lib/api": api });
  const catalog = useSideStoryCatalog({ locale: "zh-CN", show: (message, tone) => toasts.push([tone, message]) });
  return { catalog, toasts, calls };
}

const state = (running) => ({ enabled: true, running, lastRound: { episodes: 0, requests: 0, fetched: 0, officialWritten: 0, errors: 0 } });

test("resuming while a round runs says the next round follows the current one", async () => {
  const running = catalogHarness(() => ({ started: true, state: state(true) }));
  await running.catalog.runSync(false);
  await running.catalog.runSync(true);
  assert.deepEqual(running.calls, [false, true]);
  assert.deepEqual(running.toasts, [
    ["ok", "回填正在运行，本轮结束后将续跑一轮"],
    ["ok", "回填正在运行，本轮结束后将刷新目录并续跑一轮"],
  ]);

  const idle = catalogHarness(() => ({ started: true, state: state(false) }));
  await idle.catalog.runSync(false);
  await idle.catalog.runSync(true);
  assert.deepEqual(idle.toasts, [["ok", "已开始续跑一轮回填"], ["ok", "已开始刷新目录并续跑一轮回填"]]);
});
