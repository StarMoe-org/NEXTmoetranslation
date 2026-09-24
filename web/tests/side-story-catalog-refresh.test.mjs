import assert from "node:assert/strict";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const flush = () => new Promise((resolve) => setImmediate(resolve));
const summary = (fields = {}) => ({
  kind: "card", id: "10", title: "测试卡面甲", characterId: 1, areaId: 0, areaCategory: "", actionSetId: 0, releasedAt: 1,
  episodeCount: 2, fetchedEpisodeCount: 1, lineCount: 4, translatedCount: 0, untranslatedCount: 4,
  sourceCounts: {}, primarySource: "", status: "untranslated", updatedAt: 1, ...fields,
});

// Each list load waits until the test resolves or rejects it.
function mountCatalog(t) {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const runtime = createHookRuntime();
  const loads = [];
  const toasts = [];
  const api = {
    getSideStories: (kind) => new Promise((resolve, reject) => {
      loads.push({ kind, resolve: (stories) => resolve({ stories }), reject });
    }),
    getSideStorySyncStatus: async () => { throw new Error("unused"); },
    triggerSideStorySync: async () => { throw new Error("unused"); },
  };
  const { useSideStoryCatalog } = loadSourceModule("components/console/useSideStoryCatalog.ts", { react: runtime.react, "@/lib/api": api });
  const catalog = useSideStoryCatalog({ locale: "zh-CN", show: (message, tone) => toasts.push([tone, message]) });
  runtime.mount();
  t.after(() => runtime.unmount());
  return { catalog, loads, toasts, unmount: () => runtime.unmount() };
}

async function loadCard(catalog, loads, stories) {
  catalog.ensureList("card");
  loads.shift().resolve(stories);
  await flush();
}

test("refreshes load at most 1.5 s after the first request even while saves keep arriving", async (t) => {
  const { catalog, loads } = mountCatalog(t);
  catalog.ensureList("card");
  catalog.ensureList("area");
  loads.splice(0).forEach((load) => load.resolve([]));
  await flush();

  const synced = [];
  catalog.refreshLists("card");
  t.mock.timers.tick(1000);
  catalog.refreshLists(undefined, (kind) => synced.push(kind));
  t.mock.timers.tick(500);
  assert.deepEqual(loads.map((load) => load.kind), ["card", "area"], "the later request joins the first one's window");
  loads.splice(0).forEach((load) => load.resolve([]));
  await flush();
  assert.deepEqual(synced, ["card", "area"]);

  // Saves one second apart: each window still ends 1.5 s after it opened.
  const issued = [];
  for (let second = 0; second < 12; second++) {
    catalog.refreshLists("card");
    t.mock.timers.tick(1000);
    loads.splice(0).forEach((load) => { issued.push(load.kind); load.resolve([]); });
    await flush();
  }
  assert.deepEqual(issued, Array(6).fill("card"));
});

test("a sync callback outlives a load a manual reload superseded and sees the list from before both", async (t) => {
  const { catalog, loads } = mountCatalog(t);
  const original = [summary()];
  await loadCard(catalog, loads, original);

  const synced = [];
  catalog.refreshLists(undefined, (kind, before, after) => synced.push([kind, before, after]));
  t.mock.timers.tick(1500);
  const syncLoad = loads.shift();
  catalog.reloadList("card");
  const retryLoad = loads.shift();

  const backfilled = [summary({ fetchedEpisodeCount: 2 })];
  syncLoad.resolve(backfilled);
  await flush();
  assert.deepEqual(synced, [], "a superseded load leaves the callback");
  retryLoad.resolve(backfilled);
  await flush();
  assert.deepEqual(synced, [["card", original, backfilled]]);

  catalog.refreshLists("card");
  t.mock.timers.tick(1500);
  loads.shift().resolve(backfilled);
  await flush();
  assert.equal(synced.length, 1, "the adopted load consumed the callback");
});

test("a sync callback outlives a failed load and goes to the next load of its kind", async (t) => {
  const { catalog, loads, toasts } = mountCatalog(t);
  const original = [summary()];
  await loadCard(catalog, loads, original);

  const synced = [];
  catalog.refreshLists(undefined, (kind, before, after) => synced.push([kind, before, after]));
  t.mock.timers.tick(1500);
  loads.shift().reject(new Error("测试网关超时"));
  await flush();
  assert.deepEqual(synced, []);
  assert.deepEqual(toasts, [["err", "测试网关超时"]]);

  const backfilled = [summary({ fetchedEpisodeCount: 2 })];
  catalog.refreshLists("card");
  t.mock.timers.tick(1500);
  loads.shift().resolve(backfilled);
  await flush();
  assert.deepEqual(synced, [["card", original, backfilled]]);
});

test("a second sync during a list load keeps its callback for the next load", async (t) => {
  const { catalog, loads } = mountCatalog(t);
  await loadCard(catalog, loads, [summary({ fetchedEpisodeCount: 1 })]);

  const synced = [];
  const onSynced = (kind, before, after) => synced.push([kind, before[0].fetchedEpisodeCount, after[0].fetchedEpisodeCount]);
  catalog.refreshLists(undefined, onSynced);
  t.mock.timers.tick(1500);
  const firstLoad = loads.shift();
  catalog.refreshLists(undefined, onSynced);
  firstLoad.resolve([summary({ fetchedEpisodeCount: 2 })]);
  await flush();
  t.mock.timers.tick(1500);
  loads.shift().resolve([summary({ fetchedEpisodeCount: 3 })]);
  await flush();
  assert.deepEqual(synced, [["card", 1, 2], ["card", 2, 3]]);
});

