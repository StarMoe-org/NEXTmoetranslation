import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";

import { loadSourceModule } from "./source-module-harness.mjs";

const require = createRequire(import.meta.url);
const { createElement } = require("react");
const { renderToStaticMarkup } = require("react-dom/server");

const model = loadSourceModule("lib/side-story-console.ts");
const { SideStoryToolbar } = loadSourceModule("components/console/SideStoryToolbar.tsx", {
  "@/components/SideStoryTxtImport": { SideStoryTxtImport: () => null },
});

// All lines below are synthetic test data.
function episode(key, fetched, lines) {
  return { key, scenarioId: `test_${key}`, title: "", fetched, scriptSha256: "", cnState: "pending", enState: "pending", lines, translatedCount: 0, untranslatedCount: 0 };
}

function story(kind, episodes) {
  return { kind, id: "test_story", title: "测试", characterId: 0, areaId: 0, areaCategory: "", actionSetId: 0, locale: "zh-CN", episodes };
}

function chaptersOf(entries) {
  const keys = [...new Set(entries.map((entry) => entry.episodeNo))];
  return keys.map((episodeNo) => {
    const own = entries.filter((entry) => entry.episodeNo === episodeNo);
    return { episodeNo, title: "", total: own.length, untranslated: own.filter(model.sideStoryEntryUntranslated).length };
  });
}

function render({ locale = "zh-CN", kind = "area", detail = null, selectedEpisode = "all" }) {
  const entries = detail ? model.buildSideStoryEntries(detail, locale === "ja-JP") : [];
  return renderToStaticMarkup(createElement(SideStoryToolbar, {
    role: "editor", locale, kind, storyId: "test_story", detail, entries, chapters: chaptersOf(entries), selectedEpisode, selectChapter() {},
    busy: false, saving: false, writesLocked: false, entryDirty: false, saveBatch: async () => ({ status: "saved", updated: 0, unchanged: 0 }),
    onAI() {}, onRefresh() {}, onReload() {},
  }));
}

const translated = { jp: "テスト台詞", role: "talk", speaker: "", position: 1, text: "测试台词", source: "human", revision: 1 };
const title = { jp: "テスト題", role: "title", position: 0, text: "测试标题", source: "official", revision: 1 };

test("a story without entries or with an unfetched script shows no completion state", () => {
  const loading = render({});
  assert.doesNotMatch(loading, /已全部翻译|story-dot done/, "loading or a failed load");
  const unfetched = render({ detail: story("area", [episode("1", false, [])]) });
  assert.match(unfetched, /本篇：剧本尚未获取/);
  assert.doesNotMatch(unfetched, /已全部翻译|story-dot done/);
  const titleOnly = render({ kind: "card", detail: story("card", [episode("1", false, [title])]), selectedEpisode: "1" });
  assert.match(titleOnly, /第 1 话：剧本尚未获取/);
  assert.doesNotMatch(titleOnly, /已全部翻译|story-dot done/);
  assert.match(render({ detail: story("area", [episode("1", true, [translated])]) }), /story-dot done"><\/span> 已全部翻译/);
});

test("the read-only Japanese view never claims an episode or story is complete", () => {
  const card = render({ locale: "ja-JP", kind: "card", detail: story("card", [episode("1", true, [title, translated]), episode("2", true, [title])]) });
  assert.doesNotMatch(card, /已完成/);
  assert.match(card, /第 1 话 · 2 条/);
  assert.match(card, /第 2 话 · 1 条/);
  const area = render({ locale: "ja-JP", detail: story("area", [episode("1", true, [translated])]) });
  assert.match(area, /日文原文（只读）/);
  assert.doesNotMatch(area, /story-dot done|已全部翻译/);
});
