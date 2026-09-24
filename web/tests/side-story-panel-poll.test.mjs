import assert from "node:assert/strict";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const NOW = Date.parse("2026-01-01T00:00:00Z");
const idleRound = { episodes: 0, requests: 0, fetched: 0, officialWritten: 0, errors: 0, retrying: 0 };
const flush = () => new Promise((resolve) => setImmediate(resolve));

// status() answers each GET /story/sync; throwing makes it fail.
function mountCatalog(t, status) {
  t.mock.timers.enable({ apis: ["setInterval", "Date"], now: NOW });
  const runtime = createHookRuntime();
  const calls = { status: 0 };
  const toasts = [];
  const api = {
    getSideStories: async () => ({ stories: [] }),
    getSideStorySyncStatus: async () => {
      calls.status++;
      return { state: { enabled: true, lastRound: idleRound, ...status() }, totals: {} };
    },
    triggerSideStorySync: async () => { throw new Error("unused"); },
  };
  const { useSideStoryCatalog } = loadSourceModule("components/console/useSideStoryCatalog.ts", { react: runtime.react, "@/lib/api": api });
  const catalog = useSideStoryCatalog({ locale: "zh-CN", show: (message, tone) => toasts.push([tone, message]) });
  runtime.mount();
  return { catalog, calls, toasts, runtime };
}

test("a watched catalog polls every 15 s while a round is running and stops when unwatched", async (t) => {
  // A resumed round runs while nextRoundAt is still the later time the previous round set.
  const { catalog, calls, runtime } = mountCatalog(t, () => ({ running: true, nextRoundAt: "2026-01-01T01:00:00Z" }));
  catalog.watchSyncStatus(true);
  await flush();
  assert.equal(calls.status, 1, "watching loads once");
  t.mock.timers.tick(14_999);
  assert.equal(calls.status, 1);
  t.mock.timers.tick(1);
  assert.equal(calls.status, 2);
  t.mock.timers.tick(15_000);
  assert.equal(calls.status, 3);
  catalog.watchSyncStatus(false);
  t.mock.timers.tick(60_000);
  assert.equal(calls.status, 3);
  runtime.unmount();
});

test("an idle watched catalog polls only once the next round time has passed", async (t) => {
  const { catalog, calls, runtime } = mountCatalog(t, () => ({ running: false, nextRoundAt: "2026-01-01T00:01:00Z" }));
  catalog.watchSyncStatus(true);
  await flush();
  t.mock.timers.tick(45_000);
  assert.equal(calls.status, 1);
  t.mock.timers.tick(15_000);
  assert.equal(calls.status, 2);
  runtime.unmount();
});

test("a poll waits for a 刷新进度 still under way, so the refresh's failure is still reported", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval", "Date"], now: NOW });
  const runtime = createHookRuntime();
  const waiting = [];
  const toasts = [];
  let calls = 0;
  const running = { state: { enabled: true, running: true, nextRoundAt: "2026-01-01T01:00:00Z", lastRound: idleRound }, totals: {} };
  const api = {
    getSideStories: async () => ({ stories: [] }),
    getSideStorySyncStatus: () => {
      calls++;
      return calls === 1 ? Promise.resolve(running) : new Promise((resolve, reject) => waiting.push({ resolve, reject }));
    },
    triggerSideStorySync: async () => { throw new Error("unused"); },
  };
  const { useSideStoryCatalog } = loadSourceModule("components/console/useSideStoryCatalog.ts", { react: runtime.react, "@/lib/api": api });
  const catalog = useSideStoryCatalog({ locale: "zh-CN", show: (message, tone) => toasts.push([tone, message]) });
  runtime.mount();
  catalog.watchSyncStatus(true);
  await flush();

  void catalog.loadSyncStatus();
  t.mock.timers.tick(15_000);
  assert.equal(calls, 2, "no poll while the refresh is under way");
  waiting[0].reject(new Error("测试网关超时"));
  await flush();
  assert.deepEqual(toasts, [["err", "测试网关超时"]]);
  t.mock.timers.tick(15_000);
  assert.equal(calls, 3, "polling resumes after it");
  waiting[1].resolve(running);
  await flush();
  catalog.watchSyncStatus(false);
  runtime.unmount();
});

test("a disabled backfill is not polled", async (t) => {
  const { catalog, calls, runtime } = mountCatalog(t, () => ({ enabled: false, running: false, nextRoundAt: "" }));
  catalog.watchSyncStatus(true);
  await flush();
  t.mock.timers.tick(60_000);
  assert.equal(calls.status, 1);
  runtime.unmount();
});

test("failed polls keep polling without a toast; the first load and 刷新进度 still report failures", async (t) => {
  let down = false;
  const { catalog, calls, toasts, runtime } = mountCatalog(t, () => {
    if (down) throw new Error("测试网络中断");
    return { running: true, nextRoundAt: "2026-01-01T01:00:00Z" };
  });
  catalog.watchSyncStatus(true);
  await flush();
  down = true;
  for (let tick = 0; tick < 4; tick++) {
    t.mock.timers.tick(15_000);
    await flush();
  }
  assert.equal(calls.status, 5, "the last running state keeps the poll going");
  assert.deepEqual(toasts, []);
  await catalog.loadSyncStatus();
  assert.deepEqual(toasts, [["err", "测试网络中断"]]);
  catalog.watchSyncStatus(false);
  catalog.watchSyncStatus(true);
  await flush();
  assert.deepEqual(toasts, [["err", "测试网络中断"], ["err", "测试网络中断"]]);
  runtime.unmount();
});

test("the panel watches while expanded and stops watching when collapsed or unmounted", () => {
  const runtime = createHookRuntime();
  const { SideStoryBackfillPanel } = loadSourceModule("components/console/SideStoryBackfillPanel.tsx", { react: runtime.react });
  const watched = [];
  const panel = (expanded) => SideStoryBackfillPanel({
    role: "editor", expanded, setExpanded() {}, busy: false, reload() {}, runSync() {},
    watch: (watching) => watched.push(watching),
    status: { state: { enabled: true, running: true, lastRound: idleRound }, totals: {} },
  });
  panel(true);
  runtime.mount();
  runtime.unmount();
  panel(false);
  runtime.mount();
  runtime.unmount();
  assert.deepEqual(watched, [true, false, false, false]);
});
