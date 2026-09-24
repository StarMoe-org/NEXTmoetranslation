import assert from "node:assert/strict";
import test from "node:test";

import { loadSourceModule } from "./source-module-harness.mjs";
import { read } from "./source-surfaces.mjs";

const { translationTextareaAction } = loadSourceModule("lib/translation-shortcuts.ts");

function keydown(key, { shiftKey = false, ctrlKey = false, metaKey = false, keyCode = 0, isComposing = false } = {}) {
  return { key, shiftKey, ctrlKey, metaKey, keyCode, nativeEvent: { isComposing } };
}

test("the Enter that confirms an IME candidate never saves in either save mode", () => {
  for (const enterSaves of [true, false]) {
    const shiftKey = !enterSaves;
    assert.equal(translationTextareaAction(keydown("Enter", { shiftKey, keyCode: 13 }), enterSaves), "save");
    assert.equal(translationTextareaAction(keydown("Enter", { shiftKey, keyCode: 229, isComposing: true }), enterSaves), null);
    assert.equal(translationTextareaAction(keydown("Enter", { shiftKey, keyCode: 229 }), enterSaves), null,
      "Safari reports the confirming Enter after compositionend with keyCode 229 only");
  }
});

test("Escape and the navigation shortcuts also belong to the IME while it composes", () => {
  assert.equal(translationTextareaAction(keydown("Escape", { keyCode: 27 }), true), "close");
  assert.equal(translationTextareaAction(keydown("Escape", { keyCode: 229, isComposing: true }), true), null);
  assert.equal(translationTextareaAction(keydown("ArrowUp", { ctrlKey: true, keyCode: 38 }), false), "previous");
  assert.equal(translationTextareaAction(keydown("ArrowDown", { metaKey: true, keyCode: 40 }), false), "next");
  assert.equal(translationTextareaAction(keydown("ArrowDown", { metaKey: true, isComposing: true }), false), null);
  assert.equal(translationTextareaAction(keydown("ArrowDown", { keyCode: 40 }), false), null);
  assert.equal(translationTextareaAction(keydown("a", { keyCode: 65 }), true), null);
});

test("the console translation textarea routes every key through the composition-aware mapping", async () => {
  const consoleSource = await read("src/components/Console.tsx");
  const handler = consoleSource.slice(consoleSource.indexOf("const onTextareaKey"), consoleSource.indexOf("useEffect(", consoleSource.indexOf("const onTextareaKey")));
  assert.match(handler, /const action = translationTextareaAction\(e, enterSaves\);/);
  assert.doesNotMatch(handler, /e\.key ===/, "no key is handled before the composition check");
});
