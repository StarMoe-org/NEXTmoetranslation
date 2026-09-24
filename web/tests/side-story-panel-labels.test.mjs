import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";

import { loadSourceModule } from "./source-module-harness.mjs";

const require = createRequire(import.meta.url);
const { createElement } = require("react");
const { renderToStaticMarkup } = require("react-dom/server");

const { SideStoryBackfillPanel } = loadSourceModule("components/console/SideStoryBackfillPanel.tsx");

test("the backfill panel names official import states like the toolbar", () => {
  const progress = {
    stories: 1, episodes: 2, fetched: 2, pendingFetch: 0, errors: 0,
    cnImported: 1, cnPending: 2, cnAbsent: 3, cnMismatch: 4, cnError: 5,
    enImported: 6, enPending: 7, enAbsent: 8, enMismatch: 9, enError: 10,
  };
  const html = renderToStaticMarkup(createElement(SideStoryBackfillPanel, {
    role: "editor", expanded: true, setExpanded() {}, busy: false, watch() {}, reload() {}, runSync() {},
    status: { state: { enabled: true, running: false, lastRound: { episodes: 0, requests: 0, fetched: 0, officialWritten: 0, errors: 0, retrying: 0 } }, totals: { card: progress } },
  }));
  assert.match(html, /简中官方：已导入 1 · 待导入 2 · 未发布 3 · 剧本结构不一致 4 · 导入失败 5/);
  assert.match(html, /英文官方：已导入 6 · 待导入 7 · 未发布 8 · 剧本结构不一致 9 · 导入失败 10/);
});
