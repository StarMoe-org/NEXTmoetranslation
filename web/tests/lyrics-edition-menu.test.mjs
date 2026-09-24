import assert from "node:assert/strict";
import test from "node:test";

import { read, readLyricsEditor } from "./source-surfaces.mjs";


test("LyricsEditionMenu is a portal-backed mixed selection and command menu", async () => {
  const menu = await read("src/components/LyricsEditionMenu.tsx");
  assert.match(menu, /createPortal\([\s\S]*document\.body/);
  assert.match(menu, /role="menu"/);
  assert.match(menu, /role="menuitemradio"/);
  assert.match(menu, /aria-checked=\{edition\.key === activeEditionKey\}/);
  assert.match(menu, /role="separator"/);
  assert.equal((menu.match(/<button type="button" role="menuitem"/g) || []).length, 4);
  for (const command of ["新建空白译本", "克隆当前译本", "重命名当前译本", "设为默认译本"]) {
    assert.ok(menu.includes(command), `missing edition command ${command}`);
  }
  assert.match(menu, /activeEdition\?\.label/);
  assert.match(menu, /activeEdition\?\.key/);
  assert.match(menu, /默认译本/);
  assert.match(menu, /lyrics-edition-chevron/);
  assert.equal((menu.match(/disabled=\{editions\.length >= 16\}/g) || []).length, 2);
});

test("edition menu keyboard, dismissal, focus restoration, and viewport clamping are explicit", async () => {
  const menu = await read("src/components/LyricsEditionMenu.tsx");
  for (const key of ["ArrowDown", "ArrowUp", "Home", "End", "Enter", "Escape", "Tab"]) {
    assert.ok(menu.includes(`event.key === "${key}"`), `missing keyboard behavior for ${key}`);
  }
  assert.match(menu, /event\.key === " "/);
  assert.match(menu, /triggerRef\.current\?\.focus\(\)/);
  assert.match(menu, /focusAdjacentControl\(event\.shiftKey \? -1 : 1\)/);
  assert.match(menu, /event\.key === "Tab"[\s\S]*event\.preventDefault\(\)/);
  assert.match(menu, /document\.addEventListener\("pointerdown"/);
  assert.match(menu, /window\.addEventListener\("resize"/);
  assert.match(menu, /window\.addEventListener\("scroll"/);
  assert.match(menu, /window\.visualViewport/);
  assert.match(menu, /const onViewportChange = \(\) => closeMenu\(true\)/);
  assert.match(menu, /onViewportScroll[\s\S]*closeMenu\(true\)/);
  assert.match(menu, /document\.documentElement\.clientWidth/);
  assert.match(menu, /document\.documentElement\.clientHeight/);
  assert.match(menu, /VIEWPORT_MARGIN/);
  assert.match(menu, /maxHeight: position\.maxHeight/);
  assert.doesNotMatch(menu, /maxHeight: Math\.max\(/);
  assert.match(menu, /aria-disabled=\{disabled \|\| editions\.length === 0\}/);
  assert.doesNotMatch(menu, /\n\s*disabled=\{disabled \|\| editions\.length === 0\}/);
  assert.match(menu, /open && mounted \? createPortal/);
});

test("translation-edition selection never becomes a sideways control", async () => {
  const [menu, css, editor] = await Promise.all([
    read("src/components/LyricsEditionMenu.tsx"),
    read("src/app/globals.css"),
    readLyricsEditor(),
  ]);
  const editionCSS = css.split(".lyrics-edition-selector")[1].split(".lyrics-error")[0];
  assert.doesNotMatch(`${menu}\n${editionCSS}`, /overflow-x|carousel|swipe|scroll-snap|touch-action|border-radius:\s*999px/i);
  assert.match(editionCSS, /\.lyrics-edition-menu-list[^}]*display: grid/);
  assert.match(editionCSS, /\.lyrics-edition-menu-commands[^}]*display: grid/);
  assert.match(editionCSS, /border-radius: var\(--radius-sm\)/);
  assert.match(editionCSS, /overflow-y: auto/);
  assert.match(css, /@media \(prefers-reduced-motion: reduce\)[\s\S]*\.lyrics-edition-menu \{ animation: none; \}/);
  assert.match(css, /\.lyrics-edition-trigger, \.lyrics-edition-menu-item, \.lyrics-edition-menu-commands button \{ min-height: 44px; \}/);
  assert.match(editor, /lyrics-editor-title-row[\s\S]*<LyricsEditionMenu/);
  assert.doesNotMatch(editor, /lyrics-version-tabs[^<]*<LyricsEditionMenu/);
});

test("clean authoritative reload retains the current edition and falls back through server default metadata", async () => {
  const editor = await readLyricsEditor();
  assert.match(editor, /activeTranslationEditionKeyRef\.current \|\| currentEditionDocument\.translationEditionKey/);
  assert.match(editor, /getLyrics\(item\.musicId, preferredEditionKey \|\| undefined\)/);
  assert.match(editor, /preferredEditionKey && reason instanceof APIError && reason\.status === 404[\s\S]*getLyrics\(item\.musicId\)/);
  assert.match(editor, /selectTranslationEditionKey\([\s\S]*editableEditionDocument\.translationEditionKey[\s\S]*editableEditionDocument\.defaultTranslationEditionKey[\s\S]*editableEditionDocument\.translationEditions/);
  assert.match(editor, /acceptAuthoritativeDocument\(loaded, preferredRenditionKey, preferredVersion\)/);
});

test("LyricsEditor guards every edition transition and keeps clone discard semantics explicit", async () => {
  const editor = await readLyricsEditor();
  assert.match(editor, /setPendingTransition\(\{ kind: "edition-switch", editionKey \}\)/);
  assert.match(editor, /setPendingTransition\(\{ kind: "edition-command", command \}\)/);
  assert.match(editor, /const saveAndContinueLabel = sourceV3SaveWording\(sourceV3PublicState\)\?\.continueLabel \?\? "保存并继续"/);
  assert.match(editor, /放弃并继续/);
  assert.match(editor, />取消</);
  assert.match(editor, /克隆只会复制服务器上已保存的当前译本，明确不会复制这份未保存草稿/);
  assert.match(editor, /只克隆当前译本在服务器上已保存的内容，不会读取或复制任何未保存的浏览器草稿/);
  assert.match(editor, /writeLockedRef\.current/);
  assert.match(editor, /documentGenerationRef\.current !== documentGeneration/);
  assert.match(editor, /`ed-\$\{crypto\.randomUUID\(\)\}`/);
  assert.match(editor, /replaceState\(window\.history\.state/);
  assert.doesNotMatch(editor, /pushState\(/);
  assert.match(editor, /url\.searchParams\.set\("edition", editionKey\)/);
  assert.doesNotMatch(editor, /searchParams\.set\("translation"/);
});
