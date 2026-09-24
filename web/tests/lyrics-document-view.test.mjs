import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";

import { createHookRuntime, loadSourceModule } from "./source-module-harness.mjs";

const require = createRequire(import.meta.url);
const { createElement } = require("react");
const { renderToStaticMarkup } = require("react-dom/server");

const esm = {
  "@/lib/lyrics-segmentation.mjs": await import("../src/lib/lyrics-segmentation.mjs"),
  "@/lib/lyrics-versioning.mjs": await import("../src/lib/lyrics-versioning.mjs"),
  "@/lib/performer-colors.mjs": await import("../src/lib/performer-colors.mjs"),
  "@/lib/music-vocals.mjs": await import("../src/lib/music-vocals.mjs"),
};
const runtime = createHookRuntime();
const yjsStub = {
  canonicalLyricsJSON: (value) => JSON.stringify(value),
  lyricsDocumentDirty: (document, baseline, localChanges) =>
    document != null && localChanges !== false && JSON.stringify(document) !== baseline,
  lyricsDocumentSaveable: (document, baseline) => document != null && JSON.stringify(document) !== baseline,
};
const editorState = loadSourceModule("components/lyrics/lyricsEditorState.ts", { ...esm, react: runtime.react, "@/lib/yjs-lyrics": yjsStub });
const model = loadSourceModule("components/lyrics/lyricsDocumentModel.ts", esm);
const { LyricsDocumentView } = loadSourceModule("components/lyrics/LyricsDocumentView.tsx", esm);

const noop = new Proxy({}, { get: () => () => {} });
const music = { musicId: 7, title: { "ja-JP": "テスト曲", "zh-CN": "测试曲" } };
const servedMusic = { ...music, runtimeLyrics: { source: "bundle", state: "complete", availableVersions: ["full"] } };
const withdrawnMusic = { ...music, lyricsWithdrawn: true };

function legacyLyrics(revision) {
  return {
    musicId: 7,
    status: "draft",
    revision,
    updatedAt: "2026-09-01T00:00:00Z",
    attribution: "",
    translationCredit: "译者",
    proofreadingCredit: "",
    sourceUrl: "",
    lines: [{
      id: "line-1", order: 0, japanese: "あいう", "zh-CN": "阿伊乌", "en-US": "",
      segments: [{ text: "あいう", performerIds: [], ruby: [{ text: "あいう" }] }],
    }],
  };
}

function renditionLyrics() {
  return {
    musicId: 7,
    status: "draft",
    revision: 4,
    updatedAt: "2026-09-01T00:00:00Z",
    translationEditionKey: "ed-default",
    defaultTranslationEditionKey: "ed-default",
    translationEditions: [{ key: "ed-default", label: "默认译本" }],
    renditions: [{
      key: "sekai",
      kind: "sekai",
      label: "SEKAI",
      availableVersions: ["full"],
      performers: [],
      full: {
        version: { kind: "sekai", label: "SEKAI" },
        lines: [{
          id: "sekai-line-1", order: 0, japanese: "かきく", "zh-CN": "", "en-US": "",
          segments: [{ text: "かきく", performerIds: [], ruby: [{ text: "かきく", reading: "かきく" }] }],
          trailingPerformerIds: [],
        }],
      },
      relation: { kind: "none" },
      sourceTabPaths: [["SEKAI"]],
      provenance: [],
      translationCredits: { translation: "译者" },
    }],
  };
}

function render(lyrics, overrides = {}) {
  const selectedMusic = overrides.selectedMusic ?? music;
  const state = {
    ...editorState.useLyricsEditorState(false),
    selectedMusic,
    lyrics,
    sourceV3PublicState: model.sourceV3PublicStateFor(lyrics, selectedMusic),
    dirty: true,
    saveable: true,
    collaborationStatus: "synced",
    activeRenditionKey: lyrics.renditions ? "sekai" : "",
    activeTranslationEditionKey: lyrics.renditions ? "ed-default" : "",
    ...overrides,
  };
  const activeTarget = editorState.useLyricsActiveTarget(state);
  return renderToStaticMarkup(createElement(LyricsDocumentView, {
    state, activeTarget, loader: noop, commands: noop, source: noop, persistence: noop,
    role: "admin", publicationChecks: [{ label: "已保存草稿", complete: false }], publicationComplete: false,
  }));
}

function technicalDetails(markup) {
  const start = markup.indexOf('<details class="lyrics-technical-details">');
  assert.ok(start >= 0, "technical details disclosure is missing or open by default");
  const end = markup.indexOf("</details>", start);
  return { start, end, body: markup.slice(start, end) };
}

