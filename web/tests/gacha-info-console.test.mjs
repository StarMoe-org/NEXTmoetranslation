import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import test from "node:test";

import { loadSourceModule } from "./source-module-harness.mjs";

const require = createRequire(import.meta.url);
const { createElement } = require("react");
const { renderToStaticMarkup } = require("react-dom/server");

const labels = loadSourceModule("lib/labels.ts");
const shortcuts = loadSourceModule("lib/translation-shortcuts.ts");
const { ConsoleHeader } = loadSourceModule("components/console/ConsoleHeader.tsx");
const { EntryRow } = loadSourceModule("components/console/EntryRow.tsx");
const { TranslationEntryWorkspace } = loadSourceModule("components/console/TranslationEntryWorkspace.tsx");
const read = (path) => readFile(new URL(`../${path}`, import.meta.url), "utf8");

const multiLineKey = "第一行的说明\n第二行的说明\n第三行的说明";
const entry = { key: multiLineKey, text: "译文第一行\n译文第二行", source: "llm", ids: ["12"] };

test("gachaInfo has its own category and field labels without renaming other categories' fields", () => {
  assert.equal(labels.CATEGORY_LABELS.gachaInfo, "卡池简介与说明");
  assert.equal(labels.fieldLabel("gachaInfo", "summary"), "卡池简介");
  assert.equal(labels.fieldLabel("gachaInfo", "bubbleText"), "气泡文字");
  assert.equal(labels.fieldLabel("gachaInfo", "description"), "招募说明");
  assert.equal(labels.fieldLabel("cards", "description"), "描述");
  assert.equal(labels.fieldLabel("gacha", "name"), "名称");
  assert.equal(labels.fieldLabel("gachaInfo", "unlistedField"), "unlistedField");
});

test("gachaInfo entries and event stories link to existing Moesekai detail pages", () => {
  // gachaInfo trace ids are gacha ids; the main site serves event stories under /story/event/.
  assert.match(labels.buildMoesekaiUrl("gachaInfo", "description", ["12", "40"]), /^https?:\/\/[^/]+(\/.*)?\/gacha\/12\/$/);
  assert.equal(labels.buildMoesekaiUrl("gachaInfo", "summary", ["12"]), labels.buildMoesekaiUrl("gacha", "name", ["12"]));
  assert.equal(labels.buildMoesekaiUrl("gachaInfo", "bubbleText", []), null);
  const eventStory = labels.buildMoesekaiUrl("eventStory", "163");
  assert.match(eventStory, /\/story\/event\/163\/$/);
  assert.doesNotMatch(eventStory, /\/eventstory\//);
  assert.equal(labels.buildMoesekaiUrl("sticker", "name", ["3"]), null);
});

test("console surfaces resolve field labels per category", async () => {
  const header = renderToStaticMarkup(createElement(ConsoleHeader, {
    category: "gachaInfo", field: "bubbleText", realtimeState: "connected",
    onlineUsers: [], selectedIndex: 0, filteredCount: 1,
  }));
  assert.match(header, /卡池简介与说明 \/ 气泡文字/);
  const [sidebar, settings] = await Promise.all([
    read("src/components/console/ConsoleSidebar.tsx"), read("src/components/SettingsModal.tsx"),
  ]);
  for (const source of [sidebar, settings]) {
    assert.match(source, /fieldLabel\(cat\.name, f\.name\)/);
    assert.doesNotMatch(source, /FIELD_LABELS\[/);
  }
});

test("multi-line keys keep their line breaks in a clamped entry preview", async () => {
  const row = renderToStaticMarkup(createElement("table", null, createElement("tbody", null, createElement(EntryRow, {
    entry, isSelected: false, isRemoteHighlighted: false, isEventStory: false, isReadOnly: false,
    writesLocked: false, eventTxtDraftDirty: false, hasRemoteConflict: false, hasCanonicalIdentity: true,
    onSelect() {}, onSourceChange() {},
  }))));
  assert.ok(row.includes(`<div class="entry-preview">${multiLineKey}</div>`), row);
  assert.ok(row.includes(`<div class="cn entry-preview">${entry.text}</div>`), row);
  const css = await read("src/app/globals.css");
  const rule = css.match(/\.entry-row \.entry-preview \{([^}]*)\}/)?.[1] || "";
  assert.match(rule, /white-space: pre-wrap/);
  assert.match(rule, /-webkit-line-clamp: \d/);
  assert.match(rule, /overflow: hidden/);
});

function workspace(enterSaves) {
  return renderToStaticMarkup(createElement(TranslationEntryWorkspace, {
    category: "gachaInfo", field: "description", isEventStory: false, isReadOnly: false,
    loading: false, saving: false, writesLocked: false, filtered: [entry], selectedKey: entry.key,
    selectedEntry: entry, selectedIndex: 0, selectedEventStoryIdentityMissing: false,
    remoteHighlights: {}, remoteConflict: null, setRemoteConflict() {}, eventTxtDraftDirty: false,
    editValue: entry.text, setEditValue() {}, editRef: { current: null },
    translationEntryListRef: { current: null }, enterSaves, setEnterSaves() {},
    onTextareaKey() {}, navigate() {}, save: async () => true, selectEntry() {}, handleSourceChange() {},
  }));
}

test("the translation textarea advertises the newline key next to the save key", () => {
  const enterInsertsNewline = workspace(false);
  assert.match(enterInsertsNewline, /保存 <kbd>Shift\+Enter<\/kbd>/);
  assert.match(enterInsertsNewline, /换行 <kbd>Enter<\/kbd>/);
  const enterSaves = workspace(true);
  assert.match(enterSaves, /保存 <kbd>Enter<\/kbd>/);
  assert.match(enterSaves, /换行 <kbd>Shift\+Enter<\/kbd>/);
  const rows = Number(enterInsertsNewline.match(/<textarea[^>]*rows="(\d+)"/)?.[1]);
  assert.ok(rows >= 3, `textarea rows ${rows} should fit the three-line source`);
});

test("the save shortcut leaves the other Enter combination to insert a newline", () => {
  const key = (shiftKey) => ({ key: "Enter", shiftKey, ctrlKey: false, metaKey: false, keyCode: 13, nativeEvent: { isComposing: false } });
  assert.equal(shortcuts.translationTextareaAction(key(false), true), "save");
  assert.equal(shortcuts.translationTextareaAction(key(true), true), null);
  assert.equal(shortcuts.translationTextareaAction(key(true), false), "save");
  assert.equal(shortcuts.translationTextareaAction(key(false), false), null);
});
