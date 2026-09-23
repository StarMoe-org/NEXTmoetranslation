import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { readLyricsEditor } from "./source-surfaces.mjs";


import {
  canMergeAdjacentLyricSegments,
  editableLyricSegments,
  lyricGraphemeMidpoint,
  lyricSegmentCanSplit,
  mergeAdjacentLyricRubySpans,
  mergeAdjacentLyricSegments,
  replaceLyricRubySpan,
  replaceLyricSegmentText,
  splitLyricRubySpanAt,
  splitLyricSegmentAt,
} from "../src/lib/lyrics-segmentation.mjs";

test("plain Japanese line text becomes one editable metadata-free segment", () => {
  const segments = editableLyricSegments("まだ歌える", []);

  assert.deepEqual(segments, [{ text: "まだ歌える", performerIds: [], ruby: [{ text: "まだ歌える" }] }]);
  assert.deepEqual(editableLyricSegments("まだ歌える", undefined), [{ text: "まだ歌える", performerIds: [], ruby: [{ text: "まだ歌える" }] }]);
  assert.deepEqual(Object.keys(segments[0]).sort(), ["performerIds", "ruby", "text"]);
  assert.equal(Object.hasOwn(segments[0], "color"), false);
});

test("manual split uses the exact cursor boundary and partitions plain ruby", () => {
  const result = splitLyricSegmentAt([{ text: "歌詞です", performerIds: [] }], 0, 2);

  assert.equal(result.status, "applied");
  assert.equal(result.destructive, false);
  assert.deepEqual(result.segments, [
    { text: "歌詞", performerIds: [], ruby: [{ text: "歌詞" }] },
    { text: "です", performerIds: [], ruby: [{ text: "です" }] },
  ]);
  for (const segment of result.segments) assert.deepEqual(Object.keys(segment).sort(), ["performerIds", "ruby", "text"]);
});

test("segment split preserves complete ruby spans and modeled performer IDs", () => {
  const source = [{
    text: "明日🌸へ",
    performerIds: [7],
    color: "#fff",
    ruby: [{ text: "明日", reading: "あした" }, { text: "🌸" }, { text: "へ", reading: "へ" }],
  }];

  assert.equal(lyricSegmentCanSplit(source[0].text), true);
  assert.equal(splitLyricSegmentAt(source, 0, 0), null, "must not create an empty leading segment");
  assert.equal(splitLyricSegmentAt(source, 0, 3), null, "must not split inside the emoji surrogate pair");
  assert.equal(splitLyricSegmentAt(source, 0, source[0].text.length), null, "must not create an empty trailing segment");
  const result = splitLyricSegmentAt(source, 0, 4);
  assert.equal(result.status, "applied");
  assert.equal(result.destructive, false);
  assert.deepEqual(result.segments, [
    { text: "明日🌸", performerIds: [7], ruby: [{ text: "明日", reading: "あした" }, { text: "🌸" }] },
    { text: "へ", performerIds: [7], ruby: [{ text: "へ", reading: "へ" }] },
  ]);
});

test("splitting inside an annotated span requires confirmation and never duplicates its reading", () => {
  const source = [{
    text: "明日へ",
    performerIds: [7],
    ruby: [{ text: "明日", reading: "あした" }, { text: "へ", reading: "へ" }],
  }];

  assert.deepEqual(splitLyricSegmentAt(source, 0, 1), {
    status: "confirmation-required",
    reason: "annotated-span-split",
  });
  const confirmed = splitLyricSegmentAt(source, 0, 1, true);
  assert.equal(confirmed.status, "applied");
  assert.equal(confirmed.destructive, true);
  assert.deepEqual(confirmed.segments, [
    { text: "明", performerIds: [7], ruby: [{ text: "明" }] },
    { text: "日へ", performerIds: [7], ruby: [{ text: "日" }, { text: "へ", reading: "へ" }] },
  ]);
  assert.equal(JSON.stringify(confirmed).includes("あした"), false);
});

test("segment text edits preserve unaffected annotations and confirm only affected annotation loss", () => {
  const source = {
    text: "歌詞です",
    performerIds: [1],
    ruby: [{ text: "歌詞", reading: "かし" }, { text: "です" }],
  };

  const plainEdit = replaceLyricSegmentText(source, "歌詞でした");
  assert.equal(plainEdit.status, "applied");
  assert.equal(plainEdit.destructive, false);
  assert.deepEqual(plainEdit.segment.ruby, [
    { text: "歌詞", reading: "かし" }, { text: "で" }, { text: "した" },
  ]);

  assert.deepEqual(replaceLyricSegmentText(source, "曲詞です"), {
    status: "confirmation-required",
    reason: "annotation-invalidated",
  });
  const confirmed = replaceLyricSegmentText(source, "曲詞です", true);
  assert.equal(confirmed.status, "applied");
  assert.equal(confirmed.destructive, true);
  assert.deepEqual(confirmed.segment.ruby, [{ text: "曲" }, { text: "詞" }, { text: "です" }]);
});