test("technical cards collapse into one closed 技术详情 disclosure above the lyric lines", () => {
  const markup = render(legacyLyrics(3));
  const details = technicalDetails(markup);
  assert.equal((markup.match(/<details/g) || []).length, 1);
  assert.match(details.body, /<summary>[\s\S]*技术详情[\s\S]*<\/summary>/);
  for (const card of [
    "lyrics-collaboration-state", "lyrics-lock-notice", "lyrics-publication-progress",
    "lyrics-component-provenance", "lyrics-source-metadata", "lyrics-version-notes",
  ]) {
    assert.ok(details.body.includes(`class="${card}`), `${card} belongs inside 技术详情`);
    assert.equal(markup.split(`class="${card}`).length, 2, `${card} renders once`);
  }
  assert.ok(markup.indexOf('class="lyric-line"') > details.end, "lyric lines follow the disclosure");
  const outside = markup.slice(0, details.start) + markup.slice(details.end);
  for (const control of ["保存草稿", "译者名称", 'role="tablist"', "添加 Full 歌词行"]) {
    assert.ok(outside.includes(control), `${control} stays outside the disclosure`);
  }
  assert.ok(details.body.includes("内部来源备注"), "internal notes stay reachable inside the disclosure");
});

test("a collaboration state that locks writes stays visible outside the disclosure", () => {
  for (const [overrides, className] of [
    [{ collaborationStatus: "reconnecting" }, "reconnecting"],
    [{ collaborationStatus: "error", collaborationStructuralConflict: true }, "structural-conflict"],
  ]) {
    const markup = render(legacyLyrics(3), overrides);
    const details = technicalDetails(markup);
    const banner = markup.indexOf(`class="lyrics-collaboration-state ${className}"`);
    assert.ok(banner >= 0 && banner < details.start, `${className} banner precedes the disclosure`);
    assert.equal(details.body.includes("lyrics-collaboration-state"), false);
  }
});

test("saved legacy lyrics no longer claim the source structure is permanently locked", () => {
  const markup = render(legacyLyrics(3));
  assert.doesNotMatch(markup, /永久锁定|已锁定来源|已永久锁定/);
  assert.match(markup, /添加 Full 歌词行/);
  assert.match(markup, />保存草稿</);
  assert.match(markup, /Ruby 注音</);
  assert.doesNotMatch(markup, /可编辑）/);
});

test("source-v3 saves are labelled as publishing and ruby is labelled read-only", () => {
  const markup = render(renditionLyrics(), { selectedMusic: servedMusic });
  assert.match(markup, />保存并公开</);
  assert.doesNotMatch(markup, />保存草稿</);
  assert.match(markup, /Ruby 注音（只读）/);
  assert.doesNotMatch(markup, /Ruby 注音（可编辑）/);
  assert.match(markup, /保存即公开/);
  assert.doesNotMatch(markup, /添加 Full 歌词行/);
});

test("a withdrawn source-v3 song is not labelled as publishing on save", () => {
  const markup = render(renditionLyrics(), { selectedMusic: withdrawnMusic });
  assert.match(markup, />保存（已撤下）</);
  assert.doesNotMatch(markup, />保存并公开</);
  assert.match(markup, /当前已撤下，未公开/);
  assert.doesNotMatch(markup, /保存即公开/);
});

test("a source-v3 song the public mirror does not serve is neither withdrawn nor published on save", () => {
  const markup = render(renditionLyrics(), { selectedMusic: music });
  assert.match(markup, />保存（尚未公开）</);
  assert.doesNotMatch(markup, />保存并公开</);
  assert.doesNotMatch(markup, /已撤下/);
  assert.doesNotMatch(markup, /保存即公开/);
  assert.match(markup, /<strong>当前未公开<\/strong>/);
  assert.match(markup, /也没有撤下记录/);
  assert.match(markup, /翻译或校对署名/);
});

test("the save button follows unsaved shared edits rather than this client's own changes", () => {
  const saveButton = (markup) => markup.match(/<button class="btn btn-primary"[^>]*>保存草稿<\/button>/)?.[0] || "";
  const leftover = saveButton(render(legacyLyrics(3), { dirty: false, saveable: true }));
  assert.ok(leftover, "the save button renders");
  assert.doesNotMatch(leftover, /disabled/, "a collaborator's unsaved edits stay savable without local changes");
  assert.match(saveButton(render(legacyLyrics(3), { dirty: false, saveable: false })), /disabled=""/);
});
