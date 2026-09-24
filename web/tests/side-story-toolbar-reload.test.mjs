import assert from "node:assert/strict";
import test from "node:test";

import { loadSourceModule } from "./source-module-harness.mjs";

const { SideStoryToolbar } = loadSourceModule("components/console/SideStoryToolbar.tsx", {
  "@/components/SideStoryTxtImport": { SideStoryTxtImport: () => null },
});

function findButton(node, label) {
  if (Array.isArray(node)) {
    for (const child of node) {
      const found = findButton(child, label);
      if (found) return found;
    }
    return null;
  }
  if (!node || typeof node !== "object" || !node.props) return null;
  if (node.type === "button" && node.props.children === label) return node;
  return findButton(node.props.children, label);
}

test("every role can reload the open story from the toolbar", () => {
  for (const [role, locale] of [["editor", "zh-CN"], ["admin", "en-US"], ["", "ja-JP"]]) {
    let reloads = 0;
    const toolbar = SideStoryToolbar({
      role, locale, kind: "area", storyId: "test_story", detail: null, entries: [], chapters: [], selectedEpisode: "all", selectChapter() {},
      busy: false, saving: false, writesLocked: false, entryDirty: false, saveBatch: async () => ({ status: "saved", updated: 0, unchanged: 0 }),
      onAI() {}, onRefresh() {}, onReload: () => { reloads++; },
    });
    const button = findButton(toolbar, "重新载入本篇");
    assert.ok(button, `${role || "no role"} in ${locale} sees the reload action`);
    button.props.onClick();
    assert.equal(reloads, 1);
  }
});
