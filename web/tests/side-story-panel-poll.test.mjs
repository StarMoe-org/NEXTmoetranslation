import assert from "node:assert/strict";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const NOW = Date.parse("2026-01-01T00:00:00Z");
const idleRound = { episodes: 0, requests: 0, fetched: 0, officialWritten: 0, errors: 0, retrying: 0 };

function mountPanel(t, { expanded = true, state }) {
  t.mock.timers.enable({ apis: ["setInterval", "Date"], now: NOW });
  const runtime = createHookRuntime();
  const { SideStoryBackfillPanel } = loadSourceModule("components/console/SideStoryBackfillPanel.tsx", { react: runtime.react });
  const calls = { reload: 0 };
  SideStoryBackfillPanel({
    role: "editor", expanded, setExpanded() {}, busy: false, watch() {}, runSync() {},
    reload: () => { calls.reload++; },
    status: { state: { enabled: true, lastRound: idleRound, ...state }, totals: {} },
  });
  runtime.mount();
  return { calls, runtime };
}

test("an expanded panel polls every 15 s while a round is running and stops when unmounted", (t) => {
  // A resumed round runs while nextRoundAt is still the later time the previous round set.
  const { calls, runtime } = mountPanel(t, { state: { running: true, nextRoundAt: "2026-01-01T01:00:00Z" } });
  t.mock.timers.tick(14_999);
  assert.equal(calls.reload, 0);
  t.mock.timers.tick(1);
  assert.equal(calls.reload, 1);
  t.mock.timers.tick(15_000);
  assert.equal(calls.reload, 2);
  runtime.unmount();
  t.mock.timers.tick(60_000);
  assert.equal(calls.reload, 2);
});

test("an idle panel polls only once the next round time has passed", (t) => {
  const { calls, runtime } = mountPanel(t, { state: { running: false, nextRoundAt: "2026-01-01T00:01:00Z" } });
  t.mock.timers.tick(45_000);
  assert.equal(calls.reload, 0);
  t.mock.timers.tick(15_000);
  assert.equal(calls.reload, 1);
  runtime.unmount();
});

test("a collapsed or disabled panel does not poll", (t) => {
  const collapsed = mountPanel(t, { expanded: false, state: { running: true } });
  t.mock.timers.tick(60_000);
  assert.equal(collapsed.calls.reload, 0);
  collapsed.runtime.unmount();
  t.mock.timers.reset();
  const disabled = mountPanel(t, { state: { enabled: false, running: false, nextRoundAt: "" } });
  t.mock.timers.tick(60_000);
  assert.equal(disabled.calls.reload, 0);
  disabled.runtime.unmount();
});