test("invalid ruby structures require confirmation and confirmed operations rebuild only the affected segment", () => {
  const invalid = {
    text: "歌詞です",
    performerIds: [1],
    ruby: [{ text: "歌詞", reading: "かし" }],
  };

  assert.deepEqual(replaceLyricSegmentText(invalid, "歌詞でした"), {
    status: "confirmation-required",
    reason: "invalid-ruby-structure",
  });
  const edited = replaceLyricSegmentText(invalid, "歌詞でした", true);
  assert.deepEqual(edited, {
    status: "applied",
    destructive: true,
    segment: { text: "歌詞でした", performerIds: [1], ruby: [{ text: "歌詞でした" }] },
  });

  assert.deepEqual(splitLyricSegmentAt([invalid], 0, 2), {
    status: "confirmation-required",
    reason: "invalid-ruby-structure",
  });
  const split = splitLyricSegmentAt([invalid], 0, 2, true);
  assert.equal(split.status, "applied");
  assert.equal(split.destructive, true);
  assert.deepEqual(split.segments, [
    { text: "歌詞", performerIds: [1], ruby: [{ text: "歌詞" }] },
    { text: "です", performerIds: [1], ruby: [{ text: "です" }] },
  ]);
  assert.equal(JSON.stringify(split).includes("かし"), false);
});

test("ruby midpoint splitting follows grapheme boundaries for emoji and decomposed combining text", () => {
  for (const { text, offset, left, right } of [
    { text: "A🌸B", offset: 1, left: "A", right: "🌸B" },
    { text: "e\u0301x", offset: 2, left: "e\u0301", right: "x" },
    { text: "A👩‍🚀B", offset: 1, left: "A", right: "👩‍🚀B" },
  ]) {
    assert.equal(lyricGraphemeMidpoint(text), offset);
    const source = [{ text, performerIds: [1], ruby: [{ text }] }];
    const result = splitLyricRubySpanAt(source, 0, 0, offset);
    assert.equal(result.status, "applied");
    assert.equal(result.destructive, false);
    assert.deepEqual(result.segments[0].ruby, [{ text: left }, { text: right }]);
    assert.equal(result.segments[0].ruby.map((span) => span.text).join(""), text);
    assert.deepEqual(source, [{ text, performerIds: [1], ruby: [{ text }] }]);
  }
  assert.equal(lyricGraphemeMidpoint("🌸"), null);
});

test("ruby midpoint fallback keeps emoji and combining clusters intact without Intl.Segmenter", () => {
  const descriptor = Object.getOwnPropertyDescriptor(Intl, "Segmenter");
  Object.defineProperty(Intl, "Segmenter", { configurable: true, value: undefined });
  try {
    assert.equal(lyricGraphemeMidpoint("e\u0301x"), 2);
    assert.equal(lyricGraphemeMidpoint("A👩‍🚀B"), 1);
    assert.equal(lyricGraphemeMidpoint("A🇯🇵B"), 1);
  } finally {
    if (descriptor) Object.defineProperty(Intl, "Segmenter", descriptor);
    else delete Intl.Segmenter;
  }
});

test("grapheme fallback keeps decomposed Hangul syllables and Indic conjuncts intact", () => {
  const descriptor = Object.getOwnPropertyDescriptor(Intl, "Segmenter");
  Object.defineProperty(Intl, "Segmenter", { configurable: true, value: undefined });
  try {
    for (const { text, boundary, invalidBoundary, left } of [
      { text: "\u1100\u1161X", boundary: 2, invalidBoundary: 1, left: "\u1100\u1161" },
      { text: "\u1100\u1161\u11a8X", boundary: 3, invalidBoundary: 2, left: "\u1100\u1161\u11a8" },
      { text: "\u0915\u094d\u0937X", boundary: 3, invalidBoundary: 2, left: "\u0915\u094d\u0937" },
      { text: "\u0995\u09cd\u09b7X", boundary: 3, invalidBoundary: 2, left: "\u0995\u09cd\u09b7" },
    ]) {
      assert.equal(lyricGraphemeMidpoint(text), boundary);
      const source = [{ text, performerIds: [1], ruby: [{ text }] }];
      assert.equal(splitLyricSegmentAt(source, 0, invalidBoundary), null);
      const result = splitLyricSegmentAt(source, 0, boundary);
      assert.equal(result.status, "applied");
      assert.deepEqual(result.segments.map((segment) => segment.text), [left, "X"]);
    }
  } finally {
    if (descriptor) Object.defineProperty(Intl, "Segmenter", descriptor);
    else delete Intl.Segmenter;
  }
});

