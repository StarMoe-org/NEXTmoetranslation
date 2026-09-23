import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("official CN entries warn that a human save is overwritten unless it is pinned", async () => {
  const workspace = await readFile(new URL("../src/components/console/TranslationEntryWorkspace.tsx", import.meta.url), "utf8");

  assert.match(workspace, /!isEventStory && selectedEntry\.source === "cn" && \([\s\S]{0,400}SOURCE_LABELS\.pinned\}保存/);
  assert.match(workspace, /保存后来源变为\{SOURCE_LABELS\.human\}，下次\{SOURCE_LABELS\.cn\}同步仍会覆盖这条译文/);
});
