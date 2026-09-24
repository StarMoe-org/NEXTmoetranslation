import assert from "node:assert/strict";
import test from "node:test";

import { createRenderer, loadSourceModule } from "./source-module-harness.mjs";

// All ids, messages and details below are synthetic test data.

const flush = () => new Promise((resolve) => setImmediate(resolve));

class TestAPIError extends Error {
  constructor(status, body) {
    super(body.error);
    this.status = status;
    this.code = body.error;
    this.details = body.details || [];
  }
}

// Opens category/field and returns the toasts of the load that follows.
async function loadFailure(t, category, field, failure) {
  const renderer = createRenderer();
  t.after(() => renderer.unmount());
  const toasts = [];
  const api = {
    APIError: TestAPIError,
    getCategories: async () => [],
    getEventStories: async () => [],
    getEventAssociations: async () => ({ categories: {} }),
    getSideStory: async () => { throw failure; },
    getEventStory: async () => { throw failure; },
    getEntries: async () => { throw failure; },
  };
  const { useConsoleEntries } = loadSourceModule("components/console/useConsoleEntries.ts", { react: renderer.react, "@/lib/api": api });
  const props = {
    username: "tester", locale: "zh-CN", setLocale() {}, show: (message, tone) => toasts.push([tone, message]),
    savingRef: { current: false }, contextGenerationRef: { current: 0 }, setRemoteConflict() {}, setSidebarOpen() {},
    freezeRecoveredConflictRef: { current() {} },
  };
  renderer.render(useConsoleEntries, props).performSelectField(category, field);
  renderer.render(useConsoleEntries, props);
  await flush();
  return toasts;
}

test("a failed side-story detail load names the contract error instead of its code", async (t) => {
  assert.deepEqual(await loadFailure(t, "cardStory", "101", new TestAPIError(404, { error: "not_found" })), [["err", "剧情不存在"]]);
  assert.deepEqual(
    await loadFailure(t, "areaTalk", "areatalk_ev_1", new TestAPIError(404, { error: "not_found", details: ["测试详情"] })),
    [["err", "剧情不存在：测试详情"]],
  );
  assert.deepEqual(await loadFailure(t, "cardStory", "101", new Error("")), [["err", "剧情载入失败"]]);
});

test("other categories keep the raw load error message", async (t) => {
  assert.deepEqual(await loadFailure(t, "cards", "prefix", new TestAPIError(404, { error: "not_found" })), [["err", "not_found"]]);
});