test("a sync callback another kind's load consumed still reaches this kind's next load", async (t) => {
  const { catalog, loads } = mountCatalog(t);
  catalog.ensureList("card");
  catalog.ensureList("area");
  const original = [summary()];
  const areaOriginal = [summary({ kind: "area", id: "20" })];
  const [cardFirst, areaFirst] = loads.splice(0);
  cardFirst.resolve(original);
  areaFirst.resolve(areaOriginal);
  await flush();

  const synced = [];
  catalog.refreshLists(undefined, (kind, before, after) => synced.push([kind, before, after]));
  t.mock.timers.tick(1500);
  const [cardSync, areaSync] = loads.splice(0);
  catalog.refreshLists("card");
  const areaBackfilled = [summary({ kind: "area", id: "20", fetchedEpisodeCount: 2 })];
  areaSync.resolve(areaBackfilled);
  await flush();
  t.mock.timers.tick(1500);
  cardSync.reject(new Error("测试网关超时"));
  await flush();

  const backfilled = [summary({ fetchedEpisodeCount: 2 })];
  assert.equal(loads.length, 1, "the save's refresh loads card once its sync load settles");
  loads.shift().resolve(backfilled);
  await flush();
  assert.deepEqual(synced, [["area", areaOriginal, areaBackfilled], ["card", original, backfilled]]);
});

test("on a slow link a save every second still gets lists adopted and never supersedes a load", async (t) => {
  const { catalog, loads } = mountCatalog(t);
  await loadCard(catalog, loads, [summary({ fetchedEpisodeCount: 0 })]);

  let now = 0;
  let issued = 0;
  let inFlight = 0;
  let maxInFlight = 0;
  const adopted = [];
  const onAdopted = (kind, before, after) => adopted.push({ load: after[0].fetchedEpisodeCount, at: now });
  const step = async () => {
    t.mock.timers.tick(100);
    now += 100;
    await flush();
    loads.splice(0).forEach((load) => {
      const number = ++issued;
      maxInFlight = Math.max(maxInFlight, ++inFlight);
      setTimeout(() => { inFlight--; load.resolve([summary({ fetchedEpisodeCount: number })]); }, 2200);
    });
  };
  for (let second = 0; second < 30; second++) {
    catalog.refreshLists("card", onAdopted);
    for (let tenth = 0; tenth < 10; tenth++) await step();
  }
  const adoptedDuringSaves = adopted.length;
  for (let tenth = 0; tenth < 100; tenth++) await step();

  assert.ok(adoptedDuringSaves >= 1, `adopted ${adoptedDuringSaves} of ${issued} loads during the saves`);
  assert.equal(maxInFlight, 1, "a refresh never starts a load while one is in flight");
  assert.deepEqual(adopted.map((entry) => entry.load), Array.from({ length: issued }, (_, index) => index + 1));
});

test("a refresh window that fires during a load starts one load once it settles, also when it fails", async (t) => {
  const { catalog, loads, toasts } = mountCatalog(t);
  await loadCard(catalog, loads, [summary()]);

  for (const settle of [(load) => load.resolve([summary()]), (load) => load.reject(new Error("测试网关超时"))]) {
    catalog.refreshLists("card");
    t.mock.timers.tick(1500);
    const inFlight = loads.shift();
    for (let window = 0; window < 2; window++) {
      catalog.refreshLists("card");
      t.mock.timers.tick(1500);
    }
    assert.equal(loads.length, 0, "no load supersedes the one in flight");
    settle(inFlight);
    await flush();
    assert.equal(loads.length, 1, "one load follows");
    loads.shift().resolve([summary()]);
    await flush();
    t.mock.timers.tick(3000);
    assert.equal(loads.length, 0);
  }
  assert.deepEqual(toasts, [["err", "测试网关超时"]]);

  // A window still open when the load settles starts the next load itself.
  catalog.refreshLists("card");
  t.mock.timers.tick(1500);
  const inFlight = loads.shift();
  catalog.refreshLists("card");
  t.mock.timers.tick(1500);
  catalog.refreshLists("card");
  inFlight.resolve([summary()]);
  await flush();
  assert.equal(loads.length, 0);
  t.mock.timers.tick(1500);
  assert.equal(loads.length, 1);
});

test("a load that settles after unmount starts no follow-up load", async (t) => {
  const { catalog, loads, toasts, unmount } = mountCatalog(t);
  await loadCard(catalog, loads, [summary()]);
  catalog.refreshLists("card");
  t.mock.timers.tick(1500);
  const inFlight = loads.shift();
  catalog.refreshLists("card");
  t.mock.timers.tick(1500);
  unmount();
  inFlight.resolve([summary()]);
  await flush();
  assert.equal(loads.length, 0);
  assert.deepEqual(toasts, []);
});

test("the card filter accepts the #id the sidebar and toolbar show", () => {
  const { filterCardStories } = loadSourceModule("lib/side-story-catalog.ts");
  const stories = [summary({ id: "1473", title: "测试卡面甲" }), summary({ id: "14", title: "测试卡面乙" })];
  const ids = (query) => filterCardStories(stories, { unit: "", characterId: 0, query }).map((story) => story.id);
  assert.deepEqual(ids("#1473"), ["1473"]);
  assert.deepEqual(ids(" #1473 "), ["1473"]);
  assert.deepEqual(ids("1473"), ["1473"]);
});