test("ruby edits and splits require explicit confirmation before dropping a reading", () => {
  const segments = [{
    text: "歌詞です",
    performerIds: [1],
    ruby: [{ text: "歌詞", reading: "かし" }, { text: "です" }],
  }];

  assert.equal(replaceLyricRubySpan(segments, 0, 0, { text: "曲詞" }).status, "confirmation-required");
  const edited = replaceLyricRubySpan(segments, 0, 0, { text: "曲詞" }, true);
  assert.deepEqual(edited.segments[0].ruby, [{ text: "曲詞" }, { text: "です" }]);
  assert.deepEqual(splitLyricRubySpanAt(segments, 0, 0, 1), {
    status: "confirmation-required",
    reason: "annotated-span-split",
  });
  const split = splitLyricRubySpanAt(segments, 0, 0, 1, true);
  assert.deepEqual(split.segments[0].ruby, [{ text: "歌" }, { text: "詞" }, { text: "です" }]);
  assert.equal(JSON.stringify(split).includes("かし"), false);
});

test("ruby merge preserves compatible readings and confirms incompatible annotation boundaries", () => {
  const annotated = [{
    text: "歌詞",
    performerIds: [1],
    ruby: [{ text: "歌", reading: "か" }, { text: "詞", reading: "し" }],
  }];
  const merged = mergeAdjacentLyricRubySpans(annotated, 0, 0);
  assert.deepEqual(merged.segments[0].ruby, [{ text: "歌詞", reading: "かし" }]);

  const mixed = [{
    text: "歌です",
    performerIds: [1],
    ruby: [{ text: "歌", reading: "うた" }, { text: "です" }],
  }];
  assert.equal(mergeAdjacentLyricRubySpans(mixed, 0, 0).status, "confirmation-required");
  const confirmed = mergeAdjacentLyricRubySpans(mixed, 0, 0, true);
  assert.deepEqual(confirmed.segments[0].ruby, [{ text: "歌です" }]);
});

test("adjacent plain segments merge back while preserving editable ruby spans", () => {
  const merged = mergeAdjacentLyricSegments([
    { text: "歌詞", performerIds: [] },
    { text: "です", performerIds: [] },
  ], 0);

  assert.deepEqual(merged, [{
    text: "歌詞です", performerIds: [], ruby: [{ text: "歌詞" }, { text: "です" }],
  }]);
  assert.deepEqual(Object.keys(merged[0]).sort(), ["performerIds", "ruby", "text"]);
});

test("merge refuses different performer assignments instead of fabricating a combined assignment", () => {
  const segments = [
    { text: "ソロ", performerIds: [1] },
    { text: "デュオ", performerIds: [1, 2] },
  ];

  assert.equal(canMergeAdjacentLyricSegments(segments, 0), false);
  assert.equal(mergeAdjacentLyricSegments(segments, 0), null);
  assert.equal(canMergeAdjacentLyricSegments([
    { text: "重複", performerIds: [1, 1] },
    { text: "指定", performerIds: [1, 1] },
  ], 0), false);
  assert.deepEqual(segments, [
    { text: "ソロ", performerIds: [1] },
    { text: "デュオ", performerIds: [1, 2] },
  ]);
});

test("LyricsEditor centralizes confirmation-aware annotation mutations", async () => {
  const [editor, lineEditor] = await Promise.all([
    readLyricsEditor(),
    readFile(new URL("../src/components/lyrics/LyricsLineEditor.tsx", import.meta.url), "utf8"),
  ]);

  assert.match(editor, /segments: editableLyricSegments\(line\.japanese, line\.segments\)/);
  assert.match(editor, /selectionStart/);
  assert.match(editor, /selectionEnd/);
  assert.match(editor, /splitLyricSegmentAt\(activeLines\[lineIndex\]\.segments, segmentIndex, splitOffset, confirmed\)/);
  assert.match(editor, /const splitOffset = lyricGraphemeMidpoint\(span\.text\)/);
  assert.doesNotMatch(editor, /Math\.floor\(span\.text\.length \/ 2\)/);
  assert.match(editor, /replaceLyricSegmentText\(line\.segments\[segmentIndex\], text, confirmed\)/);
  assert.match(editor, /setPendingAnnotationOperation/);
  assert.match(editor, /确认移除受影响的 ruby 注音/);
  assert.match(editor, /系统不会把一个完整读音复制到拆分后的两边/);
  assert.match(editor, /mergeAdjacentLyricSegments\(segments, segmentIndex - 1\)/);
  assert.match(editor, /<LyricsLineEditor/);
  assert.match(lineEditor, />在光标处分段<\/button>/);
  assert.match(lineEditor, />与上一段合并<\/button>/);
  assert.match(lineEditor, /function LyricRubySpanEditor/);
  assert.match(lineEditor, /function LyricSegmentEditor/);
  assert.doesNotMatch(lineEditor, /value={segment\.text} readOnly=/);
});
