import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import test from "node:test";

const require = createRequire(import.meta.url);
const ts = require("typescript");
const segmentation = await import("../src/lib/lyrics-segmentation.mjs");

// The editor is TSX, so transpile it in memory and evaluate the real module
// instead of asserting on its source text.
const source = await readFile(new URL("../src/components/lyrics/LyricsLineEditor.tsx", import.meta.url), "utf8");
const transpiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
}).outputText;
const container = { exports: {} };
new Function("require", "exports", "module", transpiled)(
  (specifier) => specifier === "@/lib/lyrics-segmentation.mjs" ? segmentation : require(specifier),
  container.exports,
  container,
);
const { LyricsLineEditor } = container.exports;

function flatten(node, nodes = []) {
  if (Array.isArray(node)) {
    for (const child of node) flatten(child, nodes);
    return nodes;
  }
  if (!node || typeof node !== "object" || !node.props) return nodes;
  if (typeof node.type === "function") return flatten(node.type(node.props), nodes);
  nodes.push(node);
  return flatten(node.props.children, nodes);
}

function renderLine(overrides = {}) {
  const calls = { segment: [], ruby: [], splitRuby: [] };
  const nodes = flatten(LyricsLineEditor({
    line: {
      id: "line-1",
      order: 0,
      japanese: "初音",
      "zh-CN": "",
      "en-US": "",
      segments: [{ text: "初音", performerIds: [], ruby: [{ text: "初" }, { text: "音" }] }],
      trailingPerformerIds: [],
    },
    lineIndex: 0,
    lineCount: 1,
    sourceMutable: true,
    writeLocked: false,
    showPerformerSegmentation: false,
    performers: [],
    performerName: () => "",
    performerColor: () => undefined,
    registerSegmentInput: () => {},
    onUpdateLine: () => {},
    onMoveLine: () => {},
    onRemoveLine: () => {},
    onUpdateSegment: (...args) => calls.segment.push(args),
    onUpdateRubySpan: (...args) => calls.ruby.push(args),
    onSplitRubySpan: (...args) => calls.splitRuby.push(args),
    onMergeRubyWithPrevious: () => {},
    onAddSegment: () => {},
    onSplitSegment: () => {},
    onMergeWithPreviousSegment: () => {},
    onRemoveSegment: () => {},
    onMoveSegment: () => {},
    ...overrides,
  }));
  const labelled = (label) => {
    const node = nodes.find((candidate) => candidate.props["aria-label"] === label);
    assert.ok(node, `missing control: ${label}`);
    return node;
  };
  const buttons = (text) => nodes.filter((node) => node.type === "button" && node.props.children === text);
  return { calls, labelled, buttons };
}

test("revision-0 manual lines accept Japanese segment and ruby input", () => {
  const { calls, labelled, buttons } = renderLine();

  const segmentInput = labelled("第 1 行分段 1");
  const rubyText = labelled("第 1 行分段 1 ruby 1 原文");
  const rubyReading = labelled("第 1 行分段 1 ruby 1 注音");
  assert.equal(segmentInput.props.readOnly, false);
  assert.equal(rubyText.props.readOnly, false);
  assert.equal(rubyReading.props.readOnly, false);

  segmentInput.props.onChange({ target: { value: "初音ミク" } });
  rubyText.props.onChange({ target: { value: "初音" } });
  rubyReading.props.onChange({ target: { value: "はつね" } });
  assert.deepEqual(calls.segment, [[0, "初音ミク", undefined]]);
  assert.deepEqual(calls.ruby, [[0, 0, { text: "初音" }], [0, 0, { reading: "はつね" }]]);

  const splitButtons = buttons("拆分 ruby");
  assert.equal(splitButtons.length, 2);
  assert.equal(splitButtons[0].props.disabled, false);
  splitButtons[1].props.onClick();
  assert.deepEqual(calls.splitRuby, [[0, 1]]);

  const mergeButtons = buttons("与上一 ruby 合并");
  assert.equal(mergeButtons[0].props.disabled, true, "the first span has no previous span");
  assert.equal(mergeButtons[1].props.disabled, false);
});

test("imported or write-locked lines keep segment and ruby inputs read-only", () => {
  for (const overrides of [{ sourceMutable: false }, { writeLocked: true }]) {
    const { labelled, buttons } = renderLine(overrides);

    assert.equal(labelled("第 1 行分段 1").props.readOnly, true);
    assert.equal(labelled("第 1 行分段 1 ruby 1 原文").props.readOnly, true);
    assert.equal(labelled("第 1 行分段 1 ruby 1 注音").props.readOnly, true);
    assert.equal(labelled("第 1 行分段 1 ruby 1 注音").props.placeholder, "注音（已锁死）");
    for (const button of [...buttons("拆分 ruby"), ...buttons("与上一 ruby 合并")]) {
      assert.equal(button.props.disabled, true);
    }
  }
});
