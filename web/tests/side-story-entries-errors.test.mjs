import assert from "node:assert/strict";
import test from "node:test";

import { loadSourceModule } from "./source-module-harness.mjs";

// All ids, messages and details below are synthetic test data.

// Re-renderable stand-in for React hooks: slots persist across render() calls in call
// order, effects run after each render whose deps changed, unmount() runs cleanups.
function createRenderer() {
  const slots = [];
  let cursor = 0;
  let queue = [];
  const changed = (previous, deps) => !previous || !deps || deps.some((dep, index) => !Object.is(dep, previous.deps[index]));
  const memo = (factory, deps) => {
    const index = cursor++;
    if (changed(slots[index], deps)) slots[index] = { value: factory(), deps };
    return slots[index].value;
  };
  const react = {
    useState(initial) {
      const slot = (slots[cursor++] ??= { value: typeof initial === "function" ? initial() : initial });
      return [slot.value, (next) => { slot.value = typeof next === "function" ? next(slot.value) : next; }];
    },
    useRef: (current) => (slots[cursor++] ??= { current }),
    useMemo: memo,
    useCallback: (callback, deps) => memo(() => callback, deps),
    useEffect(effect, deps) {
      const index = cursor++;
      const previous = slots[index];
      if (!changed(previous, deps)) return;
      slots[index] = { deps, cleanup: previous?.cleanup };
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
    unmount() {
      for (const slot of slots) if (typeof slot?.cleanup === "function") slot.cleanup();
    },
  };
}

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
